// Package tsdbwriter is the tsdb-writer process (docs/PLAN.md §3.1, §5.2;
// spec 05 §5, §6): the only writer of the time-series database.
//
// One Pipeline per stream: a durable pull consumer tsdb-writer-<STREAM>
// over the whole of TRK (trk.v1, own flights' telemetry), MAN (man.v1,
// manned and e-conspicuity tracks), PEER (peer.v1, peer flights),
// TRAFFIC (sampled traffic products) and CONF (conformance outcomes),
// each with explicit ack and a bounded max_ack_pending. Rows are
// batched (at most 1000 rows or 1 s), copied with pgx.CopyFrom into a
// staging table and moved into the hypertable ON CONFLICT DO NOTHING on
// the (msg_id, time) unique index (store.TSWriter); a message is
// acknowledged only after its transaction commits (B-05), and a
// redelivered or republished message writes nothing twice (dedupe_hits,
// from the 10 s in-memory window or from the index).
//
// The queue holds whole messages: at most USSP_WRITER_QUEUE_S (10 s)
// while writes succeed, and at most USSP_WRITER_HOLD_ROWS (50 000) rows
// always, also while TimescaleDB is down (B-07). At either bound the
// consumer stops pulling and the stream holds the rest (spills); the
// hot path never waits, since it publishes to core NATS and the streams
// capture it. The writer never drops a message it was delivered. What
// the streams' limits or a purge remove before the writer reads it is a
// writer_gaps row (stream_removed), written in the transaction of the
// message after the hole and so before that message is acknowledged,
// counted in dropped_rows and logged at error level with the subject
// and the time range around it. Malformed messages and rows the database
// refuses are gaps too (malformed, rejected), never silent drops.
//
// Metrics: the counters (rows_written, dedupe_hits, dropped_rows, gaps,
// spills, ...), and per stream the queue depth in rows and seconds, the
// batch size and the write latency.
package tsdbwriter

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/app/proc"
	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/config"
	"github.com/rootxkit/uspace-ussp/internal/store"
)

// ConsumerPrefix names each stream's durable consumer.
const ConsumerPrefix = "tsdb-writer-"

// Options are what a test replaces.
type Options struct {
	// Streams overrides Streams (tests use streams of their own).
	Streams []Stream
	// Topology holds the streams' configurations (default
	// proc.TopologyOf(rt.Config)).
	Topology *bus.Topology
	// Config overrides the pipeline configuration built from the
	// process configuration.
	Config *Config
	// Pipelines, when set, receives the pipelines once they exist.
	Pipelines func([]*Pipeline)
}

// Spec declares the process and the dependencies it reads.
var Spec = SpecWith(Options{})

// SpecWith is the process with o.
func SpecWith(o Options) proc.Spec {
	return proc.Spec{
		Process:     config.ProcessTSDBWriter,
		TimescaleDB: proc.Optional,
		NATS:        proc.Required,
		Migrate:     true,
		Routes:      func(ctx context.Context, mux *http.ServeMux, rt *proc.Runtime) error { return routes(ctx, mux, rt, o) },
	}
}

// Run runs tsdb-writer with cfg until ctx ends.
func Run(ctx context.Context, cfg config.Config) error {
	return proc.Run(ctx, cfg, Spec, proc.Options{})
}

// PipelineConfig is the pipeline configuration of cfg.
func PipelineConfig(cfg config.Config) Config {
	c := DefaultConfig()
	c.QueueMaxAge = time.Duration(cfg.WriterQueueS) * time.Second
	c.HoldMaxRows = cfg.WriterHoldRows
	return c
}

func routes(ctx context.Context, mux *http.ServeMux, rt *proc.Runtime, o Options) error {
	h := proc.HealthHandlers{Health: rt.Health}
	mux.HandleFunc("GET /healthz", h.GetHealthz)
	mux.HandleFunc("GET /readyz", h.GetReadyz)

	counters := &core.Counters{}
	proc.Publish(rt, "tsdb_writer", counters)
	streams := o.Streams
	if streams == nil {
		streams = Streams
	}
	top := proc.TopologyOf(rt.Config)
	if o.Topology != nil {
		top = *o.Topology
	}
	pcfg := PipelineConfig(rt.Config)
	if o.Config != nil {
		pcfg = *o.Config
	}
	m := newMetrics()
	js := rt.Bus.JetStream()
	pipes := make([]*Pipeline, 0, len(streams))
	for _, s := range streams {
		src := &bus.StreamSource{MaxDeletedDetails: 100_000, Counters: counters, Open: bus.PullOpener(js, top, s.Name, bus.PullSpec{
			Durable: ConsumerPrefix + s.Name, FilterSubject: s.Subject,
			MaxAckPending: pcfg.HoldMaxRows + pcfg.FetchMax, AckWait: pcfg.AckWait,
		})}
		pipes = append(pipes, &Pipeline{
			Stream: s, Source: src, Store: store.TSWriter{Pool: rt.Store.TS}, Config: pcfg, Counters: counters,
			Logger: rt.Logger, Observer: m,
		})
	}
	m.pipes = pipes
	rt.Registry.MustRegister(m)
	if o.Pipelines != nil {
		o.Pipelines(pipes)
	}
	names := make([]string, len(streams))
	for i, s := range streams {
		names[i] = s.Name
	}
	rt.Logger.LogAttrs(ctx, slog.LevelInfo, "tsdb-writer consuming", slog.Any("streams", names),
		slog.Int("batch_max_rows", pcfg.BatchMaxRows), slog.Float64("queue_max_s", pcfg.QueueMaxAge.Seconds()),
		slog.Int("hold_max_rows", pcfg.HoldMaxRows))
	for _, p := range pipes {
		rt.Go(ctx, p.Run)
	}
	return nil
}

// metrics are the writer's own Prometheus series beside the counters:
// the queue depth per stream (rows and age), the batch size and the
// write latency.
type metrics struct {
	pipes     []*Pipeline
	queueRows *prometheus.Desc
	queueAge  *prometheus.Desc
	batchRows *prometheus.HistogramVec
	latency   *prometheus.HistogramVec
}

func newMetrics() *metrics {
	return &metrics{
		queueRows: prometheus.NewDesc("ussp_tsdb_writer_queue_rows", "rows held in the writer's memory", []string{"stream"}, nil),
		queueAge:  prometheus.NewDesc("ussp_tsdb_writer_queue_age_seconds", "age of the oldest held message", []string{"stream"}, nil),
		batchRows: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "ussp_tsdb_writer_batch_rows", Help: "rows per committed batch",
			Buckets: []float64{1, 10, 50, 100, 250, 500, 1000},
		}, []string{"stream"}),
		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "ussp_tsdb_writer_write_seconds", Help: "time to commit one batch",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}, []string{"stream"}),
	}
}

// ObserveBatch implements Observer.
func (m *metrics) ObserveBatch(stream string, rows int, latency time.Duration) {
	m.batchRows.WithLabelValues(stream).Observe(float64(rows))
	m.latency.WithLabelValues(stream).Observe(latency.Seconds())
}

// Describe implements prometheus.Collector.
func (m *metrics) Describe(ch chan<- *prometheus.Desc) {
	ch <- m.queueRows
	ch <- m.queueAge
	m.batchRows.Describe(ch)
	m.latency.Describe(ch)
}

// Collect implements prometheus.Collector.
func (m *metrics) Collect(ch chan<- prometheus.Metric) {
	for _, p := range m.pipes {
		s := p.Snapshot()
		ch <- prometheus.MustNewConstMetric(m.queueRows, prometheus.GaugeValue, float64(s.QueueRows), s.Stream)
		ch <- prometheus.MustNewConstMetric(m.queueAge, prometheus.GaugeValue, s.QueueAgeS, s.Stream)
	}
	m.batchRows.Collect(ch)
	m.latency.Collect(ch)
}
