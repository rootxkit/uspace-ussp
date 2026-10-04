package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/coordination"
	"github.com/rootxkit/uspace-ussp/internal/national"
	"github.com/rootxkit/uspace-ussp/internal/occurrence"
	"github.com/rootxkit/uspace-ussp/internal/records"
	"github.com/rootxkit/uspace-ussp/internal/status"
)

const (
	seamFlight = "0b5d4c3a-2e1f-4a0b-9c8d-7e6f5a4b3c2d"
	seamIntent = "6f1c0d2e-3b4a-4c5d-8e6f-7a8b9c0d1e2f"
)

type coordStore struct {
	items []coordination.Item
	n     int
	err   error
}

func (c *coordStore) Open(_ context.Context, n int) ([]coordination.Item, bool, error) {
	c.n = n
	return c.items, true, c.err
}

// The notice store's items in the contract's form, bounded by
// coordination.MaxListed; a malformed id or a failed read is an error,
// never a list.
func TestCoordinationListSeam(t *testing.T) {
	flight, errText := seamFlight, "503"
	st := &coordStore{items: []coordination.Item{
		{ID: 1, NoticeRef: "r1", Kind: "nonconformance", IntentID: seamIntent, FlightID: &flight, State: "pending", LastError: &errText},
		{ID: 2, NoticeRef: "r2", Kind: "intent_notice", IntentID: seamIntent, State: "failed"},
	}}
	out, err := coordinationList{Store: st}.Open(context.Background())
	if err != nil || st.n != coordination.MaxListed || !out.Truncated || len(out.Notices) != 2 ||
		out.Notices[0].IntentId.String() != seamIntent || out.Notices[0].FlightId == nil || out.Notices[0].FlightId.String() != seamFlight ||
		*out.Notices[0].LastError != errText || out.Notices[1].FlightId != nil || out.Notices[1].State != "failed" {
		t.Fatalf("%+v %v", out, err)
	}
	bad := "not-a-uuid"
	for name, s := range map[string]*coordStore{
		"bad intent": {items: []coordination.Item{{IntentID: bad}}},
		"bad flight": {items: []coordination.Item{{IntentID: seamIntent, FlightID: &bad}}},
		"down":       {err: errors.New("db")},
	} {
		if _, err := (coordinationList{Store: s}).Open(context.Background()); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

type occSeam struct {
	items []occurrence.Item
	err   error
	n     int
}

func (o *occSeam) Flag(context.Context, string, string, string, string) (occurrence.Item, bool, error) {
	if o.err != nil {
		return occurrence.Item{}, false, o.err
	}
	return o.items[0], true, nil
}

func (o *occSeam) Open(_ context.Context, n int) ([]occurrence.Item, bool, error) {
	o.n = n
	return o.items, false, o.err
}

// Occurrence reports converted with their flights; the service's
// FlagError becomes national.RefusedError with NotFound kept; another error
// passes through; a malformed flight id is an error.
func TestOccurrenceSeams(t *testing.T) {
	ok := &occSeam{items: []occurrence.Item{{ReportRef: "o1", Kind: "airprox", Critical: true, FlightIDs: []string{seamFlight}}}}
	g, created, err := occurrenceFlags{Service: ok}.Flag(context.Background(), "s", "a", "airprox", "")
	if err != nil || !created || g.ReportRef != "o1" || !g.Critical || len(g.FlightIds) != 1 || g.FlightIds[0].String() != seamFlight {
		t.Fatalf("%+v %v %v", g, created, err)
	}
	items, _, err := occurrenceList{Store: ok}.Open(context.Background())
	if err != nil || len(items) != 1 || ok.n != occurrence.MaxListed {
		t.Fatalf("%+v %v n=%d", items, err, ok.n)
	}
	var re *national.RefusedError
	_, _, err = occurrenceFlags{Service: &occSeam{err: &occurrence.FlagError{NotFound: true, Reason: "no alert"}}}.Flag(context.Background(), "", "", "", "")
	if !errors.As(err, &re) || !re.NotFound || re.Reason != "no alert" {
		t.Fatalf("not found: %v", err)
	}
	_, _, err = occurrenceFlags{Service: &occSeam{err: &occurrence.FlagError{Reason: "kind"}}}.Flag(context.Background(), "", "", "", "")
	if !errors.As(err, &re) || re.NotFound {
		t.Fatalf("refused: %v", err)
	}
	down := errors.New("db")
	if _, _, err := (occurrenceFlags{Service: &occSeam{err: down}}).Flag(context.Background(), "", "", "", ""); !errors.Is(err, down) || errors.As(err, &re) {
		t.Fatalf("down: %v", err)
	}
	bad := &occSeam{items: []occurrence.Item{{FlightIDs: []string{"x"}}}}
	if _, _, err := (occurrenceList{Store: bad}).Open(context.Background()); err == nil {
		t.Error("malformed flight listed")
	}
	if _, _, err := (occurrenceFlags{Service: bad}).Flag(context.Background(), "", "", "", ""); err == nil {
		t.Error("malformed flight flagged")
	}
	if _, _, err := (occurrenceList{Store: &occSeam{err: down}}).Open(context.Background()); !errors.Is(err, down) {
		t.Errorf("list down: %v", err)
	}
	if o := occurrencesAPI(&occurrence.Service{}); o.MaxNarrative != occurrence.MaxNarrative || o.MaxNarrative <= 0 {
		t.Errorf("narrative bound %d", o.MaxNarrative)
	}
}

type statusSeam struct {
	n   status.Notice
	err error
}

func (s statusSeam) Request(context.Context, string, string) (status.Notice, bool, error) {
	return s.n, true, s.err
}

func (s statusSeam) List(context.Context) ([]status.Notice, error) {
	if s.err != nil {
		return nil, s.err
	}
	return []status.Notice{s.n}, nil
}

// Status notices converted; RequestError becomes national.RefusedError, a
// store error passes through.
func TestStatusSeam(t *testing.T) {
	n := status.Notice{Kind: "start", CertificateID: "c", Reference: "ref", State: "pending"}
	g, created, err := statusNotices{Service: statusSeam{n: n}}.Request(context.Background(), "s", "start")
	if err != nil || !created || g.Reference != "ref" || g.CertificateId != "c" || g.Kind != "start" {
		t.Fatalf("%+v %v", g, err)
	}
	if l, err := (statusNotices{Service: statusSeam{n: n}}).List(context.Background()); err != nil || len(l) != 1 || l[0].State != "pending" {
		t.Fatalf("%+v %v", l, err)
	}
	var re *national.RefusedError
	if _, _, err := (statusNotices{Service: statusSeam{err: &status.RequestError{Reason: "cease first"}}}).Request(context.Background(), "", ""); !errors.As(err, &re) || re.Reason != "cease first" {
		t.Fatalf("refused: %v", err)
	}
	down := errors.New("db")
	if _, _, err := (statusNotices{Service: statusSeam{err: down}}).Request(context.Background(), "", ""); !errors.Is(err, down) || errors.As(err, &re) {
		t.Fatalf("down: %v", err)
	}
	if _, err := (statusNotices{Service: statusSeam{err: down}}).List(context.Background()); !errors.Is(err, down) {
		t.Fatalf("list down: %v", err)
	}
}

type recSeam struct {
	rec  records.Record
	b    records.Bundle
	path string
	err  error
}

func (r recSeam) Flight(context.Context, string) (records.Record, error) { return r.rec, r.err }

func (r recSeam) Open(context.Context, time.Time) (records.Bundle, *os.File, error) {
	if r.err != nil {
		return records.Bundle{}, nil, r.err
	}
	f, err := os.Open(r.path)
	return r.b, f, err
}

// The records' errors mapped onto the route's (the original kept in the
// chain); the record and the bundle passed through.
func TestRecordSeams(t *testing.T) {
	ctx := context.Background()
	rec, err := flightRecords{Builder: recSeam{rec: records.Record{Schema: records.Schema, FlightID: seamFlight}}}.Flight(ctx, seamFlight)
	if r, ok := rec.(records.Record); err != nil || !ok || r.FlightID != seamFlight {
		t.Fatalf("%+v %v", rec, err)
	}
	for name, c := range map[string]struct {
		in, want error
	}{
		"not found": {records.ErrNotFound, national.ErrRecordNotFound},
		"corrupt":   {records.ErrBundleCorrupt, national.ErrBundleCorrupt},
	} {
		_, err := flightRecords{Builder: recSeam{err: c.in}}.Flight(ctx, seamFlight)
		_, _, derr := dailyRecords{Daily: recSeam{err: c.in}}.Open(ctx, time.Time{})
		if !errors.Is(err, c.want) || !errors.Is(err, c.in) || !errors.Is(derr, c.want) {
			t.Errorf("%s: %v / %v", name, err, derr)
		}
	}
	down := errors.New("db")
	if _, err := (flightRecords{Builder: recSeam{err: down}}).Flight(ctx, ""); !errors.Is(err, down) ||
		errors.Is(err, national.ErrRecordNotFound) || errors.Is(err, national.ErrBundleCorrupt) {
		t.Errorf("down: %v", err)
	}
	path := filepath.Join(t.TempDir(), "b.jsonl.gz")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	b, f, err := dailyRecords{Daily: recSeam{b: records.Bundle{Date: day, Hash: "h", Flights: 2}, path: path}}.Open(ctx, day)
	if err != nil || b.Hash != "h" || b.Flights != 2 || !b.Date.Equal(day) || f == nil {
		t.Fatalf("%+v %v", b, err)
	}
	_ = f.Close()
}
