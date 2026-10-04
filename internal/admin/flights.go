package admin

import (
	"context"
	"fmt"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/store"
)

// SampleReader reads the capture time of the newest sample of each
// flight in the telemetry record (TimescaleDB, read only).
type SampleReader interface {
	LastSamples(ctx context.Context, flightIDs []string) (map[string]time.Time, error)
}

// TSSamples is SampleReader on the store's time-series pool.
type TSSamples struct{ S *store.Store }

// LastSamples implements SampleReader.
func (t TSSamples) LastSamples(ctx context.Context, ids []string) (map[string]time.Time, error) {
	if t.S == nil || t.S.TS == nil {
		return nil, store.ErrNoPool
	}
	us, err := store.UUIDs("flight_id", ids)
	if err != nil {
		return nil, err
	}
	rows, err := t.S.TSQueries().ConsoleLastSamples(ctx, us)
	if err != nil {
		return nil, err
	}
	out := make(map[string]time.Time, len(rows))
	for _, r := range rows {
		out[store.UUIDText(r.FlightID)] = r.LastAt
	}
	return out, nil
}

// Conformance is a flight's newest conformance state recorded.
type Conformance struct {
	State  string    `json:"state"`
	At     time.Time `json:"at"`
	Reason *string   `json:"reason,omitempty"`
}

// Flight is one active flight on the console.
type Flight struct {
	FlightID            string       `json:"flight_id"`
	IntentID            *string      `json:"intent_id,omitempty"`
	AuthorisationNumber *string      `json:"authorisation_number,omitempty"`
	UASSerial           string       `json:"uas_serial"`
	OperatorReg         *string      `json:"operator_reg,omitempty"`
	ClientID            *string      `json:"client_id,omitempty"`
	StartedAt           time.Time    `json:"started_at"`
	LastState           *string      `json:"last_state,omitempty"`
	Emergency           bool         `json:"emergency"`
	IntentState         *string      `json:"intent_state,omitempty"`
	DSSState            *string      `json:"dss_state,omitempty"`
	Conformance         *Conformance `json:"conformance,omitempty"`
	LastSampleAt        *time.Time   `json:"last_sample_at,omitempty"`
	LastSampleAgeS      *float64     `json:"last_sample_age_s,omitempty"`
	EmergencyCaseOpen   bool         `json:"emergency_case_open"`
}

// Samples says whether the last samples were read.
type Samples struct {
	Available bool    `json:"available"`
	Detail    *string `json:"detail,omitempty"`
}

// Flights is GET /v1/admin/flights.
type Flights struct {
	Flights   []Flight `json:"flights"`
	Truncated bool     `json:"truncated"`
	Samples   Samples  `json:"samples"`
}

// CounterSamplesUnavailable counts a flights answer made without the
// telemetry record.
const CounterSamplesUnavailable = "admin_flights_samples_unavailable"

// Flights lists the active flights (newest first, at most MaxFlights)
// with the time of their last sample from the telemetry record. The
// relational read is done before the time-series one: no connection is
// held while waiting for the other database. Without the record the
// flights are listed without last samples and Samples says why.
func (s *Service) Flights(ctx context.Context) (Flights, error) {
	rows, err := s.Store.Queries().AdminActiveFlights(ctx, MaxFlights+1)
	if err != nil {
		return Flights{}, fmt.Errorf("active flights: %w", err)
	}
	out := Flights{Flights: make([]Flight, 0, min(len(rows), MaxFlights)), Samples: Samples{Available: true}}
	if len(rows) > MaxFlights {
		rows, out.Truncated = rows[:MaxFlights], true
	}
	ids := make([]string, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		f := Flight{FlightID: store.UUIDText(r.ID), AuthorisationNumber: r.AuthorisationNumber, UASSerial: r.UasSerial,
			OperatorReg: r.OperatorReg, ClientID: r.ClientID, StartedAt: r.StartedAt.UTC(), LastState: r.LastState, Emergency: r.Emergency,
			IntentState: r.IntentState, DSSState: r.DssState, EmergencyCaseOpen: r.CaseOpen}
		f.IntentID = strp(store.UUIDText(r.IntentID))
		if r.ConformanceState != nil && r.ConformanceAt != nil {
			f.Conformance = &Conformance{State: *r.ConformanceState, At: r.ConformanceAt.UTC(), Reason: r.ConformanceReason}
		}
		out.Flights = append(out.Flights, f)
		ids = append(ids, f.FlightID)
	}
	if len(ids) == 0 {
		return out, nil
	}
	if s.Samples == nil {
		d := "the telemetry record (TimescaleDB) is not configured on this process: no last sample is shown"
		out.Samples = Samples{Detail: &d}
		return out, nil
	}
	last, err := s.Samples.LastSamples(ctx, ids)
	now := s.now()
	if err != nil {
		s.count(CounterSamplesUnavailable)
		s.logger().WarnContext(ctx, "last samples not read; flights listed without them", "error", err.Error())
		d := fmt.Sprintf("the telemetry record (TimescaleDB) cannot be read since %s: no last sample is shown, which is not an absence of telemetry",
			s.samplesDown(now, true).UTC().Format(time.RFC3339))
		out.Samples = Samples{Detail: &d}
		return out, nil
	}
	s.samplesDown(now, false)
	for i := range out.Flights {
		if at, ok := last[out.Flights[i].FlightID]; ok {
			a, age := at.UTC(), ageS(now, at)
			out.Flights[i].LastSampleAt, out.Flights[i].LastSampleAgeS = &a, &age
		}
	}
	return out, nil
}

// samplesDown records whether the telemetry record failed at now and
// returns since when it fails (the first failure after a success).
func (s *Service) samplesDown(now time.Time, failed bool) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case !failed:
		s.samplesSince = time.Time{}
	case s.samplesSince.IsZero():
		s.samplesSince = now
	}
	return s.samplesSince
}
