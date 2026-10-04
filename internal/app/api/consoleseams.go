package api

import (
	"context"
	"errors"
	"os"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/rootxkit/uspace-ussp/internal/coordination"
	"github.com/rootxkit/uspace-ussp/internal/national"
	"github.com/rootxkit/uspace-ussp/internal/national/gen"
	"github.com/rootxkit/uspace-ussp/internal/occurrence"
	"github.com/rootxkit/uspace-ussp/internal/records"
	"github.com/rootxkit/uspace-ussp/internal/status"
)

// The seams between the national routes and the WP-15 services. PLAN §4
// puts internal/national in the same layer as internal/coordination,
// internal/records, internal/occurrence and internal/status, so national
// declares its seams in the contract's generated types and this process
// converts the services' answers and maps their errors onto
// national.RefusedError, national.ErrRecordNotFound and
// national.ErrBundleCorrupt.

func uuidOf(s string) (openapi_types.UUID, error) {
	var u openapi_types.UUID
	err := u.UnmarshalText([]byte(s))
	return u, err
}

// coordinationList is national.CoordinationLister over the notice store.
type coordinationList struct {
	Store interface {
		Open(ctx context.Context, n int) ([]coordination.Item, bool, error)
	}
}

// Open lists the open notices, at most coordination.MaxListed.
func (c coordinationList) Open(ctx context.Context) (gen.CoordinationNotices, error) {
	items, truncated, err := c.Store.Open(ctx, coordination.MaxListed)
	if err != nil {
		return gen.CoordinationNotices{}, err
	}
	out := gen.CoordinationNotices{Notices: make([]gen.CoordinationNoticeItem, 0, len(items)), Truncated: truncated}
	for i := range items {
		it := &items[i]
		g := gen.CoordinationNoticeItem{Id: it.ID, NoticeRef: it.NoticeRef, Kind: gen.CoordinationNoticeItemKind(it.Kind),
			State: gen.CoordinationNoticeItemState(it.State), CreatedAt: it.CreatedAt, AgeS: it.AgeS, Attempts: it.Attempts,
			LastError: it.LastError, AckId: it.AckID, ReceivedAt: it.ReceivedAt, EscalatedAt: it.EscalatedAt, FailedAt: it.FailedAt, NextAt: it.NextAt}
		if g.IntentId, err = uuidOf(it.IntentID); err != nil {
			return gen.CoordinationNotices{}, err
		}
		if it.FlightID != nil {
			f, err := uuidOf(*it.FlightID)
			if err != nil {
				return gen.CoordinationNotices{}, err
			}
			g.FlightId = &f
		}
		out.Notices = append(out.Notices, g)
	}
	return out, nil
}

func reportItem(it *occurrence.Item) (gen.OccurrenceReportItem, error) {
	g := gen.OccurrenceReportItem{ReportRef: it.ReportRef, Kind: gen.OccurrenceReportItemKind(it.Kind), State: gen.OccurrenceReportItemState(it.State),
		Channel: gen.OccurrenceReportItemChannel(it.Channel), FlaggedBy: gen.OccurrenceReportItemFlaggedBy(it.FlaggedBy), BecameAwareAt: it.BecameAwareAt,
		DeadlineAt: it.DeadlineAt, TimeToDeadlineS: it.TimeToDeadlineS, Critical: it.Critical, Attempts: it.Attempts, LastError: it.LastError,
		SubmittedAt: it.SubmittedAt, AuthorityRef: it.AuthorityRef, FailedAt: it.FailedAt, NextAt: it.NextAt, FlightIds: make([]openapi_types.UUID, 0, len(it.FlightIDs))}
	for _, f := range it.FlightIDs {
		u, err := uuidOf(f)
		if err != nil {
			return g, err
		}
		g.FlightIds = append(g.FlightIds, u)
	}
	return g, nil
}

// occurrenceFlags is the Service seam of national.Occurrences.
type occurrenceFlags struct {
	Service interface {
		Flag(ctx context.Context, staffID, alertID, kind, narrative string) (occurrence.Item, bool, error)
	}
}

// Flag queues a supervisor's report, its refusal as national.RefusedError.
func (o occurrenceFlags) Flag(ctx context.Context, staffID, alertID, kind, narrative string) (gen.OccurrenceReportItem, bool, error) {
	it, created, err := o.Service.Flag(ctx, staffID, alertID, kind, narrative)
	if fe := (*occurrence.FlagError)(nil); errors.As(err, &fe) {
		return gen.OccurrenceReportItem{}, false, &national.RefusedError{Reason: fe.Reason, NotFound: fe.NotFound}
	}
	if err != nil {
		return gen.OccurrenceReportItem{}, false, err
	}
	g, err := reportItem(&it)
	return g, created, err
}

// occurrenceList is the List seam of national.Occurrences.
type occurrenceList struct {
	Store interface {
		Open(ctx context.Context, n int) ([]occurrence.Item, bool, error)
	}
}

// Open lists the open reports, at most occurrence.MaxListed.
func (l occurrenceList) Open(ctx context.Context) ([]gen.OccurrenceReportItem, bool, error) {
	items, truncated, err := l.Store.Open(ctx, occurrence.MaxListed)
	if err != nil {
		return nil, false, err
	}
	out := make([]gen.OccurrenceReportItem, 0, len(items))
	for i := range items {
		g, err := reportItem(&items[i])
		if err != nil {
			return nil, false, err
		}
		out = append(out, g)
	}
	return out, truncated, nil
}

// occurrencesAPI wires national.Occurrences over the service.
func occurrencesAPI(svc *occurrence.Service) *national.Occurrences {
	return &national.Occurrences{Service: occurrenceFlags{Service: svc}, List: occurrenceList{Store: svc.Store},
		Delivery: deliveryOf(svc), MaxNarrative: occurrence.MaxNarrative}
}

func statusNotice(n *status.Notice) gen.StatusNotice {
	return gen.StatusNotice{Kind: gen.StatusNoticeKind(n.Kind), At: n.At, CertificateId: n.CertificateID, Reference: n.Reference,
		RequestedBy: n.RequestedBy, State: gen.StatusNoticeState(n.State), Attempts: n.Attempts, NextAt: n.NextAt, LastError: n.LastError,
		SubmittedAt: n.SubmittedAt, AuthorityRef: n.AuthorityRef, FailedAt: n.FailedAt, CreatedAt: n.CreatedAt}
}

// statusNotices is national.StatusNotices over the status service.
type statusNotices struct {
	Service interface {
		Request(ctx context.Context, staffID, kind string) (status.Notice, bool, error)
		List(ctx context.Context) ([]status.Notice, error)
	}
}

// Request stores a notice, a kind not admitted as national.RefusedError.
func (s statusNotices) Request(ctx context.Context, staffID, kind string) (gen.StatusNotice, bool, error) {
	n, created, err := s.Service.Request(ctx, staffID, kind)
	if re := (*status.RequestError)(nil); errors.As(err, &re) {
		return gen.StatusNotice{}, false, &national.RefusedError{Reason: re.Reason}
	}
	if err != nil {
		return gen.StatusNotice{}, false, err
	}
	return statusNotice(&n), created, nil
}

// List lists the stored notices.
func (s statusNotices) List(ctx context.Context) ([]gen.StatusNotice, error) {
	ns, err := s.Service.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]gen.StatusNotice, 0, len(ns))
	for i := range ns {
		out = append(out, statusNotice(&ns[i]))
	}
	return out, nil
}

// recordErr maps internal/records' errors onto the route's.
func recordErr(err error) error {
	switch {
	case errors.Is(err, records.ErrNotFound):
		return errors.Join(national.ErrRecordNotFound, err)
	case errors.Is(err, records.ErrBundleCorrupt):
		return errors.Join(national.ErrBundleCorrupt, err)
	}
	return err
}

// flightRecords is the Builder seam of national.Records.
type flightRecords struct {
	Builder interface {
		Flight(ctx context.Context, flightID string) (records.Record, error)
	}
}

// Flight builds one flight's record.
func (f flightRecords) Flight(ctx context.Context, flightID string) (any, error) {
	rec, err := f.Builder.Flight(ctx, flightID)
	if err != nil {
		return nil, recordErr(err)
	}
	return rec, nil
}

// dailyRecords is the Daily seam of national.Records.
type dailyRecords struct {
	Daily interface {
		Open(ctx context.Context, day time.Time) (records.Bundle, *os.File, error)
	}
}

// Open opens a day's bundle, its bytes checked against the hash.
func (d dailyRecords) Open(ctx context.Context, day time.Time) (national.RecordBundle, *os.File, error) {
	b, f, err := d.Daily.Open(ctx, day)
	if err != nil {
		return national.RecordBundle{}, nil, recordErr(err)
	}
	return national.RecordBundle{Date: b.Date, Hash: b.Hash, Flights: b.Flights}, f, nil
}
