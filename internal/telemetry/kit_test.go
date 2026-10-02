package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3411"
	"github.com/rootxkit/uspace-core/serial"
	coresources "github.com/rootxkit/uspace-core/sources"
	"github.com/rootxkit/uspace-core/zones"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/registry"
)

// The fixture: the vectors' point in Tbilisi, a fixed clock.
var (
	t0      = time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	tbilisi = Position{Lat: 41.7151, Lng: 44.8271}
)

const (
	clientA = "op-client-a"
	clientB = "op-client-b"
	snA     = "TEST-SN-A"
	snB     = "TEST-SN-B"
	intent1 = "8c1f3f2e-7d0e-4a8b-9a51-0e4b7d6f2c11"
)

func f64(v float64) *float64 { return &v }
func str(s string) *string   { return &s }

// frame is a sample of sn taken at ts.
func frame(sn string, seq int64, ts time.Time) Frame {
	return Frame{
		TS: ts, Serial: sn, Seq: seq, Position: tbilisi, AltWGS84M: f64(650), HeightM: f64(80), HeightRef: str("TakeoffLocation"),
		SpeedMS: f64(8), TrackDeg: f64(90), VSpeedMS: f64(0), Status: f3411.Airborne, AccuracyH: f3411.HA3m, AccuracyV: f3411.VA10m,
		TimestampAccuracyS: f64(0.1),
	}
}

// clock is a settable clock.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = at
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// bindings is BindingsSource in memory.
type bindings struct {
	mu     sync.Mutex
	folds  map[string][]string
	loaded bool
}

func newBindings(loaded bool) *bindings {
	return &bindings{folds: map[string][]string{}, loaded: loaded}
}

func (b *bindings) bind(client string, sns ...string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, sn := range sns {
		b.folds[client] = append(b.folds[client], serial.FoldKey(sn))
	}
}

func (b *bindings) Folds(c string) ([]string, float64, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.folds[c], 0, b.loaded
}

// intents is IntentSource in memory.
type intents struct {
	mu     sync.Mutex
	all    map[string]IntentFacts
	loaded bool
}

func (s *intents) Intent(id string) (IntentFacts, bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.all[id]
	return f, ok, s.loaded
}

func (s *intents) set(f IntentFacts) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.all[f.IntentID] = f
}

// registrySrc is RegistrySource in memory.
type registrySrc struct {
	all    map[registry.Key]registry.Entry
	loaded bool
}

func (r *registrySrc) Entry(k registry.Key) (registry.Entry, bool, bool) {
	e, ok := r.all[k]
	return e, ok, r.loaded
}

// airspace is an AirspaceJudge that answers v.
type airspace struct {
	mu sync.Mutex
	v  AirspaceVerdict
}

func (a *airspace) At(core.LatLon, zones.Aircraft, zones.Env) AirspaceVerdict {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.v
}

func (a *airspace) set(v AirspaceVerdict) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.v = v
}

// undulation is a geoid of one value.
type undulation float64

func (u undulation) UndulationM(core.LatLon) (float64, error) { return float64(u), nil }

// gate is a SourceGate with switches by instance.
type gate struct {
	mu  sync.Mutex
	off map[string]bool
}

func (g *gate) Query(_ string, inst *string) coresources.Decision {
	g.mu.Lock()
	defer g.mu.Unlock()
	if inst != nil && g.off[*inst] {
		w := coresources.WhyInstance
		return coresources.Decision{WhyDisabled: &w}
	}
	return coresources.Decision{Enabled: true}
}

func (g *gate) set(inst string, off bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.off == nil {
		g.off = map[string]bool{}
	}
	g.off[inst] = off
}

// published is one message a publisher was given.
type published struct {
	subject string
	data    []byte
}

// pub is a MessagePublisher that keeps what it was given and fails
// while fail is set.
type pub struct {
	mu   sync.Mutex
	msgs []published
	fail map[string]bool // by subject kind
}

func (p *pub) Publish(_ context.Context, subject string, m bus.Enveloped) error {
	s, err := bus.Parse(subject)
	if err != nil {
		return err
	}
	if err := m.Head().Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail[s.Kind] {
		return errors.New("bus down")
	}
	p.msgs = append(p.msgs, published{subject: subject, data: data})
	return nil
}

func (p *pub) setFail(kind string, fail bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail == nil {
		p.fail = map[string]bool{}
	}
	p.fail[kind] = fail
}

// kind returns the messages of one subject kind.
func (p *pub) kind(k string) []published {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []published
	for _, m := range p.msgs {
		if s, _ := bus.Parse(m.subject); s.Kind == k {
			out = append(out, m)
		}
	}
	return out
}

// tracks decodes the published trk messages.
func (p *pub) tracks(t *testing.T) []Track {
	t.Helper()
	var out []Track
	for _, m := range p.kind(bus.KindTrk) {
		var tr Track
		if err := json.Unmarshal(m.data, &tr); err != nil {
			t.Fatal(err)
		}
		out = append(out, tr)
	}
	return out
}

// binderCall is one Bind of the fake binder.
type binderCall struct {
	key, client, serial string
	intentID            *string
	live                bool
	at                  time.Time
}

// binder is a FlightBinder that hands out one flight per key and intent.
type binder struct {
	mu    sync.Mutex
	calls []binderCall
	ends  []string
	ids   map[string]string
}

func (b *binder) Bind(key, clientID, sn string, intentID, _, _ *string, _ core.LatLon, at time.Time, live bool) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, binderCall{key: key, client: clientID, serial: sn, intentID: intentID, live: live, at: at})
	if b.ids == nil {
		b.ids = map[string]string{}
	}
	k := key
	if intentID != nil {
		k += "/" + *intentID
	}
	if _, ok := b.ids[k]; !ok {
		b.ids[k] = flightIDs[len(b.ids)%len(flightIDs)]
	}
	return b.ids[k]
}

func (b *binder) End(key, reason string, _ time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ends = append(b.ends, key+":"+reason)
}

var flightIDs = []string{
	"5b0f7c1e-2a4d-4c8e-9f3a-6d2b1e0c9a87", "6c1e8d2f-3b5e-4d9f-8a4b-7e3c2f1d0b98",
	"7d2f9e3a-4c6f-4e0a-9b5c-8f4d3a2e1ca9", "8e3a0f4b-5d7a-4f1b-ac6d-9a5e4b3f2dba",
}

// rig is an Ingestor on fakes with a running outbox.
type rig struct {
	t        *testing.T
	in       *Ingestor
	clk      *clock
	bind     *bindings
	intents  *intents
	reg      *registrySrc
	air      *airspace
	gate     *gate
	pub      *pub
	flights  *binder
	outbox   *Outbox
	events   *Events
	pol      policy.Record
	counters *core.Counters
	stop     context.CancelFunc
	done     sync.WaitGroup
}

type rigOpts struct {
	geoid       bool
	queueFrames int
	noRun       bool // the outbox is not running (tests drive it)
	maxAircraft int
	seen        SeenStore
}

func newRig(t *testing.T, o rigOpts) *rig {
	t.Helper()
	r := &rig{t: t, clk: &clock{at: t0}, bind: newBindings(true), intents: &intents{all: map[string]IntentFacts{}, loaded: true},
		reg: &registrySrc{all: map[registry.Key]registry.Entry{}, loaded: true}, air: &airspace{v: AirspaceVerdict{Judged: true}},
		gate: &gate{}, pub: &pub{}, flights: &binder{}, pol: policy.Record{Version: 7, Values: policy.Defaults()},
		counters: &core.Counters{}}
	r.bind.bind(clientA, snA, snB)
	r.outbox = NewOutbox(r.pub, o.queueFrames, 16, nil, nil)
	r.outbox.Now = r.clk.now
	r.events = NewEvents(r.pub, 0, nil, nil)
	cfg := Config{
		Bindings: r.bind, Intents: r.intents, Registry: r.reg, Airspace: r.air, Sources: r.gate,
		Policy: func() policy.Record { return r.pol }, Flights: r.flights, Outbox: r.outbox, Events: r.events,
		Counters: r.counters, Now: r.clk.now, MaxAircraft: o.maxAircraft, Seen: o.seen,
	}
	if o.geoid {
		cfg.Geoid = undulation(15.9)
	}
	r.in = New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	r.stop = cancel
	if !o.noRun {
		r.done.Go(func() { r.outbox.Run(ctx) })
		r.done.Go(func() { r.outbox.RunSpill(ctx) })
		r.done.Go(func() { r.events.Run(ctx) })
	}
	t.Cleanup(func() { cancel(); r.done.Wait() })
	return r
}

// take delivers frames of client at the rig's clock and waits until
// every accepted one was handed.
func (r *rig) take(client string, s *Session, frames ...Frame) []Result {
	r.t.Helper()
	var mu sync.Mutex
	handed := map[int]bool{}
	res := r.in.Take(context.Background(), Delivery{ClientID: client, Session: s, RxTS: r.clk.now(), Frames: frames,
		Handed: func(i int, _ *Frame, ok bool) { mu.Lock(); handed[i] = ok; mu.Unlock() }})
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		all := true
		for _, x := range res {
			if _, ok := handed[x.Index]; x.Reason == OutcomeAccepted && !ok {
				all = false
			}
		}
		mu.Unlock()
		if all {
			return res
		}
		if time.Now().After(deadline) {
			r.t.Fatal("accepted samples not handed within 5 s")
		}
		time.Sleep(time.Millisecond)
	}
}

func reasons(rs []Result) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Reason
	}
	return out
}
