package smoke

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"

	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/dss/fakedss"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/ansp"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/authority"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/cisp"
)

// tokenIssuer is the iss of the peers' token service: a name only, never
// resolved (its JWKS URL is what the stack fetches).
const tokenIssuer = "https://authority.smoke.test"

// The zone the CISP publishes: 3 km north of where the operator flies
// (smokeLat, smokeLng), so the intent is authorised beside a published
// zone rather than in an empty CIS.
var zoneBox = [4]float64{smokeLat + 0.027, smokeLng, smokeLat + 0.037, smokeLng + 0.015}

// TestStagingSmokePeers serves the peers until the control listener's
// POST /stop, SIGTERM or the test's deadline. Environment (run.sh sets
// it): STAGING_SMOKE_PEERS=1, STAGING_SMOKE_PEERS_BIND (the address the
// proxies listen on), STAGING_SMOKE_PEERS_HOST (the name the containers
// reach that address by), STAGING_SMOKE_PEERS_PORT (the first of five
// proxy ports), STAGING_SMOKE_CONTROL (the control listener, a loopback
// address), STAGING_SMOKE_PEERS_ENV (where the settings are written),
// STAGING_SMOKE_PEERS_CERT and _KEY (the proxies' certificate for the
// host name, issued by the smoke's root: the processes fetch JWKS over
// https only).
func TestStagingSmokePeers(t *testing.T) {
	if os.Getenv("STAGING_SMOKE_PEERS") != "1" {
		t.Skip("STAGING_SMOKE_PEERS is not 1: run by deploy/smoke/run.sh")
	}
	bind, host, envFile, control := need(t, "STAGING_SMOKE_PEERS_BIND"), need(t, "STAGING_SMOKE_PEERS_HOST"),
		need(t, "STAGING_SMOKE_PEERS_ENV"), need(t, "STAGING_SMOKE_CONTROL")
	certFile, keyFile := need(t, "STAGING_SMOKE_PEERS_CERT"), need(t, "STAGING_SMOKE_PEERS_KEY")
	if h, _, err := net.SplitHostPort(control); err != nil || !net.ParseIP(h).IsLoopback() {
		t.Fatalf("STAGING_SMOKE_CONTROL %q is not a loopback address", control)
	}
	first, err := strconv.Atoi(need(t, "STAGING_SMOKE_PEERS_PORT"))
	if err != nil || first <= 0 || first > 65530 {
		t.Fatalf("STAGING_SMOKE_PEERS_PORT %q is not a port", os.Getenv("STAGING_SMOKE_PEERS_PORT"))
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	sk, err := auth.NewSigningKey(key)
	if err != nil {
		t.Fatal(err)
	}
	iss, err := coreauth.NewIssuer(tokenIssuer, key, sk.KID)
	if err != nil {
		t.Fatal(err)
	}
	// The token service answers every client with the bearer the other
	// fakes accept; this USSP's own client id and secret are not checked
	// here (the authority's token service is not under test).
	tokens := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/oauth/token" {
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": cisp.Token, "token_type": "Bearer", "expires_in": 3600})
			return
		}
		_ = json.NewEncoder(w).Encode(iss.JWKS())
	})
	registry := authority.New()
	defer registry.Close()
	registry.AcceptBearer(cisp.Token)
	cis, err := cisp.New()
	if err != nil {
		t.Fatal(err)
	}
	defer cis.Close()
	cis.Publish("zones", zone("TZSMK01", "PROHIBITED", zoneBox))
	cis.Publish("uspace_airspace")
	cis.Publish("restrictions")
	dss := fakedss.New()
	defer dss.Close()
	manned, err := ansp.New()
	if err != nil {
		t.Fatal(err)
	}
	defer manned.Close()

	// One proxy per peer on the address the containers reach.
	base := func(i int) string { return "https://" + net.JoinHostPort(host, strconv.Itoa(first+i)) }
	targets := []struct {
		name    string
		handler http.Handler
	}{
		{"token service", tokens},
		{"authority", proxyTo(t, registry.URL())},
		{"cisp", proxyTo(t, cis.URL())},
		{"dss", proxyTo(t, dss.URL())},
		{"ansp", proxyTo(t, manned.URL())},
	}
	var servers []*http.Server
	for i, tg := range targets {
		ln, err := net.Listen("tcp", net.JoinHostPort(bind, strconv.Itoa(first+i)))
		if err != nil {
			t.Fatalf("%s: %v", tg.name, err)
		}
		s := &http.Server{Handler: tg.handler, ReadHeaderTimeout: 5 * time.Second}
		servers = append(servers, s)
		go func() { _ = s.ServeTLS(ln, certFile, keyFile) }()
	}
	defer func() {
		for _, s := range servers {
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = s.Shutdown(sctx)
			cancel()
		}
	}()

	// The settings that point the stack at them (run.sh appends them to
	// the smoke's settings file). Every URL is the proxy's.
	vars := []string{
		"USSP_TOKEN_ISSUERS=" + tokenIssuer + "=" + base(0) + "/.well-known/jwks.json",
		"USSP_AUTHORITY_BASE_URL=" + base(1),
		"USSP_CISP_BASE_URL=" + base(2),
		"USSP_CIS_NOTIFY_ISSUERS=" + cis.Signer.Issuer + "=" + base(2) + "/.well-known/jwks.json",
		"USSP_CIS_PUBLISHER_KEYS=" + strings.ReplaceAll(cis.PublisherKeysEnv(), cis.URL(), base(2)),
		"USSP_DSS_BASE_URL=" + base(3),
		"USSP_ANSP_BASE_URL=" + base(4),
		"USSP_ANSP_STREAM_URL=" + strings.ReplaceAll(manned.StreamURL(), "ws://"+strings.TrimPrefix(manned.URL(), "http://"), "wss://"+strings.TrimPrefix(base(4), "https://")),
		"USSP_CIS_RECONCILE_S=5",
		fmt.Sprintf("USSP_TRAFFIC_INPUT_BBOX=%.3f,%.3f,%.3f,%.3f", smokeLng-0.4, smokeLat-0.2, smokeLng+0.4, smokeLat+0.3),
	}
	tmp := envFile + ".partial"
	if err := os.WriteFile(tmp, []byte(strings.Join(vars, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, envFile); err != nil {
		t.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if dl, ok := t.Deadline(); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, dl.Add(-10*time.Second))
		defer cancel()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("POST /registry", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Operator, Serial string }
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil || (in.Operator == "" && in.Serial == "") {
			http.Error(w, "want {operator, serial}", http.StatusBadRequest)
			return
		}
		if in.Operator != "" {
			registry.SetOperator(in.Operator, "active", nil)
		}
		if in.Serial != "" {
			registry.SetUAS(in.Serial, "active", "", "")
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /status-notices", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(registry.StatusNotices())
	})
	mux.HandleFunc("POST /stop", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
		cancel()
	})
	cln, err := net.Listen("tcp", control)
	if err != nil {
		t.Fatal(err)
	}
	cs := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = cs.Serve(cln) }()
	t.Logf("peers up: %s (%s) ports %d-%d, control %s, settings %s", bind, host, first, first+len(targets)-1, control, filepath.Base(envFile))
	<-ctx.Done()
	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	if err := cs.Shutdown(sctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Logf("control shutdown: %v", err)
	}
	t.Logf("peers stopped: %v", context.Cause(ctx))
}

func need(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Fatalf("%s is not set", name)
	}
	return v
}

// proxyTo forwards to a fake's own listener (a loopback address the
// containers cannot reach), WebSocket upgrades included.
func proxyTo(t *testing.T, target string) http.Handler {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	return httputil.NewSingleHostReverseProxy(u)
}

// zone is an ED-318 feature over [lat0, lon0, lat1, lon1] from 0 to 120
// m AGL (test/e2e/stack's).
func zone(id, typ string, b [4]float64) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"type": "Feature",
		"geometry": map[string]any{"type": "Polygon",
			"coordinates": []any{[]any{[]any{b[1], b[0]}, []any{b[3], b[0]}, []any{b[3], b[2]}, []any{b[1], b[2]}, []any{b[1], b[0]}}},
			"layer":       map[string]any{"lower": 0, "lowerReference": "AGL", "upper": 120, "upperReference": "AGL", "uom": "m"}},
		"properties": map[string]any{"identifier": id, "country": "GEO", "type": typ, "variant": "COMMON",
			"name":   []any{map[string]any{"text": "Smoke zone " + id, "lang": "en-GB"}, map[string]any{"text": "Smoke ზონა " + id, "lang": "ka-GE"}},
			"reason": []string{"SENSITIVE"},
			"zoneAuthority": []any{map[string]any{"name": []any{map[string]any{"text": "Test authority", "lang": "en-GB"}},
				"purpose": "AUTHORIZATION"}}},
	})
	return raw
}
