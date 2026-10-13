//go:build integration && lab

package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3411"

	"github.com/rootxkit/uspace-ussp/internal/app/monitor"
	"github.com/rootxkit/uspace-ussp/internal/app/ridsp"
	"github.com/rootxkit/uspace-ussp/internal/app/trafficws"
	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/cell"
	"github.com/rootxkit/uspace-ussp/internal/telemetry"
	"github.com/rootxkit/uspace-ussp/internal/traffic"
)

// The WP-14 lab scenario (INV-02; docs/RUNBOOKS/WP-14.md): S-M3 "a SITL
// aircraft and a lab manned track" (a CPA alert from WP-11 with the
// ANSP's stream as its input) and "a lab ADS-B e-conspicuity source
// feeds traffic information directly", S-M4 "peer flights via F3411 as
// provider". It runs in a container on the lab network (internal) with
// the lab's InterUSS DSS and issuer (uspace-lab deploy/dss-up.sh), the
// lab's manned simulators sim-ansp-feed and sim-adsb and its peer USSP
// sim-ussp, started here from their Linux binaries, and this USSP's
// monitor, rid-sp and traffic-ws in process on a NATS of this run.
// Two ArduCopter SITL vehicles fly in WSL: the lab's mav_reader writes
// their sim/vehicle/v1 lines to files this test tails (LAB_OWN_VEHICLE,
// LAB_PEER_VEHICLE): the first is one of this USSP's flights, published
// on trk.v1 as telemetry-ingest publishes an operator's samples; the
// second is sim-ussp's aircraft, sent to it as its vehicle stream.
//
// Environment: LAB_ROOT (an export of uspace-lab), LAB_BIN (its
// sim-ansp-feed, sim-adsb and sim-ussp for Linux), LAB_ISSUER (iss),
// LAB_ISSUER_BASE (http://lab-issuer:8080), LAB_SECRETS (the issuer's
// client-secrets.json), LAB_JWKS_FILE (its public JWKS), LAB_DSS_URL,
// LAB_SELF_HOST (this container's alias, also "ansp" and "sim-ussp"),
// LAB_OWN_VEHICLE, LAB_PEER_VEHICLE, USSP_TEST_NATS_URL; LAB_RX_RECORDING
// (the receiver's recording, default the stream's) and LAB_PEER_ISA
// (sim-ussp's ISA) are optional.
func TestLabWP14Inputs(t *testing.T) {
	root, bin := labEnv(t, "LAB_ROOT"), labEnv(t, "LAB_BIN")
	issuer, issuerBase := labEnv(t, "LAB_ISSUER"), labEnv(t, "LAB_ISSUER_BASE")
	secrets, jwksFile := labEnv(t, "LAB_SECRETS"), labEnv(t, "LAB_JWKS_FILE")
	dssURL, self := labEnv(t, "LAB_DSS_URL"), labEnv(t, "LAB_SELF_HOST")
	ownFile, peerFile := labEnv(t, "LAB_OWN_VEHICLE"), labEnv(t, "LAB_PEER_VEHICLE")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The lab issuer on a loopback address (uspace-core verifies a JWKS
	// over https or loopback http only): its JWKS and its token service.
	target, err := url.Parse(issuerBase)
	if err != nil {
		t.Fatal(err)
	}
	broker := httptest.NewServer(httputil.NewSingleHostReverseProxy(target))
	t.Cleanup(broker.Close)
	issuers := issuer + "=" + broker.URL + "/.well-known/jwks.json"
	var all map[string]string
	raw, err := os.ReadFile(secrets)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &all); err != nil {
		t.Fatal(err)
	}
	ours := all["ussp-DEV01-01"]
	if strings.TrimSpace(ours) == "" {
		t.Fatal("no secret for ussp-DEV01-01")
	}
	secretFile := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secretFile, []byte(strings.TrimSpace(ours)), 0o600); err != nil {
		t.Fatal(err)
	}

	// The lab's simulators, from their binaries.
	recording := filepath.Join(root, "testdata", "adsb", "synthetic-approach.jsonl")
	sims := []struct {
		name string
		args []string
		up   string
	}{
		{"sim-ansp-feed", []string{"--listen=0.0.0.0:8091", "--recording=" + recording, "--issuer=" + issuer,
			"--jwks-url=" + broker.URL + "/.well-known/jwks.json", "--audience=ansp", "--instance=lab-adsb-1",
			"--policy=" + filepath.Join(root, "scenarios", "policy", "demo.yaml")}, "http://127.0.0.1:8091/healthz"},
		{"sim-adsb", []string{"--listen=0.0.0.0:8092", "--recording=" + labEnvOr("LAB_RX_RECORDING", recording)}, "http://127.0.0.1:8092/data/aircraft.json"},
		{"sim-ussp", []string{"--listen=0.0.0.0:8093", "--base-url=http://sim-ussp:8093", "--dss-rid-base=" + dssURL + "/rid/v2",
			"--dss-utm-base=" + dssURL, "--dss-audience=dss", "--token-url=" + issuerBase + "/oauth/token", "--client-id=sim-ussp-01",
			"--client-secret-json=" + secrets, "--issuer=" + issuer, "--jwks-file=" + jwksFile, "--audience=sim-ussp",
			"--isa=" + labEnvOr("LAB_PEER_ISA", "41.7595,44.8600,2000,500,900"), "--intent=41.6500,44.9500,200,600,700",
			"--aircraft=2=TESTPEER-WP14-0001", "--vehicles=udp:127.0.0.1:15562"}, "http://127.0.0.1:8093/healthz"},
	}
	logs := map[string]*logBuffer{}
	for _, s := range sims {
		lb := &logBuffer{}
		logs[s.name] = lb
		cmd := exec.CommandContext(ctx, filepath.Join(bin, s.name), s.args...)
		cmd.Dir, cmd.Stdout, cmd.Stderr = root, lb, lb
		if err := cmd.Start(); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		t.Cleanup(func() { cancel(); _ = cmd.Wait() })
	}
	t.Cleanup(func() {
		if t.Failed() {
			for n, lb := range logs {
				t.Logf("%s log:\n%s", n, lb.String())
			}
		}
	})
	for _, s := range sims {
		within(t, 30*time.Second, func() bool {
			resp, err := http.Get(s.up)
			if err != nil {
				return false
			}
			_ = resp.Body.Close()
			return resp.StatusCode < 500
		})
	}
	started := time.Now()
	t.Logf("lab simulators up: sim-ansp-feed (the recording's T0), sim-adsb, sim-ussp")

	// Our bus, our own issuer (a staff session for traffic-ws), and the
	// own flight's intent in intent_active.
	nc := busConn(t, mustEnv(t, "USSP_TEST_NATS_URL"), true)
	if _, err := bus.Ensure(ctx, nc.JetStream(), bus.DefaultTopology()); err != nil {
		t.Fatal(err)
	}
	pub := bus.NewPublisher(nc, &core.Counters{})
	kv := bus.NewProjector(nc, nil)
	own := newOwnTokens(t)
	flight, intentID, client := newUUID(), newUUID(), "lab-operator-1"
	geoid := geoidFile(t)

	// This USSP: rid-sp (our uss_base_url), the monitor with every
	// WP-14 input on, traffic-ws.
	ridAddr := runAt(t, ridsp.Spec, map[string]string{"USSP_RID_SP_ADDR": "0.0.0.0:8095", "USSP_NATS_URL": mustEnv(t, "USSP_TEST_NATS_URL"),
		"USSP_AUDIENCES": self, "USSP_TOKEN_ISSUERS": issuers})
	_ = ridAddr
	monAddr, _, monLogs := runLogged(t, monitor.SpecWith(monitor.Options{}), map[string]string{
		"USSP_MONITOR_ADDR": "127.0.0.1:0", "USSP_NATS_URL": mustEnv(t, "USSP_TEST_NATS_URL"),
		"USSP_TOKEN_ISSUERS": issuers, "USSP_TOKEN_CLIENT_SECRET_FILE": secretFile, "USSP_SYSTEM_ID": "DEV01",
		"USSP_ANSP_STREAM_URL": "ws://ansp:8091/v1/manned-traffic/stream", "USSP_MTLS_MODE": "off",
		"USSP_ADSB_SOURCE": "http://127.0.0.1:8092/data/aircraft.json", "USSP_ADSB_RECEIVER_ID": "lab-rx-1",
		"USSP_DSS_BASE_URL": dssURL, "USSP_USS_BASE_URL": "http://" + self + ":8095",
		"USSP_TRAFFIC_INPUT_BBOX": "44.70,41.65,44.95,41.85", "USSP_GEOID_FILE": geoid,
	})
	mon := "http://" + monAddr
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("monitor log:\n%s", monLogs.String())
		}
	})
	wsAddr := runAt(t, trafficws.SpecWith(trafficws.Options{}), map[string]string{
		"USSP_TRAFFIC_WS_ADDR": "127.0.0.1:0", "USSP_NATS_URL": mustEnv(t, "USSP_TEST_NATS_URL"),
		"USSP_AUDIENCES": testHost, "USSP_TOKEN_ISSUERS": testIssuer + "=" + own.jwks, "USSP_ISSUER_URL": testIssuer,
		"USSP_WS_ALLOWED_ORIGINS": "https://console.test", "USSP_GEOID_FILE": geoid,
	})
	g := &trafficRig{t: t, nc: nc, pub: pub, kv: kv, own: own, ws: "ws://" + wsAddr, ss: compileSchemas(t)}

	// The proximity alerts of our flight, and every man.v1 and peer.v1.
	var mu sync.Mutex
	var alerts []recvProx
	stop, err := nc.Listen("alrt.v1.proximity.>", func(_ string, data []byte) {
		var m traffic.AlertMessage
		if json.Unmarshal(data, &m) == nil && m.Body.FlightID == flight {
			mu.Lock()
			alerts = append(alerts, recvProx{time.Now(), m.Body})
			mu.Unlock()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	man := listen(t, g, "man.v1.>")
	peerMsgs := listen(t, g, "peer.v1.>")

	// The SITL vehicles: the own one's lines onto trk.v1, the peer's to
	// sim-ussp; the intent of the own flight around its first position.
	first := make(chan core.LatLon, 1)
	go tailVehicles(ctx, t, ownFile, func(s vehicleLine) {
		select {
		case first <- s.pos():
		default:
		}
		publishOwn(ctx, pub, flight, intentID, client, s)
	})
	go tailVehicles(ctx, t, peerFile, func(s vehicleLine) { forwardPeer(s) })
	var home core.LatLon
	select {
	case home = <-first:
	case <-time.After(3 * time.Minute):
		t.Fatal("no own SITL sample within 3 min")
	}
	t.Logf("own SITL vehicle at %.6f, %.6f", home.LatDeg, home.LonDeg)
	g.o = home
	g.putIntent(trafficOperator{client: client, intent: intentID, flight: flight})

	// A staff subscription over the input box.
	staff := g.staffToken()
	s := g.dial("/v1/traffic?bbox=44.70,41.65,44.95,41.85", staff, true)

	// S-M3: the lab's ADS-B source feeds traffic information directly,
	// as broadcast from adsb_rx; the ANSP's stream as surveillance.
	sawRx, sawANSP := "", ""
	within(t, 60*time.Second, func() bool {
		p, _ := s.lastProduct()
		for _, tr := range p.Tracks {
			if tr.Source == traffic.SourceAdsbRx && tr.Trust == core.TrustBroadcast && sawRx == "" {
				sawRx = fmt.Sprintf("%s at %.0f s, alt_source %s", tr.TrackID, time.Since(started).Seconds(), tr.AltSource)
			}
			if tr.Source == traffic.SourceANSPFeed && tr.Trust == core.TrustSurveillance && sawANSP == "" {
				sawANSP = fmt.Sprintf("%s at %.0f s, alt_source %s", tr.TrackID, time.Since(started).Seconds(), tr.AltSource)
			}
		}
		return sawRx != "" && sawANSP != ""
	})
	t.Logf("product: e-conspicuity track %s; ANSP track %s", sawRx, sawANSP)
	rx, ansp := 0, 0
	for _, raw := range man() {
		switch {
		case bytes.Contains(raw, []byte(`"source":"adsb_rx"`)):
			rx++
		case bytes.Contains(raw, []byte(`"source":"ansp_feed"`)):
			ansp++
		}
	}
	t.Logf("man.v1: %d from adsb_rx (broadcast), %d from ansp_feed (surveillance)", rx, ansp)

	// S-M4: sim-ussp's SITL aircraft through F3411 as provider.
	sawPeer := ""
	within(t, 150*time.Second, func() bool {
		p, _ := s.lastProduct()
		for _, tr := range p.Tracks {
			if tr.Source == "network_rid" && tr.Trust == core.TrustProvider {
				sawPeer = fmt.Sprintf("%s age %.1f s at %.0f s", tr.TrackID, tr.AgeS, time.Since(started).Seconds())
				return true
			}
		}
		return false
	})
	t.Logf("product: peer flight %s; %d peer.v1 messages", sawPeer, len(peerMsgs()))
	st, d := dep(mon, monitor.DepNetworkRID)
	t.Logf("network_rid %s: %s", st, d)

	// S-M3: the SITL aircraft and the lab manned track: a proximity
	// alert from the CPA path, and its clear.
	within(t, 5*time.Minute, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, a := range alerts {
			if a.b.State == "raised" {
				return true
			}
		}
		return false
	})
	mu.Lock()
	var raised recvProx
	for _, a := range alerts {
		if a.b.State == "raised" {
			raised = a
			break
		}
	}
	mu.Unlock()
	rawAlert, _ := json.Marshal(raised.b)
	t.Logf("proximity raised at %s (%.0f s after the feed's T0): %s", raised.at.UTC().Format(time.RFC3339Nano), raised.at.Sub(started).Seconds(), rawAlert)
	within(t, 5*time.Minute, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, a := range alerts {
			if a.b.State == "cleared" && a.b.AlertID == raised.b.AlertID {
				return true
			}
		}
		return false
	})
	mu.Lock()
	for _, a := range alerts {
		if a.b.State == "cleared" && a.b.AlertID == raised.b.AlertID {
			rawAlert, _ = json.Marshal(a.b)
			t.Logf("proximity cleared at %s, %.1f s after the raise: %s", a.at.UTC().Format(time.RFC3339Nano), a.at.Sub(raised.at).Seconds(), rawAlert)
			break
		}
	}
	mu.Unlock()
	for _, dn := range []string{monitor.DepANSPFeed, monitor.DepAdsbRx, monitor.DepNetworkRID} {
		st, d := dep(mon, dn)
		t.Logf("monitor /readyz %s: %s %s", dn, st, d)
	}
}

func labEnvOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// vehicleLine is the part of a sim/vehicle/v1 line read here.
type vehicleLine struct {
	Sysid    int      `json:"sysid"`
	TS       *string  `json:"ts"`
	LatDeg   float64  `json:"lat_deg"`
	LonDeg   float64  `json:"lon_deg"`
	AltAMSLM float64  `json:"alt_amsl_m"`
	AltHAEM  *float64 `json:"alt_hae_m"`
	SpeedMS  float64  `json:"speed_ms"`
	TrackDeg *float64 `json:"track_deg"`
	VSpeedMS float64  `json:"vspeed_ms"`
	Armed    bool     `json:"armed"`
	FixOK    bool     `json:"fix_ok"`
	line     []byte
}

func (v vehicleLine) pos() core.LatLon { return core.LatLon{LatDeg: v.LatDeg, LonDeg: v.LonDeg} }

// tailVehicles follows a file mav_reader writes, from its end, and hands
// over every line with a fix.
func tailVehicles(ctx context.Context, t *testing.T, path string, take func(vehicleLine)) {
	var f *os.File
	for f == nil {
		var err error
		if f, err = os.Open(path); err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
	defer f.Close()
	_, _ = f.Seek(0, io.SeekEnd)
	r := bufio.NewReader(f)
	var pending []byte
	for ctx.Err() == nil {
		chunk, err := r.ReadBytes('\n')
		pending = append(pending, chunk...)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
			continue
		}
		var v vehicleLine
		if json.Unmarshal(pending, &v) == nil && v.FixOK {
			v.line = append([]byte(nil), pending...)
			take(v)
		}
		pending = pending[:0]
	}
}

// publishOwn publishes one own SITL sample on trk.v1 as telemetry-ingest
// publishes an operator's sample (trust authenticated, operator_ws, the
// flight and its intent), placed at its receipt.
func publishOwn(ctx context.Context, pub *bus.Publisher, flight, intentID, client string, v vehicleLine) {
	now := time.Now()
	p := v.pos()
	c5, _, err := cell.Key(p)
	if err != nil {
		return
	}
	alt, sp, vs := v.AltAMSLM, v.SpeedMS, v.VSpeedMS
	td := 0.0
	if v.TrackDeg != nil && *v.TrackDeg >= 0 && *v.TrackDeg < 360 {
		td = *v.TrackDeg
	}
	status := string(f3411.Ground)
	if v.Armed {
		status = string(f3411.Airborne)
	}
	m := &telemetry.Track{
		Envelope: bus.NewEnvelope(telemetry.SchemaTrack, telemetry.Producer, core.Times{TS: &now, RxTS: now, CapturedAt: now, Source: core.TimeSourceClock}),
		Body: telemetry.TrackBody{
			TrackID: flight, Trust: core.TrustAuthenticated, Source: telemetry.SourceOperatorWS, SourceInstance: client,
			Position: telemetry.Position{Lat: p.LatDeg, Lng: p.LonDeg}, AltAMSLM: &alt, AltSource: core.AltGeodetic,
			SpeedMS: &sp, TrackDeg: &td, VSpeedMS: &vs, Status: &status, FlightID: &flight, IntentID: &intentID,
			Identification: core.Identification{Status: core.IdentRegistered, Reason: core.ReasonSessionBinding, Basis: core.BasisAuthenticated},
		},
	}
	if subject, err := bus.Trk(c5, flight); err == nil {
		_ = pub.Publish(ctx, subject, m)
	}
}

// peerConn sends the peer vehicle's lines to sim-ussp's vehicle stream.
var peerConn struct {
	once sync.Once
	c    interface{ Write([]byte) (int, error) }
}

func forwardPeer(v vehicleLine) {
	peerConn.once.Do(func() {
		c, err := net.Dial("udp", "127.0.0.1:15562")
		if err == nil {
			peerConn.c = c
		}
	})
	if peerConn.c != nil {
		line := v.line
		// sim-ussp serves sysid 2 (--aircraft 2=...).
		line = bytes.Replace(line, []byte(fmt.Sprintf(`"sysid":%d`, v.Sysid)), []byte(`"sysid":2`), 1)
		_, _ = peerConn.c.Write(line)
	}
}
