package weather

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/geodesy"

	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	fake "github.com/rootxkit/uspace-ussp/internal/testfakes/weather"
)

// memStore is Store in memory, its clock set by the test. An area meets
// a box when the box of its circle does.
type memStore struct {
	mu       sync.Mutex
	now      time.Time
	products []Product
	status   map[string]SourceStatus
	fail     error
	n        int
}

func newMem(now time.Time) *memStore { return &memStore{now: now, status: map[string]SourceStatus{}} }

func (m *memStore) Now(context.Context) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.now, m.fail
}

func (m *memStore) Save(_ context.Context, source string, ps []NewProduct, radiusM float64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return 0, m.fail
	}
	n := 0
	for i := range ps {
		p := &ps[i]
		if slices.ContainsFunc(m.products, func(x Product) bool {
			return x.Source == source && x.Station == p.Station && x.Kind == p.Kind && x.ObservedAt.Equal(p.ObservedAt)
		}) {
			continue
		}
		m.n++
		c := p.Content
		c.Area.RadiusM = radiusM
		m.products = append(m.products, Product{ID: fmt.Sprintf("00000000-0000-4000-8000-%012d", m.n), Station: p.Station, Kind: p.Kind,
			Source: source, ObservedAt: p.ObservedAt, ValidFrom: p.ValidFrom, ValidTo: p.ValidTo, FetchedAt: m.now, Content: c})
		n++
	}
	return n, nil
}

func (m *memStore) RecordFetch(_ context.Context, source, failure string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	st := m.status[source]
	now := m.now
	st.LastAttemptAt = &now
	if failure == "" {
		st.LastSuccessAt, st.LastError = &now, ""
	} else {
		st.LastFailureAt, st.LastError = &now, failure
	}
	m.status[source] = st
	return nil
}

func (m *memStore) Status(_ context.Context, source string) (SourceStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status[source], m.fail
}

func meets(a Area, b geodesy.BBox) bool {
	dLat := a.RadiusM / 111_320
	dLon := dLat / math.Cos(a.LatDeg*math.Pi/180)
	return a.LatDeg+dLat >= b.MinLat && a.LatDeg-dLat <= b.MaxLat && a.LonDeg+dLon >= b.MinLon && a.LonDeg-dLon <= b.MaxLon
}

func (m *memStore) Newest(_ context.Context, source string, box geodesy.BBox, at time.Time, limit int) ([]Product, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return nil, m.fail
	}
	best := map[string]Product{}
	for i := range m.products {
		p := m.products[i]
		if p.Source != source || p.ObservedAt.After(at) || !meets(p.Area, box) {
			continue
		}
		k := p.Station + string(p.Kind)
		if b, ok := best[k]; !ok || p.ObservedAt.After(b.ObservedAt) {
			best[k] = p
		}
	}
	var out []Product
	for k := range best {
		out = append(out, best[k])
	}
	return out[:min(len(out), limit)], nil
}

func (m *memStore) InForce(_ context.Context, source string, boxes []geodesy.BBox, from, to time.Time, limit int) ([]Product, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return nil, m.fail
	}
	var out []Product
	for i := range m.products {
		p := m.products[i]
		if p.Source != source || p.ValidFrom.After(to) || p.ValidTo.Before(from) ||
			!slices.ContainsFunc(boxes, func(b geodesy.BBox) bool { return meets(p.Area, b) }) {
			continue
		}
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b Product) int { return b.ObservedAt.Compare(a.ObservedAt) })
	return out[:min(len(out), limit)], nil
}

func (m *memStore) Prune(_ context.Context, before time.Time, limit int) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	m.products = slices.DeleteFunc(m.products, func(p Product) bool {
		if n < limit && p.ValidTo.Before(before) {
			n++
			return true
		}
		return false
	})
	return int64(n), nil
}

func (m *memStore) advance(d time.Duration) {
	m.mu.Lock()
	m.now = m.now.Add(d)
	m.mu.Unlock()
}

// rig is the service over the fake source and the memory store at
// 2026-10-04T13:05Z, a METAR of 13:00 and a TAF of 11:00 at Tbilisi.
type rig struct {
	f   *fake.Fake
	st  *memStore
	svc *Service
	pol *policy.Record
	c   *core.Counters
}

var (
	tbilisi = geodesy.BBox{MinLat: 41.6, MinLon: 44.9, MaxLat: 41.7, MaxLon: 45.0}
	faraway = geodesy.BBox{MinLat: 43.0, MinLon: 40.0, MaxLat: 43.1, MaxLon: 40.1}
	obsAt   = time.Date(2026, 10, 4, 13, 0, 0, 0, time.UTC)
	issued  = time.Date(2026, 10, 4, 11, 0, 0, 0, time.UTC)
)

func newRig(t *testing.T, metarBody string) *rig {
	t.Helper()
	r := &rig{f: fake.New(), st: newMem(obsAt.Add(5 * time.Minute)), c: &core.Counters{}}
	t.Cleanup(r.f.Close)
	r.f.Set([]fake.METAR{fake.NewMETAR("UGTB", obsAt, 41.669, 44.955, metarBody)},
		[]fake.TAF{fake.NewTAF("UGTB", issued, issued.Add(time.Hour), issued.Add(25*time.Hour), 41.669, 44.955, "32016KT CAVOK BECMG 0418/0420 32006KT")})
	r.pol = &policy.Record{Version: 3, Values: policy.Defaults()}
	src, err := NewSource("awc:"+r.f.URL(), nil)
	if err != nil {
		t.Fatal(err)
	}
	r.svc = &Service{Source: src, Store: r.st, Policy: func() policy.Record { return *r.pol }, Counters: r.c}
	return r
}

func TestNotConfiguredIsVisible(t *testing.T) {
	svc := &Service{Store: newMem(obsAt)}
	_, err := svc.Answer(context.Background(), tbilisi, time.Time{})
	var u *UnavailableError
	if !errors.As(err, &u) || u.Reason != ReasonNotConfigured || !strings.Contains(err.Error(), "USSP_WEATHER_SOURCE") {
		t.Fatalf("%v", err)
	}
	c := svc.Check(context.Background(), []geodesy.BBox{tbilisi}, obsAt, obsAt.Add(time.Hour))
	if c.Ref() != nil || !strings.Contains(c.Unavailable, "no weather source") {
		t.Fatalf("%+v", c)
	}
	if st, d := svc.Probe()(context.Background()); st != obs.StateUp || !strings.HasPrefix(d, ReasonNotConfigured) || !strings.Contains(d, "503") {
		t.Fatal(st, d)
	}
	svc.Run(context.Background()) // returns at once without a source
	if err := svc.Poll(context.Background()); !errors.As(err, &u) {
		t.Fatal(err)
	}
	noStore := &Service{Source: newAWC(t, "http://127.0.0.1:1")}
	if _, err := noStore.Answer(context.Background(), tbilisi, time.Time{}); !errors.As(err, &u) || u.Reason != ReasonDatabase {
		t.Fatal(err)
	}
	if st, d := noStore.Probe()(context.Background()); st != obs.StateDown || !strings.HasPrefix(d, ReasonDatabase) {
		t.Fatal(st, d)
	}
}

func TestNoStationsIsVisible(t *testing.T) {
	r := newRig(t, "31013KT 9999 FEW030 17/05 Q1026")
	r.pol.Values.WeatherStationIDs = nil
	_, err := r.svc.Answer(context.Background(), tbilisi, time.Time{})
	var u *UnavailableError
	if !errors.As(err, &u) || u.Reason != ReasonNoStations {
		t.Fatal(err)
	}
	if err := r.svc.Poll(context.Background()); !errors.As(err, &u) || r.f.Calls() != 0 {
		t.Fatal(err, r.f.Calls())
	}
	if st, d := r.svc.Probe()(context.Background()); st != obs.StateDegraded || !strings.HasPrefix(d, ReasonNoStations) {
		t.Fatal(st, d)
	}
}

// E-02: configured and never fetched says so; up is fresh; the source
// down answers the last products stale with the failure time; back up
// is fresh again; too long without a delivery is stale too.
func TestAnswerFreshStaleAndBack(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, "31013KT 9999 FEW030 17/05 Q1026")
	a, err := r.svc.Answer(ctx, tbilisi, time.Time{})
	if err != nil || !a.Stale || a.Source.State != StateNeverFetched || len(a.Products) != 0 || a.PolicyVersion != 3 {
		t.Fatalf("never fetched: %+v %v", a, err)
	}
	if st, d := r.svc.Probe()(ctx); st != obs.StateDegraded || !strings.Contains(d, "never delivered") {
		t.Fatal(st, d)
	}
	if err := r.svc.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	a, err = r.svc.Answer(ctx, tbilisi, time.Time{})
	if err != nil || a.Stale || a.Source.State != StateUp || len(a.Products) != 2 || a.Source.AgeS == nil || *a.Source.AgeS != 0 {
		t.Fatalf("up: %+v %v", a, err)
	}
	m := a.Products[0]
	if m.Kind != KindMETAR || m.AgeS != 300 || !m.InForce || m.Stale || m.QNHArea != "UGTB" || *m.Fields.QNHHPa != 1026 ||
		m.Area.RadiusM != 20_000 || !m.ValidTo.Equal(obsAt.Add(time.Hour)) || a.Products[1].Kind != KindTAF {
		t.Fatalf("METAR %+v", m)
	}
	if st, _ := r.svc.Probe()(ctx); st != obs.StateUp {
		t.Fatal(st)
	}
	if a, _ := r.svc.Answer(ctx, faraway, time.Time{}); len(a.Products) != 0 {
		t.Fatalf("far away: %+v", a.Products)
	}
	if a, _ := r.svc.Answer(ctx, tbilisi, obsAt.Add(-time.Minute)); len(a.Products) != 1 || a.Products[0].Kind != KindTAF {
		t.Fatalf("before the METAR: %+v", a.Products)
	}

	r.f.Down()
	r.st.advance(10 * time.Minute)
	if err := r.svc.Poll(ctx); err == nil {
		t.Fatal("a failed fetch reported success")
	}
	a, err = r.svc.Answer(ctx, tbilisi, time.Time{})
	if err != nil || !a.Stale || a.Source.State != StateFailing || a.Source.LastFailureAt == nil ||
		!a.Source.LastFailureAt.Equal(obsAt.Add(15*time.Minute)) || !strings.Contains(*a.Source.Failure, "503") ||
		len(a.Products) != 2 || !a.Products[0].Stale || a.Products[0].AgeS != 900 {
		t.Fatalf("down: %+v %v", a, err)
	}
	if st, d := r.svc.Probe()(ctx); st != obs.StateDegraded || !strings.Contains(d, "failed since 2026-10-04T13:15:00Z") {
		t.Fatal(st, d)
	}
	if r.c.Get("weather_fetch_failed") != 1 || r.c.Get("weather_answer_stale") != 2 {
		t.Fatal(r.c.Snapshot())
	}

	r.f.Up()
	if err := r.svc.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if a, _ = r.svc.Answer(ctx, tbilisi, time.Time{}); a.Stale || a.Source.State != StateUp || a.Source.Failure != nil {
		t.Fatalf("back: %+v", a)
	}
	if r.c.Get("weather_product_stored") != 2 {
		t.Fatalf("a report stored twice: %v", r.c.Snapshot())
	}
	r.st.advance(time.Duration(r.pol.Values.WeatherStaleS+1) * time.Second)
	if a, _ = r.svc.Answer(ctx, tbilisi, time.Time{}); !a.Stale || a.Source.State != StateUp {
		t.Fatalf("not delivered for longer than weather_stale_s: %+v", a)
	}
	if st, d := r.svc.Probe()(ctx); st != obs.StateDegraded || !strings.Contains(d, "last delivered") {
		t.Fatal(st, d)
	}
}

func TestDatabaseDownIsVisible(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, "31013KT 9999 Q1026")
	r.st.fail = errors.New("connection refused")
	var u *UnavailableError
	if _, err := r.svc.Answer(ctx, tbilisi, time.Time{}); !errors.As(err, &u) || u.Reason != ReasonDatabase {
		t.Fatal(err)
	}
	if err := r.svc.Poll(ctx); err == nil || r.c.Get("weather_store_failed") != 1 {
		t.Fatal(err, r.c.Snapshot())
	}
	if c := r.svc.Check(ctx, []geodesy.BBox{tbilisi}, obsAt, obsAt.Add(time.Hour)); c.Ref() != nil || c.Unavailable == "" {
		t.Fatalf("%+v", c)
	}
	if st, _ := r.svc.Probe()(ctx); st != obs.StateDown {
		t.Fatal(st)
	}
	r.f.Down()
	if err := r.svc.Poll(ctx); err == nil || r.c.Get("weather_status_not_recorded") != 1 {
		t.Fatal(err, r.c.Snapshot())
	}
}

// The decision's check: products in force carry their ids; none in
// force or none configured is null with why; a failing source says so;
// the advisory appears at the threshold and not below it (E-01).
func TestCheck(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, "31013G21KT 9999 FEW030 17/05 Q1026")
	if err := r.svc.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	c := r.svc.Check(ctx, []geodesy.BBox{tbilisi}, obsAt.Add(10*time.Minute), obsAt.Add(40*time.Minute))
	if len(c.ProductIDs) != 2 || c.Unavailable != "" || c.Stale != "" || *c.Ref() != strings.Join(c.ProductIDs, ",") {
		t.Fatalf("%+v", c)
	}
	// Gust 21 kt is 10.8 m/s: at or above the default 10 m/s; the TAF's
	// 16 kt (8.2 m/s) is not.
	if len(c.Advisories) != 1 || !strings.Contains(c.Advisories[0], "METAR UGTB") || !strings.Contains(c.Advisories[0], "10.8 m/s") {
		t.Fatalf("advisories %v", c.Advisories)
	}
	r.pol.Values.WeatherAdvisoryWindMS = 11
	if c := r.svc.Check(ctx, []geodesy.BBox{tbilisi}, obsAt.Add(10*time.Minute), obsAt.Add(40*time.Minute)); len(c.Advisories) != 0 {
		t.Fatalf("below the threshold: %v", c.Advisories)
	}
	r.pol.Values.WeatherAdvisoryWindMS = 8
	if c := r.svc.Check(ctx, []geodesy.BBox{tbilisi}, obsAt.Add(10*time.Minute), obsAt.Add(40*time.Minute)); len(c.Advisories) != 2 {
		t.Fatalf("both at 8 m/s: %v", c.Advisories)
	}
	r.pol.Values.WeatherAdvisoryWindMS = 0
	if c := r.svc.Check(ctx, []geodesy.BBox{tbilisi}, obsAt.Add(10*time.Minute), obsAt.Add(40*time.Minute)); len(c.Advisories) != 0 {
		t.Fatalf("off: %v", c.Advisories)
	}
	// Three days ahead nothing is in force.
	c = r.svc.Check(ctx, []geodesy.BBox{tbilisi}, obsAt.Add(72*time.Hour), obsAt.Add(73*time.Hour))
	if c.Ref() != nil || !strings.Contains(c.Unavailable, "no weather product") {
		t.Fatalf("ahead: %+v", c)
	}
	if c := r.svc.Check(ctx, []geodesy.BBox{faraway}, obsAt, obsAt.Add(time.Hour)); c.Ref() != nil {
		t.Fatalf("far away: %+v", c)
	}
	r.f.Down()
	_ = r.svc.Poll(ctx)
	c = r.svc.Check(ctx, []geodesy.BBox{tbilisi}, obsAt.Add(10*time.Minute), obsAt.Add(40*time.Minute))
	if len(c.ProductIDs) != 2 || !strings.Contains(c.Stale, "failed since") {
		t.Fatalf("failing: %+v", c)
	}
}

func TestCheckBounded(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, "31013KT 9999 Q1026")
	var ms []fake.METAR
	for i := range MaxChecked + 4 {
		ms = append(ms, fake.NewMETAR("UGTB", obsAt.Add(-time.Duration(i)*time.Minute), 41.669, 44.955, "31013KT 9999 Q1026"))
	}
	r.f.Set(ms, nil)
	if err := r.svc.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	c := r.svc.Check(ctx, []geodesy.BBox{tbilisi}, obsAt, obsAt.Add(time.Minute))
	if len(c.ProductIDs) != MaxChecked {
		t.Fatalf("%d checked", len(c.ProductIDs))
	}
}

func TestPollPrunesPastRetention(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, "31013KT 9999 Q1026")
	if err := r.svc.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	r.f.Set(nil, nil)
	r.st.advance(time.Duration(r.pol.Values.RecordRetentionDays)*24*time.Hour + 30*time.Hour)
	// A poll that delivers nothing is a failure and still prunes.
	if err := r.svc.Poll(ctx); !errors.Is(err, ErrNothingDelivered) {
		t.Fatal(err)
	}
	if n := r.c.Get("weather_product_pruned"); n != 2 || len(r.st.products) != 0 {
		t.Fatalf("pruned %d, left %d", n, len(r.st.products))
	}
}

func TestPollCountsRefusedReports(t *testing.T) {
	r := newRig(t, "31013KT 9999 GARBLE Q1026")
	if err := r.svc.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.c.Get("weather_report_refused") != 1 || r.c.Get("weather_product_stored") != 1 {
		t.Fatal(r.c.Snapshot())
	}
}

// Run polls at once and again after the period; it stops with ctx. The
// test waits on the fake's call count, never on a sleep.
func TestRunPollsUntilCancelled(t *testing.T) {
	r := newRig(t, "31013KT 9999 Q1026")
	r.pol.Values.WeatherRefreshS = 0.05 // below the policy's floor: only the loop is under test
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.svc.Run(ctx); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for r.f.Calls() < 4 {
		if time.Now().After(deadline) {
			t.Fatal("Run did not poll twice")
		}
		<-time.After(10 * time.Millisecond)
	}
	cancel()
	<-done
	r.f.Down()
	ctx, cancel = context.WithCancel(context.Background())
	done = make(chan struct{})
	go func() { r.svc.Run(ctx); close(done) }()
	for r.c.Get("weather_fetch_failed") < 1 {
		if time.Now().After(deadline) {
			t.Fatal("Run did not poll the failing source")
		}
		<-time.After(10 * time.Millisecond)
	}
	cancel()
	<-done
}

func TestStateNames(t *testing.T) {
	now := obsAt
	ok := now.Add(-time.Minute)
	failed := now.Add(-30 * time.Second)
	if s, stale := state("x", SourceStatus{LastSuccessAt: &ok, LastFailureAt: &failed, LastError: "boom"}, now, 600); s.State != StateFailing || !stale {
		t.Fatal(s, stale)
	}
	older := now.Add(-2 * time.Minute)
	if s, stale := state("x", SourceStatus{LastSuccessAt: &ok, LastFailureAt: &older}, now, 600); s.State != StateUp || stale || s.Failure != nil {
		t.Fatal(s, stale)
	}
	if staleDetail(SourceState{}) != "the weather source has never delivered" {
		t.Fatal()
	}
}

// The national API's view: WeatherAnswer is Answer, and the 503's
// reason reads through WeatherUnavailable.
func TestWeatherAnswerForTheNationalAPI(t *testing.T) {
	svc := &Service{Store: newMem(obsAt)}
	_, err := svc.WeatherAnswer(context.Background(), tbilisi, time.Time{})
	var u interface{ WeatherUnavailable() (string, string) }
	if !errors.As(err, &u) {
		t.Fatal(err)
	}
	if r, d := u.WeatherUnavailable(); r != ReasonNotConfigured || d == "" {
		t.Fatal(r, d)
	}
	r := newRig(t, "31013KT 9999 Q1026")
	if a, err := r.svc.WeatherAnswer(context.Background(), tbilisi, time.Time{}); err != nil || !a.(Answer).Stale {
		t.Fatal(a, err)
	}
}

// A fetch that answers but delivers nothing in force, every report
// refused or no report for any station (204), is a failed delivery:
// the source is failing, its products stale and /readyz degraded. Its
// twin: a report already held and still in force is a delivery (E-01).
func TestPollNothingDeliveredIsAFailure(t *testing.T) {
	ctx := context.Background()
	for name, set := range map[string]func(f *fake.Fake){
		"all refused": func(f *fake.Fake) {
			f.Set([]fake.METAR{fake.NewMETAR("UGTB", obsAt, 41.669, 44.955, "31013KT 9999 GARBLE Q1026")}, nil)
		},
		"no report": func(f *fake.Fake) { f.Set(nil, nil) },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, "31013KT 9999 Q1026")
			set(r.f)
			if err := r.svc.Poll(ctx); !errors.Is(err, ErrNothingDelivered) {
				t.Fatalf("Poll: %v", err)
			}
			a, err := r.svc.Answer(ctx, tbilisi, time.Time{})
			if err != nil || !a.Stale || a.Source.State != StateFailing || a.Source.LastFailureAt == nil ||
				!a.Source.LastFailureAt.Equal(r.st.now) || !strings.Contains(*a.Source.Failure, "nothing in force") {
				t.Fatalf("%+v %v", a, err)
			}
			if st, d := r.svc.Probe()(ctx); st != obs.StateDegraded || !strings.Contains(d, "failed since") {
				t.Fatal(st, d)
			}
			if r.c.Get("weather_nothing_delivered") != 1 || r.c.Get("weather_fetched") != 0 {
				t.Fatal(r.c.Snapshot())
			}
		})
	}

	r := newRig(t, "31013KT 9999 Q1026")
	for range 2 {
		if err := r.svc.Poll(ctx); err != nil {
			t.Fatal(err)
		}
		r.st.advance(time.Minute)
	}
	if a, _ := r.svc.Answer(ctx, tbilisi, time.Time{}); a.Stale || a.Source.State != StateUp || r.c.Get("weather_fetched") != 2 {
		t.Fatalf("a report held and in force: %+v %v", a, r.c.Snapshot())
	}
	// The same reports once neither is in force any more: the station
	// is stuck, nothing was delivered.
	r.st.advance(26 * time.Hour)
	if err := r.svc.Poll(ctx); !errors.Is(err, ErrNothingDelivered) {
		t.Fatalf("a stuck station: %v", err)
	}
}
