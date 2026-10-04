package occurrence

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// FlightRef is a flight as a report names it.
type FlightRef struct {
	FlightID            string
	Serial              string
	OperatorReg         *string
	AuthorisationNumber *string
	IntentID            *string
	InUSpace            bool
	Emergency           bool
	StartedAt           time.Time
}

// AlertEvent is an alert that may be an occurrence, with its flight.
type AlertEvent struct {
	AlertID  string
	Kind     string
	RaisedAt time.Time
	Detail   json.RawMessage
	// SourceRef is what the report about it is keyed by: the pair and
	// its raise for a proximity conflict (one report for both flights'
	// alerts), the alert id otherwise.
	SourceRef string
	Flight    FlightRef
}

// Queued is a report claimed for one try.
type Queued struct {
	ID       string
	Ref      string
	Body     []byte
	Attempts int
}

// Item is a report as the console lists it.
type Item struct {
	ReportRef       string     `json:"report_ref"`
	Kind            string     `json:"kind"`
	State           string     `json:"state"`
	Channel         string     `json:"channel"`
	FlaggedBy       string     `json:"flagged_by"`
	BecameAwareAt   time.Time  `json:"became_aware_at"`
	DeadlineAt      time.Time  `json:"deadline_at"`
	TimeToDeadlineS float64    `json:"time_to_deadline_s"`
	Critical        bool       `json:"critical"`
	Attempts        int        `json:"attempts"`
	LastError       *string    `json:"last_error"`
	FlightIDs       []string   `json:"flight_ids"`
	SubmittedAt     *time.Time `json:"submitted_at"`
	AuthorityRef    *string    `json:"authority_ref"`
	FailedAt        *time.Time `json:"failed_at"`
	NextAt          *time.Time `json:"next_at"`
}

// Summary counts the reports not delivered; Critical are the ones past
// their deadline.
type Summary struct {
	Pending  int
	Failed   int
	Critical int
	// NearestDeadlineS is the time to the nearest deadline of an
	// undelivered report (negative past it; 0 without one).
	NearestDeadlineS float64
}

// ErrNotFound is an alert or flight this USSP does not hold.
var ErrNotFound = errors.New("not found")

// Store is the occurrence reports of the relational database.
type Store interface {
	// AlertEvents are up to n alerts raised within lookback that are
	// occurrences by the rules of the package (an airprox within the
	// thresholds, a PROHIBITED zone, a lost link in U-space airspace)
	// and not yet reported.
	AlertEvents(ctx context.Context, lookback time.Duration, airproxHM, airproxVM float64, n int) ([]AlertEvent, error)
	// EmergencyFlights are up to n flights started within lookback that
	// declared an emergency and are not yet reported.
	EmergencyFlights(ctx context.Context, lookback time.Duration, n int) ([]FlightRef, error)
	// Alert is one alert of a flight with its flight (ErrNotFound).
	Alert(ctx context.Context, alertID string) (AlertEvent, error)
	// Flights are the flights of ids this USSP holds.
	Flights(ctx context.Context, ids []string) ([]FlightRef, error)
	// Now is the database clock.
	Now(ctx context.Context) (time.Time, error)
	// Enqueue stores r with deadline_at = became_aware_at + Deadline
	// (idempotent by source): false when it was reported before; the
	// report as stored either way.
	Enqueue(ctx context.Context, r Report) (Item, bool, error)

	Claim(ctx context.Context, n int, lease time.Duration) ([]Queued, error)
	Delivered(ctx context.Context, id, authorityRef string) error
	Retry(ctx context.Context, id, cause string, backoff time.Duration) error
	Fail(ctx context.Context, id, cause string) error

	Open(ctx context.Context, n int) ([]Item, bool, error)
	Summarise(ctx context.Context) (Summary, error)
	// Held is one page of the flights some report names (the
	// record_holds set): up to n ids greater than after ("" for the
	// first page), in ascending order.
	Held(ctx context.Context, after string, n int) ([]string, error)
}
