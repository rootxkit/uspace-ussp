package flights

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/policy"
)

var t0 = time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)

type clock struct {
	mu sync.Mutex
	at time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.at = c.at.Add(d); c.mu.Unlock() }

func str(s string) *string { return &s }

// binderRig is a Binder whose facts are collected.
type binderRig struct {
	b      *Binder
	clk    *clock
	events []*Event
	active map[string]bool
	known  bool
}

func newBinderRig() *binderRig {
	r := &binderRig{clk: &clock{at: t0}, active: map[string]bool{}, known: true}
	r.b = &Binder{
		Emit:         func(e *Event) { r.events = append(r.events, e) },
		Policy:       policy.Defaults,
		IntentActive: func(id string) (bool, bool) { return r.active[id], r.known },
		Now:          r.clk.now,
	}
	return r
}

func (r *binderRig) kinds() []string {
	out := make([]string, len(r.events))
	for i, e := range r.events {
		out[i] = e.Body.Event
		if e.Body.EndReason != nil {
			out[i] += ":" + *e.Body.EndReason
		}
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var uuid4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// One flight per aircraft: the same flight for its samples, a version 4
// UUID, started once; another aircraft has its own.
func TestBindStartsOneFlightPerAircraft(t *testing.T) {
	r := newBinderRig()
	a := r.b.Bind("c|A", "c", "A", nil, nil, nil, t0, true)
	if !uuid4.MatchString(a) || r.b.Bind("c|A", "c", "A", nil, nil, nil, t0.Add(time.Second), true) != a {
		t.Fatalf("flight %q", a)
	}
	if b := r.b.Bind("c|B", "c", "B", nil, nil, nil, t0, true); b == a {
		t.Fatal("two aircraft share a flight")
	}
	if !equal(r.kinds(), []string{EventStarted, EventStarted}) || r.b.Len() != 2 {
		t.Fatalf("events %v", r.kinds())
	}
	e := r.events[0]
	if e.Body.FlightID != a || !e.Body.StartedAt.Equal(t0) || e.Body.ClientID != "c" || e.Body.UASSerial != "A" || e.Body.EndReason != nil {
		t.Fatalf("started %+v", e.Body)
	}
}

// A flight without an intent whose samples then fly an intent (the
// intent_active projection caught up, or the operator activated it in
// flight) is that intent's flight: the same flight, bound to the intent,
// never ended operator_ended; its later facts carry the intent. An
// intent flight followed by another intent ends intent_ended and starts
// the next (the twin: a real change of intent does end the flight).
func TestIntentChangeStartsTheNextFlight(t *testing.T) {
	r := newBinderRig()
	r.active["i-1"], r.active["i-2"] = true, true
	session := r.b.Bind("c|A", "c", "A", nil, nil, nil, t0, true)
	i1 := r.b.Bind("c|A", "c", "A", str("i-1"), str("GE-1"), str("GEO-TEST-1"), t0.Add(time.Second), true)
	if session != i1 {
		t.Fatalf("a flight without an intent was not bound to the intent: %v", r.kinds())
	}
	r.clk.add(7 * time.Second)
	r.b.Tick() // telemetry_lost: a fact of the bound flight
	i2 := r.b.Bind("c|A", "c", "A", str("i-2"), nil, nil, t0.Add(8*time.Second), true)
	if i1 == i2 {
		t.Fatal("no new flight for another intent")
	}
	want := []string{EventStarted, EventTelemetryLost, "ended:" + EndIntentEnded, EventStarted}
	if !equal(r.kinds(), want) {
		t.Fatalf("events %v", r.kinds())
	}
	for _, e := range r.events[1:3] {
		if e.Body.FlightID != i1 || e.Body.IntentID == nil || *e.Body.IntentID != "i-1" ||
			e.Body.AuthorisationNumber == nil || e.Body.OperatorReg == nil || !e.Body.StartedAt.Equal(t0) {
			t.Fatalf("bound flight %+v", e.Body)
		}
	}
	if r.b.Counters.Get(CounterIntentBound) != 1 {
		t.Fatalf("binding not counted: %v", r.b.Counters.Snapshot())
	}
}

// Tick: telemetry_lost after telemetry_lost_s of no live sample (not an
// end), resumed by the next live one (a backlog sample does not resume
// it), ended after flight_end_after_s of silence.
func TestTickLostResumedEnded(t *testing.T) {
	r := newBinderRig()
	id := r.b.Bind("c|A", "c", "A", nil, nil, nil, t0, true)
	r.clk.add(4 * time.Second)
	r.b.Tick()
	if len(r.events) != 1 {
		t.Fatalf("lost early: %v", r.kinds())
	}
	r.clk.add(time.Second)
	r.b.Tick()
	r.b.Tick()
	if !equal(r.kinds(), []string{EventStarted, EventTelemetryLost}) {
		t.Fatalf("lost: %v", r.kinds())
	}
	r.b.Bind("c|A", "c", "A", nil, nil, nil, t0.Add(-time.Minute), false) // backlog
	if len(r.events) != 2 {
		t.Fatalf("a backlog sample resumed it: %v", r.kinds())
	}
	if r.b.Bind("c|A", "c", "A", nil, nil, nil, r.clk.now(), true) != id {
		t.Fatal("a new flight after a loss")
	}
	if r.events[2].Body.Event != EventTelemetryResumed {
		t.Fatalf("resumed: %v", r.kinds())
	}
	r.clk.add(119 * time.Second)
	if r.b.Tick() != 0 {
		t.Fatal("ended before flight_end_after_s")
	}
	r.clk.add(time.Second)
	if r.b.Tick() != 1 || r.b.Len() != 0 {
		t.Fatalf("not ended: %v", r.kinds())
	}
	last := r.events[len(r.events)-1]
	if last.Body.Event != EventEnded || *last.Body.EndReason != EndTelemetryLost || last.Body.FlightID != id {
		t.Fatalf("ended %+v", last.Body)
	}
}

// A flight whose intent left intent_active ends intent_ended; while the
// projection is unread nothing is ended on its account (E-01 pair).
func TestIntentGoneEndsTheFlight(t *testing.T) {
	r := newBinderRig()
	r.active["i-1"] = true
	r.b.Bind("c|A", "c", "A", str("i-1"), nil, nil, t0, true)
	r.b.Tick()
	if r.b.Len() != 1 {
		t.Fatal("ended while active")
	}
	r.active["i-1"], r.known = false, false
	r.b.Tick()
	if r.b.Len() != 1 {
		t.Fatal("ended on an unread projection")
	}
	r.known = true
	r.b.Tick()
	if r.b.Len() != 0 || r.kinds()[len(r.events)-1] != "ended:"+EndIntentEnded {
		t.Fatalf("not ended: %v", r.kinds())
	}
}

// The operator's end ends the flight; an end without a flight is
// nothing.
func TestEnd(t *testing.T) {
	r := newBinderRig()
	r.b.End("c|A", EndOperator, t0)
	if len(r.events) != 0 {
		t.Fatal("an end without a flight")
	}
	r.b.Bind("c|A", "c", "A", nil, nil, nil, t0, true)
	r.b.End("c|A", EndOperator, t0.Add(time.Second))
	if r.b.Len() != 0 || r.kinds()[1] != "ended:"+EndOperator || !r.events[1].Body.At.Equal(t0.Add(time.Second)) {
		t.Fatalf("%v", r.kinds())
	}
}

// E-10: beyond its bound the binder ends the longest silent flight to
// make room, counted.
func TestBinderBound(t *testing.T) {
	r := newBinderRig()
	r.b.Max = 2
	r.b.Bind("c|A", "c", "A", nil, nil, nil, t0, true)
	r.clk.add(time.Second)
	r.b.Bind("c|B", "c", "B", nil, nil, nil, t0, true)
	r.clk.add(time.Second)
	r.b.Bind("c|C", "c", "C", nil, nil, nil, t0, true)
	if r.b.Len() != 2 || r.b.Counters.Get(CounterOverBound) != 1 {
		t.Fatalf("len %d counters %v", r.b.Len(), r.b.Counters.Snapshot())
	}
	if r.events[2].Body.UASSerial != "A" || *r.events[2].Body.EndReason != EndTelemetryLost {
		t.Fatalf("evicted %+v", r.events[2].Body)
	}
}

// E-03: every fact validates against schemas/flight/event/v1 and reads
// back through Decode; Decode refuses what the schema refuses.
func TestEventsValidateAndDecode(t *testing.T) {
	sch := flightSchema(t)
	r := newBinderRig()
	r.b.Bind("c|A", "c", "A", str("8c1f3f2e-7d0e-4a8b-9a51-0e4b7d6f2c11"), str("GE-1"), str("GEO-TEST-1"), t0, true)
	r.clk.add(10 * time.Second)
	r.b.Tick()
	r.b.Bind("c|A", "c", "A", str("8c1f3f2e-7d0e-4a8b-9a51-0e4b7d6f2c11"), nil, nil, r.clk.now(), true)
	r.b.End("c|A", EndOperator, r.clk.now())
	if len(r.events) != 4 {
		t.Fatalf("%v", r.kinds())
	}
	for _, e := range r.events {
		raw, _ := json.Marshal(e)
		inst, _ := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err := sch.Validate(inst); err != nil {
			t.Fatalf("%s: %v\n%s", e.Body.Event, err, raw)
		}
		back, err := Decode(raw)
		if err != nil || back.Body.FlightID != e.Body.FlightID {
			t.Fatalf("decode %v", err)
		}
		if s, err := e.Subject(); err != nil || s != "flight.v1."+e.Body.Event+"."+e.Body.FlightID {
			t.Fatalf("subject %q %v", s, err)
		}
	}
	good, _ := json.Marshal(r.events[0])
	for name, mut := range map[string]func(*Event){
		"schema":       func(e *Event) { e.Schema = "x/v1" },
		"flight id":    func(e *Event) { e.Body.FlightID = "FL-1" },
		"event":        func(e *Event) { e.Body.Event = "paused" },
		"reason early": func(e *Event) { e.Body.EndReason = str(EndLanded) },
		"no reason":    func(e *Event) { e.Body.Event = EventEnded },
		"bad reason":   func(e *Event) { e.Body.Event, e.Body.EndReason = EventEnded, str("crashed") },
		"no client":    func(e *Event) { e.Body.ClientID = "" },
		"intent id":    func(e *Event) { e.Body.IntentID = str("i") },
		"no time":      func(e *Event) { e.Body.At = bus.Stamp{} },
		"envelope":     func(e *Event) { e.MsgID = "x" },
	} {
		var e Event
		_ = json.Unmarshal(good, &e)
		mut(&e)
		raw, _ := json.Marshal(&e)
		if _, err := Decode(raw); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
	if _, err := Decode([]byte(`nope`)); err == nil {
		t.Error("garbage decoded")
	}
}

func flightSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	root := filepath.Join("..", "..", "schemas")
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	for _, n := range []string{"envelope/v1", "flight/event/v1"} {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(n), "schema.json"))
		if err != nil {
			t.Fatal(err)
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.AddResource("https://schemas.uspace.ge/"+n+".json", doc); err != nil {
			t.Fatal(err)
		}
	}
	s, err := c.Compile("https://schemas.uspace.ge/flight/event/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// msg is a FLIGHT message of a test.
type msg struct {
	data         []byte
	acked, naked bool
}

func (m *msg) Data() []byte               { return m.data }
func (m *msg) Subject() string            { return "flight.v1.started.x" }
func (m *msg) StreamSeq() (uint64, error) { return 1, nil }
func (m *msg) Ack() error                 { m.acked = true; return nil }
func (m *msg) Nak() error                 { m.naked = true; return nil }
func (m *msg) InProgress() error          { return nil }

type store struct {
	mu   sync.Mutex
	got  []Body
	fail bool
}

func (s *store) Record(_ context.Context, b Body) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return errors.New("database down")
	}
	s.got = append(s.got, b)
	return nil
}

type source struct {
	mu   sync.Mutex
	msgs [][]bus.Msg
}

func (s *source) Fetch(context.Context, int, time.Duration) ([]bus.Msg, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.msgs) == 0 {
		return nil, errors.New("timeout")
	}
	m := s.msgs[0]
	s.msgs = s.msgs[1:]
	return m, nil
}

// B-05 pairs: a fact is recorded, then acknowledged; one the store
// cannot take stays in the stream (nak) with the rest of the fetch; one
// that does not read is logged, counted and acknowledged.
func TestRecorder(t *testing.T) {
	r := newBinderRig()
	r.b.Bind("c|A", "c", "A", nil, nil, nil, t0, true)
	r.b.End("c|A", EndOperator, t0)
	started, _ := json.Marshal(r.events[0])
	ended, _ := json.Marshal(r.events[1])
	st := &store{}
	rec := &Recorder{Store: st}
	a, b, bad := &msg{data: started}, &msg{data: ended}, &msg{data: []byte(`{}`)}
	if !rec.Take(context.Background(), []bus.Msg{a, bad, b}) {
		t.Fatal("failed")
	}
	if !a.acked || !b.acked || !bad.acked || len(st.got) != 2 || rec.counters().Get(CounterRecordRefused) != 1 {
		t.Fatalf("got %d counters %v", len(st.got), rec.counters().Snapshot())
	}
	st.fail = true
	c, d := &msg{data: started}, &msg{data: ended}
	if rec.Take(context.Background(), []bus.Msg{c, d}) || c.acked || !c.naked || !d.naked {
		t.Fatal("an unrecorded fact acknowledged")
	}
	// Run reads until stopped.
	st.fail = false
	src := &source{msgs: [][]bus.Msg{{&msg{data: started}}}}
	rec2 := &Recorder{Source: src, Store: st, Wait: time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { rec2.Run(ctx) })
	deadline := time.Now().Add(5 * time.Second)
	for {
		st.mu.Lock()
		n := len(st.got)
		st.mu.Unlock()
		if n == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("not recorded")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	wg.Wait()
}
