package records

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"
)

// memReader is Reader, BundleStore and GapStore in memory, each read
// failing on its own when its error is set (the sections are read on
// their own; a failing store is one section's, never the record's).
type memReader struct {
	mu        sync.Mutex
	now       time.Time
	flights   map[string]FlightRow
	intents   map[string]IntentRow
	versions  []Version
	alerts    []Alert
	states    []ConformanceState
	notices   []Notice
	gaps      []IngestGap
	policies  []PolicyVersion
	bundles   map[string]Bundle
	stored    []Gap
	errFlight error
	errOf     map[string]error
}

func newMemReader(now time.Time) *memReader {
	return &memReader{now: now, flights: map[string]FlightRow{}, intents: map[string]IntentRow{}, bundles: map[string]Bundle{}, errOf: map[string]error{}}
}

func (m *memReader) err(what string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.errOf[what]
}

func (m *memReader) Flight(_ context.Context, id string) (FlightRow, error) {
	if m.errFlight != nil {
		return FlightRow{}, m.errFlight
	}
	f, ok := m.flights[id]
	if !ok {
		return FlightRow{}, ErrNotFound
	}
	return f, nil
}

func (m *memReader) Intent(_ context.Context, id string) (IntentRow, error) {
	if err := m.err("intent"); err != nil {
		return IntentRow{}, err
	}
	return m.intents[id], nil
}

func (m *memReader) Versions(_ context.Context, _ string, n int) ([]Version, error) {
	if err := m.err("versions"); err != nil {
		return nil, err
	}
	return take(m.versions, n), nil
}

func take[T any](s []T, n int) []T {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (m *memReader) Alerts(_ context.Context, _ string, n int) ([]Alert, error) {
	if err := m.err("alerts"); err != nil {
		return nil, err
	}
	return take(m.alerts, n), nil
}

func (m *memReader) Conformance(_ context.Context, _ string, n int) ([]ConformanceState, error) {
	if err := m.err("conformance"); err != nil {
		return nil, err
	}
	return take(m.states, n), nil
}

func (m *memReader) Notices(_ context.Context, _, _ string, n int) ([]Notice, error) {
	if err := m.err("notices"); err != nil {
		return nil, err
	}
	return take(m.notices, n), nil
}

func (m *memReader) IngestGaps(_ context.Context, _ string, from, to time.Time, n int) ([]IngestGap, error) {
	if err := m.err("gaps"); err != nil {
		return nil, err
	}
	var out []IngestGap
	for _, g := range m.gaps {
		if !g.Ended.Before(from) && !g.Started.After(to) {
			out = append(out, g)
		}
	}
	return take(out, n), nil
}

func (m *memReader) Policies(_ context.Context, versions []int64) ([]PolicyVersion, error) {
	if err := m.err("policies"); err != nil {
		return nil, err
	}
	var out []PolicyVersion
	for _, p := range m.policies {
		for _, v := range versions {
			if p.PolicyVersion == v {
				out = append(out, p)
			}
		}
	}
	return out, nil
}

func (m *memReader) Now(context.Context) (time.Time, error) {
	if err := m.err("now"); err != nil {
		return time.Time{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.now, nil
}

func (m *memReader) DayFlights(_ context.Context, start, end, afterAt time.Time, afterID string, n int) ([]DayFlight, error) {
	if err := m.err("day"); err != nil {
		return nil, err
	}
	var all []DayFlight
	for id := range m.flights {
		f := m.flights[id]
		if !f.StartedAt.Before(start) && f.StartedAt.Before(end) {
			all = append(all, DayFlight{ID: f.ID, StartedAt: f.StartedAt})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if !all[i].StartedAt.Equal(all[j].StartedAt) {
			return all[i].StartedAt.Before(all[j].StartedAt)
		}
		return all[i].ID < all[j].ID
	})
	var out []DayFlight
	for _, f := range all {
		if f.StartedAt.After(afterAt) || (f.StartedAt.Equal(afterAt) && f.ID > afterID) {
			out = append(out, f)
		}
	}
	return take(out, n), nil
}

func (m *memReader) InsertBundle(_ context.Context, b Bundle) (bool, error) {
	if err := m.err("insert"); err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	k := b.Date.Format(time.DateOnly)
	if _, ok := m.bundles[k]; ok {
		return false, nil
	}
	b.BuiltAt = m.now
	m.bundles[k] = b
	return true, nil
}

func (m *memReader) Bundle(_ context.Context, day time.Time) (Bundle, error) {
	if err := m.err("bundle"); err != nil {
		return Bundle{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.bundles[day.Format(time.DateOnly)]
	if !ok {
		return Bundle{}, ErrNotFound
	}
	return b, nil
}

func (m *memReader) MissingDays(_ context.Context, first, last time.Time) ([]time.Time, error) {
	if err := m.err("missing"); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []time.Time
	for d := Day(first); !d.After(Day(last)); d = d.AddDate(0, 0, 1) {
		if _, ok := m.bundles[d.Format(time.DateOnly)]; !ok {
			out = append(out, d)
		}
	}
	return out, nil
}

func (m *memReader) AuditBundle(context.Context, Bundle) error { return m.err("audit") }

func (m *memReader) RecordGap(_ context.Context, g Gap) error {
	if err := m.err("record_gap"); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stored = append(m.stored, g)
	return nil
}

func (m *memReader) storedGaps() []Gap {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Gap(nil), m.stored...)
}

// memSeries is Series in memory.
type memSeries struct {
	summary    Summary
	silences   []Silence
	writerGaps []WriterGap
	products   []Product
	errOf      map[string]error
}

func (s *memSeries) Summary(context.Context, string) (Summary, error) {
	if err := s.errOf["summary"]; err != nil {
		return Summary{}, err
	}
	return s.summary, nil
}

func (s *memSeries) Silences(_ context.Context, _ string, _ float64, n int) ([]Silence, error) {
	if err := s.errOf["silences"]; err != nil {
		return nil, err
	}
	return take(s.silences, n), nil
}

func (s *memSeries) WriterGaps(_ context.Context, _, _ time.Time, n int) ([]WriterGap, error) {
	if err := s.errOf["writer"]; err != nil {
		return nil, err
	}
	return take(s.writerGaps, n), nil
}

func (s *memSeries) Products(_ context.Context, _ string, _, _ time.Time, n int) ([]Product, int64, error) {
	if err := s.errOf["products"]; err != nil {
		return nil, 0, err
	}
	return take(s.products, n), int64(len(s.products)), nil
}

var errDown = errors.New("relation is unreadable")

func rawJSON(s string) json.RawMessage { return json.RawMessage(s) }
