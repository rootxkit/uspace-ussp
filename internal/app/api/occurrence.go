package api

import (
	"context"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/app/proc"
	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/occurrence"
	occstore "github.com/rootxkit/uspace-ussp/internal/occurrence/pgstore"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/records"
)

// startOccurrences runs the occurrence reports (WP-15,
// internal/occurrence): the detection of airprox, nonconformance in a
// PROHIBITED zone, lost link in U-space airspace and emergencies from
// the recorded alerts and flights, the queue, the holds of the flights
// they name in record_holds (written after each report's commit), the
// delivery of each report after its commit to the authority's POST
// /v1/occurrences (USSP_AUTHORITY_BASE_URL, a token of scope
// occurrences.write whose aud is the authority's host), and occurrences
// on /readyz. Without an authority or a token client nothing delivers:
// the reports stay queued and listed with their deadlines, and /readyz
// says why.
func startOccurrences(ctx context.Context, rt *proc.Runtime, current func() policy.Values, kv *bus.Projector, tokens *auth.Outgoing) (*occurrence.Service, error) {
	cfg := rt.Config
	counters := &core.Counters{}
	proc.Publish(rt, "occurrences", counters)
	st := occstore.Store{S: rt.Store}
	svc := &occurrence.Service{Store: st, Holds: holdProjector{kv}, Policy: current, SystemID: cfg.SystemID, RecordsURL: cfg.USSBaseURL,
		MaxAttempts: cfg.OccurrenceMaxAttempts, BackoffMax: time.Duration(cfg.OccurrenceBackoffMaxS) * time.Second,
		Counters: counters, Logger: rt.Logger.With("component", "occurrence")}
	if cfg.AuthorityBaseURL != "" && tokens != nil {
		c, err := occurrence.NewClient(cfg.AuthorityBaseURL, tokens, nil)
		if err != nil {
			return nil, err
		}
		svc.Deliverer = c
	}
	rt.Health.Register(occurrence.DepOccurrences, false, occurrence.Probe(st, svc.Deliverer != nil))
	rt.Go(ctx, func(ctx context.Context) { svc.Run(ctx, occurrence.DefaultEvery) })
	return svc, nil
}

// holdProjector writes record_holds (occurrence.HoldProjector).
type holdProjector struct{ kv *bus.Projector }

// Hold implements occurrence.HoldProjector.
func (h holdProjector) Hold(ctx context.Context, flightID string, reasons []string, since time.Time) error {
	return h.kv.PutJSON(ctx, bus.BucketRecordHolds, bus.KeyToken(flightID), records.Hold{FlightID: flightID, Reasons: reasons, Since: since.UTC()})
}

// deliveryOf is why svc sends no report ("" when it does).
func deliveryOf(svc *occurrence.Service) string {
	if svc.Deliverer != nil {
		return ""
	}
	return occurrence.NotDelivering()
}
