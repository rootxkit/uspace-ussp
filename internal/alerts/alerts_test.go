package alerts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
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
	acked   bool
	naked   bool
}

func (m *msg) Data() []byte               { return m.data }
func (m *msg) Subject() string            { return m.subject }
func (m *msg) Ack() error                 { m.acked = true; return nil }
func (m *msg) Nak() error                 { m.naked = true; return nil }
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
	// A flight not recorded yet: naked and kept, then given up.
	other := body(StateRaised, t0)
	other.AlertID, other.FlightID = "6f0c2a8e-3b1d-4c6e-9a7f-1d2e3f4a5b6d", "33333333-3333-4333-8333-333333333333"
	m := alertMsg(t, other)
	r.MaxAttempts = 2
	if r.Take(ctx, []bus.Msg{m}) || !m.naked {
		t.Fatal("not left in the stream")
	}
	if !r.Take(ctx, []bus.Msg{m}) || !m.acked || r.Counters.Get(CounterGaveUp) != 1 {
		t.Fatal("not given up at the bound")
	}
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
