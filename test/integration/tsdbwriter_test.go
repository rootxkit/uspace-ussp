//go:build integration

package integration

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/app/tsdbwriter"
	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/bus/bustest"
	"github.com/rootxkit/uspace-ussp/internal/national/client"
	"github.com/rootxkit/uspace-ussp/internal/store"
)

var streamSeq atomic.Int64

// writerRig is tsdb-writer running against the real TimescaleDB and
// NATS on a stream of its own (so a test never reads another test's
// messages), with every telemetry row it writes under one flight id.
type writerRig struct {
	t        *testing.T
	stream   string
	subject  string
	flight   string
	nc       *bus.Conn
	pipes    []*tsdbwriter.Pipeline
	api      *client.ClientWithResponses
	base     string
	tsProxy  *proxy
	natsProx *proxy
}

type writerOpts struct {
	maxMsgs  int64
	holdRows int
	queueS   time.Duration
	// dedupeWindow overrides the in-memory window (1 ns: the index only).
	dedupeWindow time.Duration
	// natsDown starts the writer with NATS unreachable (a stopped proxy).
	natsDown bool
}

func newWriterRig(t *testing.T, o writerOpts) *writerRig {
	t.Helper()
	ensureSchemas(t)
	n := streamSeq.Add(1)
	r := &writerRig{t: t, stream: fmt.Sprintf("WP6T%d%d", time.Now().UnixNano()%1e8, n), flight: newFlightID()}
	r.subject = strings.ToLower(r.stream) + ".trk"
	r.nc = busConn(t, mustEnv(t, "USSP_TEST_NATS_URL"), true)
	top := bustest.TrackStream(t, r.nc, r.stream, r.subject, o.maxMsgs)
	pc := tsdbwriter.DefaultConfig()
	if o.holdRows > 0 {
		pc.HoldMaxRows = o.holdRows
	}
	if o.queueS > 0 {
		pc.QueueMaxAge = o.queueS
	}
	pc.RetryMax = time.Second
	if o.dedupeWindow > 0 {
		pc.DedupeWindow = o.dedupeWindow
	}
	tsURL, err := url.Parse(mustEnv(t, "USSP_TEST_TS_OWNER_URL"))
	if err != nil {
		t.Fatal(err)
	}
	r.tsProxy = newProxy(t, tsURL.Host)
	tsURL.Host = r.tsProxy.addr
	q := tsURL.Query()
	q.Set("connect_timeout", "2")
	tsURL.RawQuery = q.Encode()
	natsURL, err := url.Parse(mustEnv(t, "USSP_TEST_NATS_URL"))
	if err != nil {
		t.Fatal(err)
	}
	r.natsProx = newProxy(t, natsURL.Host)
	natsURL.Host = r.natsProx.addr
	if o.natsDown {
		r.natsProx.stop()
	}
	got := make(chan []*tsdbwriter.Pipeline, 1)
	spec := tsdbwriter.SpecWith(tsdbwriter.Options{
		Streams:   []tsdbwriter.Stream{{Name: r.stream, Subject: r.subject + ".>", Decode: tsdbwriter.DecodeTelemetry, Tables: []*store.CopyTable{&store.TableTelemetry}}},
		Topology:  &top,
		Config:    &pc,
		Pipelines: func(p []*tsdbwriter.Pipeline) { got <- p },
	})
	r.api = run(t, spec, map[string]string{
		"USSP_TSDB_WRITER_ADDR": "127.0.0.1:0",
		"USSP_TS_URL":           tsURL.String(),
		"USSP_NATS_URL":         natsURL.String(),
	})
	r.pipes = <-got
	r.base = r.api.ClientInterface.(*client.Client).Server
	return r
}

func newFlightID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// track is a track/telemetry/v1 message of the rig's flight.
func (r *writerRig) track(at time.Time) []byte {
	m := struct {
		bus.Envelope
		Body map[string]any `json:"body"`
	}{bus.NewEnvelope("track/telemetry/v1", "ussp/telemetry-ingest", core.Times{TS: &at, RxTS: at, CapturedAt: at, Source: core.TimeSourceClock}),
		map[string]any{
			"track_id": "trk-1", "trust": "authenticated", "source": "operator_ws", "source_instance": "client-1",
			"position": map[string]any{"lat": 41.7151, "lng": 44.8271}, "alt_wgs84_m": 650.0, "alt_amsl_m": 630.0, "alt_source": "geodetic",
			"alt_pressure_m": nil, "height_m": nil, "height_ref": nil, "speed_ms": 12.5, "track_deg": 90.0, "vspeed_ms": 0.0,
			"accuracy_h_m": 3.0, "accuracy_v_m": 4.0, "status": "Airborne", "emergency": false, "identification": map[string]any{},
			"flight_id": r.flight, "intent_id": nil,
		}}
	data, err := json.Marshal(m)
	if err != nil {
		r.t.Fatal(err)
	}
	return data
}

// publish sends msgs on core NATS, as telemetry-ingest does; the stream
// captures them.
func (r *writerRig) publish(msgs [][]byte) {
	for _, m := range msgs {
		if err := r.nc.Publish(r.subject+".c5:1317:2248.trk-1", m); err != nil {
			r.t.Fatal(err)
		}
	}
	if err := r.nc.Flush(); err != nil {
		r.t.Fatal(err)
	}
}

func (r *writerRig) rows() int64 {
	return count(r.t, tsOwner(r.t), "SELECT count(*) FROM telemetry WHERE flight_id = $1", r.flight)
}

func (r *writerRig) counter(name string) uint64 { return r.pipes[0].Counters.Get(name) }

func (r *writerRig) waitRows(want int64, within time.Duration) {
	r.t.Helper()
	deadline := time.Now().Add(within)
	for r.rows() != want {
		if time.Now().After(deadline) {
			r.t.Fatalf("%d rows, want %d; counters %v", r.rows(), want, r.pipes[0].Counters.Snapshot())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// metric reads one series of the writer's /metrics.
func (r *writerRig) metric(name string) string {
	r.t.Helper()
	resp, err := http.Get(r.base + "/metrics")
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), name+"{") && strings.Contains(sc.Text(), r.stream) {
			return sc.Text()
		}
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return ""
}

// B-05 both ways: 1000 trk messages are 1000 telemetry rows; the same
// 1000 republished write no new row and are 1000 dedupe hits.
func TestIntegrationWriterThousandThenRepublished(t *testing.T) {
	r := newWriterRig(t, writerOpts{})
	start := time.Now().Add(-time.Minute)
	msgs := make([][]byte, 1000)
	for i := range msgs {
		msgs[i] = r.track(start.Add(time.Duration(i) * 50 * time.Millisecond))
	}
	r.publish(msgs)
	r.waitRows(1000, 30*time.Second)
	t.Logf("first 1000: rows %d, rows_written %d, dedupe_hits %d", r.rows(), r.counter(tsdbwriter.CounterRowsWritten), r.counter(tsdbwriter.CounterDedupeHits))
	r.publish(msgs)
	deadline := time.Now().Add(30 * time.Second)
	for r.counter(tsdbwriter.CounterDedupeHits) < 1000 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if r.rows() != 1000 || r.counter(tsdbwriter.CounterDedupeHits) != 1000 || r.counter(tsdbwriter.CounterRowsWritten) != 1000 {
		t.Fatalf("republished: rows %d counters %v", r.rows(), r.pipes[0].Counters.Snapshot())
	}
	if n := count(t, tsOwner(t), "SELECT count(*) FROM writer_gaps WHERE stream = $1", r.stream); n != 0 {
		t.Fatalf("%d gaps on a clean run", n)
	}
	t.Logf("republished 1000: rows %d, dedupe_hits %d, %s", r.rows(), r.counter(tsdbwriter.CounterDedupeHits), r.metric("ussp_tsdb_writer_batch_rows_count"))
}

// SC-18: TimescaleDB unreachable for 30 s under 100 msg/s: nothing is
// lost, the rows land after recovery, and the queue depth is read from
// /metrics during the outage.
func TestIntegrationWriterSurvivesTimescaleDBOutage(t *testing.T) {
	r := newWriterRig(t, writerOpts{})
	r.publish([][]byte{r.track(time.Now())})
	r.waitRows(1, 15*time.Second)

	r.tsProxy.stop()
	outage := time.Now()
	sent := 1
	tick := time.NewTicker(10 * time.Millisecond) // 100 msg/s
	var depth string
	for time.Since(outage) < 30*time.Second {
		<-tick.C
		r.publish([][]byte{r.track(time.Now())})
		sent++
		if depth == "" && time.Since(outage) > 20*time.Second {
			depth = r.metric("ussp_tsdb_writer_queue_rows")
		}
	}
	tick.Stop()
	snap := r.pipes[0].Snapshot()
	t.Logf("after 30 s down: sent %d, %s, state %s, queue_age_s %.1f, write_failed %d",
		sent, depth, snap.State, snap.QueueAgeS, r.counter(tsdbwriter.CounterWriteFailed))
	if depth == "" || strings.HasSuffix(depth, " 0") || snap.State == tsdbwriter.StateOK {
		t.Fatalf("queue depth %q state %s while the database was down", depth, snap.State)
	}
	if r.rows() != 1 {
		t.Fatalf("rows written while the database was down: %d", r.rows())
	}
	r.tsProxy.start()
	recovered := time.Now()
	r.waitRows(int64(sent), 60*time.Second)
	t.Logf("recovered: %d rows in %v; dropped_rows %d, dedupe_hits %d, spills %d", r.rows(), time.Since(recovered).Round(time.Millisecond),
		r.counter(tsdbwriter.CounterDroppedRows), r.counter(tsdbwriter.CounterDedupeHits), r.counter(tsdbwriter.CounterSpills))
	if r.counter(tsdbwriter.CounterDroppedRows) != 0 {
		t.Fatal("rows dropped in a 30 s outage")
	}
}

// Over the cap: the writer holds 200 rows while TimescaleDB is down and
// the stream keeps only 500 messages, so the stream removes the rest
// before the writer reads them. On recovery the hole is a writer_gaps
// row, dropped_rows moves by exactly the removed messages, the log says
// so, and every row still held or still in the stream is written.
func TestIntegrationWriterOverTheCapRecordsTheGap(t *testing.T) {
	r := newWriterRig(t, writerOpts{maxMsgs: 500, holdRows: 200})
	r.publish([][]byte{r.track(time.Now())})
	r.waitRows(1, 15*time.Second)
	r.tsProxy.stop()
	time.Sleep(500 * time.Millisecond)
	msgs := make([][]byte, 2000)
	for i := range msgs {
		msgs[i] = r.track(time.Now().Add(time.Duration(i) * time.Millisecond))
	}
	r.publish(msgs)
	deadline := time.Now().Add(15 * time.Second)
	for r.pipes[0].Snapshot().QueueRows < 200 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if s := r.pipes[0].Snapshot(); s.QueueRows != 200 || s.State != tsdbwriter.StateSpilling {
		t.Fatalf("held %+v", s)
	}
	r.tsProxy.start()
	// 1 + 200 held + 500 left in the stream; 2..201 held, 202..1501 removed.
	r.waitRows(701, 60*time.Second)
	var cause string
	var from, to, n int64
	if err := tsOwner(t).QueryRow(context.Background(), "SELECT cause, from_seq, to_seq, count FROM writer_gaps WHERE stream = $1", r.stream).
		Scan(&cause, &from, &to, &n); err != nil {
		t.Fatal(err)
	}
	if cause != store.CauseStreamRemoved || from != 202 || to != 1501 || n != 1300 || r.counter(tsdbwriter.CounterDroppedRows) != 1300 {
		t.Fatalf("gap %s %d-%d count %d; counters %v", cause, from, to, n, r.pipes[0].Counters.Snapshot())
	}
	t.Logf("over the cap: rows %d, gap %s %d..%d (%d rows), dropped_rows %d", r.rows(), cause, from, to, n, r.counter(tsdbwriter.CounterDroppedRows))
}

// E-02, SC-08 step 8: NATS unreachable at start: the writer starts,
// /readyz says nats down, and once NATS appears it reconnects and
// writes.
func TestIntegrationWriterStartsWithoutNATS(t *testing.T) {
	r := newWriterRig(t, writerOpts{natsDown: true})
	code, body := readyz(t, r.api, client.ReadinessStatusNotReady)
	if code != 503 || body.Dependencies["nats"].State != client.DependencyStateDown {
		t.Fatalf("readyz %d %+v", code, body)
	}
	if d := body.Dependencies["nats"].Detail; d == nil || !strings.Contains(*d, "not connected") {
		t.Fatalf("nats detail %v", d)
	}
	r.publish([][]byte{r.track(time.Now())}) // waits in the stream
	r.natsProx.start()
	code, body = readyz(t, r.api, client.ReadinessStatusReady)
	if code != 200 || body.Dependencies["nats"].State != client.DependencyStateUp || body.Dependencies["timescaledb"].State != client.DependencyStateUp {
		t.Fatalf("after NATS appeared: %d %+v", code, body)
	}
	r.waitRows(1, 30*time.Second)
}
