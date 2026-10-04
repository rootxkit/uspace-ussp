//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/alerts"
	alertstore "github.com/rootxkit/uspace-ussp/internal/alerts/pgstore"
	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/cis"
	"github.com/rootxkit/uspace-ussp/internal/geo"
	"github.com/rootxkit/uspace-ussp/internal/intent"
	"github.com/rootxkit/uspace-ussp/internal/policy"
)

// noticeLog keeps every restriction_activated alert on the bus.
type noticeLog struct {
	mu   sync.Mutex
	msgs []geo.NoticeMessage
	at   []time.Time
}

func (l *noticeLog) of(intentID string) []geo.NoticeMessage {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []geo.NoticeMessage
	for _, m := range l.msgs {
		if m.Body.IntentID == intentID {
			out = append(out, m)
		}
	}
	return out
}

// The standing re-check scenario in process (brief WP-12 done-when;
// N-M1 "the USSP raises restriction_activated on the affected intent
// within one tick", S-M4 "a lab constraint from the ANSP triggers
// restriction_activated and an authorisation update"): the ANSP's
// restriction published through the fake CISP withdraws an accepted
// intent (state withdrawn, change_reason, intent/decision/v1 with the
// reason, an audit row, restriction_activated to the operator, recorded
// with no flight), marks an activated one (withdrawn true with the
// window), leaves an intent 5 km away untouched (E-01 twin); the
// restriction ending later raises nothing more and the withdrawn
// intent stays withdrawn.
func TestIntegrationRestrictionRecheck(t *testing.T) {
	g := newIntentRig(t)
	pol := policy.Record{Version: 1, Values: policy.Defaults()}
	pol.Values.ActivationLeadS = 7200
	g.pol.Store(&pol)
	conn := busConn(t, mustEnv(t, "USSP_TEST_NATS_URL"), true)
	pub := bus.NewPublisher(conn, g.counters)
	g.svc.Projector = intent.BusProjector{KV: g.kv, Pub: pub, Notices: geo.NoticeBus{Pub: pub, Counters: g.counters}}
	re := geo.NewRechecker(g.svc, g.cis.eval, g.counters, quiet())
	hook := cis.ChangeHook(re.Changed)
	g.cis.hook.Store(&hook)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go re.Run(ctx)
	g.publishAll(nil, nil, nil)

	// The operator's intents: A (accepted) and C (activated) at the
	// origin, at two altitudes; B 5 km east.
	number, serial, token := g.operatorWith("ussp.intents")
	a := g.file(token, g.request(number, serial, "a-"+unique(), g.box(0.001, 0.001, 0.004)))
	cReq := g.request(number, serial, "c-"+unique(), g.box(0.001, 0.001, 0.004))
	start, end := window()
	cReq["volumes"] = []any{g.volume(g.box(0.001, 0.001, 0.004), 400, 450, start, end)}
	c := g.file(token, cReq)
	b := g.file(token, g.request(number, serial, "b-"+unique(), g.box(0.001, 0.07, 0.004)))
	for _, d := range []resp{a, b, c} {
		if d.status != 201 || d.str("state") != "accepted" {
			t.Fatalf("file: %d %s", d.status, d.raw)
		}
	}
	if r := g.stack.call("PATCH", "/v1/intents/"+c.str("intent_id"), map[string]any{"action": "activate"}, bearer(token)); r.status != 200 || r.str("state") != "activated" {
		t.Fatalf("activate: %d %s", r.status, r.raw)
	}
	// Every restriction_activated on the bus, and api's alerts record.
	log := &noticeLog{}
	stop, err := conn.Listen("alrt.v1.restriction_activated.>", func(_ string, data []byte) {
		var m geo.NoticeMessage
		if json.Unmarshal(data, &m) == nil {
			log.mu.Lock()
			log.msgs, log.at = append(log.msgs, m), append(log.at, time.Now())
			log.mu.Unlock()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	rec := &alerts.Recorder{
		Source: &bus.StreamSource{Open: bus.PullOpener(conn.JetStream(), bus.DefaultTopology(), bus.StreamALRT, bus.PullSpec{
			Durable: "it-notices-" + unique(), FilterSubject: "alrt.v1.restriction_activated.>", MaxAckPending: 256,
		})},
		Store: alertstore.Store{S: appStore(t)}, Logger: quiet(),
	}
	go rec.Run(ctx)

	// The ANSP activates a restriction over the origin (published by the
	// fake CISP as the ANSP's): installed, then re-checked.
	now := time.Now().UTC()
	u := unique()
	rid := "TR" + u[len(u)-5:]
	rsFeature := geoFeature(rid, "PROHIBITED", g.box(-0.002, -0.002, 0.01), 0, 3000, period(now.Add(-time.Minute), now.Add(3*time.Hour)),
		restrictionExt(rid, "active", now.Add(-time.Minute), now.Add(3*time.Hour)))
	published := time.Now()
	g.publish(cis.Restrictions, rsFeature)
	within(t, 10*time.Second, func() bool { return len(log.of(a.str("intent_id"))) == 1 && len(log.of(c.str("intent_id"))) == 1 })
	log.mu.Lock()
	first := log.at[0]
	log.mu.Unlock()
	t.Logf("restriction %s installed to restriction_activated on the bus: %v", rid, first.Sub(published))
	if d := first.Sub(published); d > 2*time.Second {
		t.Fatalf("restriction_activated %v after the publication, beyond one tick", d)
	}
	na := log.of(a.str("intent_id"))[0]
	var detail map[string]any
	_ = json.Unmarshal(na.Body.Detail, &detail)
	if na.Body.Severity != core.SeverityCritical || na.Body.FlightID != nil || detail["decision"] != "withdrawn" || detail["withdrawn"] != true ||
		detail["restriction_id"] != rid || detail["change_reason"] != "restriction "+rid {
		t.Fatalf("A's notice %+v %v", na.Body, detail)
	}
	ga := g.stack.call("GET", "/v1/intents/"+a.str("intent_id"), nil, bearer(token))
	if ga.str("state") != "withdrawn" || ga.str("change_reason") != "restriction "+rid {
		t.Fatalf("A: %s", ga.raw)
	}
	if n := events(t, "operational_intent", a.str("intent_id"), intent.EventWithdrawn); n != 1 {
		t.Fatalf("A's audit rows: %d", n)
	}
	nc := log.of(c.str("intent_id"))[0]
	_ = json.Unmarshal(nc.Body.Detail, &detail)
	win, _ := detail["window"].(map[string]any)
	if detail["decision"] != "marked" || detail["withdrawn"] != true || detail["intent_state"] != "activated" || win == nil || win["ends_at"] == nil {
		t.Fatalf("C's notice %v", detail)
	}
	if gc := g.stack.call("GET", "/v1/intents/"+c.str("intent_id"), nil, bearer(token)); gc.str("state") != "activated" || gc.str("change_reason") != "restriction "+rid {
		t.Fatalf("C: %s", gc.raw)
	}
	if gb := g.stack.call("GET", "/v1/intents/"+b.str("intent_id"), nil, bearer(token)); gb.str("state") != "accepted" || gb.body["version"].(float64) != 1 {
		t.Fatalf("B (5 km away): %s", gb.raw)
	}
	if n := len(log.of(b.str("intent_id"))); n != 0 {
		t.Fatalf("B told %d times", n)
	}
	// Recorded by api's alerts record, with no flight.
	db := relOwner(t)
	within(t, 10*time.Second, func() bool {
		return count(t, db, "SELECT count(*) FROM alerts WHERE kind = 'restriction_activated' AND flight_id IS NULL AND intent_id = ANY($1::uuid[])",
			[]string{a.str("intent_id"), c.str("intent_id")}) == 2
	})
	// The restriction ends: nothing more, and A stays withdrawn.
	ended := geoFeature(rid, "PROHIBITED", g.box(-0.002, -0.002, 0.01), 0, 3000, period(now.Add(-time.Minute), now.Add(3*time.Hour)),
		restrictionExt(rid, "ended", now.Add(-time.Minute), now.Add(3*time.Hour)))
	runs := g.counters.Get(geo.CounterRecheckRuns)
	g.publish(cis.Restrictions, ended)
	// The re-check of the ended restriction ran (and a sweep after it
	// would change nothing either).
	within(t, 10*time.Second, func() bool { return g.counters.Get(geo.CounterRecheckRuns) > runs })
	if n := len(log.of(a.str("intent_id"))) + len(log.of(c.str("intent_id"))); n != 2 {
		t.Fatalf("an ended restriction raised more: %d notices", n)
	}
	if ga := g.stack.call("GET", "/v1/intents/"+a.str("intent_id"), nil, bearer(token)); ga.str("state") != "withdrawn" {
		t.Fatalf("A after the end: %s", ga.raw)
	}
	// A traffic-ws that restarted holds the open notices again: api
	// republishes them from its record (both intents are not over), and
	// the closing pass leaves them open.
	svc := &alerts.Service{Store: alertstore.Store{S: appStore(t)}, Bus: pub, Logger: quiet()}
	if n, err := svc.RepublishOpenNotices(ctx); err != nil || n < 2 {
		t.Fatalf("republished %d: %v", n, err)
	}
	within(t, 10*time.Second, func() bool { return len(log.of(a.str("intent_id"))) == 2 && len(log.of(c.str("intent_id"))) == 2 })
	if again := log.of(a.str("intent_id"))[1]; again.Body.AlertID != na.Body.AlertID || again.Body.State != geo.StateRaised {
		t.Fatalf("A's notice republished as %+v", again.Body)
	}
}

// A planned restriction is not in force (spec 02 F2): published over an
// activated intent it raises nothing, even with its starts_at come; the
// same restriction activated raises restriction_activated within one
// tick; ended, api's alerts record clears that alert resolved while the
// intent keeps its change_reason (the lab's ussp-wp12-restriction plan,
// activate and end steps, in process; presence and absence pairs).
func TestIntegrationPlannedRestrictionRaisesOnActivationAndClearsOnEnd(t *testing.T) {
	g := newIntentRig(t)
	pol := policy.Record{Version: 1, Values: policy.Defaults()}
	pol.Values.ActivationLeadS = 7200
	g.pol.Store(&pol)
	conn := busConn(t, mustEnv(t, "USSP_TEST_NATS_URL"), true)
	pub := bus.NewPublisher(conn, g.counters)
	g.svc.Projector = intent.BusProjector{KV: g.kv, Pub: pub, Notices: geo.NoticeBus{Pub: pub, Counters: g.counters}}
	re := geo.NewRechecker(g.svc, g.cis.eval, g.counters, quiet())
	hook := cis.ChangeHook(re.Changed)
	g.cis.hook.Store(&hook)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go re.Run(ctx)
	g.publishAll(nil, nil, nil)

	number, serial, token := g.operatorWith("ussp.intents")
	c := g.file(token, g.request(number, serial, "p-"+unique(), g.box(0.001, 0.001, 0.004)))
	if c.status != 201 || c.str("state") != "accepted" {
		t.Fatalf("file: %d %s", c.status, c.raw)
	}
	id := c.str("intent_id")
	if r := g.stack.call("PATCH", "/v1/intents/"+id, map[string]any{"action": "activate"}, bearer(token)); r.status != 200 || r.str("state") != "activated" {
		t.Fatalf("activate: %d %s", r.status, r.raw)
	}
	log := &noticeLog{}
	stop, err := conn.Listen("alrt.v1.restriction_activated.>", func(_ string, data []byte) {
		var m geo.NoticeMessage
		if json.Unmarshal(data, &m) == nil {
			log.mu.Lock()
			log.msgs, log.at = append(log.msgs, m), append(log.at, time.Now())
			log.mu.Unlock()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	rec := &alerts.Recorder{
		Source: &bus.StreamSource{Open: bus.PullOpener(conn.JetStream(), bus.DefaultTopology(), bus.StreamALRT, bus.PullSpec{
			Durable: "it-planned-" + unique(), FilterSubject: "alrt.v1.restriction_activated.>", MaxAckPending: 256,
		})},
		Store: alertstore.Store{S: appStore(t)}, Logger: quiet(),
	}
	go rec.Run(ctx)

	now := time.Now().UTC()
	u := unique()
	rid := "TP" + u[len(u)-5:]
	feature := func(state string) json.RawMessage {
		return geoFeature(rid, "PROHIBITED", g.box(-0.002, -0.002, 0.01), 0, 3000, period(now, now.Add(3*time.Hour)),
			restrictionExt(rid, state, now, now.Add(3*time.Hour)))
	}
	// Planned with its start now: the re-check runs and tells nothing.
	runs := g.counters.Get(geo.CounterRecheckRuns)
	g.publish(cis.Restrictions, feature("planned"))
	within(t, 10*time.Second, func() bool { return g.counters.Get(geo.CounterRecheckRuns) > runs })
	if n := len(log.of(id)); n != 0 {
		t.Fatalf("a planned restriction raised %d notices", n)
	}
	if gc := g.stack.call("GET", "/v1/intents/"+id, nil, bearer(token)); gc.str("state") != "activated" || gc.body["version"].(float64) != 2 {
		t.Fatalf("after the plan: %s", gc.raw)
	}
	// Activated: raised within one tick.
	published := time.Now()
	g.publish(cis.Restrictions, feature("active"))
	within(t, 10*time.Second, func() bool { return len(log.of(id)) == 1 })
	log.mu.Lock()
	raisedAfter := log.at[0].Sub(published)
	log.mu.Unlock()
	t.Logf("restriction %s activated to restriction_activated on the bus: %v", rid, raisedAfter)
	if raisedAfter > 2*time.Second {
		t.Fatalf("restriction_activated %v after the activation, beyond one tick", raisedAfter)
	}
	raised := log.of(id)[0]
	if raised.Body.State != geo.StateRaised || raised.Body.Severity != core.SeverityCritical {
		t.Fatalf("raised %+v", raised.Body)
	}
	db := relOwner(t)
	within(t, 10*time.Second, func() bool {
		return count(t, db, "SELECT count(*) FROM alerts WHERE id = $1::uuid AND cleared_at IS NULL", raised.Body.AlertID) == 1
	})
	// While it is active the clearing pass leaves it open (the twin).
	// The record is shared with the other tests: what this pass clears
	// of theirs (restrictions this rig's CIS does not hold) is theirs.
	svc := &alerts.Service{Store: alertstore.Store{S: appStore(t)}, Bus: pub, Restrictions: g.cis.eval, Logger: quiet()}
	if _, err := svc.ClearLiftedNotices(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(log.of(id)); n != 1 {
		t.Fatalf("the clearing pass published %d messages of an active restriction's notice", n-1)
	}
	// Ended: the pass clears it resolved, recorded by api's record.
	g.publish(cis.Restrictions, feature("ended"))
	within(t, 10*time.Second, func() bool {
		lifted, judged := g.cis.eval.RestrictionLifted(rid)
		return lifted && judged
	})
	if n, err := svc.ClearLiftedNotices(ctx); err != nil || n < 1 {
		t.Fatalf("cleared on the end: %d %v", n, err)
	}
	within(t, 10*time.Second, func() bool {
		return count(t, db, "SELECT count(*) FROM alerts WHERE id = $1::uuid AND cleared_at IS NOT NULL AND clear_reason = 'resolved'",
			raised.Body.AlertID) == 1
	})
	ms := log.of(id)
	last := ms[len(ms)-1]
	if last.Body.AlertID != raised.Body.AlertID || last.Body.State != "cleared" || last.Body.ClearReason == nil || *last.Body.ClearReason != "resolved" {
		t.Fatalf("clear on the bus %+v", last.Body)
	}
	// The intent keeps its state and change_reason; nothing more is open.
	if gc := g.stack.call("GET", "/v1/intents/"+id, nil, bearer(token)); gc.str("state") != "activated" || gc.str("change_reason") != "restriction "+rid {
		t.Fatalf("after the end: %s", gc.raw)
	}
	before := len(log.of(id))
	if _, err := svc.ClearLiftedNotices(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(log.of(id)); n != before {
		t.Fatalf("cleared twice: %d messages after the record took the clear", n-before)
	}
}
