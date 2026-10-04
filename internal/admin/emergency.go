package admin

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/store"
	"github.com/rootxkit/uspace-ussp/internal/store/relational"
)

// The emergency workflow (spec 01 §3 S11): a communication checklist
// with timestamps for one flight. Nothing in it reaches an aircraft; the
// emergency contact is the intent's reference, which the authority
// resolves (no PII here).
const (
	EntityCase      = "emergency_case"
	EventCaseOpened = "emergency_case_opened"
	EventCaseNote   = "emergency_case_note"
	EventCaseClosed = "emergency_case_closed"
	// The actions of POST /v1/admin/emergency/{flight_id}.
	ActionOpen  = "open"
	ActionNote  = "note"
	ActionClose = "close"
	// MaxStep bounds a checklist step's name.
	MaxStep = 64
)

// Counters of the workflow.
const (
	CounterCaseOpened = "admin_emergency_opened"
	CounterCaseNote   = "admin_emergency_notes"
	CounterCaseClosed = "admin_emergency_closed"
)

// Step is one step of the checklist, ticked by the first note that
// names it.
type Step struct {
	Step   string     `json:"step"`
	DoneAt *time.Time `json:"done_at,omitempty"`
	DoneBy *string    `json:"done_by,omitempty"`
}

// Note is one timestamped note of a case.
type Note struct {
	At     time.Time `json:"at"`
	Author string    `json:"author"`
	Step   *string   `json:"step,omitempty"`
	Text   string    `json:"text"`
}

// Case is one emergency case.
type Case struct {
	CaseID              string     `json:"case_id"`
	FlightID            string     `json:"flight_id"`
	IntentID            *string    `json:"intent_id,omitempty"`
	AuthorisationNumber *string    `json:"authorisation_number,omitempty"`
	UASSerial           string     `json:"uas_serial"`
	OpenedAt            time.Time  `json:"opened_at"`
	OpenedBy            string     `json:"opened_by"`
	Reason              string     `json:"reason"`
	ContactRef          *string    `json:"contact_ref,omitempty"`
	ContactProcedure    string     `json:"contact_procedure"`
	Checklist           []Step     `json:"checklist"`
	Notes               []Note     `json:"notes"`
	ClosedAt            *time.Time `json:"closed_at,omitempty"`
	ClosedBy            *string    `json:"closed_by,omitempty"`
	Outcome             *string    `json:"outcome,omitempty"`
	RecordLink          string     `json:"record_link"`
}

// Cases is GET /v1/admin/emergency.
type Cases struct {
	Cases     []Case `json:"cases"`
	Truncated bool   `json:"truncated"`
}

// CaseAction is one step asked for.
type CaseAction struct {
	Action  string
	Reason  string
	Text    string
	Step    string
	Outcome string
}

// caseRow is the columns of a case read with its flight.
type caseRow struct {
	ID, FlightID                    string
	OpenedAt                        time.Time
	OpenedBy, Reason                string
	ClosedAt                        *time.Time
	ClosedBy, Outcome               *string
	IntentID                        string
	AuthorisationNumber, ContactRef *string
	UASSerial                       string
}

func (s *Service) recordLink(flightID string) string {
	if s.RecordLink != nil {
		return s.RecordLink(flightID)
	}
	return "/v1/records/flights/" + flightID
}

// build is the cases of rows with their notes and checklists.
func (s *Service) build(ctx context.Context, rows []caseRow) ([]Case, error) {
	pol := s.policy().Values
	ids := make([]string, 0, len(rows))
	actors := []string{}
	for i := range rows {
		r := &rows[i]
		ids = append(ids, r.ID)
		actors = append(actors, r.OpenedBy)
		if r.ClosedBy != nil {
			actors = append(actors, *r.ClosedBy)
		}
	}
	notes := map[string][]relational.AdminCaseNotesRow{}
	if len(ids) > 0 {
		us, err := store.UUIDs("case_id", ids)
		if err != nil {
			return nil, err
		}
		ns, err := s.Store.Queries().AdminCaseNotes(ctx, relational.AdminCaseNotesParams{CaseIds: us, MaxRows: MaxNotes})
		if err != nil {
			return nil, fmt.Errorf("case notes: %w", err)
		}
		for _, n := range ns {
			k := store.UUIDText(n.CaseID)
			notes[k] = append(notes[k], n)
			actors = append(actors, n.Author)
		}
	}
	names := s.staffNames(ctx, actors)
	out := make([]Case, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		id, flight := r.ID, r.FlightID
		c := Case{CaseID: id, FlightID: flight, IntentID: strp(r.IntentID), AuthorisationNumber: r.AuthorisationNumber,
			UASSerial: r.UASSerial, OpenedAt: r.OpenedAt.UTC(), OpenedBy: names(r.OpenedBy), Reason: r.Reason, ContactRef: r.ContactRef,
			ContactProcedure: pol.EmergencyContactProcedure, ClosedAt: utc(r.ClosedAt), Outcome: r.Outcome,
			RecordLink: s.recordLink(flight), Notes: []Note{}, Checklist: make([]Step, 0, len(pol.EmergencyChecklist))}
		if r.ClosedBy != nil {
			by := names(*r.ClosedBy)
			c.ClosedBy = &by
		}
		for _, step := range pol.EmergencyChecklist {
			c.Checklist = append(c.Checklist, Step{Step: step})
		}
		for _, n := range notes[id] {
			c.Notes = append(c.Notes, Note{At: n.At.UTC(), Author: names(n.Author), Step: n.Step, Text: n.Text})
			if n.Step == nil {
				continue
			}
			i := slices.IndexFunc(c.Checklist, func(st Step) bool { return st.Step == *n.Step })
			if i < 0 {
				// A step of an earlier checklist version stays shown.
				c.Checklist = append(c.Checklist, Step{Step: *n.Step})
				i = len(c.Checklist) - 1
			}
			if c.Checklist[i].DoneAt == nil {
				at, by := n.At.UTC(), names(n.Author)
				c.Checklist[i].DoneAt, c.Checklist[i].DoneBy = &at, &by
			}
		}
		out = append(out, c)
	}
	return out, nil
}

// Cases lists the open cases and those closed in the last ClosedCases,
// open first, newest first, at most MaxCases.
func (s *Service) Cases(ctx context.Context) (Cases, error) {
	rows, err := s.Store.Queries().AdminCases(ctx, relational.AdminCasesParams{ClosedS: ClosedCases.Seconds(), MaxRows: MaxCases + 1})
	if err != nil {
		return Cases{}, fmt.Errorf("emergency cases: %w", err)
	}
	out := Cases{}
	if len(rows) > MaxCases {
		rows, out.Truncated = rows[:MaxCases], true
	}
	cr := make([]caseRow, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		cr = append(cr, caseRow{ID: store.UUIDText(r.ID), FlightID: store.UUIDText(r.FlightID), OpenedAt: r.OpenedAt, OpenedBy: r.OpenedBy, Reason: r.Reason,
			ClosedAt: r.ClosedAt, ClosedBy: r.ClosedBy, Outcome: r.Outcome, IntentID: store.UUIDText(r.IntentID), AuthorisationNumber: r.AuthorisationNumber,
			ContactRef: r.EmergencyContactRef, UASSerial: r.UasSerial})
	}
	if out.Cases, err = s.build(ctx, cr); err != nil {
		return Cases{}, err
	}
	return out, nil
}

// Case is the flight's newest case; 404 when it has none.
func (s *Service) Case(ctx context.Context, flightID string) (Case, error) {
	u, err := store.UUID("flight_id", flightID)
	if err != nil {
		return Case{}, err
	}
	r, err := s.Store.Queries().AdminCaseOfFlight(ctx, u)
	if store.IsNoRows(err) {
		return Case{}, notFound("emergency case of this flight")
	}
	if err != nil {
		return Case{}, fmt.Errorf("emergency case: %w", err)
	}
	cs, err := s.build(ctx, []caseRow{{ID: store.UUIDText(r.ID), FlightID: store.UUIDText(r.FlightID), OpenedAt: r.OpenedAt, OpenedBy: r.OpenedBy,
		Reason: r.Reason, ClosedAt: r.ClosedAt, ClosedBy: r.ClosedBy, Outcome: r.Outcome, IntentID: store.UUIDText(r.IntentID), AuthorisationNumber: r.AuthorisationNumber,
		ContactRef: r.EmergencyContactRef, UASSerial: r.UasSerial}})
	if err != nil {
		return Case{}, err
	}
	return cs[0], nil
}

// validate checks an action before anything is written.
func (s *Service) validateAction(a CaseAction) error {
	switch a.Action {
	case ActionOpen:
		return reason("reason", a.Reason, MaxReason)
	case ActionNote:
		if err := reason("text", a.Text, MaxNote); err != nil {
			return err
		}
		if a.Step != "" {
			if len(a.Step) > MaxStep || !slices.Contains(s.policy().Values.EmergencyChecklist, a.Step) {
				return core.Fieldf("step", "not a step of the policy's emergency checklist")
			}
		}
		return nil
	case ActionClose:
		return reason("outcome", a.Outcome, MaxReason)
	}
	return core.Fieldf("action", "must be open, note or close")
}

// Act opens a case, adds a note or closes it, for one of this USSP's
// flights: validated first, then the write and its events row (the
// supervisor and the text) in one transaction on the database clock. It
// reports whether a case was opened (201). open while a case is open,
// and note or close without one, are 409 (permanent); a flight this
// USSP does not hold is 404.
func (s *Service) Act(ctx context.Context, staffID, flightID string, a CaseAction) (Case, bool, error) {
	u, err := store.UUID("flight_id", flightID)
	if err != nil {
		return Case{}, false, err
	}
	if err := s.validateAction(a); err != nil {
		return Case{}, false, err
	}
	err = s.Store.Tx(ctx, func(q *relational.Queries) error {
		f, err := q.AdminFlightForCase(ctx, u)
		if store.IsNoRows(err) {
			return notFound("flight")
		}
		if err != nil {
			return err
		}
		switch a.Action {
		case ActionOpen:
			id, err := q.AdminOpenCase(ctx, relational.AdminOpenCaseParams{FlightID: u, OpenedBy: staffID, Reason: a.Reason})
			if store.IsNoRows(err) {
				return conflict(SlugCaseOpen, "a case of this flight is open: add notes to it, or close it first")
			}
			if err != nil {
				return err
			}
			_, err = store.Audit(ctx, q, store.Event{ActorType: store.ActorStaff, ActorID: staffID, EntityType: EntityCase, EntityID: store.UUIDText(id),
				EventType: EventCaseOpened, Payload: map[string]any{"flight_id": flightID, "reason": a.Reason, "uas_serial": f.UasSerial,
					"contact_ref": f.EmergencyContactRef}})
			return err
		default:
			id, err := q.AdminOpenCaseOfFlight(ctx, u)
			if store.IsNoRows(err) {
				return conflict(SlugNoOpenCase, "no case of this flight is open: open one first")
			}
			if err != nil {
				return err
			}
			caseID := store.UUIDText(id)
			if a.Action == ActionNote {
				if _, err := q.AdminAddNote(ctx, relational.AdminAddNoteParams{CaseID: id, Author: staffID, Step: strp(a.Step), Text: a.Text}); err != nil {
					return err
				}
				_, err = store.Audit(ctx, q, store.Event{ActorType: store.ActorStaff, ActorID: staffID, EntityType: EntityCase, EntityID: caseID,
					EventType: EventCaseNote, Payload: map[string]any{"flight_id": flightID, "step": strp(a.Step), "text": a.Text}})
				return err
			}
			if _, err := q.AdminCloseCase(ctx, relational.AdminCloseCaseParams{ClosedBy: staffID, Outcome: a.Outcome, ID: id}); err != nil {
				return err
			}
			_, err = store.Audit(ctx, q, store.Event{ActorType: store.ActorStaff, ActorID: staffID, EntityType: EntityCase, EntityID: caseID,
				EventType: EventCaseClosed, Payload: map[string]any{"flight_id": flightID, "outcome": a.Outcome}})
			return err
		}
	})
	if err != nil {
		return Case{}, false, err
	}
	switch a.Action {
	case ActionOpen:
		s.count(CounterCaseOpened)
	case ActionNote:
		s.count(CounterCaseNote)
	default:
		s.count(CounterCaseClosed)
	}
	c, err := s.Case(ctx, flightID)
	return c, a.Action == ActionOpen, err
}
