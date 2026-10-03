package dss

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3548"
	"github.com/rootxkit/uspace-core/geodesy"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/intent"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/peeruss"
)

// call makes a request to our F3548 endpoints as sub.
func (g *rig) call(method, path, sub string, body any) (int, []byte) {
	g.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, g.us.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if sub != "" {
		req.Header.Set("Authorization", "Bearer "+fakeJWT(sub))
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		g.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b
}

// Our intent's details as the DSS holds it, for a peer; 404 for one we
// do not manage, or that the DSS does not hold.
func TestServerDetails(t *testing.T) {
	g := newRig(t)
	g.in.put(pending(oursID, volume(41.7, 44.8)))
	if st, _ := g.call(http.MethodGet, "/uss/v1/operational_intents/"+oursID, peerManager, nil); st != http.StatusNotFound {
		t.Fatalf("an intent the DSS does not hold: %d", st)
	}
	if err := g.w.Mirror(context.Background(), oursID); err != nil {
		t.Fatal(err)
	}
	st, b := g.call(http.MethodGet, "/uss/v1/operational_intents/"+oursID, peerManager, nil)
	var d f3548.GetOperationalIntentDetailsResponse
	if st != http.StatusOK || json.Unmarshal(b, &d) != nil || d.OperationalIntent.Reference.Id != oursID ||
		d.OperationalIntent.Reference.Ovn == nil || d.OperationalIntent.Details.Priority == nil || len(*d.OperationalIntent.Details.Volumes) != 1 {
		t.Fatalf("%d %s", st, b)
	}
	if _, err := f3548.UnmarshalOperationalIntent(mustJSON(t, d.OperationalIntent)); err != nil {
		t.Fatalf("our details do not read as F3548: %v", err)
	}
	for _, id := range []string{peerID, "not-a-uuid"} {
		if st, _ := g.call(http.MethodGet, "/uss/v1/operational_intents/"+id, peerManager, nil); st != http.StatusNotFound {
			t.Fatalf("%s: %d", id, st)
		}
	}
	if g.count(CounterDetailsServed) != 1 || g.count(CounterDetailsNotFound) != 3 {
		t.Fatalf("%v", g.counters.Snapshot())
	}
	// Every exchange of the endpoints and of the writer is in the log set
	// of the intent.
	go g.exlog.Run(t.Context())
	within(t, 5*time.Second, func() bool {
		_, b := g.call(http.MethodGet, "/uss/v1/log_sets/"+oursID, peerManager, nil)
		var ls f3548.USSLogSet
		_ = json.Unmarshal(b, &ls)
		var client, server bool
		for _, m := range *ls.Messages {
			client = client || m.RecorderRole == f3548.Client
			server = server || m.RecorderRole == f3548.Server
		}
		return client && server
	})
	_, b = g.call(http.MethodGet, "/uss/v1/log_sets/"+oursID, peerManager, nil)
	if bytes.Contains(b, []byte("Bearer")) {
		t.Fatal("a bearer token reached the exchange log")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type fakeTelemetry struct {
	t  *f3548.VehicleTelemetry
	ok bool
}

func (f fakeTelemetry) Latest(context.Context, string, time.Time) (*f3548.VehicleTelemetry, bool, error) {
	return f.t, f.ok, nil
}

// Telemetry is served for a Nonconforming or Contingent intent with a
// recent sample: 404 not ours, 409 nominal, 412 no store or no sample.
func TestServerTelemetry(t *testing.T) {
	g := newRig(t)
	path := "/uss/v1/operational_intents/" + oursID + "/telemetry"
	if st, _ := g.call(http.MethodGet, path, peerManager, nil); st != http.StatusNotFound {
		t.Fatalf("%d", st)
	}
	g.in.put(pending(oursID, volume(41.7, 44.8)))
	if err := g.w.Mirror(context.Background(), oursID); err != nil {
		t.Fatal(err)
	}
	if st, _ := g.call(http.MethodGet, path, peerManager, nil); st != http.StatusConflict {
		t.Fatalf("nominal: %d", st)
	}
	nc := string(f3548.Nonconforming)
	g.in.setState(oursID, intent.StateNonconforming, &nc)
	if err := g.w.Mirror(context.Background(), oursID); err != nil {
		t.Fatal(err)
	}
	if st, _ := g.call(http.MethodGet, path, peerManager, nil); st != http.StatusPreconditionFailed {
		t.Fatalf("no store: %d", st)
	}
	g.srv.Telemetry = fakeTelemetry{}
	if st, _ := g.call(http.MethodGet, path, peerManager, nil); st != http.StatusPreconditionFailed {
		t.Fatalf("no sample: %d", st)
	}
	lat, lng := 41.7, 44.8
	g.srv.Telemetry = fakeTelemetry{ok: true, t: &f3548.VehicleTelemetry{TimeMeasured: f3548.Time{Format: f3548.RFC3339, Value: time.Now()},
		Position: &f3548.Position{Latitude: &lat, Longitude: &lng}}}
	st, b := g.call(http.MethodGet, path, peerManager, nil)
	var r f3548.GetOperationalIntentTelemetryResponse
	if st != http.StatusOK || json.Unmarshal(b, &r) != nil || r.OperationalIntentId != oursID || r.Telemetry == nil || r.NextTelemetryOpportunity == nil {
		t.Fatalf("%d %s", st, b)
	}
}

type recPublisher struct{ got []PeerIntentBody }

func (p *recPublisher) PublishPeerIntent(_ context.Context, b PeerIntentBody) error {
	p.got = append(p.got, b)
	return nil
}

// A peer's notification: stored by version as trust provider, published,
// judged against our intents; a deletion removes it; a notification
// about another manager's intent is refused 403, one about ours ignored,
// a malformed one 400; an index going backwards is counted and applied.
func TestServerPeerNotifications(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	pub := &recPublisher{}
	g.srv.Publisher = pub
	ref, err := g.peer.File(ctx, peeruss.Spec{ID: peerID, Volumes: []f3548.Volume4D{volume(41.7, 44.8)}})
	if err != nil {
		t.Fatal(err)
	}
	subID := SubscriptionID(g.us.URL, "TUA001")
	if err := g.st.UpsertSubscription(ctx, SubscriptionRecord{ID: subID, NotificationIndex: 5}); err != nil {
		t.Fatal(err)
	}
	g.in.conflicts = []intent.PeerConflict{{IntentID: oursID, PeerWinsByPriority: false}, {IntentID: oursID}, {IntentID: peer2ID, PeerWinsByPriority: true}}
	st, err := g.peer.NotifyTo(ctx, g.us.URL, peerID, []f3548.SubscriptionState{{SubscriptionId: subID, NotificationIndex: 3}, {SubscriptionId: peer2ID, NotificationIndex: 1}})
	if err != nil || st != http.StatusNoContent {
		t.Fatalf("%d %v", st, err)
	}
	p, _ := g.st.PeerIntent(ctx, peerID)
	if p == nil || p.Manager != peerManager || p.Version != int64(ref.Version) || p.OVN != *ref.Ovn {
		t.Fatalf("stored %+v", p)
	}
	if len(pub.got) != 1 || pub.got[0].Trust != core.TrustProvider || pub.got[0].Deleted || pub.got[0].Details == nil {
		t.Fatalf("published %+v", pub.got)
	}
	// The message is peer/intent/v1.
	validatePeerIntent(t, pub.got[0])
	if g.count(CounterIndexBackwards) != 1 || g.count(CounterIndexUnknown) != 1 {
		t.Fatalf("%v", g.counters.Snapshot())
	}
	if len(g.st.auditsOf("dss_conflict_reported")) != 1 || len(g.in.displaced) != 1 || g.in.displaced[0] != peer2ID+"<"+peerID {
		t.Fatalf("audits %v displaced %v", g.st.audits, g.in.displaced)
	}
	// An older version than the one held changes nothing.
	old := *p
	old.Version--
	if w, _ := g.st.UpsertPeerIntent(ctx, old); w {
		t.Fatal("an older version replaced a newer one")
	}
	// Another USS speaking for the peer's intent: 403.
	oi, _ := g.peer.Intent(peerID)
	body := f3548.PutOperationalIntentDetailsParameters{OperationalIntentId: peerID, OperationalIntent: &oi, Subscriptions: []f3548.SubscriptionState{}}
	if st, _ := g.call(http.MethodPost, "/uss/v1/operational_intents", "someone-else", body); st != http.StatusForbidden {
		t.Fatalf("not the manager: %d", st)
	}
	// About one of ours: ignored, nothing stored.
	mine := oi
	mine.Reference.Manager, mine.Reference.Id = ourManager, oursID
	body = f3548.PutOperationalIntentDetailsParameters{OperationalIntentId: oursID, OperationalIntent: &mine, Subscriptions: []f3548.SubscriptionState{}}
	if st, _ := g.call(http.MethodPost, "/uss/v1/operational_intents", ourManager, body); st != http.StatusNoContent || g.count(CounterPeerOwnIgnored) != 1 {
		t.Fatalf("own: %d", st)
	}
	if p, _ := g.st.PeerIntent(ctx, oursID); p != nil {
		t.Fatal("our own intent stored as a peer's")
	}
	// Malformed: 400.
	for _, b := range []any{
		map[string]any{"operational_intent_id": "nope", "subscriptions": []any{}},
		map[string]any{"operational_intent_id": peerID, "subscriptions": []any{}, "operational_intent": map[string]any{"reference": map[string]any{}}},
		f3548.PutOperationalIntentDetailsParameters{OperationalIntentId: peer2ID, OperationalIntent: &oi, Subscriptions: []f3548.SubscriptionState{}},
	} {
		if st, _ := g.call(http.MethodPost, "/uss/v1/operational_intents", peerManager, b); st != http.StatusBadRequest {
			t.Fatalf("%v: %d", b, st)
		}
	}
	// The deletion removes it and publishes the deletion.
	if err := g.peer.Delete(ctx, peerID); err != nil {
		// Delete notifies the subscribers the DSS lists; we are none here.
		t.Fatal(err)
	}
	if st, err := g.peer.NotifyTo(ctx, g.us.URL, peerID, nil); err != nil || st != http.StatusNoContent {
		t.Fatalf("%d %v", st, err)
	}
	if p, _ := g.st.PeerIntent(ctx, peerID); p != nil || g.count(CounterPeerDeleted) != 1 || !pub.got[len(pub.got)-1].Deleted {
		t.Fatalf("not deleted: %+v %v", p, g.counters.Snapshot())
	}
	validatePeerIntent(t, pub.got[len(pub.got)-1])
}

// validatePeerIntent checks a message against schemas/peer/intent/v1.
func validatePeerIntent(t *testing.T, b PeerIntentBody) {
	t.Helper()
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	for name, file := range map[string]string{"envelope/v1": "../../schemas/envelope/v1/schema.json", "peer/intent/v1": "../../schemas/peer/intent/v1/schema.json"} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.AddResource("https://schemas.uspace.ge/"+name+".json", doc); err != nil {
			t.Fatal(err)
		}
	}
	s, err := c.Compile("https://schemas.uspace.ge/peer/intent/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	m := struct {
		bus.Envelope
		Body PeerIntentBody `json:"body"`
	}{Envelope: bus.SystemEnvelope(SchemaPeerIntent, intent.Producer, time.Now()), Body: b}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(mustJSON(t, m)))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(inst); err != nil {
		t.Fatalf("peer/intent/v1: %v\n%s", err, mustJSON(t, m))
	}
}

type recRechecker struct {
	refs  []string
	boxes [][]geodesy.BBox
}

func (r *recRechecker) Constraint(_ context.Context, ref string, boxes []geodesy.BBox, _, _ *time.Time) []intent.RecheckResult {
	r.refs = append(r.refs, ref)
	r.boxes = append(r.boxes, boxes)
	return []intent.RecheckResult{{Outcome: intent.RecheckUntouched}}
}

// A constraint notification: stored, joined to the CIS restriction its
// geozone names, and re-checked over its volumes (WP-12's
// restriction_activated path); a deletion removes it; a malformed one is
// 400 and another manager's 403. Our constraint details: 404 (we manage
// none).
func TestServerConstraints(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	re := &recRechecker{}
	g.srv.Constraints = re
	const cid = "44444444-4444-4444-8444-444444444444"
	if _, err := g.peer.Constrain(ctx, cid, []f3548.Volume4D{volume(41.7, 44.8)}, "TRS001"); err != nil {
		t.Fatal(err)
	}
	c, _ := g.peer.Constraint(cid)
	body := f3548.PutConstraintDetailsParameters{ConstraintId: cid, Constraint: &c, Subscriptions: []f3548.SubscriptionState{}}
	if st, b := g.call(http.MethodPost, "/uss/v1/constraints", peerManager, body); st != http.StatusNoContent {
		t.Fatalf("%d %s", st, b)
	}
	stored, _ := g.st.Constraint(ctx, cid)
	if stored == nil || stored.CISRestrictionID != "TRS001" || stored.OVN == "" || len(re.refs) != 1 || re.refs[0] != "TRS001" || len(re.boxes[0]) != 1 {
		t.Fatalf("stored %+v rechecked %v", stored, re.refs)
	}
	if st, _ := g.call(http.MethodPost, "/uss/v1/constraints", "someone-else", body); st != http.StatusForbidden {
		t.Fatalf("not the manager: %d", st)
	}
	for _, b := range []any{
		map[string]any{"constraint_id": "nope", "subscriptions": []any{}},
		f3548.PutConstraintDetailsParameters{ConstraintId: peerID, Constraint: &c, Subscriptions: []f3548.SubscriptionState{}},
	} {
		if st, _ := g.call(http.MethodPost, "/uss/v1/constraints", peerManager, b); st != http.StatusBadRequest {
			t.Fatalf("%v: %d", b, st)
		}
	}
	empty := c
	empty.Details.Volumes = nil
	if st, _ := g.call(http.MethodPost, "/uss/v1/constraints", peerManager,
		f3548.PutConstraintDetailsParameters{ConstraintId: cid, Constraint: &empty, Subscriptions: []f3548.SubscriptionState{}}); st != http.StatusBadRequest {
		t.Fatalf("no volume: %d", st)
	}
	if st, _ := g.call(http.MethodPost, "/uss/v1/constraints", peerManager,
		f3548.PutConstraintDetailsParameters{ConstraintId: cid, Subscriptions: []f3548.SubscriptionState{}}); st != http.StatusNoContent {
		t.Fatalf("delete: %d", st)
	}
	if s, _ := g.st.Constraint(ctx, cid); s != nil || g.count(CounterConstraintDeleted) != 1 {
		t.Fatalf("not deleted %+v", s)
	}
	if st, _ := g.call(http.MethodGet, "/uss/v1/constraints/"+cid, peerManager, nil); st != http.StatusNotFound {
		t.Fatalf("our constraint details: %d", st)
	}
}

// A report is stored with the id it is given back; a log set of an
// unknown id is empty.
func TestServerReportsAndLogSets(t *testing.T) {
	g := newRig(t)
	rep := f3548.ErrorReport{Exchange: f3548.ExchangeRecord{Method: "GET", Url: "http://x/y", RecorderRole: f3548.Client,
		RequestTime: f3548.Time{Format: f3548.RFC3339, Value: time.Now()}}}
	st, b := g.call(http.MethodPost, "/uss/v1/reports", peerManager, rep)
	var out f3548.ErrorReport
	if st != http.StatusCreated || json.Unmarshal(b, &out) != nil || out.ReportId == nil || g.st.reports[*out.ReportId] == nil {
		t.Fatalf("%d %s", st, b)
	}
	st, b = g.call(http.MethodGet, "/uss/v1/log_sets/"+peer2ID, peerManager, nil)
	var ls f3548.USSLogSet
	if st != http.StatusOK || json.Unmarshal(b, &ls) != nil || ls.Messages == nil || len(*ls.Messages) != 0 {
		t.Fatalf("%d %s", st, b)
	}
	if st, _ := g.call(http.MethodGet, "/uss/v1/log_sets/x", peerManager, nil); st != http.StatusOK {
		t.Fatalf("%d", st)
	}
	r := recordOf(Exchange{Method: "PUT", URL: "u", Role: RoleClient, RequestBody: "{}", ResponseCode: 409, ResponseBody: "{}",
		ResponseTime: time.Now(), Problem: "p"})
	if r.ResponseCode == nil || *r.ResponseCode != 409 || r.Problem == nil || r.RequestBody == nil || r.ResponseTime == nil {
		t.Fatalf("%+v", r)
	}
}
