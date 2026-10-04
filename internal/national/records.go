package national

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/records"
)

// ScopeRecords is the scope of the service-record reads (02 F7; the
// authority's token).
const ScopeRecords = "ussp.records"

// Records serves /v1/records (internal/records).
type Records struct {
	Builder interface {
		Flight(ctx context.Context, flightID string) (records.Record, error)
	}
	Daily interface {
		Open(ctx context.Context, day time.Time) (records.Bundle, *os.File, error)
	}
	// Audit writes the events row of a read, before the body is sent.
	Audit interface {
		AuditRead(ctx context.Context, actorID, entity, entityID string) error
	}
}

func recordsUnavailable(w http.ResponseWriter, r *http.Request, detail string) {
	httpx.NewProblem(http.StatusServiceUnavailable, "records_unavailable", "", detail).Write(w, r)
}

// audit records the read by the token's subject; false (503 written)
// when it cannot be: nothing is served unaudited.
func (s *Server) auditRead(w http.ResponseWriter, r *http.Request, entity, id string) bool {
	if err := s.Records.Audit.AuditRead(r.Context(), principal(r).Claims.Subject, entity, id); err != nil {
		obs.Error(r.Context(), s.logger(), "record read not audited; not served", err)
		recordsUnavailable(w, r, "the read cannot be audited")
		return false
	}
	return true
}

// GetFlightRecord is GET /v1/records/flights/{flight_id}: one flight's
// service record, audited with the caller.
func (s *Server) GetFlightRecord(w http.ResponseWriter, r *http.Request, flightID openapi_types.UUID) {
	if s.Records == nil || s.Records.Builder == nil || s.Records.Audit == nil {
		recordsUnavailable(w, r, "the service records are not configured on this process")
		return
	}
	rec, err := s.Records.Builder.Flight(r.Context(), flightID.String())
	switch {
	case errors.Is(err, records.ErrNotFound):
		httpx.NewProblem(http.StatusNotFound, "record_not_found", "", "this USSP holds no flight with this id").Write(w, r)
		return
	case err != nil:
		obs.Error(r.Context(), s.logger(), "flight record not built", err)
		recordsUnavailable(w, r, "the flight cannot be read")
		return
	}
	if !s.auditRead(w, r, "flight", rec.FlightID) {
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// GetDailyRecords is GET /v1/records/daily/{date}: the day's bundle,
// its bytes verified against the recorded hash first, audited with the
// caller.
func (s *Server) GetDailyRecords(w http.ResponseWriter, r *http.Request, date openapi_types.Date) {
	if s.Records == nil || s.Records.Daily == nil || s.Records.Audit == nil {
		recordsUnavailable(w, r, "the daily record bundles are not configured on this process")
		return
	}
	day := date.Time
	b, f, err := s.Records.Daily.Open(r.Context(), day)
	switch {
	case errors.Is(err, records.ErrNotFound):
		httpx.NewProblem(http.StatusNotFound, "record_bundle_not_found", "", "no bundle is built for this day").Write(w, r)
		return
	case errors.Is(err, records.ErrBundleCorrupt):
		obs.Error(r.Context(), s.logger(), "daily record bundle refused", err)
		httpx.NewProblem(http.StatusInternalServerError, "record_bundle_corrupt", "", "the stored bundle does not match its recorded hash").Write(w, r)
		return
	case err != nil:
		obs.Error(r.Context(), s.logger(), "daily record bundle not opened", err)
		recordsUnavailable(w, r, "the bundle cannot be read")
		return
	}
	defer func() { _ = f.Close() }()
	if !s.auditRead(w, r, "record_bundle", b.Date.Format(time.DateOnly)) {
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/gzip")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-SHA256", b.Hash)
	h.Set("X-Record-Flights", strconv.Itoa(b.Flights))
	h.Set("Content-Disposition", `attachment; filename="`+b.Date.Format(time.DateOnly)+`.jsonl.gz"`)
	if st, err := f.Stat(); err == nil {
		h.Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	}
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, f); err != nil {
		s.logger().LogAttrs(r.Context(), slog.LevelWarn, "daily record bundle cut short while sent", obs.Err(err))
	}
}
