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

// dateLayout is a UTC day as the API writes it.
const dateLayout = "2006-01-02"

// RecordDay is one built daily bundle.
type RecordDay struct {
	Date        string    `json:"date"`
	BuiltAt     time.Time `json:"built_at"`
	ContentHash string    `json:"content_hash"`
	Flights     int32     `json:"flights"`
}

// RecordDays is GET /v1/admin/records/days.
type RecordDays struct {
	Days    []RecordDay `json:"days"`
	Missing []string    `json:"missing"`
}

// RecordDays lists the bundles of the last RecordWindowDays UTC days and
// the days before today without one.
func (s *Service) RecordDays(ctx context.Context) (RecordDays, error) {
	today := s.now().UTC().Truncate(24 * time.Hour)
	first := today.AddDate(0, 0, -RecordWindowDays)
	q := s.Store.Queries()
	rows, err := q.AdminRecordDays(ctx, store.Date(first))
	if err != nil {
		return RecordDays{}, fmt.Errorf("record days: %w", err)
	}
	out := RecordDays{Days: make([]RecordDay, 0, len(rows)), Missing: []string{}}
	for i := range rows {
		r := &rows[i]
		out.Days = append(out.Days, RecordDay{Date: store.DateTime(r.Date).Format(dateLayout), BuiltAt: r.BuiltAt.UTC(), ContentHash: r.ContentHash, Flights: r.Flights})
	}
	missing, err := q.MissingRecordDays(ctx, relational.MissingRecordDaysParams{
		FirstDay: store.Date(first), LastDay: store.Date(today.AddDate(0, 0, -1))})
	if err != nil {
		return RecordDays{}, fmt.Errorf("days without a bundle: %w", err)
	}
	for _, d := range missing {
		out.Missing = append(out.Missing, store.DateTime(d).Format(dateLayout))
	}
	slices.Sort(out.Missing)
	slices.Reverse(out.Missing)
	return out, nil
}

// Event is one audit row.
type Event struct {
	ID         int64          `json:"id"`
	TS         time.Time      `json:"ts"`
	ActorType  string         `json:"actor_type"`
	ActorID    string         `json:"actor_id"`
	EntityType string         `json:"entity_type"`
	EntityID   *string        `json:"entity_id,omitempty"`
	EventType  string         `json:"event_type"`
	Payload    map[string]any `json:"payload"`
}

// Events is GET /v1/admin/events.
type Events struct {
	Events    []Event `json:"events"`
	Truncated bool    `json:"truncated"`
}

// EventEntities are the entity types whose rows the console may read:
// sign-ins and operator accounts are not among them.
var EventEntities = []string{EntityAlert, EntityCase, "policy", "source_control", "occurrence_report"}

// Events lists the events rows of one console entity, oldest first, at
// most limit (DefaultEvents when 0, at most MaxEvents).
func (s *Service) Events(ctx context.Context, entityType, entityID string, limit int) (Events, error) {
	if !slices.Contains(EventEntities, entityType) {
		return Events{}, core.Fieldf("entity_type", "not a console entity")
	}
	if entityID == "" || len(entityID) > 200 {
		return Events{}, core.Fieldf("entity_id", "1 to 200 bytes")
	}
	if limit == 0 {
		limit = DefaultEvents
	}
	if limit < 1 || limit > MaxEvents {
		return Events{}, core.Fieldf("limit", "from 1 to %d", MaxEvents)
	}
	rows, err := s.Store.Queries().AdminEvents(ctx, relational.AdminEventsParams{EntityType: entityType, EntityID: entityID, MaxRows: int32(limit + 1)})
	if err != nil {
		return Events{}, fmt.Errorf("events: %w", err)
	}
	out := Events{Events: make([]Event, 0, min(len(rows), limit))}
	if len(rows) > limit {
		rows, out.Truncated = rows[:limit], true
	}
	for i := range rows {
		r := &rows[i]
		out.Events = append(out.Events, Event{ID: r.ID, TS: r.Ts.UTC(), ActorType: r.ActorType, ActorID: r.ActorID, EntityType: r.EntityType,
			EntityID: r.EntityID, EventType: r.EventType, Payload: object(r.Payload)})
	}
	return out, nil
}
