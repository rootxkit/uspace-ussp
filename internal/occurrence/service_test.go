package occurrence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
)

var t0 = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

const (
	flightA = "0b5d4c3a-2e1f-4a0b-9c8d-7e6f5a4b3c2d"
	flightB = "1b5d4c3a-2e1f-4a0b-9c8d-7e6f5a4b3c2d"
	intentA = "6f1c0d2e-3b4a-4c5d-8e6f-7a8b9c0d1e2f"
)

func ptr[T any](v T) *T { return &v }

type memReport struct {
	Report
	id        string
	state     string
	deadline  time.Time
	attempts  int
	nextAt    time.Time
	lastError string
	authRef   string
}

// memStore is Store in memory; AlertEvents returns what the test put
// (the selection rules are the SQL's, run in test/integration).
type memStore struct {
	mu      sync.Mutex
	now     time.Time
	events  []AlertEvent
	alerts  map[string]AlertEvent
	flights map[string]FlightRef
	emerg   []FlightRef
	reports []*memReport
	errOf   map[string]error
	// heldPages are the page sizes Held was asked for.
	heldPages []int
}

func newMem() *memStore {
	return &memStore{now: t0, alerts: map[string]AlertEvent{}, flights: map[string]FlightRef{}, errOf: map[string]error{}}
}

func (m *memStore) has(kind, ref string) bool {
	return slices.ContainsFunc(m.reports, func(r *memReport) bool { return r.SourceKind == kind && r.SourceRef == ref })
}

func (m *memStore) AlertEvents(context.Context, time.Duration, float64, float64, int) ([]AlertEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.errOf["events"]; err != nil {
		return nil, err
	}
	var out []AlertEvent
	for i := range m.events {
		if !m.has("alert", m.events[i].SourceRef) {
			out = append(out, m.events[i])
		}
	}
	return out, nil
}

func (m *memStore) EmergencyFlights(context.Context, time.Duration, int) ([]FlightRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.errOf["emergency"]; err != nil {
		return nil, err
	}
	var out []FlightRef
	for _, f := range m.emerg {
		if !m.has("flight", f.FlightID) {
			out = append(out, f)
		}
	}
	return out, nil
}

func (m *memStore) Alert(_ context.Context, id string) (AlertEvent, error) {
	if err := m.errOf["alert"]; err != nil {
		return AlertEvent{}, err
	}
	e, ok := m.alerts[id]
	if !ok {
		return AlertEvent{}, ErrNotFound
	}
	return e, nil
}

func (m *memStore) Flights(_ context.Context, ids []string) ([]FlightRef, error) {
	var out []FlightRef
	for _, id := range ids {
		if f, ok := m.flights[id]; ok {
			out = append(out, f)
		}
	}
	return out, nil
}

func (m *memStore) Now(context.Context) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.errOf["now"]; err != nil {
		return time.Time{}, err
	}
	return m.now, nil
}

func (m *memStore) item(r *memReport) Item {
	ttd := r.deadline.Sub(m.now).Seconds()
	it := Item{ReportRef: r.Ref, Kind: r.Kind, State: r.state, Channel: r.Channel, FlaggedBy: r.FlaggedBy, BecameAwareAt: r.AwareAt,
		DeadlineAt: r.deadline, TimeToDeadlineS: ttd, Critical: r.state != "delivered" && ttd < 0, Attempts: r.attempts, FlightIDs: r.FlightIDs}
	if r.lastError != "" {
		it.LastError = ptr(r.lastError)
	}
	return it
}

func (m *memStore) Enqueue(_ context.Context, r Report) (Item, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.errOf["enqueue"]; err != nil {
		return Item{}, false, err
	}
	for _, x := range m.reports {
		if x.SourceKind == r.SourceKind && x.SourceRef == r.SourceRef {
			return m.item(x), false, nil
		}
	}
	x := &memReport{Report: r, id: fmt.Sprint(len(m.reports) + 1), state: "pending", deadline: r.AwareAt.Add(Deadline), nextAt: m.now}
	m.reports = append(m.reports, x)
	return m.item(x), true, nil
}

func (m *memStore) Claim(_ context.Context, n int, lease time.Duration) ([]Queued, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.errOf["claim"]; err != nil {
		return nil, err
	}
	var out []Queued
	for _, r := range m.reports {
		if r.state == "pending" && !r.nextAt.After(m.now) && len(out) < n {
			r.attempts++
			r.nextAt = m.now.Add(lease)
			out = append(out, Queued{ID: r.id, Ref: r.Ref, Body: r.Body, Attempts: r.attempts})
		}
	}
	return out, nil
}

func (m *memStore) get(id string) *memReport {
	for _, r := range m.reports {
		if r.id == id {
			return r
		}
	}
	return nil
}

func (m *memStore) Delivered(_ context.Context, id, ref string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.errOf["delivered"]; err != nil {
		return err
	}
	if r := m.get(id); r != nil && r.state == "pending" {
		r.state, r.authRef, r.lastError = "delivered", ref, ""
	}
	return nil
}

func (m *memStore) Retry(_ context.Context, id, cause string, backoff time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.get(id); r != nil && r.state == "pending" {
		r.nextAt, r.lastError = m.now.Add(backoff), cause
	}
	return m.errOf["retry"]
}

func (m *memStore) Fail(_ context.Context, id, cause string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.get(id); r != nil && r.state == "pending" {
		r.state, r.lastError = "failed", cause
	}
	return nil
}

func (m *memStore) Open(context.Context, int) ([]Item, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Item
	for _, r := range m.reports {
		if r.state != "delivered" {
			out = append(out, m.item(r))
		}
	}
	return out, false, nil
}

func (m *memStore) Summarise(context.Context) (Summary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.errOf["summary"]; err != nil {
		return Summary{}, err
	}
	var s Summary
	first := true
	for _, r := range m.reports {
		if r.state == "delivered" {
			continue
		}
		if r.state == "pending" {
			s.Pending++
		} else {
			s.Failed++
		}
		ttd := r.deadline.Sub(m.now).Seconds()
		if ttd < 0 {
			s.Critical++
		}
		if first || ttd < s.NearestDeadlineS {
			s.NearestDeadlineS, first = ttd, false
		}
	}
	return s, nil
}

func (m *memStore) Held(_ context.Context, after string, n int) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.heldPages = append(m.heldPages, n)
	if err := m.errOf["held"]; err != nil {
		return nil, err
	}
	var out []string
	for _, r := range m.reports {
		for _, id := range r.FlightIDs {
			if id > after {
				out = append(out, id)
			}
		}
	}
	slices.Sort(out)
	out = slices.Compact(out)
	return out[:min(n, len(out))], nil
}

func (m *memStore) report(kind, ref string) *memReport {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.reports {
		if r.SourceKind == kind && r.SourceRef == ref {
			c := *r
			return &c
		}
	}
	return nil
}

// fakeAuthority is a Deliverer: down answers a retryable error,
// refuse a permanent one.
type fakeAuthority struct {
	mu     sync.Mutex
	down   bool
	refuse bool
	got    [][]byte
	refs   int
}

func (f *fakeAuthority) Submit(_ context.Context, body []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case f.refuse:
		return "", &PermanentError{Status: 409, Detail: "409 report_ref_conflict"}
	case f.down:
		return "", errors.New("503 from the authority")
	}
	f.got = append(f.got, body)
	f.refs++
	return fmt.Sprintf("OCC-%d", f.refs), nil
}

type memHolds struct {
	mu   sync.Mutex
	held map[string][]string
	fail bool
}

func (h *memHolds) Hold(_ context.Context, id string, reasons []string, _ time.Time) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.fail {
		return errors.New("KV down")
	}
	if h.held == nil {
		h.held = map[string][]string{}
	}
	h.held[id] = reasons
	return nil
}

func flight(id string) FlightRef {
	return FlightRef{FlightID: id, Serial: "TEST" + id[:4], OperatorReg: ptr("GEO87astrdge12k8-xyz"), AuthorisationNumber: ptr("GE-USSP-DEV-1"),
		IntentID: ptr(intentA), InUSpace: true, StartedAt: t0.Add(-time.Hour)}
}

func proximity(id, peer string, h, v any) AlertEvent {
	d, _ := json.Marshal(map[string]any{"d_cpa_h_m": h, "d_alt_m": v, "pair_id": "pair-1", "peer": map[string]any{"track_id": peer, "trust": "authenticated"}})
	return AlertEvent{AlertID: id, Kind: "proximity", RaisedAt: t0.Add(-10 * time.Minute), Detail: d, SourceRef: "pair:pair-1@1", Flight: flight(flightA)}
}

func newService(st *memStore, d Deliverer) (*Service, *memHolds) {
	h := &memHolds{}
	return &Service{Store: st, Deliverer: d, Holds: h, SystemID: "USSP-DEV", RecordsURL: "https://ussp.test/", Counters: &core.Counters{}}, h
}

// The done-when airprox: the proximity alert becomes a report queued
// with became_aware_at now and deadline_at 72 h later, naming both own
// flights (their public registration part), the separation and the
// evidence links; its flights are held; a second sweep queues nothing.
func TestAirproxQueuedOnceWithItsDeadline(t *testing.T) {
	st := newMem()
	st.flights[flightB] = flight(flightB)
	st.events = []AlertEvent{proximity("a1", "trk:"+flightB, 41.5, 9.0)}
	s, holds := newService(st, nil)
	for range 2 {
		if err := s.Detect(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(st.reports) != 1 {
		t.Fatalf("%d reports", len(st.reports))
	}
	r := st.reports[0]
	if r.Kind != KindAirprox || r.Channel != ChannelMandatory || !r.deadline.Equal(t0.Add(72*time.Hour)) || !r.AwareAt.Equal(t0) ||
		!slices.Equal(r.FlightIDs, []string{flightA, flightB}) {
		t.Fatalf("report %+v", r.Report)
	}
	var p Payload
	if err := json.Unmarshal(r.Body, &p); err != nil {
		t.Fatal(err)
	}
	if p.Schema != Schema || p.ReportRef != "USSP-DEV:occurrence:alert:pair:pair-1@1" || len(p.Aircraft) != 2 || *p.Aircraft[0].OperatorReg != "GEO87astrdge12k8" ||
		p.MinSeparation == nil || *p.MinSeparation.HM != 41.5 || *p.MinSeparation.VM != 9 || p.Reporter.PersonRef != ReporterSystem ||
		len(p.EvidenceURLs) != 2 || p.EvidenceURLs[0] != "https://ussp.test/v1/records/flights/"+flightA || strings.Contains(string(r.Body), "xyz") {
		t.Fatalf("payload %s", r.Body)
	}
	if len(holds.held) != 2 || s.Counters.Get(CounterQueued) != 1 {
		t.Fatalf("holds %v counters %v", holds.held, s.Counters.Snapshot())
	}
}

// A manned peer is named by its ICAO address; an unjudged vertical is
// said in the narrative, never taken as separation.
func TestAirproxWithMannedAndUnknownVertical(t *testing.T) {
	st := newMem()
	st.events = []AlertEvent{proximity("a2", "man:4ca123", 30.0, nil)}
	s, _ := newService(st, nil)
	if err := s.Detect(context.Background()); err != nil {
		t.Fatal(err)
	}
	var p Payload
	_ = json.Unmarshal(st.reports[0].Body, &p)
	if len(p.Manned) != 1 || p.Manned[0].ICAO24 != "4ca123" || p.MinSeparation.VM != nil || !strings.Contains(p.Narrative, "vertical separation not judged") {
		t.Fatalf("%s", st.reports[0].Body)
	}
}

// The other kinds: a PROHIBITED zone incursion, a lost link, an
// emergency.
func TestOtherKinds(t *testing.T) {
	st := newMem()
	zone, _ := json.Marshal(map[string]any{"zone_type": "PROHIBITED", "zone_id": "Z1"})
	st.events = []AlertEvent{
		{AlertID: "z1", Kind: "zone_incursion", RaisedAt: t0.Add(-time.Minute), Detail: zone, SourceRef: "z1", Flight: flight(flightA)},
		{AlertID: "l1", Kind: "lost_link", RaisedAt: t0.Add(-time.Minute), Detail: []byte(`{}`), SourceRef: "l1", Flight: flight(flightA)},
	}
	f := flight(flightB)
	f.Emergency = true
	st.emerg = []FlightRef{f}
	s, _ := newService(st, nil)
	if err := s.Detect(context.Background()); err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, r := range st.reports {
		kinds = append(kinds, r.Kind)
	}
	if !slices.Equal(kinds, []string{KindNonconformanceInProhib, KindLostLinkInUSpace, KindEmergency}) {
		t.Fatalf("kinds %v", kinds)
	}
	if !strings.Contains(string(st.reports[0].Body), "Zone Z1 (PROHIBITED)") {
		t.Errorf("%s", st.reports[0].Body)
	}
}

// Delivered to the authority; the authority down: tried again with
// backoff and counted, delivered once it is back (E-02); a refusal fails
// the report for good; past MaxAttempts it gives up (E-10).
func TestDelivery(t *testing.T) {
	st := newMem()
	st.events = []AlertEvent{proximity("a1", "", 10.0, 2.0)}
	auth := &fakeAuthority{down: true}
	s, _ := newService(st, auth)
	if err := s.Detect(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		if s.DeliverDue(context.Background()) != 0 {
			t.Fatal("delivered while down")
		}
		r := st.report("alert", "pair:pair-1@1")
		if r.attempts != i || r.state != "pending" || !strings.Contains(r.lastError, "503") {
			t.Fatalf("try %d: %+v", i, r)
		}
		st.now = st.now.Add(Backoff(i))
	}
	auth.down = false
	if s.DeliverDue(context.Background()) != 1 || st.report("alert", "pair:pair-1@1").authRef != "OCC-1" || len(auth.got) != 1 {
		t.Fatal("not delivered once back")
	}
	if s.Counters.Get(CounterRetried) != 3 || s.Counters.Get(CounterDelivered) != 1 {
		t.Errorf("counters %v", s.Counters.Snapshot())
	}

	st2 := newMem()
	st2.events = []AlertEvent{proximity("a1", "", 10.0, 2.0)}
	s2, _ := newService(st2, &fakeAuthority{refuse: true})
	_ = s2.Detect(context.Background())
	s2.DeliverDue(context.Background())
	if r := st2.report("alert", "pair:pair-1@1"); r.state != "failed" || r.attempts != 1 || !strings.Contains(r.lastError, "409") {
		t.Fatalf("refusal %+v", r)
	}
	if s2.Counters.Get(CounterConflict) != 1 || s2.Counters.Get(CounterFailed) != 1 {
		t.Errorf("a 409 is not counted as a conflict: %v", s2.Counters.Snapshot())
	}

	st3 := newMem()
	st3.events = []AlertEvent{proximity("a1", "", 10.0, 2.0)}
	s3, _ := newService(st3, &fakeAuthority{down: true})
	s3.MaxAttempts = 2
	_ = s3.Detect(context.Background())
	for range 4 {
		s3.DeliverDue(context.Background())
		st3.now = st3.now.Add(time.Hour)
	}
	if r := st3.report("alert", "pair:pair-1@1"); r.state != "failed" || !strings.Contains(r.lastError, "gave up after 2 tries") {
		t.Fatalf("bound %+v", r)
	}
}

// The deadline (E-01 pair): a report undelivered past 72 h is a critical
// item and /readyz is down; one delivered in time is not flagged.
func TestPastDeadlineIsCritical(t *testing.T) {
	st := newMem()
	st.events = []AlertEvent{proximity("a1", "", 10.0, 2.0)}
	s, _ := newService(st, nil)
	_ = s.Detect(context.Background())
	if state, detail := Probe(st, false)(context.Background()); state != obs.StateDegraded || !strings.Contains(detail, "no authority configured") {
		t.Fatalf("pending: %s %s", state, detail)
	}
	st.now = t0.Add(Deadline + time.Second)
	items, _, _ := st.Open(context.Background(), 10)
	if len(items) != 1 || !items[0].Critical || items[0].TimeToDeadlineS >= 0 {
		t.Fatalf("items %+v", items)
	}
	if state, _ := Probe(st, false)(context.Background()); state != obs.StateDown {
		t.Fatalf("past the deadline: %s", state)
	}

	ok := newMem()
	ok.events = []AlertEvent{proximity("a1", "", 10.0, 2.0)}
	s2, _ := newService(ok, &fakeAuthority{})
	_ = s2.Detect(context.Background())
	s2.DeliverDue(context.Background())
	ok.now = t0.Add(Deadline + time.Hour)
	items, _, _ = ok.Open(context.Background(), 10)
	if len(items) != 0 {
		t.Fatalf("a delivered report is listed: %+v", items)
	}
	if state, _ := Probe(ok, true)(context.Background()); state != obs.StateUp {
		t.Fatalf("delivered in time: %s", state)
	}
	ok.errOf["summary"] = errors.New("db")
	if state, _ := Probe(ok, true)(context.Background()); state != obs.StateUnknown {
		t.Fatalf("unreadable: %s", state)
	}
}

// Without a deliverer nothing is claimed.
func TestNoDeliverer(t *testing.T) {
	st := newMem()
	st.events = []AlertEvent{proximity("a1", "", 10.0, 2.0)}
	s, _ := newService(st, nil)
	_ = s.Detect(context.Background())
	if s.DeliverDue(context.Background()) != 0 || st.reports[0].attempts != 0 {
		t.Fatal("claimed without a deliverer")
	}
	if !strings.Contains(NotDelivering(), "USSP_AUTHORITY_BASE_URL") {
		t.Error(NotDelivering())
	}
	if state, detail := Probe(newMem(), false)(context.Background()); state != obs.StateUp || !strings.Contains(detail, "no authority configured") {
		t.Errorf("no report, no deliverer: %s %s", state, detail)
	}
}

// A supervisor's flag: queued under the supervisor's reference, then the
// same event answers its report; an unknown alert or kind is refused.
func TestFlag(t *testing.T) {
	st := newMem()
	st.alerts["a1"] = proximity("a1", "", 100.0, 50.0)
	s, _ := newService(st, nil)
	it, created, err := s.Flag(context.Background(), "staff-7", "a1", "", "seen on the console")
	if err != nil || !created || it.Kind != KindAirprox || it.FlaggedBy != "supervisor" {
		t.Fatalf("%+v %v %v", it, created, err)
	}
	var p Payload
	_ = json.Unmarshal(st.reports[0].Body, &p)
	if p.Reporter.PersonRef != "staff-7" || !strings.HasPrefix(p.Narrative, "seen on the console (") {
		t.Fatalf("%s", st.reports[0].Body)
	}
	if _, created, _ := s.Flag(context.Background(), "staff-8", "a1", KindOther, ""); created {
		t.Fatal("reported twice")
	}
	var fe *FlagError
	if _, _, err := s.Flag(context.Background(), "staff-7", "nope", "", ""); !errors.As(err, &fe) || !fe.NotFound {
		t.Fatalf("unknown alert: %v", err)
	}
	if _, _, err := s.Flag(context.Background(), "staff-7", "a1", "violation", ""); !errors.As(err, &fe) || fe.NotFound {
		t.Fatalf("unknown kind: %v", err)
	}
	st.alerts["a3"] = AlertEvent{AlertID: "a3", Kind: "nonconformance", RaisedAt: t0, Detail: []byte(`{}`), SourceRef: "a3", Flight: flight(flightA)}
	it, _, err = s.Flag(context.Background(), "staff-7", "a3", KindOther, "")
	if err != nil || it.Channel != ChannelVoluntary {
		t.Fatalf("other is voluntary: %+v %v", it, err)
	}
	st.errOf["alert"] = errors.New("db")
	if _, _, err := s.Flag(context.Background(), "staff-7", "a1", "", ""); err == nil || errors.As(err, &fe) {
		t.Fatalf("store failure: %v", err)
	}
}

// The holds: a failing projection is counted and written again by
// ProjectHolds once the KV takes it.
func TestHolds(t *testing.T) {
	st := newMem()
	st.events = []AlertEvent{proximity("a1", "", 10.0, 2.0)}
	s, holds := newService(st, nil)
	holds.fail = true
	_ = s.Detect(context.Background())
	if len(holds.held) != 0 || s.Counters.Get(CounterHoldFailed) != 1 {
		t.Fatalf("%v %v", holds.held, s.Counters.Snapshot())
	}
	holds.fail = false
	if err := s.ProjectHolds(context.Background()); err != nil || len(holds.held) != 1 {
		t.Fatalf("%v %v", err, holds.held)
	}
	st.errOf["held"] = errors.New("db")
	if err := s.ProjectHolds(context.Background()); err == nil {
		t.Fatal("unreadable holds")
	}
}

// The projection reads the held flights in pages of HoldsBatch and
// writes every one, past the bound of one page (E-10).
func TestProjectHoldsPagesPastTheBatch(t *testing.T) {
	st := newMem()
	total := 2*HoldsBatch + 1
	for i := range total {
		id := fmt.Sprintf("%08x-0000-4000-8000-000000000000", i)
		st.reports = append(st.reports, &memReport{Report: Report{SourceKind: "flight", SourceRef: id, FlightIDs: []string{id}}, id: id, state: "pending"})
	}
	s, holds := newService(st, nil)
	if err := s.ProjectHolds(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(holds.held) != total {
		t.Fatalf("%d of %d flights held", len(holds.held), total)
	}
	if want := []int{HoldsBatch, HoldsBatch, HoldsBatch}; !slices.Equal(st.heldPages, want) {
		t.Fatalf("pages %v, want %v", st.heldPages, want)
	}
}

// gatedHolds blocks every projection write (reason "occurrence") until
// release is closed, and takes a report's own hold at once.
type gatedHolds struct {
	memHolds
	blocked chan struct{}
	release chan struct{}
	once    sync.Once
}

func (h *gatedHolds) Hold(ctx context.Context, id string, reasons []string, since time.Time) error {
	if slices.Equal(reasons, []string{"occurrence"}) {
		h.once.Do(func() { close(h.blocked) })
		<-h.release
	}
	return h.memHolds.Hold(ctx, id, reasons, since)
}

// A supervisor's flag is not held up by a projection of every held
// flight that is stuck on the KV: the flag's report is queued and its
// flight held while the projection still waits.
func TestFlagDoesNotWaitForTheProjection(t *testing.T) {
	st := newMem()
	st.events = []AlertEvent{proximity("a1", "", 10.0, 2.0)}
	h := &gatedHolds{blocked: make(chan struct{}), release: make(chan struct{})}
	s := &Service{Store: st, Holds: h, SystemID: "USSP-DEV", Counters: &core.Counters{}}
	if err := s.Detect(context.Background()); err != nil {
		t.Fatal(err)
	}
	projected := make(chan error, 1)
	go func() { projected <- s.ProjectHolds(context.Background()) }()
	<-h.blocked
	st.alerts["a2"] = AlertEvent{AlertID: "a2", Kind: "lost_link", RaisedAt: t0, Detail: []byte(`{}`), SourceRef: "a2", Flight: flight(flightB)}
	flagged := make(chan error, 1)
	go func() {
		_, _, err := s.Flag(context.Background(), "staff-7", "a2", "", "")
		flagged <- err
	}()
	select {
	case err := <-flagged:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		close(h.release)
		t.Fatal("the flag waited for the projection")
	}
	h.mu.Lock()
	got := h.held[flightB]
	h.mu.Unlock()
	if !slices.Equal(got, []string{KindLostLinkInUSpace}) {
		t.Fatalf("the flag's flight is not held: %v", got)
	}
	close(h.release)
	if err := <-projected; err != nil {
		t.Fatal(err)
	}
}

func TestBuildRefusals(t *testing.T) {
	ok := func() *Report {
		return &Report{Kind: KindAirprox, Channel: ChannelMandatory, SourceKind: "alert", SourceRef: "x", Reporter: "system", OccurredAt: t0, AwareAt: t0}
	}
	air := []Aircraft{{Serial: "TEST1", FlightID: flightA}}
	for name, c := range map[string]struct {
		mutate func(*Report)
		air    []Aircraft
		sep    *Separation
	}{
		"kind":     {mutate: func(r *Report) { r.Kind = "violation" }, air: air},
		"channel":  {mutate: func(r *Report) { r.Channel = "rumour" }, air: air},
		"aircraft": {air: nil},
		"aware":    {mutate: func(r *Report) { r.AwareAt = t0.Add(-time.Hour) }, air: air},
		"reporter": {mutate: func(r *Report) { r.Reporter = "" }, air: air},
		"sep":      {air: air, sep: &Separation{HM: ptr(-1.0)}},
	} {
		r := ok()
		if c.mutate != nil {
			c.mutate(r)
		}
		var be *BuildError
		if err := Build(r, "USSP-DEV", c.air, nil, c.sep, "", nil); !errors.As(err, &be) {
			t.Errorf("%s: %v", name, err)
		}
	}
	r := ok()
	many := make([]Aircraft, MaxAircraft+3)
	for i := range many {
		many[i] = Aircraft{Serial: "TEST", FlightID: flightA}
	}
	if err := Build(r, "USSP-DEV", many, nil, nil, strings.Repeat("n", MaxNarrative+10), nil); err != nil || len(r.Payload.Aircraft) != MaxAircraft ||
		len(r.Payload.Narrative) != MaxNarrative {
		t.Fatalf("bounds: %v %d %d", err, len(r.Payload.Aircraft), len(r.Payload.Narrative))
	}
}

// Store failures are the sweep's error; Run stops with its context.
func TestDetectFailuresAndRun(t *testing.T) {
	for _, what := range []string{"now", "events", "emergency", "enqueue"} {
		st := newMem()
		st.events = []AlertEvent{proximity("a1", "", 10.0, 2.0)}
		st.emerg = []FlightRef{flight(flightB)}
		st.errOf[what] = errors.New("db")
		s, _ := newService(st, nil)
		if err := s.Detect(context.Background()); err == nil {
			t.Errorf("%s: no error", what)
		}
	}
	st := newMem()
	st.events = []AlertEvent{proximity("a1", "", 10.0, 2.0)}
	auth := &fakeAuthority{}
	s, _ := newService(st, auth)
	s.Policy = policy.Defaults
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx, 5*time.Millisecond); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for s.Counters.Get(CounterDelivered) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if s.Counters.Get(CounterDelivered) != 1 {
		t.Fatal("Run did not deliver")
	}
}
