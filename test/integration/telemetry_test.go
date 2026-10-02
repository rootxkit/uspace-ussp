//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"
	"github.com/rootxkit/uspace-core/serial"

	"github.com/rootxkit/uspace-ussp/internal/app/telemetryingest"
	"github.com/rootxkit/uspace-ussp/internal/app/tsdbwriter"
	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/bus/bustest"
	"github.com/rootxkit/uspace-ussp/internal/flights"
	flightstore "github.com/rootxkit/uspace-ussp/internal/flights/pgstore"
	"github.com/rootxkit/uspace-ussp/internal/national/client"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/operator"
)

// ownTokens is this USSP's operator issuer as telemetry-ingest sees it:
// tokens it signs and the JWKS it publishes (api's, in a deployment).
type ownTokens struct {
	iss  *coreauth.Issuer
	jwks string
}

func newOwnTokens(t *testing.T) *ownTokens {
	t.Helper()
	own, _, _ := testKeys(t)
	sk, err := auth.NewSigningKey(own)
	if err != nil {
		t.Fatal(err)
	}
	iss, err := coreauth.NewIssuer(testIssuer, own, sk.KID)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(iss.JWKS())
	}))
	t.Cleanup(srv.Close)
	return &ownTokens{iss: iss, jwks: srv.URL + "/.well-known/jwks.json"}
}

func (o *ownTokens) token(t *testing.T, clientID string) string {
	t.Helper()
	tok, err := o.iss.Issue(clientID, testHost, []string{auth.ScopeTelemetry}, time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// ingestRig is telemetry-ingest against the real NATS, with a client
// whose serials are bound in client_bindings before it starts.
type ingestRig struct {
	t       *testing.T
	api     *client.ClientWithResponses
	base    string
	nc      *bus.Conn
	client  string
	serials []string
	token   string
}

func newIngestRig(t *testing.T, n int, o telemetryingest.Options, vars map[string]string) *ingestRig {
	t.Helper()
	own := newOwnTokens(t)
	r := &ingestRig{t: t, client: "wp8-" + unique(), nc: busConn(t, mustEnv(t, "USSP_TEST_NATS_URL"), true)}
	folds := make([]string, 0, n)
	for i := range n {
		sn := fmt.Sprintf("TEST-WP8-%s-%03d", r.client, i)
		r.serials = append(r.serials, sn)
		folds = append(folds, serial.FoldKey(sn))
	}
	kv := bus.NewProjector(r.nc, nil)
	if err := kv.PutJSON(context.Background(), bus.BucketClientBindings, bus.KeyToken(r.client), folds); err != nil {
		t.Fatal(err)
	}
	if vars == nil {
		vars = map[string]string{}
	}
	vars["USSP_TELEMETRY_INGEST_ADDR"] = "127.0.0.1:0"
	vars["USSP_NATS_URL"] = mustEnv(t, "USSP_TEST_NATS_URL")
	vars["USSP_AUDIENCES"] = testHost
	vars["USSP_TOKEN_ISSUERS"] = testIssuer + "=" + own.jwks
	vars["USSP_ISSUER_URL"] = testIssuer
	r.api = run(t, telemetryingest.SpecWith(o), vars)
	r.base = r.api.ClientInterface.(*client.Client).Server
	r.token = own.token(t, r.client)
	r.waitDependency("client_bindings", "up")
	return r
}

// waitDependency polls /readyz until the dependency is in state.
func (r *ingestRig) waitDependency(name, state string) *client.Readiness {
	r.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		resp, err := r.api.GetReadyzWithResponse(context.Background())
		if err != nil {
			r.t.Fatal(err)
		}
		body := resp.JSON200
		if body == nil {
			body = resp.JSON503
		}
		if body != nil {
			if d, ok := body.Dependencies[name]; ok && string(d.State) == state {
				return body
			}
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("%s not %s: %s", name, state, resp.Body)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// trackRecorder keeps the trk.v1 messages of one client.
type trackRecorder struct {
	mu     sync.Mutex
	tracks map[string]trackSeen // by msg_id
}

type trackSeen struct {
	ts, rx, captured time.Time
	backlog          bool
	serial           string
	altAMSL          *float64
	altSource        string
}

func recordTracks(t *testing.T, nc *bus.Conn, clientID string) *trackRecorder {
	t.Helper()
	tr := &trackRecorder{tracks: map[string]trackSeen{}}
	bustest.Subscribe(t, nc, bus.SubjectTrkAll, func(data []byte) {
		var v struct {
			MsgID      string    `json:"msg_id"`
			TS         time.Time `json:"ts"`
			RxTS       time.Time `json:"rx_ts"`
			CapturedAt time.Time `json:"captured_at"`
			Backlog    bool      `json:"backlog"`
			Body       struct {
				SourceInstance string   `json:"source_instance"`
				AltAMSLM       *float64 `json:"alt_amsl_m"`
				AltSource      string   `json:"alt_source"`
				Identification struct {
					Serial *string `json:"serial"`
				} `json:"identification"`
			} `json:"body"`
		}
		if json.Unmarshal(data, &v) != nil || v.Body.SourceInstance != clientID {
			return
		}
		sn := ""
		if v.Body.Identification.Serial != nil {
			sn = *v.Body.Identification.Serial
		}
		tr.mu.Lock()
		tr.tracks[v.MsgID] = trackSeen{ts: v.TS, rx: v.RxTS, captured: v.CapturedAt, backlog: v.Backlog, serial: sn,
			altAMSL: v.Body.AltAMSLM, altSource: v.Body.AltSource}
		tr.mu.Unlock()
	})
	return tr
}

func (tr *trackRecorder) all() []trackSeen {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	out := make([]trackSeen, 0, len(tr.tracks))
	for _, s := range tr.tracks {
		out = append(out, s)
	}
	return out
}

// startWriter runs tsdb-writer on the default streams.
func startWriter(t *testing.T) {
	t.Helper()
	run(t, tsdbwriter.Spec, map[string]string{
		"USSP_TSDB_WRITER_ADDR": "127.0.0.1:0",
		"USSP_TS_URL":           mustEnv(t, "USSP_TEST_TS_OWNER_URL"),
		"USSP_NATS_URL":         mustEnv(t, "USSP_TEST_NATS_URL"),
	})
}

// rows waits until the telemetry hypertable holds want rows of the
// client and returns the count (and how many are backlog).
func rows(t *testing.T, clientID string, want int64, within time.Duration) (total, backlog int64) {
	t.Helper()
	ts := tsOwner(t)
	deadline := time.Now().Add(within)
	for {
		total = count(t, ts, `SELECT count(*) FROM telemetry WHERE source_client_id = $1`, clientID)
		if total >= want || time.Now().After(deadline) {
			backlog = count(t, ts, `SELECT count(*) FROM telemetry WHERE source_client_id = $1 AND backlog`, clientID)
			return total, backlog
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// Done-when (brief WP-8): a simulated operator streams 100 samples/s for
// 60 s over the socket; every one is acknowledged, published on trk.v1
// placed by the network rule (captured_at = its own time on one clock,
// never later than its receipt, none backlog) and written by tsdb-writer:
// 6000 rows, no drop.
func TestIntegrationTelemetryHundredPerSecondForAMinute(t *testing.T) {
	ensureSchemas(t)
	startWriter(t)
	r := newIngestRig(t, 100, telemetryingest.Options{}, nil)
	seen := recordTracks(t, r.nc, r.client)
	at := origin(t)
	ctx := context.Background()
	var fakes []*operator.Client
	for i := range 4 {
		f := operator.New(r.base, r.token, at.LatDeg, at.LonDeg, r.serials[i*25:(i+1)*25]...)
		if err := f.Up(ctx); err != nil {
			t.Fatal(err)
		}
		fakes = append(fakes, f)
	}
	start := time.Now()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	sent := 0
	for range 60 {
		now := time.Now()
		for _, f := range fakes {
			n, err := f.Tick(ctx, now)
			if err != nil {
				t.Fatal(err)
			}
			sent += n
		}
		<-tick.C
	}
	intake := time.Since(start)
	for _, f := range fakes {
		if err := f.WaitAcked(ctx, 30*time.Second); err != nil {
			t.Fatal(err)
		}
		f.Down()
	}
	total, backlog := rows(t, r.client, 6000, 60*time.Second)
	got := seen.all()
	t.Logf("sent %d samples in %v (%.1f/s); trk.v1 %d messages; telemetry rows %d (backlog %d)",
		sent, intake.Round(time.Millisecond), float64(sent)/intake.Seconds(), len(got), total, backlog)
	if sent != 6000 || total != 6000 || backlog != 0 || len(got) != 6000 {
		t.Fatalf("sent %d rows %d backlog %d trk %d", sent, total, backlog, len(got))
	}
	for _, s := range got {
		if s.backlog || !s.captured.Equal(s.ts) || s.captured.After(s.rx) {
			t.Fatalf("placement: ts %v rx %v captured %v backlog %v", s.ts, s.rx, s.captured, s.backlog)
		}
	}
}

// Done-when (brief WP-8, SC-14, T-04): a 60 s outage with the client
// queueing, then live again while it drains its queue by batch: every
// sample lands, the outage's as backlog at their own time (no live alert
// consumer sees them as live: the trk.v1 messages say backlog), the live
// ones not, and the drain clearly exceeds intake (measured, printed).
func TestIntegrationTelemetryOutageThenDrain(t *testing.T) {
	ensureSchemas(t)
	startWriter(t)
	const aircraft = 20
	r := newIngestRig(t, aircraft, telemetryingest.Options{}, nil)
	seen := recordTracks(t, r.nc, r.client)
	at := origin(t)
	ctx := context.Background()
	f := operator.New(r.base, r.token, at.LatDeg, at.LonDeg, r.serials...)
	if err := f.Up(ctx); err != nil {
		t.Fatal(err)
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	live := func(n int) {
		for range n {
			if _, err := f.Tick(ctx, time.Now()); err != nil {
				t.Fatal(err)
			}
			<-tick.C
		}
	}
	live(5)
	f.Down()
	outageFrom := time.Now().Truncate(time.Millisecond) // the envelope's times are milliseconds
	live(60)                                            // queued: the socket is down
	outageTo := time.Now().Truncate(time.Millisecond)
	if p, _ := f.Pending(); p < 60*aircraft {
		t.Fatalf("queued %d", p)
	}
	if err := f.Up(ctx); err != nil {
		t.Fatal(err)
	}
	var drain operator.DrainStats
	var derr error
	var wg sync.WaitGroup
	wg.Go(func() { drain, derr = f.Drain(ctx, http.DefaultClient) })
	live(5)
	wg.Wait()
	if derr != nil {
		t.Fatal(derr)
	}
	if err := f.WaitAcked(ctx, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	f.Down()
	intakeRate := float64(aircraft) // 1 Hz per aircraft
	t.Logf("drain: %d samples in %d batches, %d accepted in %v: %.0f samples/s = %.1fx the intake of %.0f/s (refused %v)",
		drain.Samples, drain.Batches, drain.Accepted, drain.Duration.Round(time.Millisecond), drain.RatePerS, drain.RatePerS/intakeRate,
		intakeRate, drain.Refused)
	if drain.Accepted != 60*aircraft || len(drain.Refused) != 0 {
		t.Fatalf("drained %+v", drain)
	}
	if drain.RatePerS < 5*intakeRate {
		t.Fatalf("drain %.0f/s is less than 5x the intake (B-01, SC-14)", drain.RatePerS)
	}
	want := int64(70 * aircraft)
	total, backlog := rows(t, r.client, want, 60*time.Second)
	if total != want || backlog != int64(60*aircraft) {
		t.Fatalf("rows %d backlog %d, want %d and %d", total, backlog, want, 60*aircraft)
	}
	for _, s := range seen.all() {
		inOutage := !s.ts.Before(outageFrom) && s.ts.Before(outageTo)
		if inOutage != s.backlog {
			t.Fatalf("ts %v (outage %v..%v) backlog %v", s.ts, outageFrom, outageTo, s.backlog)
		}
		if d := s.captured.Sub(s.ts); d < -time.Second || d > time.Second {
			t.Fatalf("a drained sample placed %v from its own time", d)
		}
	}
}

// 05 §5 with the real work queue: with no publisher memory every sample
// goes to ingest.v1.<cell3>, is acknowledged once JetStream stored it,
// and the drain replays it on trk.v1 as backlog with its own times.
func TestIntegrationTelemetryWorkQueueDrain(t *testing.T) {
	ensureSchemas(t)
	r := newIngestRig(t, 2, telemetryingest.Options{QueueFrames: -1}, nil)
	seen := recordTracks(t, r.nc, r.client)
	at := origin(t)
	ctx := context.Background()
	f := operator.New(r.base, r.token, at.LatDeg, at.LonDeg, r.serials...)
	if err := f.Up(ctx); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if _, err := f.Tick(ctx, time.Now()); err != nil {
			t.Fatal(err)
		}
		if i < 2 {
			time.Sleep(time.Second)
		}
	}
	if err := f.WaitAcked(ctx, 20*time.Second); err != nil {
		t.Fatal(err)
	}
	f.Down()
	deadline := time.Now().Add(20 * time.Second)
	for len(seen.all()) < 6 {
		if time.Now().After(deadline) {
			t.Fatalf("replayed %d of 6", len(seen.all()))
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, s := range seen.all() {
		if !s.backlog || !s.captured.Equal(s.ts) {
			t.Fatalf("replay %+v", s)
		}
	}
}

// E-02 (R-07, SC-22): without a geoid /readyz is degraded with geoid
// down ("missing") and the tracks have no AMSL altitude; with one, geoid
// is up and AMSL is HAE - N at the point.
func TestIntegrationTelemetryGeoid(t *testing.T) {
	ensureSchemas(t)
	for _, withGrid := range []bool{false, true} {
		t.Run(fmt.Sprintf("geoid=%v", withGrid), func(t *testing.T) {
			vars := map[string]string{}
			if withGrid {
				withGeoid(t, vars)
			}
			r := newIngestRig(t, 1, telemetryingest.Options{}, vars)
			state := "down"
			if withGrid {
				state = "up"
			}
			body := r.waitDependency("geoid", state)
			if !withGrid && (body.Dependencies["geoid"].Detail == nil || !strings.HasPrefix(*body.Dependencies["geoid"].Detail, "missing")) {
				t.Fatalf("geoid detail %+v", body.Dependencies["geoid"])
			}
			seen := recordTracks(t, r.nc, r.client)
			at := origin(t)
			f := operator.New(r.base, r.token, at.LatDeg, at.LonDeg, r.serials...)
			if err := f.Up(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := f.Tick(context.Background(), time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := f.WaitAcked(context.Background(), 20*time.Second); err != nil {
				t.Fatal(err)
			}
			f.Down()
			deadline := time.Now().Add(10 * time.Second)
			for len(seen.all()) < 1 && time.Now().Before(deadline) {
				time.Sleep(50 * time.Millisecond)
			}
			got := seen.all()
			if len(got) != 1 {
				t.Fatalf("%d tracks", len(got))
			}
			if withGrid {
				if got[0].altAMSL == nil || *got[0].altAMSL != 650-geoidUndulationM || got[0].altSource != "geodetic" {
					t.Fatalf("AMSL %v %s", got[0].altAMSL, got[0].altSource)
				}
				return
			}
			if got[0].altAMSL != nil || got[0].altSource != "none" {
				t.Fatalf("AMSL without a geoid: %v %s", got[0].altAMSL, got[0].altSource)
			}
		})
	}
}

// The flight facts reach the flights table (api's recorder over the
// FLIGHT stream and pgstore): started creates the row, ended closes it,
// a replay changes nothing and an ended flight is never reopened.
func TestIntegrationFlightFactsRecorded(t *testing.T) {
	ensureSchemas(t)
	nc := busConn(t, mustEnv(t, "USSP_TEST_NATS_URL"), true)
	pub := bus.NewPublisher(nc, nil)
	var events []*flights.Event
	b := &flights.Binder{Emit: func(e *flights.Event) { events = append(events, e) }}
	sn := "TEST-WP8-FLIGHT-" + unique()
	id := b.Bind("c|"+sn, "wp8-unknown-client", sn, nil, nil, nil, time.Now(), true)
	b.End("c|"+sn, flights.EndOperator, time.Now().Add(time.Second))
	for _, e := range append(events, events[0]) { // the started fact again: a replay
		subject, err := e.Subject()
		if err != nil {
			t.Fatal(err)
		}
		if err := pub.Publish(context.Background(), subject, e); err != nil {
			t.Fatal(err)
		}
	}
	rec := &flights.Recorder{
		Source: &bus.StreamSource{Open: bus.PullOpener(nc.JetStream(), bus.DefaultTopology(), bus.StreamFLIGHT, bus.PullSpec{
			Durable: "wp8-test-" + unique(), FilterSubject: bus.SubjectFlightAll, MaxAckPending: 64,
		})},
		Store: flightstore.Store{S: appStore(t)}, Wait: 200 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { rec.Run(ctx) })
	defer func() { cancel(); wg.Wait() }()
	rel := relOwner(t)
	deadline := time.Now().Add(20 * time.Second)
	for count(t, rel, `SELECT count(*) FROM flights WHERE id = $1::uuid AND ended_at IS NOT NULL`, id) != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("flight %s not recorded ended", id)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if n := count(t, rel, `SELECT count(*) FROM flights WHERE id = $1::uuid AND end_reason = 'operator_ended' AND last_state = 'ended'
		AND client_id IS NULL AND uas_serial = $2`, id, sn); n != 1 {
		t.Fatalf("row %d", n)
	}
	if n := count(t, rel, `SELECT count(*) FROM events WHERE entity_type = 'flight' AND entity_id = $1`, id); n < 2 {
		t.Fatalf("audit rows %d", n)
	}
}
