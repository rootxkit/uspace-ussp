package peers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3411"
	"github.com/rootxkit/uspace-core/geodesy"
	coresources "github.com/rootxkit/uspace-core/sources"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/dss/fakedss"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/sources"
	"github.com/rootxkit/uspace-ussp/internal/telemetry"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/peersp"
	"github.com/rootxkit/uspace-ussp/internal/traffic"
)

type sink struct {
	mu   sync.Mutex
	msgs map[string][][]byte
}

func (s *sink) Publish(_ context.Context, subject string, m bus.Enveloped) error {
	if _, err := bus.Parse(subject); err != nil {
		return err
	}
	if err := m.Head().Validate(); err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.msgs == nil {
		s.msgs = map[string][][]byte{}
	}
	kind := strings.SplitN(subject, ".", 2)[0]
	s.msgs[kind] = append(s.msgs[kind], b)
	return nil
}

func (s *sink) peers() []telemetry.Track {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []telemetry.Track
	for _, b := range s.msgs["peer"] {
		var t telemetry.Track
		_ = json.Unmarshal(b, &t)
		out = append(out, t)
	}
	return out
}

func (s *sink) count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.msgs["peer"]) }

type gate struct {
	mu  sync.Mutex
	off map[string]bool
}

func (g *gate) set(k string, off bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.off == nil {
		g.off = map[string]bool{}
	}
	g.off[k] = off
}

func (g *gate) Query(t string, inst *string) coresources.Decision {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.off[t] {
		w := coresources.WhyType
		return coresources.Decision{WhyDisabled: &w}
	}
	if inst != nil && g.off[t+"/"+*inst] {
		w := coresources.WhyInstance
		return coresources.Decision{WhyDisabled: &w}
	}
	return coresources.Decision{Enabled: true}
}

type tokens struct{}

func (tokens) Token(context.Context, string, ...string) (string, error) { return "tok", nil }

type ownSet map[string]bool

func (o ownSet) OwnFlight(id string) bool { return o[id] }

// receiver is our USS base URL as rid-sp is: it keeps the ISA
// notifications the peers send (rid_isa_notifications).
type receiver struct {
	srv   *httptest.Server
	mu    sync.Mutex
	notes map[string]ISANotification
}

func newReceiver(t *testing.T) *receiver {
	r := &receiver{notes: map[string]ISANotification{}}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		id, ok := strings.CutPrefix(req.URL.Path, "/uss/identification_service_areas/")
		if !ok || req.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var n f3411.PutIdentificationServiceAreaNotificationParameters
		if json.NewDecoder(req.Body).Decode(&n) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		r.mu.Lock()
		r.notes[id] = ISANotification{ISAID: id, ReceivedAt: time.Now(), Deleted: n.ServiceArea == nil, ServiceArea: n.ServiceArea, Extents: n.Extents}
		r.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) snapshot() (map[string]ISANotification, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]ISANotification{}
	for k, v := range r.notes {
		out[k] = v
	}
	return out, true
}

var (
	here   = core.LatLon{LatDeg: 41.75, LonDeg: 44.85}
	area   = geodesy.BBox{MinLat: 41.73, MinLon: 44.83, MaxLat: 41.77, MaxLon: 44.87}
	isaBox = geodesy.BBox{MinLat: 41.745, MinLon: 44.845, MaxLat: 41.755, MaxLon: 44.855}
)

type rig struct {
	dss  *fakedss.DSS
	peer *peersp.Fake
	rx   *receiver
	sink *sink
	gate *gate
	dp   *DP
	pol  atomic.Pointer[policy.Values]
}

func newRig(t *testing.T) *rig {
	t.Helper()
	g := &rig{dss: fakedss.New(), sink: &sink{}, gate: &gate{}, rx: newReceiver(t)}
	t.Cleanup(g.dss.Close)
	g.peer = peersp.New(g.dss.URL(), func(string, f3411.Scope) string { return "peer-tok" })
	t.Cleanup(g.peer.Close)
	v := policy.Defaults()
	g.pol.Store(&v)
	g.dp = &DP{DSSBaseURL: g.dss.URL(), USSBaseURL: g.rx.srv.URL, Tokens: tokens{},
		Areas:         func() ([]Area, bool) { return []Area{{ID: "TSA-1", Box: area}}, true },
		Notifications: g.rx.snapshot, Sink: g.sink, Gate: g.gate, Geoid: flatGeoid{},
		Policy: func() policy.Values { return *g.pol.Load() },
		Every:  30 * time.Millisecond, SlowEvery: 90 * time.Millisecond, SearchEvery: 100 * time.Millisecond,
		ReconcileEvery: 15 * time.Millisecond, StatusEvery: 20 * time.Millisecond}
	g.dp.init()
	return g
}

func (g *rig) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { g.dp.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

// ticks waits for n more status periods (StatusEvery, about one poll
// period each): an observation window measured by the DP's own loop.
func (g *rig) ticks(t *testing.T, n int) {
	t.Helper()
	count := func() int { g.sink.mu.Lock(); defer g.sink.mu.Unlock(); return len(g.sink.msgs["src"]) }
	start := count()
	within(t, 5*time.Second, "status periods", func() bool { return count() >= start+2*n })
}

func within(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("not within %s: %s", d, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func status(d *DP, base string) sources.StatusBody {
	for _, b := range d.Statuses() {
		if (base == "" && b.SourceInstance == nil) || (b.SourceInstance != nil && *b.SourceInstance == base) {
			return b
		}
	}
	return sources.StatusBody{}
}

const peerFlight = "6f1d7a52-9b0e-4c1f-8d2a-0f3e4b5c6d7e"

// Discovery through the DSS search (an ISA filed before we looked): the
// peer polled at about 1 Hz (here every 30 ms), with a display provider's
// token; its flight on peer.v1 as provider from network_rid, placed and
// valid, read by the CPA path; the peer live on src.v1 and /readyz.
func TestDPDiscoversBySearchAndPolls(t *testing.T) {
	g := newRig(t)
	g.peer.SetFlight(peersp.Flight{ID: peerFlight, Position: here, AltHAEM: 620, SpeedMS: 10, TrackDeg: 90})
	if _, err := g.peer.RegisterISA(context.Background(), "11111111-1111-4111-8111-111111111111", isaBox, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	g.start(t)
	within(t, 5*time.Second, "peer flights", func() bool { return g.sink.count() >= 3 })
	ss := compile(t)
	g.sink.mu.Lock()
	raws := append([][]byte(nil), g.sink.msgs["peer"]...)
	g.sink.mu.Unlock()
	for _, raw := range raws {
		validate(t, ss, "track/telemetry/v1", raw)
		tr, err := traffic.DecodeTrack(raw)
		if err != nil {
			t.Fatalf("the CPA path refuses it: %v", err)
		}
		if tr.Body.Trust != core.TrustProvider || tr.Body.Source != SourceNetworkRID || tr.Body.SourceInstance != g.peer.URL() ||
			tr.Body.TrackID != peerFlight || tr.Body.AltSource != core.AltGeodetic {
			t.Fatalf("track %+v", tr.Body)
		}
	}
	if a := g.peer.Auth(); len(a) == 0 || a[0] != "Bearer tok" {
		t.Fatalf("auth %q", a)
	}
	if g.dp.Pollers() != 1 {
		t.Fatalf("%d pollers, want one for the one view", g.dp.Pollers())
	}
	within(t, 2*time.Second, "live", func() bool { return status(g.dp, g.peer.URL()).State == sources.StateLive })
	if st, d := g.dp.Probe(context.Background()); st != obs.StateUp || !strings.Contains(d, "1 peers polled") {
		t.Fatalf("probe %s %s", st, d)
	}
	if g.dp.Counters.Get(CounterSearches) == 0 {
		t.Fatal("no search counted")
	}
}

// Discovery through the subscription (no search finds it: the ISA comes
// after): our subscription in the DSS under our base URL, the peer
// notifies it, and the notification's ISA is polled; the notification
// of its deletion stops the polls.
func TestDPDiscoversThroughTheSubscription(t *testing.T) {
	g := newRig(t)
	g.dp.SearchEvery = time.Hour
	g.peer.SetFlight(peersp.Flight{ID: peerFlight, Position: here, AltHAEM: 620})
	g.start(t)
	within(t, 3*time.Second, "subscribed", func() bool { return len(g.dss.Subscriptions()) == 1 })
	for id, s := range g.dss.Subscriptions() {
		if id != SubscriptionID(g.rx.srv.URL, "TSA-1") || s.UssBaseUrl != g.rx.srv.URL {
			t.Fatalf("subscription %s %+v", id, s)
		}
	}
	isa := "22222222-2222-4222-8222-222222222222"
	if _, err := g.peer.RegisterISA(context.Background(), isa, isaBox, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n := g.peer.Notified(); len(n) != 1 || n[0] != g.rx.srv.URL {
		t.Fatalf("notified %q", n)
	}
	within(t, 3*time.Second, "polled", func() bool { return g.sink.count() >= 2 })
	// Its deletion, notified: the pollers stop.
	g.rx.mu.Lock()
	g.rx.notes[isa] = ISANotification{ISAID: isa, Deleted: true}
	g.rx.mu.Unlock()
	within(t, 2*time.Second, "stopped", func() bool { return g.dp.Pollers() == 0 })
}

// Our own ISA is never polled (discovery, never configuration: and not
// ourselves); an ended ISA neither (E-01 pair with the polls above).
func TestDPNeverPollsItselfOrAnEndedISA(t *testing.T) {
	g := newRig(t)
	self := peersp.New(g.dss.URL(), func(string, f3411.Scope) string { return "self-tok" })
	t.Cleanup(self.Close)
	g.dp.USSBaseURL = self.URL() + "/"
	self.SetFlight(peersp.Flight{ID: "ours", Position: here})
	if _, err := self.RegisterISA(context.Background(), "33333333-3333-4333-8333-333333333333", isaBox, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	g.rx.mu.Lock()
	ended := f3411.IdentificationServiceArea{Id: "44444444-4444-4444-8444-444444444444", UssBaseUrl: g.peer.URL(),
		TimeEnd: f3411.Time{Format: f3411.RFC3339, Value: time.Now().Add(-time.Minute)}}
	g.rx.notes[ended.Id] = ISANotification{ISAID: ended.Id, ServiceArea: &ended}
	g.rx.mu.Unlock()
	g.start(t)
	within(t, 3*time.Second, "own ISA skipped", func() bool { return g.dp.Counters.Get(CounterISAOwn) > 0 })
	if g.dp.Pollers() != 0 || self.Requests() != 0 || g.peer.Requests() != 0 {
		t.Fatalf("polled: %d pollers, self %d, ended %d", g.dp.Pollers(), self.Requests(), g.peer.Requests())
	}
}

// The network_rid switch (SC-16): off, the polls stop at once (the
// peer's request counter stops) and nothing is published; on, they
// resume. The peer's own instance switched off does the same.
func TestDPFollowsTheSwitch(t *testing.T) {
	g := newRig(t)
	g.peer.SetFlight(peersp.Flight{ID: peerFlight, Position: here})
	if _, err := g.peer.RegisterISA(context.Background(), "55555555-5555-4555-8555-555555555555", isaBox, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	g.start(t)
	within(t, 3*time.Second, "polled", func() bool { return g.peer.Requests() >= 2 })
	for _, key := range []string{SourceNetworkRID, SourceNetworkRID + "/" + g.peer.URL()} {
		g.gate.set(key, true)
		within(t, time.Second, "stopped", func() bool { return g.dp.Pollers() == 0 })
		n := g.peer.Requests()
		within(t, time.Second, "an off status", func() bool { return status(g.dp, "").State != "" })
		g.ticks(t, 4) // several poll periods, to see none come
		if got := g.peer.Requests(); got > n+1 {
			t.Fatalf("%s off: %d more requests", key, got-n)
		}
		g.gate.set(key, false)
		within(t, 2*time.Second, "resumed", func() bool { return g.peer.Requests() > n+2 })
	}
	g.gate.set(SourceNetworkRID, true)
	within(t, time.Second, "disabled", func() bool { return status(g.dp, "").State == sources.StateDisabled })
	if st, d := g.dp.Probe(context.Background()); st != obs.StateDegraded || !strings.Contains(d, "switched off") {
		t.Fatalf("probe %s %s", st, d)
	}
}

// A peer that stops answering: down since its first failure, its flights
// no longer published; the aggregate stale with the count; back, live
// (E-01 pair).
func TestDPPeerDownAndBack(t *testing.T) {
	g := newRig(t)
	g.peer.SetFlight(peersp.Flight{ID: peerFlight, Position: here})
	if _, err := g.peer.RegisterISA(context.Background(), "66666666-6666-4666-8666-666666666666", isaBox, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	g.start(t)
	within(t, 3*time.Second, "live", func() bool { return status(g.dp, g.peer.URL()).State == sources.StateLive })
	g.peer.Down(true)
	down := time.Now()
	within(t, 2*time.Second, "down", func() bool { return status(g.dp, g.peer.URL()).State == sources.StateDown })
	b := status(g.dp, g.peer.URL())
	if b.Since.Before(down.Add(-time.Second)) || !strings.Contains(b.Detail, "503") {
		t.Fatalf("down %+v", b)
	}
	if a := status(g.dp, ""); a.State != sources.StateStale || !strings.Contains(a.Detail, "1 of 1 peers do not answer") {
		t.Fatalf("aggregate %+v", a)
	}
	n := g.sink.count()
	g.ticks(t, 4)
	if g.sink.count() != n {
		t.Fatal("published while the peer is down")
	}
	g.peer.Down(false)
	within(t, 2*time.Second, "back", func() bool { return status(g.dp, g.peer.URL()).State == sources.StateLive })
}

// A peer that failed and whose ISAs then ended is no longer polled: it
// stays down for peer_unavailable_s after its last failure, then it is
// forgotten, so the aggregate is not stale for ever over a peer nobody
// polls; a peer never answered and no longer polled likewise. The pair
// (E-01): a peer still polled stays down however long it fails, and a
// peer switched off stays disabled.
func TestDPForgetsAFailedPeerWhoseISAsEnded(t *testing.T) {
	g := newRig(t)
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	g.dp.Now = func() time.Time { return now }
	g.dp.dssOK = now
	window := time.Duration(g.dp.policy().PeerUnavailableS * float64(time.Second))
	failed, polled, never, off := "https://failed.example", "https://polled.example", "https://never.example", "https://off.example"
	g.dp.peers[failed] = &peerState{ok: now.Add(-time.Minute), failed: now, since: now.Add(-30 * time.Second), down: true, lastErr: "503"}
	g.dp.peers[polled] = &peerState{failed: now, since: now.Add(-30 * time.Second), down: true, lastErr: "503"}
	g.dp.peers[never] = &peerState{}
	g.dp.peers[off] = &peerState{ok: now}
	g.gate.set(SourceNetworkRID+"/"+off, true)
	g.dp.pollers[pollKey{polled, "v"}] = &poller{}
	if b := status(g.dp, failed); b.State != sources.StateDown {
		t.Fatalf("within the window the failed peer is not down: %+v", b)
	}
	now = now.Add(window + time.Second)
	if b := status(g.dp, failed); b.SourceInstance != nil {
		t.Fatalf("a failed peer whose ISAs ended is kept: %+v", b)
	}
	if b := status(g.dp, never); b.SourceInstance != nil {
		t.Fatalf("a peer never answered and not polled is kept: %+v", b)
	}
	if b := status(g.dp, polled); b.State != sources.StateDown {
		t.Fatalf("a polled peer that fails is forgotten: %+v", b)
	}
	if b := status(g.dp, off); b.State != sources.StateDisabled {
		t.Fatalf("a switched-off peer is forgotten: %+v", b)
	}
	if a := status(g.dp, ""); a.State != sources.StateStale || !strings.Contains(a.Detail, "1 of 2 peers do not answer") {
		t.Fatalf("aggregate %+v", a)
	}
	delete(g.dp.pollers, pollKey{polled, "v"})
	now = now.Add(window + time.Second)
	if a := status(g.dp, ""); a.State != sources.StateLive {
		t.Fatalf("no peer is polled or failing: the aggregate is %+v", a)
	}
}

// Over peer_flights_max_count the answer is refused whole and counted,
// never shown in part as if complete; at the bound it is shown (E-10).
func TestDPRefusesAnAnswerOverTheFlightCap(t *testing.T) {
	g := newRig(t)
	v := policy.Defaults()
	v.PeerFlightsMax = 3
	g.pol.Store(&v)
	g.peer.SetFlight(peersp.Flight{ID: peerFlight, Position: here})
	g.peer.Pad(2) // 3 flights: at the bound
	ctx := context.Background()
	g.dp.PollOnce(ctx, g.peer.URL(), area)
	if g.sink.count() != 3 || g.dp.Counters.Get(CounterOverMax) != 0 {
		t.Fatalf("at the bound: %d published, %v", g.sink.count(), g.dp.Counters.Snapshot())
	}
	g.peer.Pad(3) // 4 flights: over it
	g.dp.PollOnce(ctx, g.peer.URL(), area)
	if g.sink.count() != 3 || g.dp.Counters.Get(CounterOverMax) != 1 {
		t.Fatalf("over the bound: %d published, %v", g.sink.count(), g.dp.Counters.Snapshot())
	}
	if b := status(g.dp, g.peer.URL()); b.State != sources.StateDown || b.Counters["refused"] != 1 {
		t.Fatalf("status %+v", b)
	}
}

// The echo guard (Q23): a peer answer that carries one of our own
// flights publishes nothing for it, counted; another flight at the same
// place is published (E-01 pair).
func TestDPEchoGuard(t *testing.T) {
	g := newRig(t)
	g.dp.Own = ownSet{"ours-1": true}
	g.peer.SetFlight(peersp.Flight{ID: "ours-1", Position: here})
	g.peer.SetFlight(peersp.Flight{ID: "theirs-1", Position: here})
	g.dp.PollOnce(context.Background(), g.peer.URL(), area)
	ps := g.sink.peers()
	if len(ps) != 1 || ps[0].Body.TrackID != "theirs-1" || g.dp.Counters.Get(CounterEchoOwnFlight) != 1 {
		t.Fatalf("published %+v, counters %v", ps, g.dp.Counters.Snapshot())
	}
}

// A slow peer (over F3411's 1 s more than once in 20 answers) is polled
// at 0.5 Hz and its status says so; a fast one at 1 Hz.
func TestDPSlowPeer(t *testing.T) {
	ps := &peerState{latencies: []time.Duration{2 * time.Second}}
	if ps.slow() {
		t.Fatal("one slow answer is slow")
	}
	ps.latencies = append(ps.latencies, 1500*time.Millisecond)
	if !ps.slow() {
		t.Fatal("two slow answers are not slow")
	}
	g := newRig(t)
	g.dp.init()
	g.dp.peers[g.peer.URL()] = ps
	ps.ok = time.Now()
	if b := status(g.dp, g.peer.URL()); b.State != sources.StateLive || !strings.Contains(b.Detail, "0.5 Hz") {
		t.Fatalf("status %+v", b)
	}
	// The poll period follows: a poller of a slow peer waits SlowEvery.
	g.peer.SetFlight(peersp.Flight{ID: peerFlight, Position: here})
	ctx, cancel := context.WithCancel(context.Background())
	p := &poller{key: pollKey{g.peer.URL(), ViewString(area)}, box: area, cancel: cancel, done: make(chan struct{})}
	start := time.Now()
	go g.dp.poll(ctx, p)
	within(t, 3*time.Second, "three polls", func() bool { return g.peer.Requests() >= 3 })
	cancel()
	<-p.done
	if took := time.Since(start); took < 2*g.dp.SlowEvery {
		t.Fatalf("three polls in %s: not at the slow period %s", took, g.dp.SlowEvery)
	}
}

// Our subscription is renewed before the DSS's 24 h end (an update by
// version), and deleted when its area is no longer wanted.
func TestDPRenewsAndDeletesItsSubscription(t *testing.T) {
	g := newRig(t)
	var offset atomic.Int64
	g.dp.Now = func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	g.dss.Now = g.dp.Now
	g.dp.SearchEvery = time.Hour
	ctx := context.Background()
	g.dp.Discover(ctx)
	subs := g.dss.Subscriptions()
	if len(subs) != 1 {
		t.Fatalf("subscriptions %v", subs)
	}
	var v1 string
	for _, s := range subs {
		v1 = s.Version
	}
	g.dp.Discover(ctx)
	if g.dp.Counters.Get(CounterSubscriptionPut) != 1 {
		t.Fatal("renewed before it was due")
	}
	offset.Store(int64(20 * time.Hour))
	g.dp.Discover(ctx)
	for _, s := range g.dss.Subscriptions() {
		if s.Version == v1 || g.dp.Counters.Get(CounterSubscriptionPut) != 2 {
			t.Fatalf("not renewed: %+v", s)
		}
	}
	// A restart (no version known): created again, found existing,
	// updated.
	g.dp.subs = map[string]*subState{}
	g.dp.Discover(ctx)
	if g.dp.Counters.Get(CounterSubscriptionPut) != 3 || len(g.dss.Subscriptions()) != 1 {
		t.Fatalf("after a restart %v %v", g.dp.Counters.Snapshot(), g.dss.Subscriptions())
	}
	g.dp.Areas = func() ([]Area, bool) { return []Area{{ID: "TSA-2", Box: area}}, true }
	g.dp.Discover(ctx)
	subs = g.dss.Subscriptions()
	if _, ok := subs[SubscriptionID(g.rx.srv.URL, "TSA-2")]; !ok || len(subs) != 1 || g.dp.Counters.Get(CounterSubscriptionDel) != 1 {
		t.Fatalf("area change: %v %v", subs, g.dp.Counters.Snapshot())
	}
}

// The DSS down: discovery says so (stale since then, the peers known
// polled), counted; the areas unknown: nothing asked of the DSS.
func TestDPDSSDownAndUnknownAreas(t *testing.T) {
	g := newRig(t)
	g.dp.Areas = func() ([]Area, bool) { return nil, false }
	g.dp.Discover(context.Background())
	if b := status(g.dp, ""); b.State != sources.StateUnknown || len(g.dss.Calls()) != 0 {
		t.Fatalf("unknown areas: %+v, %d DSS calls", b, len(g.dss.Calls()))
	}
	g.dp.Areas = func() ([]Area, bool) { return []Area{{ID: "TSA-1", Box: area}}, true }
	g.dss.Down(true)
	g.dp.Discover(context.Background())
	if b := status(g.dp, ""); b.State != sources.StateStale || !strings.Contains(b.Detail, "the DSS cannot be searched") {
		t.Fatalf("DSS down: %+v", b)
	}
	if st, _ := g.dp.Probe(context.Background()); st != obs.StateDegraded {
		t.Fatalf("probe %s", st)
	}
	g.dss.Down(false)
	g.dp.Discover(context.Background())
	if b := status(g.dp, ""); b.State != sources.StateLive {
		t.Fatalf("DSS back: %+v", b)
	}
	// An area that cannot be tiled is refused, named.
	g.dp.Areas = func() ([]Area, bool) {
		return []Area{{ID: "HUGE", Box: geodesy.BBox{MinLat: -60, MinLon: -170, MaxLat: 60, MaxLon: 170}}}, true
	}
	g.dp.Discover(context.Background())
	if b := status(g.dp, ""); !strings.Contains(b.Detail, "HUGE") || g.dp.Counters.Get(CounterAreaRefused) == 0 {
		t.Fatalf("huge area: %+v", b)
	}
}

// Details only for a view of at most 2 km, on request (E-01 pair).
func TestDPDetails(t *testing.T) {
	g := newRig(t)
	g.peer.SetFlight(peersp.Flight{ID: peerFlight, Position: here})
	if _, err := g.dp.Details(context.Background(), g.peer.URL(), peerFlight, area); !errors.Is(err, ErrDetailsViewTooLarge) {
		t.Fatalf("a %0.f m view: %v", DiagonalM(area), err)
	}
	small := geodesy.BBox{MinLat: 41.749, MinLon: 44.849, MaxLat: 41.751, MaxLon: 44.851}
	raw, err := g.dp.Details(context.Background(), g.peer.URL(), peerFlight, small)
	if err != nil || !strings.Contains(string(raw), peerFlight) {
		t.Fatalf("details %s %v", raw, err)
	}
	if _, err := g.dp.Details(context.Background(), g.peer.URL(), "unknown", small); err == nil {
		t.Fatal("a 404 answered")
	}
	g.gate.set(SourceNetworkRID+"/"+g.peer.URL(), true)
	if _, err := g.dp.Details(context.Background(), g.peer.URL(), peerFlight, small); err == nil {
		t.Fatal("details of a switched-off peer")
	}
}

// Poll answers that are not usable are refused and counted.
func TestDPPollRefusals(t *testing.T) {
	var body atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body.Load().(string)))
	}))
	t.Cleanup(srv.Close)
	g := newRig(t)
	for _, b := range []string{`not json`, `{"timestamp":{"value":"x"}}`, `{"flights":[{"id":""}],"timestamp":{"format":"RFC3339","value":"2026-10-04T12:00:00Z"}}`} {
		body.Store(b)
		before := g.dp.Counters.Get(CounterPollRefused)
		g.dp.PollOnce(context.Background(), srv.URL, area)
		if g.dp.Counters.Get(CounterPollRefused) != before+1 {
			t.Errorf("%s: not refused", b)
		}
	}
	body.Store(`{"no_isas_present":true,"timestamp":{"format":"RFC3339","value":"2026-10-04T12:00:00Z"}}`)
	g.dp.PollOnce(context.Background(), srv.URL, area)
	if g.dp.Counters.Get(CounterNoISAs) != 1 {
		t.Fatal("no_isas_present not counted")
	}
	if checkBase("ftp://x") == nil || checkBase("https://"+strings.Repeat("x", MaxBaseURLBytes)) == nil || checkBase("https://peer") != nil {
		t.Fatal("checkBase")
	}
	if n, err := DecodeNotification("k", []byte(`{"isa_id":"x","deleted":true}`)); err != nil || !n.Deleted {
		t.Fatalf("DecodeNotification %v %v", n, err)
	}
	if _, err := DecodeNotification("k", []byte(`[`)); err == nil {
		t.Fatal("a bad notification decoded")
	}
}
