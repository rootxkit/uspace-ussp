//go:build integration && lab

package integration

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"
	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3548"
	"github.com/rootxkit/uspace-core/geodesy"

	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/dss"
	dsspg "github.com/rootxkit/uspace-ussp/internal/dss/pgstore"
	"github.com/rootxkit/uspace-ussp/internal/intent"
	"github.com/rootxkit/uspace-ussp/internal/stdapi"
)

// The S-M4 lab scenario (brief WP-13, INV-02; docs/RUNBOOKS/WP-13.md):
// this USSP's strategic coordination against the lab's InterUSS DSS
// (uspace-lab WP-L2, the image of deploy/dss/SOURCE) and the lab's second
// USSP (sim-ussp, WP-L5), with tokens of the lab issuer. It runs in a
// container on the lab network (the network is internal); the databases
// are this run's, attached to it. Every number it measures is logged for
// the runbook. The DSS outage is made from the host, by stopping and
// starting the DSS container when the test asks for it through
// LAB_SIGNAL_DIR.
//
// Environment: LAB_DSS_URL, LAB_TOKEN_URL, LAB_ISSUER, LAB_JWKS,
// LAB_CLIENT_ID, LAB_CLIENT_SECRET (our client), LAB_ARBITER_ID,
// LAB_ARBITER_SECRET (lab-01, which may set a USS's availability, as the
// authority does), LAB_SELF_HOST, LAB_LISTEN, LAB_SIM_INTENT, LAB_SIGNAL_DIR.

func labEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("%s is not set: the lab scenario runs on the lab network (docs/RUNBOOKS/WP-13.md)", name)
	}
	return v
}

// signal asks the host to do what (stop-dss, start-dss) and waits for
// its acknowledgement file.
func signal(t *testing.T, dir, what string) {
	t.Helper()
	if err := os.WriteFile(dir+"/"+what, []byte(time.Now().UTC().Format(time.RFC3339Nano)), 0o600); err != nil {
		t.Fatal(err)
	}
	within(t, 90*time.Second, func() bool { _, err := os.Stat(dir + "/" + what + ".done"); return err == nil })
}

func TestLabSM4(t *testing.T) {
	dssURL, tokenURL := labEnv(t, "LAB_DSS_URL"), labEnv(t, "LAB_TOKEN_URL")
	issuer, jwks := labEnv(t, "LAB_ISSUER"), labEnv(t, "LAB_JWKS")
	clientID, secret := labEnv(t, "LAB_CLIENT_ID"), labEnv(t, "LAB_CLIENT_SECRET")
	arbiterID, arbiterSecret := labEnv(t, "LAB_ARBITER_ID"), labEnv(t, "LAB_ARBITER_SECRET")
	self, listen, simIntent, signals := labEnv(t, "LAB_SELF_HOST"), labEnv(t, "LAB_LISTEN"), labEnv(t, "LAB_SIM_INTENT"), labEnv(t, "LAB_SIGNAL_DIR")
	ctx := context.Background()

	g := newIntentRig(t)
	cleanDSSOutbox(t)
	t.Cleanup(func() { cleanDSSOutbox(t) })
	// The U-space airspace around the lab's peer (SIM_USSP_INTENT is at
	// 41.73, 44.85).
	airspace := [4]float64{41.60, 44.70, 41.90, 45.00}
	g.publishAll(nil, []json.RawMessage{edFeature("TSA-LAB", "USPACE", airspace, 0, 3000, "AMSL", requirementsExt(nil))}, nil)
	counters := &core.Counters{}
	st := dsspg.Store{S: appStore(t)}
	exlog := dss.NewExchangeLog(st, counters, quiet())
	out, err := auth.NewOutgoing(auth.OutgoingConfig{TokenURL: tokenURL, ClientID: clientID, ClientSecret: secret})
	if err != nil {
		t.Fatal(err)
	}
	client := &dss.Client{DSSBaseURL: dssURL, Tokens: out, HTTP: &http.Client{Timeout: 5 * time.Second, Transport: &dss.Transport{Log: exlog}}}
	avail := &dss.Availability{Client: client, Store: st, USSID: clientID, Counters: counters}
	g.svc.Decider.DSS = &dss.Gate{Client: client, Availability: avail}
	v, err := auth.NewVerifier(ctx, auth.VerifierConfig{Ecosystem: coreauth.Config{
		Issuers: map[string]coreauth.IssuerConfig{issuer: {JWKSURL: jwks}}, Audiences: []string{self}, StrictSessionClaims: true}})
	if err != nil {
		t.Fatal(err)
	}
	rctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); wg.Wait() })
	wg.Go(func() { v.Run(rctx) })
	guard := &auth.Guard{Verifier: v, Counters: counters, Logger: quiet()}
	base := "http://" + self + listen
	server := &dss.Server{Intents: g.svc, Store: st, Manager: clientID, USSBaseURL: base, Counters: counters, Logger: quiet()}
	mux := http.NewServeMux()
	if err := stdapi.MountF3548(mux, server, stdapi.Options{Guard: guard.Require, Validate: auth.ValidateAccess}); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: exlog.Middleware(mux), ReadHeaderTimeout: 5 * time.Second}
	wg.Go(func() { _ = srv.Serve(ln) })
	t.Cleanup(func() { _ = srv.Close() })
	writer := &dss.Writer{Client: client, Store: st, Intents: g.svc, USSBaseURL: base, Manager: clientID, Availability: avail,
		Counters: counters, Logger: quiet(), Every: 100 * time.Millisecond}
	g.svc.Writer = writer
	wg.Go(func() { writer.Run(rctx) })
	wg.Go(func() { exlog.Run(rctx) })
	number, serial, token := g.operatorClient()
	file := func(ref string, box [4]float64, lo, hi float64) resp {
		s := time.Now().UTC().Add(5 * time.Minute).Truncate(time.Second)
		return g.file(token, g.request(number, serial, ref, box, "volumes", []any{g.volume(box, lo, hi, s, s.Add(30*time.Minute))}))
	}

	// 1. Our availability as the real DSS holds it.
	if err := avail.Poll(ctx); err != nil {
		t.Fatalf("availability: %v", err)
	}
	state, _, _, _ := avail.State()
	t.Logf("lab: uss_availability of %s in the DSS: %s", clientID, state)

	// 2. The subscription of the U-space airspace in the real DSS.
	subs := &dss.Subscriptions{Client: client, Store: st, USSBaseURL: base, Counters: counters,
		Areas: func() ([]dss.Area, bool) {
			return []dss.Area{{ID: "TSA-LAB", Box: geodesy.BBox{MinLat: airspace[0], MinLon: airspace[1], MaxLat: airspace[2], MaxLon: airspace[3]}}}, true
		}}
	if err := subs.Once(ctx); err != nil {
		t.Fatalf("subscription: %v", err)
	}
	t.Logf("lab: subscription %s of TSA-LAB put in the DSS", dss.SubscriptionID(base, "TSA-LAB"))

	// 3. pending_dss while the authority holds us Down (lab-01 sets the
	// availability as the authority would), then a special operation
	// displacing the peer once it is Normal again (PLAN §15 Q19: judged
	// at 0 until it can be verified, so its priority is set as a verified
	// one would be): the peer is told within 1 s, measured.
	arb, err := auth.NewOutgoing(auth.OutgoingConfig{TokenURL: tokenURL, ClientID: arbiterID, ClientSecret: arbiterSecret})
	if err != nil {
		t.Fatal(err)
	}
	setAvail := func(want f3548.UssAvailabilityState) {
		t.Helper()
		tok, err := arb.Token(ctx, dssURL, string(f3548.ScopeAvailabilityArbitration))
		if err != nil {
			t.Fatal(err)
		}
		get, _ := client.Availability(ctx, clientID)
		body, _ := json.Marshal(f3548.SetUssAvailabilityStatusParameters{Availability: want, OldVersion: get.Version})
		req, _ := http.NewRequest(http.MethodPut, dssURL+"/dss/v1/uss_availability/"+clientID, strings.NewReader(string(body)))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil || res.StatusCode != http.StatusOK {
			t.Fatalf("set availability %s: %v %v", want, res, err)
		}
		_ = res.Body.Close()
		if err := avail.Poll(ctx); err != nil {
			t.Fatal(err)
		}
	}
	setAvail(f3548.Down)
	simBox := [4]float64{41.7285, 44.8485, 41.7315, 44.8515}
	special := file("lab-special", simBox, 620, 740)
	t.Logf("lab: availability Down: %s %v", special.str("decision"), reasonsOf(special))
	if special.str("decision") != "pending_dss" || !slices.Contains(reasonsOf(special), intent.ReasonUSSAvailabilityDown) {
		t.Fatalf("%s", special.raw)
	}

	sid := special.str("intent_id")
	if _, err := relOwner(t).Exec(ctx, "UPDATE operational_intents SET priority = 100 WHERE id = $1", sid); err != nil {
		t.Fatal(err)
	}
	setAvail(f3548.Normal)
	t0 := time.Now()
	if err := writer.WriteNow(ctx, sid); err != nil {
		t.Fatal(err)
	}
	var lat time.Duration
	within(t, 10*time.Second, func() bool {
		es, _ := st.Exchanges(ctx, sid, 100)
		for _, e := range es {
			if e.Role == dss.RoleClient && e.Method == http.MethodPost && strings.HasPrefix(e.URL, "http://sim-ussp") && e.ResponseCode == http.StatusNoContent {
				lat = e.ResponseTime.Sub(t0)
				return true
			}
		}
		return false
	})
	t.Logf("lab: the displaced peer (sim-ussp) told %s after the write began (inline: %d, late: %d)", lat.Round(time.Millisecond),
		counters.Get(dss.CounterDisplacedInline), counters.Get(dss.CounterNotifyLate))
	if lat > time.Duration(f3548.ConflictingOIMaxUSSNotificationTimeSeconds)*time.Second {
		t.Fatalf("told after %s", lat)
	}

	// 4. The peer first: an intent over the lab peer's volume (a 300 m
	// circle at 41.73, 44.85, 600 to 760 m W84) is refused naming it.
	t0 = time.Now()
	a := file("lab-peer-first", simBox, 620, 740)
	t.Logf("lab: intent over the peer's: %s in %s, conflicts %v", a.str("decision"), time.Since(t0).Round(time.Millisecond), refsOf(a))
	if a.str("decision") != "rejected" || !slices.Contains(refsOf(a), intent.PeerPrefix+simIntent) {
		t.Fatalf("%s", a.raw)
	}
	p, _ := st.PeerIntent(ctx, simIntent)
	if p == nil || p.Manager != "sim-ussp-01" || p.OVN == "" {
		t.Fatalf("the peer's intent was not stored: %+v", p)
	}
	t.Logf("lab: peer intent %s stored, manager %s, version %d, trust provider", simIntent, p.Manager, p.Version)

	// 5. Clear of it: authorised once the real DSS took it.
	clear := [4]float64{41.7600, 44.8800, 41.7630, 44.8830}
	t0 = time.Now()
	b := file("lab-clear", clear, 620, 740)
	took := time.Since(t0)
	if b.str("decision") != "authorised" || b.str("dss_state") != "Accepted" {
		t.Fatalf("%s", b.raw)
	}
	bid := b.str("intent_id")
	ref, err := client.GetOperationalIntent(ctx, bid)
	if err != nil || ref.Manager != clientID || ref.State != f3548.Accepted || ref.Ovn == nil {
		t.Fatalf("the DSS holds %+v %v", ref, err)
	}
	t.Logf("lab: intent %s authorised in %s (POST /v1/intents with the DSS write); the DSS holds version %d, ovn %s", bid, took.Round(time.Millisecond), ref.Version, *ref.Ovn)

	// 6. Activation, nonconformance and end mirrored in the real DSS.
	if r := g.stack.call("PATCH", "/v1/intents/"+bid, map[string]any{"action": "activate"}, bearer(token)); r.str("state") != "activated" {
		t.Fatalf("%s", r.raw)
	}
	took = within(t, 10*time.Second, func() bool {
		r, err := client.GetOperationalIntent(ctx, bid)
		return err == nil && r.State == f3548.Activated
	})
	t.Logf("lab: Activated in the DSS %s after the activation", took.Round(time.Millisecond))
	if ok, err := g.svc.SetConformance(ctx, bid, intent.ConformanceNonconforming, "lab"); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	took = within(t, 10*time.Second, func() bool {
		r, err := client.GetOperationalIntent(ctx, bid)
		return err == nil && r.State == f3548.Nonconforming
	})
	t.Logf("lab: Nonconforming in the DSS %s after the transition", took.Round(time.Millisecond))
	if r := g.stack.call("PATCH", "/v1/intents/"+bid, map[string]any{"action": "end"}, bearer(token)); r.str("state") != "ended" {
		t.Fatalf("%s", r.raw)
	}
	took = within(t, 10*time.Second, func() bool {
		_, err := client.GetOperationalIntent(ctx, bid)
		return err != nil && strings.Contains(err.Error(), "not found")
	})
	t.Logf("lab: deleted from the DSS %s after the end", took.Round(time.Millisecond))

	// 7. pending_dss observed with the DSS down (its container stopped
	// from the host), drained when it is started.
	signal(t, signals, "stop-dss")
	c := file("lab-dss-down", [4]float64{41.7700, 44.8900, 41.7730, 44.8930}, 620, 740)
	t.Logf("lab: with the DSS stopped: %s %v", c.str("decision"), reasonsOf(c))
	if c.str("decision") != "pending_dss" || !slices.Contains(reasonsOf(c), intent.ReasonDSSUnavailable) {
		t.Fatalf("%s", c.raw)
	}
	signal(t, signals, "start-dss")
	took = within(t, 120*time.Second, func() bool {
		return g.stack.call("GET", "/v1/intents/"+c.str("intent_id"), nil, bearer(token)).str("decision") == "authorised"
	})
	t.Logf("lab: drained and authorised %s after the DSS was started", took.Round(time.Millisecond))

	// 8. The 24 h purge with a shortened clock: the peer's intent the
	// rejected decision relied on is kept with the decision's copy; an
	// unreferenced copy of it, moved 25 h back, is deleted.
	o := relOwner(t)
	if _, err := o.Exec(ctx, `INSERT INTO peer_intents (entity_id, manager, uss_base_url, state, ovn, version, time_start, time_end, details, priority, fetched_at)
		SELECT $2, manager, uss_base_url, state, ovn, version, time_start, time_end, details, priority, now() - interval '25 hours' FROM peer_intents WHERE entity_id = $1`,
		simIntent, newUUID()); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Exec(ctx, "UPDATE peer_intents SET fetched_at = now() - interval '25 hours' WHERE entity_id = $1", simIntent); err != nil {
		t.Fatal(err)
	}
	n, err := (&dss.Purger{Store: st}).Once(ctx)
	kept, _ := st.PeerIntent(ctx, simIntent)
	t.Logf("lab: purge with the clock 25 h on: %d peer intents deleted, the referenced one kept: %v", n.PeerIntents, kept != nil)
	if err != nil || n.PeerIntents != 1 || kept == nil {
		t.Fatalf("%+v %v", n, err)
	}
	if r := g.stack.call("GET", "/v1/intents/"+a.str("intent_id"), nil, bearer(token)); !slices.Contains(refsOf(r), intent.PeerPrefix+simIntent) {
		t.Fatalf("the decision's copy: %s", r.raw)
	}

	// 9. The exchange log of the special operation's intent.
	es, _ := st.Exchanges(ctx, sid, 1000)
	t.Logf("lab: %d exchanges recorded for %s; counters %v", len(es), sid, counters.Snapshot())
}
