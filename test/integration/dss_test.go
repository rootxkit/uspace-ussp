//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	"github.com/rootxkit/uspace-ussp/internal/dss/fakedss"
	dsspg "github.com/rootxkit/uspace-ussp/internal/dss/pgstore"
	"github.com/rootxkit/uspace-ussp/internal/intent"
	"github.com/rootxkit/uspace-ussp/internal/stdapi"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/peeruss"
)

const (
	dssOurManager  = "ussp-ussp-dev-01"
	dssPeerManager = "peer-uss-01"
)

// cleanDSSOutbox marks the DSS work left by a test done, so the next
// test's api process (with a DSS writer of its own) never takes it.
func cleanDSSOutbox(t *testing.T) {
	t.Helper()
	if _, err := relOwner(t).Exec(context.Background(),
		"UPDATE dss_outbox SET done_at = now() WHERE done_at IS NULL AND kind IN ('oir_put', 'oir_delete', 'peer_notify')"); err != nil {
		t.Fatal(err)
	}
}

// issuerTokens are ecosystem tokens of the fake authority for sub, aud
// the host of the target (M18).
type issuerTokens struct {
	a   *fakeAuthority
	sub string
}

func (k issuerTokens) Token(_ context.Context, base string, scopes ...string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	return k.a.iss.Issue(k.sub, u.Hostname(), scopes, time.Hour, time.Now())
}

// dssRig is the intent service of S-M1 with WP-13 composed on it, on the
// real database and NATS: the DSS client, availability and gate, the
// writer running in the background, our F3548 endpoints behind the real
// guard (ecosystem tokens of the fake authority), the fake DSS and a fake
// peer USSP filing through it.
type dssRig struct {
	*intentRig
	a        *fakeAuthority
	fake     *fakedss.DSS
	st       dsspg.Store
	client   *dss.Client
	avail    *dss.Availability
	writer   *dss.Writer
	server   *dss.Server
	exlog    *dss.ExchangeLog
	us       *httptest.Server
	peer     *peeruss.Fake
	counters *core.Counters
	airspace [4]float64
	number   string
	serial   string
	token    string
}

func newDSSRig(t *testing.T) *dssRig {
	t.Helper()
	g := newIntentRig(t)
	cleanDSSOutbox(t)
	t.Cleanup(func() { cleanDSSOutbox(t) })
	// The peer's data is not left to the next test's decisions (every
	// decision reads the peers' intents of its window).
	t.Cleanup(func() {
		if _, err := relOwner(t).Exec(context.Background(), "DELETE FROM peer_intents WHERE manager = $1", dssPeerManager); err != nil {
			t.Error(err)
		}
		if _, err := relOwner(t).Exec(context.Background(), "DELETE FROM constraints WHERE manager = $1", dssPeerManager); err != nil {
			t.Error(err)
		}
	})
	d := &dssRig{intentRig: g, a: newFakeAuthority(t), fake: fakedss.New(), counters: &core.Counters{}}
	t.Cleanup(d.fake.Close)
	d.airspace = g.box(0, 0, 0.2)
	g.publishAll(nil, []json.RawMessage{edFeature("TSA-DSS", "USPACE", d.airspace, 0, 3000, "AMSL", requirementsExt(nil))}, nil)
	d.st = dsspg.Store{S: appStore(t)}
	d.exlog = dss.NewExchangeLog(d.st, d.counters, quiet())
	d.client = &dss.Client{DSSBaseURL: d.fake.URL(), Tokens: issuerTokens{a: d.a, sub: dssOurManager},
		HTTP: &http.Client{Timeout: 5 * time.Second, Transport: &dss.Transport{Log: d.exlog}}}
	d.avail = &dss.Availability{Client: d.client, Store: d.st, USSID: dssOurManager, Counters: d.counters}
	g.svc.Decider.DSS = &dss.Gate{Client: d.client, Availability: d.avail}
	v, err := auth.NewVerifier(context.Background(), auth.VerifierConfig{Ecosystem: coreauth.Config{
		Issuers: map[string]coreauth.IssuerConfig{d.a.url: {JWKSURL: d.a.jwks}}, Audiences: []string{"127.0.0.1"}, StrictSessionClaims: true}})
	if err != nil {
		t.Fatal(err)
	}
	// The ecosystem verifier is built and its keys fetched in the
	// background (Run): the rig serves once a peer's token verifies.
	vctx, vcancel := context.WithCancel(context.Background())
	var vwg sync.WaitGroup
	vwg.Go(func() { v.Run(vctx) })
	t.Cleanup(func() { vcancel(); vwg.Wait() })
	probe, err := issuerTokens{a: d.a, sub: dssPeerManager}.Token(context.Background(), "http://127.0.0.1", string(f3548.ScopeStrategicCoordination))
	if err != nil {
		t.Fatal(err)
	}
	within(t, 10*time.Second, func() bool { _, err := v.Verify(context.Background(), probe); return err == nil })
	guard := &auth.Guard{Verifier: v, Counters: d.counters, Logger: quiet()}
	d.server = &dss.Server{Intents: g.svc, Store: d.st, Manager: dssOurManager, Counters: d.counters, Logger: quiet()}
	mux := http.NewServeMux()
	if err := stdapi.MountF3548(mux, d.server, stdapi.Options{Guard: guard.Require, Validate: auth.ValidateAccess}); err != nil {
		t.Fatal(err)
	}
	d.us = httptest.NewServer(d.exlog.Middleware(mux))
	t.Cleanup(d.us.Close)
	d.server.USSBaseURL = d.us.URL
	d.writer = &dss.Writer{Client: d.client, Store: d.st, Intents: g.svc, USSBaseURL: d.us.URL, Manager: dssOurManager,
		Availability: d.avail, Counters: d.counters, Logger: quiet(), Every: 50 * time.Millisecond}
	g.svc.Writer = d.writer
	d.peer = peeruss.New(dssPeerManager, d.fake.URL(), func(base string, scope f3548.Scope) string {
		tok, _ := issuerTokens{a: d.a, sub: dssPeerManager}.Token(context.Background(), base, string(scope))
		return tok
	})
	t.Cleanup(d.peer.Close)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { d.writer.Run(ctx) })
	wg.Go(func() { d.exlog.Run(ctx) })
	t.Cleanup(func() { cancel(); wg.Wait() })
	d.number, d.serial, d.token = g.operatorClient()
	return d
}

// soon is a window starting in 5 minutes (inside the activation lead)
// for 30 minutes.
func soon() (time.Time, time.Time) {
	s := time.Now().UTC().Add(5 * time.Minute).Truncate(time.Second)
	return s, s.Add(30 * time.Minute)
}

// request is an intent over box in the soon window.
func (d *dssRig) request(ref string, box [4]float64) map[string]any {
	s, e := soon()
	return d.intentRig.request(d.number, d.serial, ref, box, "volumes", []any{d.volume(box, 120, 170, s, e)})
}

// peerVolume is the same volume as an F3548 Volume4D.
func (d *dssRig) peerVolume(box [4]float64) f3548.Volume4D {
	d.t.Helper()
	s, e := soon()
	b, _ := json.Marshal(d.volume(box, 120, 170, s, e))
	var v f3548.Volume4D
	if err := json.Unmarshal(b, &v); err != nil {
		d.t.Fatal(err)
	}
	return v
}

func (d *dssRig) patch(id string, body map[string]any) resp {
	d.t.Helper()
	return d.stack.call("PATCH", "/v1/intents/"+id, body, bearer(d.token))
}

func (d *dssRig) get(id string) resp {
	d.t.Helper()
	return d.stack.call("GET", "/v1/intents/"+id, nil, bearer(d.token))
}

// notesOf are the peer's notifications about id.
func (d *dssRig) notesOf(id string) []peeruss.Notification {
	var out []peeruss.Notification
	for _, n := range d.peer.Notifications() {
		if n.Body.OperationalIntentId == id {
			out = append(out, n)
		}
	}
	return out
}

func refsOf(r resp) []string {
	var out []string
	cs, _ := r.body["conflicts"].([]any)
	for _, c := range cs {
		m, _ := c.(map[string]any)
		s, _ := m["ref"].(string)
		out = append(out, s)
	}
	return out
}

// S-M4, accept to end: an intent inside U-space airspace is written to
// the DSS in the request path and answered authorised with dss_state
// Accepted and its number; the DSS holds it with its ovn under our base
// URL; a peer watching the area is told within 5 s (measured); the
// activation, the nonconformance conformance monitoring reports (WP-10's
// intent.v1 transition) and the end are mirrored (Activated,
// Nonconforming, deleted), each told to the peer; the log set of the
// intent holds our exchanges.
func TestIntegrationDSSAcceptNotifyActivateEnd(t *testing.T) {
	d := newDSSRig(t)
	ctx := context.Background()
	a := d.airspace
	if err := d.peer.Subscribe(ctx, "77777777-7777-4777-8777-777777777777", a[0], a[1], a[2], a[3], 2*time.Hour); err != nil {
		t.Fatal(err)
	}
	box := d.box(0.05, 0.05, 0.01)
	start := time.Now()
	r := d.file(d.token, d.request("dss-accept", box))
	if r.status != 201 || r.str("decision") != "authorised" || r.str("state") != "accepted" || r.str("dss_state") != "Accepted" ||
		r.str("authorisation_number") == "" || r.body["in_uspace_airspace"] != true {
		t.Fatalf("%d %s", r.status, r.raw)
	}
	id := r.str("intent_id")
	ref, _, ok := d.fake.OIR(id)
	if !ok || ref.Ovn == nil || ref.UssBaseUrl != d.us.URL || ref.Manager != dssOurManager || ref.State != f3548.Accepted {
		t.Fatalf("the DSS holds %+v %v", ref, ok)
	}
	if h, err := d.svc.Held(ctx, id); err != nil || h == nil || h.OVN != *ref.Ovn || h.SubscriptionID == "" {
		t.Fatalf("held %+v %v", h, err)
	}
	took := within(t, time.Duration(f3548.UssOiChangeNotificationMaxSeconds)*time.Second, func() bool { return len(d.notesOf(id)) == 1 })
	n := d.notesOf(id)[0]
	t.Logf("subscriber told %s after the request (%s after it was answered)", n.At.Sub(start).Round(time.Millisecond), took.Round(time.Millisecond))
	if n.Body.OperationalIntent == nil || *n.Body.OperationalIntent.Reference.Ovn != *ref.Ovn ||
		n.At.Sub(start) > time.Duration(f3548.UssOiChangeNotificationMaxSeconds)*time.Second {
		t.Fatalf("notification %+v", n.Body)
	}
	// Activation.
	if r := d.patch(id, map[string]any{"action": "activate"}); r.status != 200 || r.str("state") != "activated" || r.str("dss_state") != "Activated" {
		t.Fatalf("activate: %d %s", r.status, r.raw)
	}
	within(t, 5*time.Second, func() bool { ref, _, _ := d.fake.OIR(id); return ref.State == f3548.Activated })
	within(t, 5*time.Second, func() bool { return len(d.notesOf(id)) == 2 })
	// Nonconforming from conformance monitoring.
	ncAt := time.Now()
	if ok, err := d.svc.SetConformance(ctx, id, intent.ConformanceNonconforming, "outside the volumes"); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	took = within(t, time.Duration(f3548.OiMaxUpdateTimeNonconfSeconds)*time.Second, func() bool {
		ref, _, _ := d.fake.OIR(id)
		return ref.State == f3548.Nonconforming
	})
	t.Logf("Nonconforming in the DSS %s after the transition", took.Round(time.Millisecond))
	within(t, 5*time.Second, func() bool { return len(d.notesOf(id)) == 3 })
	nn := d.notesOf(id)[2]
	if nn.Body.OperationalIntent == nil || nn.Body.OperationalIntent.Reference.State != f3548.Nonconforming ||
		nn.Body.OperationalIntent.Details.OffNominalVolumes == nil || len(*nn.Body.OperationalIntent.Details.OffNominalVolumes) == 0 {
		t.Fatalf("nonconforming notification %+v", nn.Body.OperationalIntent)
	}
	t.Logf("peer told of Nonconforming %s after the transition", nn.At.Sub(ncAt).Round(time.Millisecond))
	// The end.
	if r := d.patch(id, map[string]any{"action": "end"}); r.status != 200 || r.str("state") != "ended" {
		t.Fatalf("end: %d %s", r.status, r.raw)
	}
	within(t, 5*time.Second, func() bool { _, _, ok := d.fake.OIR(id); return !ok })
	within(t, 5*time.Second, func() bool {
		ns := d.notesOf(id)
		return len(ns) == 4 && ns[3].Body.OperationalIntent == nil
	})
	if h, _ := d.svc.Held(ctx, id); h != nil {
		t.Fatalf("still held %+v", h)
	}
	// The log set holds the writes of this intent.
	within(t, 5*time.Second, func() bool {
		es, err := d.st.Exchanges(ctx, id, 100)
		return err == nil && len(es) >= 4
	})
}

// S-M4, a peer first: its intent, filed in the DSS before ours over the
// same volume, refuses ours, naming it (peer:<id>, intent_filed_first);
// what the decision relied on stays in its conflicts.
func TestIntegrationDSSPeerFirstRejectsOurs(t *testing.T) {
	d := newDSSRig(t)
	ctx := context.Background()
	box := d.box(0.06, 0.06, 0.01)
	pid := newUUID()
	if _, err := d.peer.File(ctx, peeruss.Spec{ID: pid, Volumes: []f3548.Volume4D{d.peerVolume(box)}}); err != nil {
		t.Fatal(err)
	}
	r := d.file(d.token, d.request("dss-peer-first", box))
	if r.status != 201 || r.str("decision") != "rejected" || !slices.Contains(refsOf(r), intent.PeerPrefix+pid) ||
		!slices.Contains(reasonsOf(r), intent.ReasonIntentFirstCome) {
		t.Fatalf("%d %s", r.status, r.raw)
	}
	if _, _, ok := d.fake.OIR(r.str("intent_id")); ok {
		t.Fatal("a rejected intent was written to the DSS")
	}
	if p, err := d.st.PeerIntent(ctx, pid); err != nil || p == nil || p.Manager != dssPeerManager || p.OVN == "" {
		t.Fatalf("peer intent not stored: %+v %v", p, err)
	}
}

// S-M4, ours first: the peer reads our details from our endpoint (with
// our ovn) and files over us; its notification arrives, is stored
// (trust provider, by version) and, the DSS having let a conflict
// through at equal priority, reported to the authority with both
// references (spec 06 T9).
func TestIntegrationDSSOursFirstPeerNotificationStored(t *testing.T) {
	d := newDSSRig(t)
	ctx := context.Background()
	box := d.box(0.07, 0.07, 0.01)
	r := d.file(d.token, d.request("dss-ours-first", box))
	if r.str("decision") != "authorised" {
		t.Fatalf("%s", r.raw)
	}
	id := r.str("intent_id")
	pid := newUUID()
	if _, err := d.peer.File(ctx, peeruss.Spec{ID: pid, Volumes: []f3548.Volume4D{d.peerVolume(box)}}); err != nil {
		t.Fatal(err)
	}
	if d.counters.Get(dss.CounterDetailsServed) == 0 {
		t.Fatal("the peer did not read our details")
	}
	p, err := d.st.PeerIntent(ctx, pid)
	if err != nil || p == nil || p.USSBaseURL != d.peer.URL() || p.State != "Accepted" {
		t.Fatalf("notification not stored: %+v %v", p, err)
	}
	if n := events(t, "operational_intent", id, "dss_conflict_reported"); n != 1 {
		t.Fatalf("conflict reports %d", n)
	}
}

// S-M4, a special operation of ours displacing a peer: the peer is told
// within ConflictingOIMaxUSSNotificationTimeSeconds (measured from the
// write), and the displacement audited. Under PLAN §15 Q19 a special
// operation is judged at priority 0 until it can be verified, so the
// stored priority is set here directly, as a verified one would be.
func TestIntegrationDSSDisplacedPeerWithinOneSecond(t *testing.T) {
	d := newDSSRig(t)
	ctx := context.Background()
	box := d.box(0.08, 0.08, 0.01)
	pid := newUUID()
	if _, err := d.peer.File(ctx, peeruss.Spec{ID: pid, Volumes: []f3548.Volume4D{d.peerVolume(box)}}); err != nil {
		t.Fatal(err)
	}
	d.fake.Down(true)
	r := d.file(d.token, d.request("dss-special", box))
	if r.str("decision") != "pending_dss" {
		t.Fatalf("%s", r.raw)
	}
	id := r.str("intent_id")
	if _, err := relOwner(t).Exec(ctx, "UPDATE operational_intents SET priority = 100 WHERE id = $1", id); err != nil {
		t.Fatal(err)
	}
	d.fake.Down(false)
	t0 := time.Now()
	if err := d.writer.WriteNow(ctx, id); err != nil {
		t.Fatal(err)
	}
	within(t, 5*time.Second, func() bool { return len(d.notesOf(id)) >= 1 })
	lat := d.notesOf(id)[0].At.Sub(t0)
	t.Logf("displaced peer told %s after the write began", lat.Round(time.Millisecond))
	if lat > time.Duration(f3548.ConflictingOIMaxUSSNotificationTimeSeconds)*time.Second {
		t.Fatalf("told after %s", lat)
	}
	if d.counters.Get(dss.CounterDisplacedInline) != 1 || events(t, "operational_intent", id, "dss_peer_displaced") != 1 {
		t.Fatalf("%v", d.counters.Snapshot())
	}
	if g := d.get(id); g.str("decision") != "authorised" {
		t.Fatalf("%s", g.raw)
	}
}

// S-M4, E-01 both: with the DSS down an intent inside U-space airspace
// waits pending_dss naming why and one outside is accepted on the local
// checks; the DSS back, the outbox drains and the waiting intent is
// authorised, the operator told by the decision's new version.
func TestIntegrationDSSDownThenDrains(t *testing.T) {
	d := newDSSRig(t)
	d.fake.Down(true)
	r := d.file(d.token, d.request("dss-down-in", d.box(0.09, 0.09, 0.01)))
	if r.str("decision") != "pending_dss" || !slices.Contains(reasonsOf(r), intent.ReasonDSSUnavailable) || r.str("authorisation_number") != "" {
		t.Fatalf("%s", r.raw)
	}
	id := r.str("intent_id")
	out := d.file(d.token, d.request("dss-down-out", d.box(0.5, 0.5, 0.01)))
	if out.str("decision") != "authorised" || out.body["in_uspace_airspace"] != false {
		t.Fatalf("outside: %s", out.raw)
	}
	if _, _, ok := d.fake.OIR(id); ok {
		t.Fatal("written while the DSS is down")
	}
	d.fake.Down(false)
	took := within(t, 30*time.Second, func() bool { return d.get(id).str("decision") == "authorised" })
	g := d.get(id)
	t.Logf("drained %s after the DSS came back", took.Round(time.Millisecond))
	if g.str("state") != "accepted" || g.str("authorisation_number") == "" || !strings.Contains(g.str("change_reason"), "DSS") ||
		events(t, "operational_intent", id, intent.EventDSSAuthorised) != 1 {
		t.Fatalf("%s", g.raw)
	}
	if _, _, ok := d.fake.OIR(id); !ok {
		t.Fatal("not in the DSS")
	}
	if !slices.Contains(d.activeKeys(), id) {
		t.Fatal("not projected to intent_active")
	}
	// The outside intent never went to the DSS.
	if _, _, ok := d.fake.OIR(out.str("intent_id")); ok {
		t.Fatal("an intent outside U-space airspace was written")
	}
}

// S-M4, peer down: the peer's intent we hold still counts until its
// time_end (it refuses ours) and is flagged peer_unavailable.
func TestIntegrationDSSPeerDown(t *testing.T) {
	d := newDSSRig(t)
	ctx := context.Background()
	box := d.box(0.11, 0.11, 0.01)
	pid := newUUID()
	if _, err := d.peer.File(ctx, peeruss.Spec{ID: pid, Volumes: []f3548.Volume4D{d.peerVolume(box)}}); err != nil {
		t.Fatal(err)
	}
	if st, err := d.peer.NotifyTo(ctx, d.us.URL, pid, nil); err != nil || st != http.StatusNoContent {
		t.Fatalf("%d %v", st, err)
	}
	d.peer.Down(true)
	r := d.file(d.token, d.request("dss-peer-down", box))
	if r.str("decision") != "rejected" || !slices.Contains(refsOf(r), intent.PeerPrefix+pid) {
		t.Fatalf("%s", r.raw)
	}
	// Above the peer's volume (no vertical overlap) the DSS still names
	// its reference: its manager does not answer, so its stored intents
	// are marked peer_unavailable and the stored copy's ovn is used.
	s, e := soon()
	up := d.file(d.token, d.intentRig.request(d.number, d.serial, "dss-peer-down-above", box, "volumes", []any{d.volume(box, 300, 350, s, e)}))
	if up.str("decision") != "authorised" {
		t.Fatalf("%s", up.raw)
	}
	p, _ := d.st.PeerIntent(ctx, pid)
	if p == nil || !p.PeerUnavailable || d.counters.Get(dss.CounterPeerStoredUsed) == 0 {
		t.Fatalf("%+v %v", p, d.counters.Snapshot())
	}
	// The peer answers again: the mark is cleared at its next answer.
	d.peer.Down(false)
	if st, err := d.peer.NotifyTo(ctx, d.us.URL, pid, nil); err != nil || st != http.StatusNoContent {
		t.Fatalf("%d %v", st, err)
	}
	if p, _ := d.st.PeerIntent(ctx, pid); p == nil || p.PeerUnavailable {
		t.Fatalf("%+v", p)
	}
}

// The 24 h purge both ways: a peer intent fetched 25 h ago is deleted,
// one of 23 h kept, and one of 25 h that a decision relied on kept, the
// decision's copy intact.
func TestIntegrationDSSPurge(t *testing.T) {
	d := newDSSRig(t)
	ctx := context.Background()
	box := d.box(0.12, 0.12, 0.01)
	ref := newUUID()
	if _, err := d.peer.File(ctx, peeruss.Spec{ID: ref, Volumes: []f3548.Volume4D{d.peerVolume(box)}}); err != nil {
		t.Fatal(err)
	}
	r := d.file(d.token, d.request("dss-purge", box))
	if r.str("decision") != "rejected" || !slices.Contains(refsOf(r), intent.PeerPrefix+ref) {
		t.Fatalf("%s", r.raw)
	}
	old, young := newUUID(), newUUID()
	for _, id := range []string{old, young} {
		far := d.peerVolume(d.box(0.18, 0.18, 0.001))
		details, _ := json.Marshal(f3548.OperationalIntentDetails{Volumes: &[]f3548.Volume4D{far}})
		if _, err := d.st.UpsertPeerIntent(ctx, dss.PeerRecord{EntityID: id, Manager: dssPeerManager, USSBaseURL: d.peer.URL(), Version: 1,
			State: "Accepted", TimeStart: far.TimeStart.Value, TimeEnd: far.TimeEnd.Value, Details: details}); err != nil {
			t.Fatal(err)
		}
	}
	o := relOwner(t)
	for id, age := range map[string]string{old: "25 hours", young: "23 hours", ref: "25 hours"} {
		if _, err := o.Exec(ctx, "UPDATE peer_intents SET fetched_at = now() - $2::interval WHERE entity_id = $1", id, age); err != nil {
			t.Fatal(err)
		}
	}
	n, err := (&dss.Purger{Store: d.st}).Once(ctx)
	if err != nil || n.PeerIntents < 1 {
		t.Fatalf("%+v %v", n, err)
	}
	for id, want := range map[string]bool{old: false, young: true, ref: true} {
		p, _ := d.st.PeerIntent(ctx, id)
		if (p != nil) != want {
			t.Fatalf("%s kept %v, want %v", id, p != nil, want)
		}
	}
	if g := d.get(r.str("intent_id")); !slices.Contains(refsOf(g), intent.PeerPrefix+ref) {
		t.Fatalf("the decision's copy: %s", g.raw)
	}
}

// The subscription of the U-space airspace is put in the DSS and
// recorded; a notification on it with an index going backwards is
// counted and applied.
func TestIntegrationDSSSubscriptions(t *testing.T) {
	d := newDSSRig(t)
	ctx := context.Background()
	a := d.airspace
	s := &dss.Subscriptions{Client: d.client, Store: d.st, USSBaseURL: d.us.URL, Counters: d.counters,
		Areas: func() ([]dss.Area, bool) {
			return []dss.Area{{ID: "TSA-DSS", Box: coreBox(a)}}, true
		}}
	if err := s.Once(ctx); err != nil {
		t.Fatal(err)
	}
	id := dss.SubscriptionID(d.us.URL, "TSA-DSS")
	if _, ok := d.fake.UTMSubscriptions()[id]; !ok {
		t.Fatalf("%v", d.fake.UTMSubscriptions())
	}
	subs, err := d.st.Subscriptions(ctx)
	if err != nil || !slices.ContainsFunc(subs, func(r dss.SubscriptionRecord) bool { return r.ID == id && r.Version != "" }) {
		t.Fatalf("%+v %v", subs, err)
	}
	pid := newUUID()
	box := d.box(0.13, 0.13, 0.01)
	if _, err := d.peer.File(ctx, peeruss.Spec{ID: pid, Volumes: []f3548.Volume4D{d.peerVolume(box)}}); err != nil {
		t.Fatal(err)
	}
	within(t, 5*time.Second, func() bool { p, _ := d.st.PeerIntent(ctx, pid); return p != nil })
	if st, err := d.peer.NotifyTo(ctx, d.us.URL, pid, []f3548.SubscriptionState{{SubscriptionId: id, NotificationIndex: 0}}); err != nil || st != http.StatusNoContent {
		t.Fatalf("%d %v", st, err)
	}
	if d.counters.Get(dss.CounterIndexBackwards) != 1 {
		t.Fatalf("%v", d.counters.Snapshot())
	}
	if _, err := relOwner(t).Exec(ctx, "DELETE FROM dss_subscriptions WHERE subscription_id = $1", id); err != nil {
		t.Fatal(err)
	}
}

func coreBox(b [4]float64) geodesy.BBox {
	return geodesy.BBox{MinLat: b[0], MinLon: b[1], MaxLat: b[2], MaxLon: b[3]}
}
