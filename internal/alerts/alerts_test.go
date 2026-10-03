package alerts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/policy"
)

const (
	alertA  = "5f0c2a8e-3b1d-4c6e-9a7f-1d2e3f4a5b6c"
	flightA = "11111111-1111-4111-8111-111111111111"
	cellA   = "c5:1017:2248"
)

var t0 = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

// msg is a bus.Msg in memory.
type msg struct {
	subject string
	data    []byte
	mu      sync.Mutex
	acked   bool
	naked   bool
}

func (m *msg) isAcked() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.acked
}

func (m *msg) Data() []byte               { return m.data }
func (m *msg) Subject() string            { return m.subject }
func (m *msg) Ack() error                 { m.mu.Lock(); m.acked = true; m.mu.Unlock(); return nil }
func (m *msg) Nak() error                 { m.mu.Lock(); m.naked = true; m.mu.Unlock(); return nil }
func (m *msg) StreamSeq() (uint64, error) { return 1, nil }
func (m *msg) InProgress() error          { return nil }

func body(state string, at time.Time) Body {
	b := Body{
		AlertID: alertA, Kind: "proximity", Severity: core.SeverityCritical, State: state, FlightID: flightA,
		CapturedAt: t0, RaisedAt: t0, UpdatedAt: at, PolicyVersion: 3,
		Detail: json.RawMessage(`{"t_cpa_s":10,"d_cpa_h_m":1,"d_alt_m":null,"peer":{"track_id":"22222222-2222-4222-8222-222222222222","trust":"authenticated"},"evaluation_period_s":1}`),
	}
	if state == StateCleared {
		r := "resolved"
		b.ClearReason = &r
	}
	return b
}

func alertMsg(t *testing.T, b Body) *msg {
	t.Helper()
	raw, err := json.Marshal(&Message{Envelope: bus.SystemEnvelope(SchemaAlert, "ussp/monitor", b.UpdatedAt), Body: b})
	if err != nil {
		t.Fatal(err)
	}
	return &msg{subject: "alrt.v1." + b.Kind + "." + cellA + "." + b.AlertID, data: raw}
}

// memStore is the Store and FactStore in memory.
type memStore struct {
	mu         sync.Mutex
	flights    map[string]bool
	rows       map[string]Record
	writes     int
	deliveries map[string]time.Time
	fail       error
	acked      map[string]time.Time
}

func newMem() *memStore {
	return &memStore{flights: map[string]bool{flightA: true}, rows: map[string]Record{}, deliveries: map[string]time.Time{}, acked: map[string]time.Time{}}
}

func (m *memStore) RecordAlert(_ context.Context, r Record) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return false, m.fail
	}
	if !m.flights[r.FlightID] {
		return false, nil
	}
	if cur, ok := m.rows[r.AlertID]; ok && cur.State == StateCleared {
		return true, nil
	}
	m.rows[r.AlertID] = r
	m.writes++
	return true, nil
}

func (m *memStore) RecordDelivery(_ context.Context, d DeliveryBody) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rows[d.AlertID]; !ok {
		return false, nil
	}
	if _, ok := m.deliveries[d.AlertID+"|"+d.ClientID]; !ok {
		m.deliveries[d.AlertID+"|"+d.ClientID] = d.SentAt
	}
	return true, nil
}

func (m *memStore) Ack(_ context.Context, alertID, clientID string) (Stored, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[alertID]
	if !ok || clientID != "client-a" {
		return Stored{}, ErrNotFound
	}
	if _, ok := m.acked[alertID]; !ok {
		m.acked[alertID] = t0.Add(10 * time.Second)
	}
	at := m.acked[alertID]
	b := r.Body
	b.AckedAt = &at
	return Stored{Body: b, Cell5: r.Cell5, AckedBy: &clientID}, nil
}

func (m *memStore) Escalate(context.Context, float64, int) ([]Stored, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return nil, m.fail
	}
	var out []Stored
	for id := range m.rows {
		r := m.rows[id]
		if _, acked := m.acked[id]; acked || r.State == StateCleared || r.EscalatedAt != nil {
			continue
		}
		at := t0.Add(30 * time.Second)
		r.EscalatedAt = &at
		m.rows[id] = r
		out = append(out, Stored{Body: r.Body, Cell5: r.Cell5})
	}
	return out, nil
}

// OpenNotices are the open notices (the memory store does not model the
// intent; a test sets the rows), at most maxRows.
func (m *memStore) OpenNotices(ctx context.Context, maxRows int) ([]Stored, error) {
	out, err := m.EndedNotices(ctx, maxRows)
	slices.SortFunc(out, func(a, b Stored) int { return strings.Compare(a.AlertID, b.AlertID) })
	if len(out) > maxRows {
		out = out[:maxRows]
	}
	return out, err
}

func (m *memStore) EndedNotices(context.Context, int) ([]Stored, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return nil, m.fail
	}
	var out []Stored
	for id := range m.rows {
		if r := m.rows[id]; r.Kind == KindRestrictionActivated && r.State != StateCleared {
			out = append(out, Stored{Body: r.Body, Cell5: r.Cell5})
		}
	}
	return out, nil
}

// The recorder writes a raise, a clear and the first republish, keeps
// one refresh in RefreshEvery, never reopens a clear, and leaves a
// message whose flight is not recorded yet in the stream (the presence
// twin of a written one).
func TestRecorderWritesFactsAndThrottlesRefreshes(t *testing.T) {
	st := newMem()
	r := &Recorder{Store: st, RefreshEvery: 5 * time.Second}
	ctx := context.Background()
	ms := []*msg{alertMsg(t, body(StateRaised, t0))}
	for i := 1; i <= 6; i++ {
		ms = append(ms, alertMsg(t, body(StateUpdated, t0.Add(time.Duration(i)*time.Second))))
	}
	ms = append(ms, alertMsg(t, body(StateCleared, t0.Add(7*time.Second))), alertMsg(t, body(StateUpdated, t0.Add(8*time.Second))))
	for _, m := range ms {
		if !r.Take(ctx, []bus.Msg{m}) || !m.acked {
			t.Fatalf("%s not acknowledged", m.subject)
		}
	}
	// raise, the refresh at 5 s, the clear; the update after the clear
	// reaches the store and changes nothing there.
	if st.writes != 3 || st.rows[alertA].State != StateCleared || st.rows[alertA].Cell5 != cellA {
		t.Fatalf("writes %d row %+v", st.writes, st.rows[alertA])
	}
	// Skipped: the updates at 1, 2, 3, 4 and 6 s.
	if r.Counters.Get(CounterRefreshSkipped) != 5 {
		t.Fatalf("skipped %d", r.Counters.Get(CounterRefreshSkipped))
	}
	if p := st.rows[alertA].PeerRef; p == nil || *p != "22222222-2222-4222-8222-222222222222" {
		t.Fatalf("peer %v", p)
	}
	// A flight not recorded yet: left unacknowledged for redelivery
	// without holding up the next message, then given up at the bound.
	other := body(StateRaised, t0)
	other.AlertID, other.FlightID = "6f0c2a8e-3b1d-4c6e-9a7f-1d2e3f4a5b6d", "33333333-3333-4333-8333-333333333333"
	m := alertMsg(t, other)
	behind := body(StateUpdated, t0.Add(20*time.Second))
	behind.AlertID = "7f0c2a8e-3b1d-4c6e-9a7f-1d2e3f4a5b6e"
	next := alertMsg(t, behind)
	r.MaxAttempts = 2
	if !r.Take(ctx, []bus.Msg{m, next}) || m.acked || m.naked || !next.acked || r.Counters.Get(CounterDeferred) != 1 {
		t.Fatalf("deferred %v acked %v naked %v next %v", r.Counters.Get(CounterDeferred), m.acked, m.naked, next.acked)
	}
	if !r.Take(ctx, []bus.Msg{m}) || !m.acked || r.Counters.Get(CounterGaveUp) != 1 {
		t.Fatal("not given up at the bound")
	}
	// A store that fails holds the fetch (nak) and says so.
	st.fail = errors.New("db down")
	f := alertMsg(t, body(StateUpdated, t0.Add(30*time.Second)))
	if r.Take(ctx, []bus.Msg{f}) || !f.naked || r.Counters.Get(CounterRetried) != 1 {
		t.Fatal("a failing store did not hold the fetch")
	}
	st.fail = nil
}

// An unreadable message and another schema are acknowledged, counted,
// never written; a delivery is recorded once.
func TestRecorderUnreadableOtherSchemaAndDelivery(t *testing.T) {
	st := newMem()
	r := &Recorder{Store: st}
	ctx := context.Background()
	bad := &msg{subject: "alrt.v1.proximity." + cellA + "." + alertA, data: []byte(`{"schema":"alert/v1","body":{}}`)}
	other := &msg{subject: "alrt.v1.proximity." + cellA + "." + alertA, data: []byte(`{"schema":"something/v1"}`)}
	r.Take(ctx, []bus.Msg{bad, other})
	if !bad.acked || !other.acked || st.writes != 0 || r.Counters.Get(CounterUnread) != 1 || r.Counters.Get(CounterOtherSchema) != 1 {
		t.Fatalf("writes %d counters %v", st.writes, r.Counters.Snapshot())
	}
	r.Take(ctx, []bus.Msg{alertMsg(t, body(StateRaised, t0))})
	d := Delivery{Envelope: bus.SystemEnvelope(SchemaDelivery, "ussp/traffic-ws", t0), Body: DeliveryBody{AlertID: alertA, ClientID: "client-a", SentAt: t0}}
	raw, _ := json.Marshal(&d)
	dm := &msg{subject: "alrt.v1.delivery." + cellA + "." + alertA, data: raw}
	if !r.Take(ctx, []bus.Msg{dm}) || st.deliveries[alertA+"|client-a"] != t0 {
		t.Fatalf("delivery %v", st.deliveries)
	}
}

type pub struct {
	mu   sync.Mutex
	msgs []Message
	err  error
}

func (p *pub) Publish(_ context.Context, _ string, m bus.Enveloped) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	p.msgs = append(p.msgs, *m.(*Message))
	return nil
}

// Ack: the operator's own alert is acknowledged and republished with
// acked_at; another operator's is 404 and nothing is published.
func TestAckRecordsThenRepublishes(t *testing.T) {
	st := newMem()
	r := &Recorder{Store: st}
	r.Take(context.Background(), []bus.Msg{alertMsg(t, body(StateRaised, t0))})
	p := &pub{}
	s := &Service{Store: st, Bus: p}
	res, err := s.Ack(context.Background(), alertA, "client-a")
	if err != nil || res.AckedBy != "client-a" || !res.AckedAt.Equal(t0.Add(10*time.Second)) {
		t.Fatalf("%+v %v", res, err)
	}
	if len(p.msgs) != 1 || p.msgs[0].Body.AckedAt == nil {
		t.Fatalf("republished %+v", p.msgs)
	}
	for _, c := range []struct{ id, client string }{{alertA, "client-b"}, {"not-a-uuid", "client-a"}} {
		if _, err := s.Ack(context.Background(), c.id, c.client); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%v: %v", c, err)
		}
	}
	if len(p.msgs) != 1 || s.Counters.Get(CounterAckNotFound) != 2 {
		t.Fatal("a refused ack published or was not counted")
	}
	// A republish that fails leaves the acknowledgement recorded.
	p.err = errors.New("nats down")
	if _, err := s.Ack(context.Background(), alertA, "client-a"); err != nil || s.Counters.Get(CounterRepublishFailed) != 1 {
		t.Fatalf("%v %v", err, s.Counters.Snapshot())
	}
}

// Escalation: an unacknowledged alert is escalated and republished with
// escalated_at; an acknowledged one is not (the twin); a store failure
// is an error, nothing published.
func TestEscalation(t *testing.T) {
	st := newMem()
	r := &Recorder{Store: st}
	acked := body(StateRaised, t0)
	acked.AlertID = "6f0c2a8e-3b1d-4c6e-9a7f-1d2e3f4a5b6d"
	r.Take(context.Background(), []bus.Msg{alertMsg(t, body(StateRaised, t0)), alertMsg(t, acked)})
	p := &pub{}
	s := &Service{Store: st, Bus: p, Policy: policy.Defaults}
	if _, err := s.Ack(context.Background(), acked.AlertID, "client-a"); err != nil {
		t.Fatal(err)
	}
	n, err := s.Escalate(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	last := p.msgs[len(p.msgs)-1]
	if last.Body.AlertID != alertA || last.Body.EscalatedAt == nil {
		t.Fatalf("%+v", last.Body)
	}
	st.fail = errors.New("db down")
	if _, err := s.Escalate(context.Background()); err == nil || s.Counters.Get(CounterEscalateFailed) != 1 {
		t.Fatal("a failed pass not reported")
	}
}

// E-03: Decode and schemas/alert/v1 agree on every example, both ways.
func TestDecoderAgreesWithTheSchemaExamples(t *testing.T) {
	root := filepath.Join("..", "..", "schemas")
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "schema.json" {
			return err
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, filepath.Dir(p))
		return c.AddResource("https://schemas.uspace.ge/"+filepath.ToSlash(rel)+".json", doc)
	})
	if err != nil {
		t.Fatal(err)
	}
	sch, err := c.Compile("https://schemas.uspace.ge/alert/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "alert", "v1", "examples")
	n := 0
	for _, want := range []bool{true, false} {
		d := dir
		if !want {
			d = filepath.Join(dir, "invalid")
		}
		es, _ := os.ReadDir(d)
		for _, e := range es {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			raw, _ := os.ReadFile(filepath.Join(d, e.Name()))
			inst, _ := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
			_, derr := Decode(raw)
			ok := sch.Validate(inst) == nil
			// The decoder checks the envelope and the common rules; the
			// per-kind detail is the schema's (a proximity detail without
			// a peer is refused by the schema, read by the decoder).
			if ok != want {
				t.Errorf("%s: schema %v, want %v", e.Name(), ok, want)
			}
			if want && derr != nil {
				t.Errorf("%s: decoder refused a valid example: %v", e.Name(), derr)
			}
			n++
		}
	}
	if n == 0 {
		t.Fatal("no example")
	}
}

func FuzzDecode(f *testing.F) {
	for _, name := range []string{"proximity-raised-head-on", "proximity-cleared-resolved", "nonconformance-raised"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "schemas", "alert", "v1", "examples", name+".json"))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(raw)
	}
	f.Add([]byte(`{"schema":"alert/delivery/v1","body":{"alert_id":"x"}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		if m, err := Decode(data); err == nil && (m.Body.AlertID == "" || (m.Body.State == StateCleared) != (m.Body.ClearReason != nil)) {
			t.Fatalf("accepted %+v", m.Body)
		}
		_, _ = DecodeDelivery(data)
		_ = SchemaOf(data)
	})
}

// src is a Source of fixed fetches.
type src struct {
	mu    sync.Mutex
	batch [][]bus.Msg
	err   error
}

func (s *src) Fetch(_ context.Context, _ int, _ time.Duration) ([]bus.Msg, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	if len(s.batch) == 0 {
		return nil, nil
	}
	b := s.batch[0]
	s.batch = s.batch[1:]
	return b, nil
}

// Run fetches and records until stopped; a failing source waits and
// tries again.
func TestRecorderRunAndEscalationLoop(t *testing.T) {
	st := newMem()
	m := alertMsg(t, body(StateRaised, t0))
	s := &src{batch: [][]bus.Msg{{m}}}
	r := &Recorder{Source: s, Store: st, Wait: 10 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for !m.isAcked() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	s.mu.Lock()
	s.err = errors.New("nats down")
	s.mu.Unlock()
	p := &pub{}
	svc := &Service{Store: st, Bus: p}
	esc := make(chan struct{})
	go func() { svc.RunEscalation(ctx, 10*time.Millisecond); close(esc) }()
	for svc.counters().Get(CounterEscalated) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	<-esc
	if !m.isAcked() || svc.counters().Get(CounterEscalated) != 1 {
		t.Fatalf("acked %v escalated %d", m.acked, svc.counters().Get(CounterEscalated))
	}
	// An escalated alert without a cell is counted, not published.
	st2 := newMem()
	r2 := &Recorder{Store: st2}
	nc := alertMsg(t, body(StateRaised, t0))
	nc.subject = "not a subject"
	r2.Take(context.Background(), []bus.Msg{nc})
	svc2 := &Service{Store: st2, Bus: &pub{}}
	if n, err := svc2.Escalate(context.Background()); err != nil || n != 1 || svc2.counters().Get(CounterRepublishNoCell) != 1 {
		t.Fatalf("%d %v %v", n, err, svc2.counters().Snapshot())
	}
}

// Decode refuses each malformed field by name; never panics.
func TestDecodeRefusals(t *testing.T) {
	good := body(StateRaised, t0)
	for name, mut := range map[string]func(*Body){
		"alert id":  func(b *Body) { b.AlertID = "x" },
		"kind":      func(b *Body) { b.Kind = "advice" },
		"severity":  func(b *Body) { b.Severity = "high" },
		"state":     func(b *Body) { b.State = "open" },
		"reason":    func(b *Body) { r := "because"; b.State, b.ClearReason = StateCleared, &r },
		"no reason": func(b *Body) { b.State = StateCleared },
		"flight":    func(b *Body) { b.FlightID = "f" },
		"intent":    func(b *Body) { i := "i"; b.IntentID = &i },
		"number":    func(b *Body) { n := strings.Repeat("x", 65); b.AuthorisationNumber = &n },
		"time":      func(b *Body) { b.RaisedAt = time.Time{} },
		"policy":    func(b *Body) { b.PolicyVersion = -1 },
		"detail":    func(b *Body) { b.Detail = json.RawMessage(`[]`) },
		"clearing":  func(b *Body) { b.ClearingDetail = json.RawMessage(`3`) },
	} {
		b := good
		mut(&b)
		if _, err := Decode(alertMsg(t, b).data); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	for _, raw := range []string{`{`, `{"schema":"other/v1"}`, `{"schema":"alert/v1","msg_id":"x","body":{}}`, strings.Repeat(" ", MaxMessageBytes+1)} {
		if _, err := Decode([]byte(raw)); err == nil {
			t.Errorf("%.40q accepted", raw)
		}
		if _, err := DecodeDelivery([]byte(raw)); err == nil {
			t.Errorf("delivery %.40q accepted", raw)
		}
	}
	d := Delivery{Envelope: bus.SystemEnvelope(SchemaDelivery, "ussp/traffic-ws", t0), Body: DeliveryBody{AlertID: alertA, ClientID: "c", SentAt: t0}}
	for name, mut := range map[string]func(*Delivery){
		"alert":  func(d *Delivery) { d.Body.AlertID = "x" },
		"client": func(d *Delivery) { d.Body.ClientID = "" },
		"sent":   func(d *Delivery) { d.Body.SentAt = time.Time{} },
		"env":    func(d *Delivery) { d.MsgID = "x" },
	} {
		c := d
		mut(&c)
		raw, _ := json.Marshal(&c)
		if _, err := DecodeDelivery(raw); err == nil {
			t.Errorf("delivery %s accepted", name)
		}
	}
	var b Body
	if _, ok := b.Peer(); ok {
		t.Fatal("a peer from nothing")
	}
	if SchemaOf([]byte(`{`)) != "" {
		t.Fatal("schema of garbage")
	}
}

// A restriction_activated alert of an intent without a flight (WP-12)
// reads, writes flight_id null and round-trips; any other kind without
// a flight, or one with neither flight nor intent, is refused (E-01
// pair).
func TestFlightlessRestrictionAlert(t *testing.T) {
	b := body(StateRaised, t0)
	intentID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	b.Kind, b.FlightID, b.IntentID = KindRestrictionActivated, "", &intentID
	b.Detail = json.RawMessage(`{"cause":"restriction","ref":"TRS001"}`)
	m := alertMsg(t, b)
	if !strings.Contains(string(m.data), `"flight_id":null`) {
		t.Fatalf("flight_id not null: %s", m.data)
	}
	got, err := Decode(m.data)
	if err != nil || got.Body.FlightID != "" || got.Body.IntentID == nil || *got.Body.IntentID != intentID {
		t.Fatalf("decode %+v %v", got.Body, err)
	}
	b.IntentID = nil
	if _, err := Decode(alertMsg(t, b).data); err == nil {
		t.Fatal("a flightless alert without an intent accepted")
	}
	p := body(StateRaised, t0)
	p.FlightID, p.IntentID = "", &intentID
	if _, err := Decode(alertMsg(t, p).data); err == nil {
		t.Fatal("a flightless proximity alert accepted")
	}
	// A flight's alert still writes its flight id.
	if !strings.Contains(string(alertMsg(t, body(StateRaised, t0)).data), `"flight_id":"`+flightA+`"`) {
		t.Fatal("flight id lost")
	}
}

// A notice whose intent is over is cleared flight_ended on the bus; an
// open notice of an intent still flying is left alone (E-01 pair); a
// store that fails is an error.
func TestClearEndedNotices(t *testing.T) {
	st := newMem()
	intentID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	b := body(StateRaised, t0)
	b.AlertID, b.Kind, b.FlightID, b.IntentID = "4d6f0f7e-8d7c-4c1a-9e2b-3a4b5c6d7e82", KindRestrictionActivated, "", &intentID
	st.rows[b.AlertID] = Record{Body: b, Cell5: cellA}
	st.rows[alertA] = Record{Body: body(StateRaised, t0), Cell5: cellA}
	bus := &pub{}
	svc := &Service{Store: st, Bus: bus, Now: func() time.Time { return t0.Add(time.Hour) }}
	n, err := svc.ClearEndedNotices(t.Context())
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	bus.mu.Lock()
	ms := bus.msgs
	bus.mu.Unlock()
	if len(ms) != 1 {
		t.Fatalf("published %d", len(ms))
	}
	raw, err := json.Marshal(&ms[0])
	if err != nil {
		t.Fatal(err)
	}
	m, err := Decode(raw)
	if err != nil || m.Body.State != StateCleared || m.Body.ClearReason == nil || *m.Body.ClearReason != "flight_ended" || m.Body.AlertID != b.AlertID {
		t.Fatalf("%+v %v", m.Body, err)
	}
	st.fail = errors.New("down")
	if _, err := svc.ClearEndedNotices(t.Context()); err == nil {
		t.Fatal("a failed store not reported")
	}
}

// An open notice is republished from the record, unchanged (its own
// updated_at), so a traffic-ws that restarted holds it again; a cleared
// one and a monitor alert are not (E-01 pair). Past MaxNoticeRepublish
// the rest wait and are counted (E-10); a store that fails is an error.
func TestRepublishOpenNotices(t *testing.T) {
	st := newMem()
	intentID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	b := body(StateRaised, t0)
	b.AlertID, b.Kind, b.FlightID, b.IntentID = "4d6f0f7e-8d7c-4c1a-9e2b-3a4b5c6d7e82", KindRestrictionActivated, "", &intentID
	st.rows[b.AlertID] = Record{Body: b, Cell5: cellA}
	gone := b
	gone.AlertID, gone.State = "4d6f0f7e-8d7c-4c1a-9e2b-3a4b5c6d7e83", StateCleared
	st.rows[gone.AlertID] = Record{Body: gone, Cell5: cellA}
	st.rows[alertA] = Record{Body: body(StateRaised, t0), Cell5: cellA}
	bus := &pub{}
	svc := &Service{Store: st, Bus: bus, Now: func() time.Time { return t0.Add(time.Hour) }}
	n, err := svc.RepublishOpenNotices(t.Context())
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	bus.mu.Lock()
	ms := slices.Clone(bus.msgs)
	bus.mu.Unlock()
	if len(ms) != 1 {
		t.Fatalf("published %d", len(ms))
	}
	raw, err := json.Marshal(&ms[0])
	if err != nil {
		t.Fatal(err)
	}
	m, err := Decode(raw)
	if err != nil || m.Body.AlertID != b.AlertID || m.Body.State != StateRaised || !m.Body.UpdatedAt.Equal(t0) {
		t.Fatalf("%+v %v", m.Body, err)
	}
	for i := range MaxNoticeRepublish + 1 {
		x := b
		x.AlertID = fmt.Sprintf("5d6f0f7e-8d7c-4c1a-9e2b-%012d", i)
		st.rows[x.AlertID] = Record{Body: x, Cell5: cellA}
	}
	if n, err := svc.RepublishOpenNotices(t.Context()); err != nil || n != MaxNoticeRepublish || svc.counters().Get(CounterNoticeRepublishOverBound) != 1 {
		t.Fatalf("over bound: %d %v %d", n, err, svc.counters().Get(CounterNoticeRepublishOverBound))
	}
	st.fail = errors.New("down")
	if _, err := svc.RepublishOpenNotices(t.Context()); err == nil {
		t.Fatal("a failed store not reported")
	}
}
