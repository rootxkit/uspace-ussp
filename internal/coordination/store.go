package coordination

import (
	"context"
	"time"
)

// Transition is a conformance state that entered nonconforming,
// lost_link or contingent from another kind of state, with the intent it
// is about. Only flights of an authorised intent are transitions: an
// intent outside 2021/664's scope (Art. 1(3)) has no authorisation
// number for a notice to name.
type Transition struct {
	Deviation
	Intent Intent
}

// Candidate is an intent to judge: activated (or later), without a
// coordination check yet.
type Candidate struct {
	Intent
	// AirspaceIDs are the U-space airspaces its decision named.
	AirspaceIDs []string
	// Ended is true when the intent has ended already: both notices are
	// owed if it is in controlled airspace.
	Ended bool
}

// Check is the judgement of one intent against cis_current: whether its
// U-space airspaces are in controlled airspace (or not said, or no
// longer held: counted as controlled), which ones, and on which CIS
// version.
type Check struct {
	Controlled  bool
	AirspaceIDs []string
	UnstatedIDs []string
	CISVersion  string
}

// Notice is one notice to queue.
type Notice struct {
	Ref      string
	Kind     Kind
	IntentID string
	FlightID string
	// StateID is the conformance state of a deviation notice (0: none).
	StateID int64
	// Body is the exact coordination/annex_v/v1 bytes every try posts;
	// nil with FailReason for a notice that could not be built (queued
	// failed, so the console shows it).
	Body       []byte
	FailReason string
}

// Queued is a notice claimed for one try.
type Queued struct {
	ID       int64
	Ref      string
	Kind     Kind
	IntentID string
	StateID  int64
	Body     []byte
	Attempts int
}

// Receipt is the ANSP's answer to a notice (202, or 200 for a repeat).
type Receipt struct {
	AckID      string
	ReceivedAt time.Time
	Repeat     bool
}

// Polled is a received notice whose acknowledgement is due to be read.
type Polled struct {
	ID         int64
	Ref        string
	Kind       Kind
	AckID      string
	StateID    int64
	State      string
	ReceivedAt time.Time
	// SinceReceived is the time since the receipt on the database clock.
	SinceReceived time.Duration
	Polls         int
}

// NoticeState is a notice as the ANSP holds it.
type NoticeState struct {
	State          string
	AcknowledgedAt *time.Time
	AcknowledgedBy string
}

// Item is a notice as the console lists it.
type Item struct {
	ID         int64      `json:"id"`
	NoticeRef  string     `json:"notice_ref"`
	Kind       string     `json:"kind"`
	IntentID   string     `json:"intent_id"`
	FlightID   *string    `json:"flight_id"`
	State      string     `json:"state"`
	CreatedAt  time.Time  `json:"created_at"`
	AgeS       float64    `json:"age_s"`
	Attempts   int        `json:"attempts"`
	LastError  *string    `json:"last_error"`
	AckID      *string    `json:"ack_id"`
	ReceivedAt *time.Time `json:"received_at"`
	// EscalatedAt and FailedAt are set for an escalated and a failed
	// notice; NextAt is the next try of a pending one.
	EscalatedAt *time.Time `json:"escalated_at"`
	FailedAt    *time.Time `json:"failed_at"`
	NextAt      *time.Time `json:"next_at"`
}

// Summary is the count of open notices by state, with the age of the
// oldest pending one (seconds, on the database clock); Retrying are the
// pending ones a try already failed for.
type Summary struct {
	Pending           int
	Retrying          int
	OldestPendingAgeS float64
	Escalated         int
	Failed            int
}

// Store is the coordination tables of the relational database.
type Store interface {
	// Transitions are up to n conformance states recorded within lookback
	// that enter a deviation kind and have no notice yet, oldest first.
	Transitions(ctx context.Context, lookback time.Duration, n int) ([]Transition, error)
	// Candidates are up to n intents to check (Candidate), the ended ones
	// only within lookback of their end.
	Candidates(ctx context.Context, lookback time.Duration, n int) ([]Candidate, error)
	// Ended are up to n ended intents judged controlled whose ended
	// notice is not queued yet.
	Ended(ctx context.Context, n int) ([]Intent, error)
	// Enqueue queues the notices in one transaction (idempotent by
	// notice_ref: one already queued is left as it is), with the check
	// of intentID when check is not nil; queued counts the new rows.
	Enqueue(ctx context.Context, intentID string, check *Check, ns []Notice) (queued int, err error)

	// Claim leases up to n due pending notices for lease, at most one per
	// intent and only an intent's oldest pending notice, and counts a try.
	Claim(ctx context.Context, n int, lease time.Duration) ([]Queued, error)
	// Received stores the receipt and ats_notified_at on the notice's
	// conformance state; pollAfter is when its acknowledgement is first
	// read (0: never, an informational notice).
	Received(ctx context.Context, id int64, r Receipt, pollAfter time.Duration) error
	// Retry makes a pending notice due again after backoff, with cause.
	Retry(ctx context.Context, id int64, cause string, backoff time.Duration) error
	// Fail fails a notice for good with cause.
	Fail(ctx context.Context, id int64, cause string) error

	// DuePolls are up to n received or escalated notices whose
	// acknowledgement is due to be read.
	DuePolls(ctx context.Context, n int) ([]Polled, error)
	// Polled records one read that found no acknowledgement; next is when
	// to read again (0: stop polling).
	Polled(ctx context.Context, id int64, next time.Duration) error
	// Acknowledged stores the acknowledgement and ats_ack_ref on the
	// notice's conformance state.
	Acknowledged(ctx context.Context, id int64, s NoticeState) error
	// Escalate moves every received notice that needs an acknowledgement
	// and has waited longer than after to escalated, and returns them.
	Escalate(ctx context.Context, after time.Duration) ([]Item, error)

	// Open are up to n notices the console must see (pending, escalated,
	// failed), oldest first; truncated when there were more.
	Open(ctx context.Context, n int) (items []Item, truncated bool, err error)
	// Summarise counts the open notices.
	Summarise(ctx context.Context) (Summary, error)
}
