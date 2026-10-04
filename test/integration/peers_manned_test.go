//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3411"
	"github.com/rootxkit/uspace-core/geodesy"
	coresources "github.com/rootxkit/uspace-core/sources"

	"github.com/rootxkit/uspace-ussp/internal/app/monitor"
	"github.com/rootxkit/uspace-ussp/internal/app/proc"
	"github.com/rootxkit/uspace-ussp/internal/app/ridsp"
	"github.com/rootxkit/uspace-ussp/internal/app/tsdbwriter"
	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/cell"
	"github.com/rootxkit/uspace-ussp/internal/config"
	"github.com/rootxkit/uspace-ussp/internal/dss/fakedss"
	"github.com/rootxkit/uspace-ussp/internal/manned"
	"github.com/rootxkit/uspace-ussp/internal/peers"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/ansp"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/cisp"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/peersp"
	"github.com/rootxkit/uspace-ussp/internal/traffic"
)

// inputsRig is WP-14 on the real bus: the traffic rig (two operators
// with an intent each, the monitor's CPA path, traffic-ws), its monitor
// restarted with the manned and peer inputs pointed at the fakes (the
// ANSP's stream, an e-conspicuity receiver, the DSS and a peer Service
// Provider), and rid-sp receiving the peers' ISA notifications.
type inputsRig struct {
	*trafficRig
	auth *fakeAuthority
	ansp *ansp.Fake
	dss  *fakedss.DSS
	peer *peersp.Fake
	sp   string
	logs *logBuffer
	box  geodesy.BBox
}

func newInputsRig(t *testing.T) *inputsRig {
	t.Helper()
	g := &inputsRig{trafficRig: newTrafficRig(t), auth: newFakeAuthority(t), dss: fakedss.New()}
	t.Cleanup(g.dss.Close)
	var err error
	if g.ansp, err = ansp.New(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.ansp.Close)
	g.sp = "http://" + runAt(t, ridsp.Spec, map[string]string{
		"USSP_RID_SP_ADDR": "127.0.0.1:0", "USSP_NATS_URL": mustEnv(t, "USSP_TEST_NATS_URL"),
		"USSP_AUDIENCES": testHost, "USSP_TOKEN_ISSUERS": g.auth.url + "=" + g.auth.jwks,
	})
	g.peer = peersp.New(g.dss.URL(), func(string, f3411.Scope) string {
		tok, err := g.auth.iss.Issue("peer-sp-wp14", testHost, []string{string(f3411.ScopeServiceProvider), string(f3411.ScopeDisplayProvider)}, time.Hour, time.Now())
		if err != nil {
			t.Error(err)
		}
		return tok
	})
	t.Cleanup(g.peer.Close)
	g.box = geodesy.BBox{MinLat: g.o.LatDeg - 0.03, MinLon: g.o.LonDeg - 0.03, MaxLat: g.o.LatDeg + 0.03, MaxLon: g.o.LonDeg + 0.03}
	return g
}

// monitorVars are the monitor's variables with every WP-14 input on.
func (g *inputsRig) monitorVars(t *testing.T, extra map[string]string) map[string]string {
	t.Helper()
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("integration-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	v := map[string]string{
		"USSP_MONITOR_ADDR": "127.0.0.1:0", "USSP_NATS_URL": mustEnv(t, "USSP_TEST_NATS_URL"),
		"USSP_TOKEN_ISSUERS": g.auth.url + "=" + g.auth.jwks, "USSP_TOKEN_CLIENT_SECRET_FILE": secret,
		"USSP_ANSP_STREAM_URL": g.ansp.StreamURL(), "USSP_MTLS_MODE": "off",
		"USSP_DSS_BASE_URL": g.dss.URL(), "USSP_USS_BASE_URL": g.sp,
		"USSP_TRAFFIC_INPUT_BBOX": fmt.Sprintf("%f,%f,%f,%f", g.box.MinLon, g.box.MinLat, g.box.MaxLon, g.box.MaxLat),
		"USSP_GEOID_FILE":         geoidFile(t),
	}
	for k, x := range extra {
		if x == "" {
			delete(v, k)
			continue
		}
		v[k] = x
	}
	return v
}

// restartMonitor replaces the rig's monitor with one reading the WP-14
// inputs.
func (g *inputsRig) restartMonitor(t *testing.T, extra map[string]string) {
	t.Helper()
	g.stopMon()
	addr, stop, logs := runLogged(t, monitor.SpecWith(monitor.Options{}), g.monitorVars(t, extra))
	g.monitor, g.stopMon, g.logs = "http://"+addr, stop, logs
}

// runLogged is runStoppable keeping the process log for the test.
func runLogged(t *testing.T, spec proc.Spec, vars map[string]string) (string, func(), *logBuffer) {
	t.Helper()
	vars["USSP_STATUS_INTERVAL_S"] = "3600"
	cfg, err := config.LoadFrom(func(n string) (string, bool) { v, ok := vars[n]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	addr := make(chan string, 1)
	done := make(chan error, 1)
	logs := &logBuffer{}
	go func() {
		done <- proc.Run(ctx, cfg, spec, proc.Options{Out: logs, Listening: func(a string) { addr <- a }})
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("drain: %v", err)
			}
		})
	}
	t.Cleanup(func() {
		stop()
		if t.Failed() {
			t.Logf("process log:\n%s", logs.String())
		}
	})
	select {
	case a := <-addr:
		return a, stop, logs
	case err := <-done:
		t.Fatalf("Run returned before listening: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("no listener within 15 s")
	}
	return "", stop, logs
}

// dep is one dependency of a process's /readyz.
func dep(base, name string) (state, detail string) {
	resp, err := http.Get(base + "/readyz")
	if err != nil {
		return "", err.Error()
	}
	defer resp.Body.Close()
	var r struct {
		Dependencies map[string]struct {
			State  string  `json:"state"`
			Detail *string `json:"detail"`
		} `json:"dependencies"`
	}
	if json.NewDecoder(resp.Body).Decode(&r) != nil {
		return "", "unreadable"
	}
	d := r.Dependencies[name]
	if d.Detail != nil {
		detail = *d.Detail
	}
	return d.State, detail
}

func degradedIn(p traffic.ProductBody, input string) (traffic.Degraded, bool) {
	for _, d := range p.Degraded {
		if d.Input == input {
			return d, true
		}
	}
	return traffic.Degraded{}, false
}

func trackIn(p traffic.ProductBody, id string) (traffic.ProductTrack, bool) {
	for _, tr := range p.Tracks {
		if tr.TrackID == id {
			return tr, true
		}
	}
	return traffic.ProductTrack{}, false
}

// listen records every message of a subject filter on core NATS.
func listen(t *testing.T, g *trafficRig, filter string) func() [][]byte {
	t.Helper()
	var mu sync.Mutex
	var got [][]byte
	stop, err := g.nc.Listen(filter, func(_ string, data []byte) {
		mu.Lock()
		got = append(got, append([]byte(nil), data...))
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return func() [][]byte { mu.Lock(); defer mu.Unlock(); return append([][]byte(nil), got...) }
}

func pf(v float64) *float64 { return &v }

func icaoOf(t *testing.T) string {
	t.Helper()
	u := unique()
	return "c" + u[len(u)-5:]
}

// The ANSP's stream end to end (brief WP-14, E-01, E-02): a track the
// fake ANSP streams is on man.v1 with trust surveillance and in the
// operator's product; the stream cut is "unavailable since T" in the
// product and on /readyz within 10 s; back, cleared; status frames only,
// with an adapter stale, are "manned: stale since T" with the ANSP's
// time, not unavailable.
func TestIntegrationMannedStream(t *testing.T) {
	g := newInputsRig(t)
	icao := icaoOf(t)
	g.ansp.SetAircraft(ansp.Aircraft{ICAO24: icao, Position: geodesy.Destination(g.o, 0, 800), AltPressureM: pf(1500), GSMS: 50, TrackDeg: 90})
	man := listen(t, g.trafficRig, "man.v1.>")
	g.restartMonitor(t, nil)
	a := g.ops[0]
	s := g.dial("/v1/traffic?intent_id="+a.intent, a.token, false)
	within(t, 15*time.Second, func() bool {
		p, ok := s.lastProduct()
		tr, found := trackIn(p, icao)
		return ok && found && tr.Trust == core.TrustSurveillance && tr.Source == traffic.SourceANSPFeed
	})
	var m traffic.MannedTrack
	for _, raw := range man() {
		if json.Unmarshal(raw, &m) == nil && m.Body.ICAO24 == icao {
			break
		}
	}
	if m.Body.ICAO24 != icao || m.Body.Trust != core.TrustSurveillance || m.Body.Source != traffic.SourceANSPFeed || m.Producer != manned.ProducerANSP {
		t.Fatalf("man.v1 %+v", m)
	}
	within(t, 5*time.Second, func() bool {
		p, _ := s.lastProduct()
		_, deg := degradedIn(p, "manned")
		st, _ := dep(g.monitor, monitor.DepANSPFeed)
		return !deg && st == "up"
	})
	auth, _ := g.ansp.StreamAuth()
	if len(auth) == 0 || auth[0] != "Bearer "+cisp.Token {
		t.Fatalf("stream auth %q", auth)
	}

	// The stream cut.
	g.ansp.CutStream()
	cut := time.Now()
	within(t, 12*time.Second, func() bool {
		p, _ := s.lastProduct()
		d, deg := degradedIn(p, "manned")
		st, detail := dep(g.monitor, monitor.DepANSPFeed)
		return deg && strings.HasPrefix(d.Reason, "manned traffic unavailable since") && st == "down" && strings.Contains(detail, "unavailable since")
	})
	took := time.Since(cut)
	if took > 10*time.Second {
		t.Fatalf("unavailable %s after the cut, budget 10 s", took)
	}
	t.Logf("stream cut: unavailable since T in the product and on /readyz %s after the cut", took.Round(10*time.Millisecond))
	g.ansp.RestoreStream()
	back := time.Now()
	within(t, 40*time.Second, func() bool {
		p, _ := s.lastProduct()
		_, deg := degradedIn(p, "manned")
		st, _ := dep(g.monitor, monitor.DepANSPFeed)
		return !deg && st == "up"
	})
	t.Logf("stream back: cleared %s after the ANSP served again", time.Since(back).Round(10*time.Millisecond))

	// Status frames only, an adapter stale since T at the ANSP.
	since := time.Now().Add(-30 * time.Second).UTC().Truncate(time.Second)
	g.ansp.TracksOff(true)
	g.ansp.SetAdapter(ansp.AdapterState{ID: "fake-adsb-1", State: "stale", Since: since})
	within(t, 10*time.Second, func() bool {
		p, _ := s.lastProduct()
		d, deg := degradedIn(p, "manned")
		return deg && d.Since != nil && d.Since.Equal(since) &&
			strings.HasPrefix(d.Reason, "manned: stale since "+since.Format(time.RFC3339)+" (the ANSP's time)")
	})
	p, _ := s.lastProduct()
	if d, _ := degradedIn(p, "manned"); strings.Contains(d.Reason, "unavailable") {
		t.Fatalf("stale shown as unavailable: %+v", d)
	}
}

// USSP_MTLS_MODE: off logs at error level and connects without a
// certificate; required without a certificate file refuses to start,
// naming it (E-01 pair).
func TestIntegrationMannedStreamMTLSModes(t *testing.T) {
	g := newInputsRig(t)
	g.restartMonitor(t, nil)
	within(t, 10*time.Second, func() bool { return g.ansp.StreamConnects() > 0 })
	if !strings.Contains(g.logs.String(), `"level":"ERROR","msg":"USSP_MTLS_MODE=off`) {
		t.Fatalf("no error-level line for USSP_MTLS_MODE=off:\n%s", g.logs.String())
	}
	vars := g.monitorVars(t, map[string]string{"USSP_MTLS_MODE": "required"})
	cfg, err := config.LoadFrom(func(n string) (string, bool) { v, ok := vars[n]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = proc.Run(ctx, cfg, monitor.Spec, proc.Options{Out: &logBuffer{}, Listening: func(string) { t.Error("listening with no certificate") }})
	if err == nil || !strings.Contains(err.Error(), "USSP_MTLS_CERT_FILE") {
		t.Fatalf("required without a certificate: %v", err)
	}
}

// A recorded ADS-B file through the e-conspicuity adapter: its tracks on
// man.v1 as broadcast from adsb_rx and in the product with the
// altitude source the data allows: geodetic through the geoid with a
// geometric altitude, pressure (judged on the horizontal alone) without
// one, visible.
func TestIntegrationEconspicuityReplay(t *testing.T) {
	g := newInputsRig(t)
	geo, baro := icaoOf(t), icaoOf(t)
	rec := float64(time.Date(2025, 6, 1, 9, 0, 0, 0, time.UTC).Unix())
	pg, pb := geodesy.Destination(g.o, 45, 900), geodesy.Destination(g.o, 225, 900)
	var lines []string
	for i := 0; i < 40; i++ {
		lines = append(lines, fmt.Sprintf(`{"now":%.1f,"messages":%d,"aircraft":[`+
			`{"hex":"%s","flight":"GEO1","alt_baro":2000,"alt_geom":2100,"gs":90,"track":45,"lat":%f,"lon":%f,"seen_pos":0.3,"seen":0.1},`+
			`{"hex":"%s","alt_baro":2500,"gs":80,"track":225,"lat":%f,"lon":%f,"seen_pos":0.5,"seen":0.2}]}`,
			rec+float64(i)*0.5, i, geo, pg.LatDeg, pg.LonDeg, baro, pb.LatDeg, pb.LonDeg))
	}
	file := filepath.Join(t.TempDir(), "adsb.jsonl")
	if err := os.WriteFile(file, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	man := listen(t, g.trafficRig, "man.v1.>")
	g.restartMonitor(t, map[string]string{"USSP_ADSB_SOURCE": "file://" + file, "USSP_ADSB_RECEIVER_ID": "rx-wp14"})
	a := g.ops[0]
	s := g.dial("/v1/traffic?intent_id="+a.intent, a.token, false)
	within(t, 15*time.Second, func() bool {
		p, _ := s.lastProduct()
		tg, ok1 := trackIn(p, geo)
		tb, ok2 := trackIn(p, baro)
		return ok1 && ok2 && tg.AltSource == core.AltGeodetic && tb.AltSource == core.AltPressure &&
			tg.Trust == core.TrustBroadcast && tb.Trust == core.TrustBroadcast && tg.Source == traffic.SourceAdsbRx
	})
	p, _ := s.lastProduct()
	tg, _ := trackIn(p, geo)
	if tg.AltAMSLM == nil || *tg.AltAMSLM != 2100*core.FeetToMetres-geoidUndulationM {
		t.Fatalf("geodetic AMSL %v", tg.AltAMSLM)
	}
	n := 0
	for _, raw := range man() {
		var m traffic.MannedTrack
		if json.Unmarshal(raw, &m) == nil && (m.Body.ICAO24 == geo || m.Body.ICAO24 == baro) {
			n++
			if m.Body.Trust != core.TrustBroadcast || m.Body.Source != traffic.SourceAdsbRx || m.Body.SourceInstance != "rx-wp14" ||
				m.Producer != manned.ProducerEcon || m.CapturedAt.Year() != time.Now().Year() {
				t.Fatalf("man.v1 %+v", m)
			}
		}
	}
	if n == 0 {
		t.Fatal("nothing on man.v1")
	}
	st, detail := dep(g.monitor, monitor.DepAdsbRx)
	if st != "up" || !strings.Contains(detail, "broadcast") {
		t.Fatalf("adsb_rx %s %s", st, detail)
	}
}

// policyFor writes a policy to the policy bucket for the test and the
// defaults back at its end (the suite runs one test at a time).
func (g *inputsRig) policyFor(t *testing.T, set func(*policy.Values)) {
	t.Helper()
	v := policy.Defaults()
	set(&v)
	if err := g.kv.ProjectPolicy(context.Background(), policy.Record{Version: time.Now().UnixNano(), Actor: "test", Reason: "WP-14", Values: v}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = g.kv.ProjectPolicy(context.Background(), policy.Record{Version: time.Now().UnixNano(), Actor: "test", Reason: "WP-14 reset", Values: policy.Defaults()})
	})
}

// The peer Display Provider end to end: a fake peer SP registers an ISA
// in the fake DSS, which names our subscription; the peer notifies
// rid-sp; the monitor polls it at 1 Hz; its flight is on peer.v1 as
// provider and in the product with its age; the peer stops answering:
// peer_unavailable, then aged out after peer_unavailable_s; network_rid
// switched off: the peer's request counter stops within 1 s, and
// resumes on re-enable (SC-16). The flights land in peer_flights through
// the real tsdb-writer, and the table's 24 h disposal drops a day-old
// row the writer wrote and keeps the fresh one.
func TestIntegrationPeerDisplayProvider(t *testing.T) {
	g := newInputsRig(t)
	g.policyFor(t, func(v *policy.Values) { v.PeerUnavailableS = 5 })
	writer := runAt(t, tsdbwriter.Spec, map[string]string{
		"USSP_TSDB_WRITER_ADDR": "127.0.0.1:0", "USSP_TS_URL": mustEnv(t, "USSP_TEST_TS_OWNER_URL"), "USSP_NATS_URL": mustEnv(t, "USSP_TEST_NATS_URL"),
	})
	_ = writer
	peerMsgs := listen(t, g.trafficRig, "peer.v1.>")
	g.restartMonitor(t, nil)
	within(t, 10*time.Second, func() bool {
		for _, s := range g.dss.Subscriptions() {
			if s.UssBaseUrl == g.sp {
				return true
			}
		}
		return false
	})
	// The flight and its ISA south-west of the rig's place, inside one of
	// the views the input box is tiled into (one poller).
	flight := newUUID()
	at := geodesy.Destination(g.o, 225, 1500)
	g.peer.SetFlight(peersp.Flight{ID: flight, Position: at, AltHAEM: 900, SpeedMS: 8, TrackDeg: 270})
	isaBox := geodesy.BBox{MinLat: at.LatDeg - 0.003, MinLon: at.LonDeg - 0.003, MaxLat: at.LatDeg + 0.003, MaxLon: at.LonDeg + 0.003}
	registered := time.Now()
	if _, err := g.peer.RegisterISA(context.Background(), newUUID(), isaBox, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n := g.peer.Notified(); len(n) != 1 || n[0] != g.sp {
		t.Fatalf("the peer notified %q, want rid-sp %s", n, g.sp)
	}
	a := g.ops[0]
	s := g.dial("/v1/traffic?intent_id="+a.intent, a.token, false)
	within(t, 15*time.Second, func() bool {
		p, _ := s.lastProduct()
		tr, ok := trackIn(p, flight)
		return ok && tr.Trust == core.TrustProvider && tr.Source == peers.SourceNetworkRID && !tr.PeerUnavailable && tr.AgeS < 3
	})
	t.Logf("peer flight in the product %s after the ISA was registered", time.Since(registered).Round(10*time.Millisecond))
	// 1 Hz: six more polls take about five seconds.
	r0, t0 := g.peer.Requests(), time.Now()
	within(t, 15*time.Second, func() bool { return g.peer.Requests() >= r0+6 })
	if took := time.Since(t0); took < 4*time.Second || took > 9*time.Second {
		t.Fatalf("6 polls in %s, not 1 Hz", took)
	}
	t.Logf("polled at 1 Hz: 6 requests in %s", time.Since(t0).Round(10*time.Millisecond))
	var got bool
	for _, raw := range peerMsgs() {
		var m struct {
			bus.Envelope
			Body struct {
				TrackID, Source, SourceInstance string
				Trust                           core.Trust
			} `json:"body"`
		}
		if json.Unmarshal(raw, &m) == nil && strings.Contains(string(raw), flight) {
			got = m.Producer == peers.Producer && strings.Contains(string(raw), `"trust":"provider"`) && strings.Contains(string(raw), `"source":"network_rid"`)
		}
	}
	if !got {
		t.Fatal("the peer flight is not on peer.v1 as provider")
	}

	// The peer stops answering: peer_unavailable, then aged out.
	g.peer.Down(true)
	within(t, 10*time.Second, func() bool {
		p, _ := s.lastProduct()
		tr, ok := trackIn(p, flight)
		_, deg := degradedIn(p, "peers")
		return ok && tr.PeerUnavailable && deg
	})
	within(t, 15*time.Second, func() bool {
		p, _ := s.lastProduct()
		_, ok := trackIn(p, flight)
		return !ok
	})
	g.peer.Down(false)
	within(t, 10*time.Second, func() bool {
		p, _ := s.lastProduct()
		tr, ok := trackIn(p, flight)
		return ok && !tr.PeerUnavailable
	})

	// network_rid switched off: the polls stop within 1 s, and resume.
	epoch := "wp14-" + unique()
	if err := g.kv.ProjectSources(context.Background(), coresources.State{Epoch: epoch, Version: 1,
		Controls: []coresources.Control{{SourceType: peers.SourceNetworkRID, Enabled: false}}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = g.kv.ProjectSources(context.Background(), coresources.State{Epoch: "wp14-reset-" + unique(), Version: 1})
	})
	switched := time.Now()
	lastAt, last := time.Now(), g.peer.Requests()
	within(t, 5*time.Second, func() bool {
		// Stopped: no request for 1.5 s (the poll period is 1 s).
		if n := g.peer.Requests(); n != last {
			lastAt, last = time.Now(), n
			return false
		}
		return time.Since(lastAt) > 1500*time.Millisecond
	})
	if stoppedAfter := lastAt.Sub(switched); stoppedAfter > time.Second {
		t.Fatalf("a request %s after the switch, budget 1 s", stoppedAfter)
	}
	t.Logf("network_rid off: the last request %s after the switch", lastAt.Sub(switched).Round(time.Millisecond))
	n := g.peer.Requests()
	within(t, 5*time.Second, func() bool {
		st, _ := dep(g.monitor, monitor.DepNetworkRID)
		return st == "degraded"
	})
	if g.peer.Requests() != n {
		t.Fatalf("requests went on while switched off: %d -> %d", n, g.peer.Requests())
	}
	if err := g.kv.ProjectSources(context.Background(), coresources.State{Epoch: epoch, Version: 2,
		Controls: []coresources.Control{{SourceType: peers.SourceNetworkRID, Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	within(t, 5*time.Second, func() bool { return g.peer.Requests() > n+2 })

	// peer_flights through the real writer, and the 24 h disposal.
	ts := tsOwner(t)
	within(t, 20*time.Second, func() bool {
		return count(t, ts, "SELECT count(*) FROM peer_flights WHERE peer_uss = $1 AND rid_flight_id = $2", g.peer.URL(), flight) > 0
	})
	old := time.Now().Add(-25 * time.Hour)
	c5, _, _ := cell.Key(at)
	subject, err := bus.Peer(c5, flight)
	if err != nil {
		t.Fatal(err)
	}
	msg := map[string]any{"schema": "track/telemetry/v1", "msg_id": bus.NewULID(old), "producer": "ussp/peers", "ts": old, "rx_ts": old,
		"captured_at": old, "time_source": "broadcast", "backlog": false, "body": map[string]any{
			"track_id": flight, "trust": "provider", "source": "network_rid", "source_instance": g.peer.URL() + "/day-old",
			"position": map[string]any{"lat": g.o.LatDeg, "lng": g.o.LonDeg}, "alt_wgs84_m": nil, "alt_amsl_m": nil, "alt_source": "none",
			"alt_pressure_m": nil, "height_m": nil, "height_ref": nil, "speed_ms": nil, "track_deg": nil, "vspeed_ms": nil, "accuracy_h_m": nil,
			"accuracy_v_m": nil, "status": nil, "emergency": false, "flight_id": nil, "intent_id": nil,
			"identification": map[string]any{"status": "unidentified", "reason": "no_serial", "basis": "as_broadcast", "mismatch": false}}}
	raw, _ := json.Marshal(msg)
	if err := g.nc.Publish(subject, raw); err != nil {
		t.Fatal(err)
	}
	within(t, 20*time.Second, func() bool {
		return count(t, ts, "SELECT count(*) FROM peer_flights WHERE peer_uss = $1", g.peer.URL()+"/day-old") == 1
	})
	var jobID int64
	if err := ts.QueryRow(context.Background(), `SELECT job_id FROM timescaledb_information.jobs WHERE proc_name = 'policy_retention' AND hypertable_name = 'peer_flights'`).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Exec(context.Background(), "CALL run_job($1::integer)", jobID); err != nil {
		t.Fatal(err)
	}
	if n := count(t, ts, "SELECT count(*) FROM peer_flights WHERE peer_uss = $1", g.peer.URL()+"/day-old"); n != 0 {
		t.Fatalf("the day-old row the writer wrote survived the 24 h disposal: %d", n)
	}
	if n := count(t, ts, "SELECT count(*) FROM peer_flights WHERE peer_uss = $1 AND rid_flight_id = $2", g.peer.URL(), flight); n == 0 {
		t.Fatal("the fresh rows were dropped")
	}
}

// putIntentWithRegistration is putIntent with the intent's declared UA
// registration (Annex IV item 10).
func (g *inputsRig) putIntentWithRegistration(t *testing.T, op trafficOperator, reg string) {
	t.Helper()
	kv, err := g.nc.JetStream().KeyValue(context.Background(), bus.BucketIntentActive)
	if err != nil {
		t.Fatal(err)
	}
	e, err := kv.Get(context.Background(), op.intent)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(e.Value(), &body); err != nil {
		t.Fatal(err)
	}
	body["ua_registration"] = reg
	if err := g.kv.PutJSON(context.Background(), bus.BucketIntentActive, op.intent, body); err != nil {
		t.Fatal(err)
	}
}

// The echo guard (PLAN §15 Q23, the WP-14 precondition): one of our
// own flights returned by a peer's Service Provider (its RID flight id)
// and heard back by the ANSP (its callsign is the intent's UA
// registration), each beside its own trk.v1 track: no proximity alert,
// and neither echo published. The twin: a different aircraft at the
// same place from each input raises one.
func TestIntegrationEchoOfAnOwnFlightNeverPairsWithItself(t *testing.T) {
	g := newInputsRig(t)
	a := g.ops[0]
	reg := "4L-" + unique()[len(unique())-5:]
	g.putIntentWithRegistration(t, a, reg)
	echoICAO, otherICAO := icaoOf(t), icaoOf(t)
	man := listen(t, g.trafficRig, "man.v1.>")
	peerMsgs := listen(t, g.trafficRig, "peer.v1.>")
	g.restartMonitor(t, nil)
	within(t, 10*time.Second, func() bool { return len(g.dss.Subscriptions()) > 0 })
	g.fly(a, func(float64) (core.LatLon, float64, float64) { return g.o, 0, 0 })
	// The echoes: the peer serves our flight under its id, the ANSP hears
	// our aircraft with the registration as its callsign.
	g.peer.SetFlight(peersp.Flight{ID: a.flight, Position: g.o, AltHAEM: 600 + geoidUndulationM})
	g.ansp.SetAircraft(ansp.Aircraft{ICAO24: echoICAO, Callsign: &reg, Position: g.o})
	isaBox := geodesy.BBox{MinLat: g.o.LatDeg - 0.01, MinLon: g.o.LonDeg - 0.01, MaxLat: g.o.LatDeg + 0.01, MaxLon: g.o.LonDeg + 0.01}
	if _, err := g.peer.RegisterISA(context.Background(), newUUID(), isaBox, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Ten seconds of both inputs polled and streamed beside the flight.
	r0 := g.peer.Requests()
	within(t, 20*time.Second, func() bool { return g.peer.Requests() >= r0+10 })
	if raised := g.proximity(a.flight, "raised"); len(raised) != 0 {
		t.Fatalf("our own flight paired with its echo: %+v", raised[0].b)
	}
	for _, raw := range man() {
		if strings.Contains(string(raw), echoICAO) {
			t.Fatal("the ANSP's echo of our flight was published on man.v1")
		}
	}
	for _, raw := range peerMsgs() {
		if strings.Contains(string(raw), `"track_id":"`+a.flight+`"`) {
			t.Fatal("the peer's echo of our flight was published on peer.v1")
		}
	}
	if v := metric(t, g.monitor, "peer_echo_own_flight"); v == 0 {
		t.Fatal("peer echoes not counted")
	}
	if v := metric(t, g.monitor, "manned_echo_own_flight"); v == 0 {
		t.Fatal("manned echoes not counted")
	}
	// The twins: another aircraft at the same place from each input.
	other := "OTHER" + unique()[len(unique())-2:]
	g.ansp.SetAircraft(ansp.Aircraft{ICAO24: otherICAO, Callsign: &other, Position: g.o})
	peerOther := newUUID()
	g.peer.SetFlight(peersp.Flight{ID: peerOther, Position: g.o, AltHAEM: 600 + geoidUndulationM})
	seen := map[string]bool{}
	within(t, 15*time.Second, func() bool {
		for _, r := range g.proximity(a.flight, "raised") {
			if p := peerOf(r.b); p != nil {
				seen[fmt.Sprint(p["track_id"])] = true
			}
		}
		return seen[otherICAO] && seen[peerOther]
	})
}
