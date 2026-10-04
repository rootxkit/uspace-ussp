package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/app/proc"
	"github.com/rootxkit/uspace-ussp/internal/national"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/records"
	recstore "github.com/rootxkit/uspace-ussp/internal/records/pgstore"
)

// SubjectGaps is the subject of telemetry-ingest's client statuses, the
// gap records among them (src.v1.operator_ws.<client>).
const SubjectGaps = "src.v1.operator_ws.>"

// retentionEvery is how often api's own retention job runs.
const retentionEvery = time.Hour

// startRecords runs the service records (WP-15, internal/records): the
// gap records of telemetry-ingest kept from src.v1, the daily bundles
// into USSP_RECORDS_DIR with records on /readyz, the purge of the gap
// records past the telemetry retention, and the routes' builder. Without
// TimescaleDB a record says its telemetry and traffic products are
// unavailable.
func startRecords(ctx context.Context, rt *proc.Runtime, pol *policy.Service) *national.Records {
	cfg := rt.Config
	counters := &core.Counters{}
	proc.Publish(rt, "records", counters)
	logger := rt.Logger.With("component", "records")
	reader := recstore.Reader{S: rt.Store}
	b := &records.Builder{Reader: reader, USSPID: cfg.SystemID, Policy: func() policy.Record {
		if r, ok := pol.Current(); ok {
			return r
		}
		return policy.Record{Values: policy.Defaults()}
	}}
	if rt.Store.TS != nil {
		b.Series = recstore.Series{S: rt.Store}
	}
	gaps := records.NewGapRecorder(reader, counters, logger)
	rt.Go(ctx, gaps.Run)
	rt.Go(ctx, func(ctx context.Context) { listenGaps(ctx, rt, gaps.Take, logger) })
	daily := &records.Daily{Builder: b, Store: reader, Dir: cfg.RecordsDir, Counters: counters, Logger: logger}
	rt.Health.Register(records.DepRecords, false, daily.Probe())
	rt.Go(ctx, func(ctx context.Context) { daily.Run(ctx, time.Minute) })
	rt.Go(ctx, func(ctx context.Context) { purgeGaps(ctx, reader, pol, counters, logger) })
	return &national.Records{Builder: b, Daily: daily, Audit: reader}
}

// listenGaps keeps the subscription to the gap records open, retrying
// while NATS is down (B-08).
func listenGaps(ctx context.Context, rt *proc.Runtime, take func(string, []byte), logger *slog.Logger) {
	for ctx.Err() == nil {
		stop, err := rt.Bus.Listen(SubjectGaps, take)
		if err != nil {
			logger.LogAttrs(ctx, slog.LevelWarn, "gap record subscription not open; retried", obs.Err(err))
			t := time.NewTimer(2 * time.Second)
			select {
			case <-ctx.Done():
			case <-t.C:
			}
			t.Stop()
			continue
		}
		<-ctx.Done()
		stop()
	}
}

// purgeGaps deletes the gap records older than the telemetry they
// explain (telemetry_retention_days), every hour, logging and counting
// what it removed.
func purgeGaps(ctx context.Context, reader recstore.Reader, pol *policy.Service, counters *core.Counters, logger *slog.Logger) {
	t := time.NewTicker(retentionEvery)
	defer t.Stop()
	for {
		days := policy.Defaults().TelemetryRetentionDays
		if r, ok := pol.Current(); ok {
			days = r.Values.TelemetryRetentionDays
		}
		n, err := reader.PurgeGaps(ctx, days)
		switch {
		case err != nil && ctx.Err() == nil:
			logger.LogAttrs(ctx, slog.LevelWarn, "gap record purge failed; tried again", obs.Err(err))
		case err == nil:
			counters.Add("record_gaps_purged", uint64(max(n, 0)))
			logger.LogAttrs(ctx, slog.LevelInfo, "retention: gap records purged", slog.Int64("removed", n), slog.Int("older_than_days", days))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
