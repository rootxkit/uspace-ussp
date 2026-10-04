// Command stack is the portal's browser-test stack (brief WP-17): the
// seven processes of cmd/ussp-dev in one process against the real
// PostgreSQL + PostGIS, TimescaleDB and NATS of the caller (the CI web
// job's service containers), with the fakes of internal/testfakes for
// the CISP, the authority (token service, JWKS and F8 registry) and
// internal/dss/fakedss for the DSS; one same-origin front for the
// browser (the web app, and the traffic and alert WebSockets of
// traffic-ws, as the deployment's Caddy routes them); and a control
// listener the Playwright test drives: mark a registration number and a
// serial valid in the fake registry, fly a simulated operator client
// over the operator WebSocket, and bring a second operator's aircraft
// beside it. Test-only: it never ships in an image and has no send path
// to an aircraft (the simulated operator only sends to this USSP).
//
// Environment: USSP_TEST_PG_URL, USSP_TEST_TS_OWNER_URL, USSP_TEST_NATS_URL
// (as make integration reads them); E2E_FRONT_ADDR (the browser's
// origin, default 127.0.0.1:3200), E2E_WEB_URL (the Next.js server the
// front forwards pages to, default http://127.0.0.1:3100),
// E2E_CONTROL_ADDR (default 127.0.0.1:3290), E2E_PORT_BASE (the first
// of the seven process ports, default 3301).
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"
	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/geodesy"

	"github.com/rootxkit/uspace-ussp/internal/app/api"
	"github.com/rootxkit/uspace-ussp/internal/app/dsssync"
	"github.com/rootxkit/uspace-ussp/internal/app/monitor"
	"github.com/rootxkit/uspace-ussp/internal/app/proc"
	"github.com/rootxkit/uspace-ussp/internal/app/ridsp"
	"github.com/rootxkit/uspace-ussp/internal/app/telemetryingest"
	"github.com/rootxkit/uspace-ussp/internal/app/trafficws"
	"github.com/rootxkit/uspace-ussp/internal/app/tsdbwriter"
	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/config"
	"github.com/rootxkit/uspace-ussp/internal/dss/fakedss"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/authority"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/cisp"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/operator"
)

const (
	audience = "ussp.e2e.test"
	issuerAt = "https://authority.e2e.test"
)

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func must(name string) (string, error) {
	v := os.Getenv(name)
	if v == "" {
		return "", fmt.Errorf("%s is not set", name)
	}
	return v, nil
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := 0
	if err := run(ctx, logger); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("e2e stack failed", slog.String("error", err.Error()))
		code = 1
	}
	stop()
	os.Exit(code)
}

// fakes are the external systems.
type fakes struct {
	tokens   *httptest.Server
	jwks     string
	registry *authority.Fake
	cisp     *cisp.Fake
	dss      *fakedss.DSS
}

func (f *fakes) close() {
	f.tokens.Close()
	f.registry.Close()
	f.cisp.Close()
	f.dss.Close()
}

// startFakes starts the token service (the ecosystem issuer's JWKS and
// POST /oauth/token answering the fakes' bearer), the F8 registry, the
// CISP with one PROHIBITED zone, no U-space airspace and no restriction,
// and the DSS.
func startFakes(zoneAt core.LatLon) (*fakes, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	sk, err := auth.NewSigningKey(key)
	if err != nil {
		return nil, err
	}
	iss, err := coreauth.NewIssuer(issuerAt, key, sk.KID)
	if err != nil {
		return nil, err
	}
	f := &fakes{}
	f.tokens = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/oauth/token" {
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": cisp.Token, "token_type": "Bearer", "expires_in": 3600})
			return
		}
		_ = json.NewEncoder(w).Encode(iss.JWKS())
	}))
	f.jwks = f.tokens.URL + "/.well-known/jwks.json"
	f.registry = authority.New()
	f.registry.AcceptBearer(cisp.Token)
	if f.cisp, err = cisp.New(); err != nil {
		return nil, err
	}
	box := [4]float64{zoneAt.LatDeg, zoneAt.LonDeg, zoneAt.LatDeg + 0.01, zoneAt.LonDeg + 0.015}
	f.cisp.Publish("zones", zone("TZE2E01", "PROHIBITED", box))
	f.cisp.Publish("uspace_airspace")
	f.cisp.Publish("restrictions")
	f.dss = fakedss.New()
	return f, nil
}

// zone is an ED-318 feature over [lat0, lon0, lat1, lon1] from 0 to 120
// m AGL.
func zone(id, typ string, b [4]float64) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"type": "Feature",
		"geometry": map[string]any{"type": "Polygon",
			"coordinates": []any{[]any{[]any{b[1], b[0]}, []any{b[3], b[0]}, []any{b[3], b[2]}, []any{b[1], b[2]}, []any{b[1], b[0]}}},
			"layer":       map[string]any{"lower": 0, "lowerReference": "AGL", "upper": 120, "upperReference": "AGL", "uom": "m"}},
		"properties": map[string]any{"identifier": id, "country": "GEO", "type": typ, "variant": "COMMON",
			"name":   []any{map[string]any{"text": "E2E zone " + id, "lang": "en-GB"}, map[string]any{"text": "E2E ზონა " + id, "lang": "ka-GE"}},
			"reason": []string{"SENSITIVE"},
			"zoneAuthority": []any{map[string]any{"name": []any{map[string]any{"text": "Test authority", "lang": "en-GB"}},
				"purpose": "AUTHORIZATION"}}},
	})
	return raw
}

// files writes the issuer key, the client secret of the outgoing token
// and a flat geoid grid (20 m) into dir.
func files(dir string) (keyFile, secretFile, geoidFile string, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", "", err
	}
	keyFile = filepath.Join(dir, "issuer-key.pem")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600); err != nil {
		return "", "", "", err
	}
	secretFile = filepath.Join(dir, "client-secret")
	if err := os.WriteFile(secretFile, []byte("e2e-client-secret\n"), 0o600); err != nil {
		return "", "", "", err
	}
	var g bytes.Buffer
	g.WriteString("P5\n# Description e2e grid, N = 20 m\n# Offset 20\n# Scale 1\n2 3\n65535\n")
	g.Write(make([]byte, 2*2*3))
	geoidFile = filepath.Join(dir, "geoid.pgm")
	if err := os.WriteFile(geoidFile, g.Bytes(), 0o600); err != nil {
		return "", "", "", err
	}
	return keyFile, secretFile, geoidFile, nil
}

func run(ctx context.Context, logger *slog.Logger) error {
	pg, err := must("USSP_TEST_PG_URL")
	if err != nil {
		return err
	}
	ts, err := must("USSP_TEST_TS_OWNER_URL")
	if err != nil {
		return err
	}
	natsURL, err := must("USSP_TEST_NATS_URL")
	if err != nil {
		return err
	}
	front := env("E2E_FRONT_ADDR", "127.0.0.1:3200")
	webURL := env("E2E_WEB_URL", "http://127.0.0.1:3100")
	controlAddr := env("E2E_CONTROL_ADDR", "127.0.0.1:3290")
	base, err := strconv.Atoi(env("E2E_PORT_BASE", "3301"))
	if err != nil {
		return fmt.Errorf("E2E_PORT_BASE: %w", err)
	}
	addr := func(i int) string { return "127.0.0.1:" + strconv.Itoa(base+i) }

	dir, err := os.MkdirTemp("", "ussp-e2e-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	keyFile, secretFile, geoidFile, err := files(dir)
	if err != nil {
		return err
	}
	// The zone of the geo page sits 3 km north of the test's flights.
	f, err := startFakes(core.LatLon{LatDeg: 41.745, LonDeg: 44.80})
	if err != nil {
		return err
	}
	defer f.close()

	vars := map[string]string{
		"USSP_LOG_LEVEL":                "warn",
		"USSP_STATUS_INTERVAL_S":        "3600",
		"USSP_API_ADDR":                 addr(0),
		"USSP_TELEMETRY_INGEST_ADDR":    addr(1),
		"USSP_RID_SP_ADDR":              addr(2),
		"USSP_MONITOR_ADDR":             addr(3),
		"USSP_TRAFFIC_WS_ADDR":          addr(4),
		"USSP_DSS_SYNC_ADDR":            addr(5),
		"USSP_TSDB_WRITER_ADDR":         addr(6),
		"USSP_PG_URL":                   pg,
		"USSP_TS_URL":                   ts,
		"USSP_NATS_URL":                 natsURL,
		"USSP_AUDIENCES":                audience,
		"USSP_ISSUER_URL":               "https://" + audience,
		"USSP_ISSUER_KEY_FILE":          keyFile,
		"USSP_TOKEN_ISSUERS":            issuerAt + "=" + f.jwks,
		"USSP_TOKEN_CLIENT_SECRET_FILE": secretFile,
		"USSP_USS_BASE_URL":             "https://" + audience,
		"USSP_DSS_BASE_URL":             f.dss.URL(),
		"USSP_CISP_BASE_URL":            f.cisp.URL(),
		"USSP_CIS_NOTIFY_ISSUERS":       f.cisp.Signer.Issuer + "=" + f.cisp.URL() + "/.well-known/jwks.json",
		"USSP_CIS_PUBLISHER_KEYS":       f.cisp.PublisherKeysEnv(),
		"USSP_CIS_RECONCILE_S":          "5",
		"USSP_AUTHORITY_BASE_URL":       f.registry.URL(),
		"USSP_GEOID_FILE":               geoidFile,
		"USSP_MTLS_MODE":                "off",
		"USSP_WS_ALLOWED_ORIGINS":       "http://" + front,
		"USSP_TRUSTED_PROXIES":          "127.0.0.1",
	}
	lookup := func(n string) (string, bool) { v, ok := vars[n]; return v, ok }
	for _, spec := range []proc.Spec{api.Spec, tsdbwriter.Spec} {
		var out bytes.Buffer
		if c := proc.Main(ctx, spec, []string{"migrate"}, &out, &out, lookup); c != proc.ExitOK {
			return fmt.Errorf("migrate %s exited %d: %s", spec.Process, c, out.String())
		}
	}
	// Each process with its own configuration, as the deployment runs
	// them: the processes without the issuer key verify this USSP's own
	// tokens and sessions (operator tokens, portal sessions) through
	// api's JWKS, which api itself must not list (cmd/ussp-dev shares one
	// configuration, so it cannot run them so).
	own := vars["USSP_TOKEN_ISSUERS"] + ",https://" + audience + "=http://" + addr(0) + "/.well-known/jwks.json"
	specs := []proc.Spec{api.Spec, telemetryingest.Spec, ridsp.Spec, monitor.Spec, trafficws.Spec, dsssync.Spec, tsdbwriter.Spec}
	pctx, cancelAll := context.WithCancel(ctx)
	defer cancelAll()
	done := make(chan error, len(specs))
	start := func(spec proc.Spec) error {
		pv := maps.Clone(vars)
		if spec.Process != api.Spec.Process {
			pv["USSP_TOKEN_ISSUERS"] = own
		}
		cfg, err := config.LoadFrom(func(n string) (string, bool) { v, ok := pv[n]; return v, ok })
		if err != nil {
			return fmt.Errorf("%s: %w", spec.Process, err)
		}
		if err := cfg.Require(spec.Process); err != nil {
			return fmt.Errorf("%s: %w", spec.Process, err)
		}
		go func() { done <- proc.Run(pctx, cfg, spec, proc.Options{Out: os.Stdout}) }()
		return nil
	}
	// api first: the others read its JWKS when they start.
	if err := start(specs[0]); err != nil {
		return err
	}
	if err := waitFor(ctx, done, "http://"+addr(0)+"/.well-known/jwks.json"); err != nil {
		return err
	}
	for i, spec := range specs[1:] {
		if err := start(spec); err != nil {
			return err
		}
		if err := waitFor(ctx, done, "http://"+addr(i+1)+"/healthz"); err != nil {
			return err
		}
	}
	apiURL := "http://" + addr(0)
	c := &control{logger: logger, registry: f.registry, apiURL: apiURL, telemetryURL: "http://" + addr(1)}
	frontSrv, err := frontServer(webURL, "http://"+addr(4))
	if err != nil {
		return err
	}
	servers := []*http.Server{{Addr: front, Handler: frontSrv, ReadHeaderTimeout: 5 * time.Second},
		{Addr: controlAddr, Handler: c.routes(), ReadHeaderTimeout: 5 * time.Second}}
	for _, s := range servers {
		ln, err := net.Listen("tcp", s.Addr)
		if err != nil {
			return err
		}
		go func() { _ = s.Serve(ln) }()
	}
	logger.Info("e2e stack up", slog.String("front", "http://"+front), slog.String("api", apiURL), slog.String("control", "http://"+controlAddr))
	var failed error
	select {
	case failed = <-done:
	case <-ctx.Done():
	}
	cancelAll()
	c.stopAll()
	for _, s := range servers {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = s.Shutdown(sctx)
		cancel()
	}
	if failed != nil && ctx.Err() == nil {
		return fmt.Errorf("a process stopped: %w", failed)
	}
	return ctx.Err()
}

// startupBound is how long a process may take to answer after it was
// started.
const startupBound = 90 * time.Second

// waitFor polls url until it answers 200, a process stops, or the
// bound passes.
func waitFor(ctx context.Context, done <-chan error, url string) error {
	deadline := time.Now().Add(startupBound)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		if res, err := http.DefaultClient.Do(req); err == nil {
			_ = res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not answer 200 within %s", url, startupBound)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-done:
			return fmt.Errorf("a process stopped while starting: %w", err)
		case <-tick.C:
		}
	}
}

// frontServer is the browser's one origin, as the deployment's Caddy
// routes it: the traffic and alert streams (WebSocket upgrades pass
// through with Origin intact) and the snapshot to traffic-ws; /basemap/
// to nothing (the lab's bundle is not part of this stack: the map says
// it has no basemap, the layers still draw); everything else to the web
// app.
func frontServer(webURL, trafficURL string) (http.Handler, error) {
	web, err := url.Parse(webURL)
	if err != nil {
		return nil, err
	}
	tw, err := url.Parse(trafficURL)
	if err != nil {
		return nil, err
	}
	toWeb := httputil.NewSingleHostReverseProxy(web)
	toTraffic := httputil.NewSingleHostReverseProxy(tw)
	mux := http.NewServeMux()
	mux.Handle("/v1/traffic", toTraffic)
	mux.Handle("/v1/traffic/snapshot", toTraffic)
	mux.Handle("/v1/alerts", toTraffic)
	mux.HandleFunc("/basemap/", http.NotFound)
	mux.Handle("/", toWeb)
	return mux, nil
}

// control is the test's handle on the stack.
type control struct {
	logger       *slog.Logger
	registry     *authority.Fake
	apiURL       string
	telemetryURL string

	mu      sync.Mutex
	flights []context.CancelFunc
}

func (c *control) stopAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, stop := range c.flights {
		stop()
	}
	c.flights = nil
}

func (c *control) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("POST /registry", c.handle(c.registryValid))
	mux.HandleFunc("POST /fly", c.handle(c.fly))
	mux.HandleFunc("POST /intruder", c.handle(c.intruder))
	return mux
}

const maxControlBody = 16 << 10

// handle decodes a bounded JSON body into a map and answers fn's result.
func (c *control) handle(fn func(context.Context, map[string]any) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		if err := json.NewDecoder(io.LimitReader(r.Body, maxControlBody)).Decode(&in); err != nil {
			http.Error(w, "body: "+err.Error(), http.StatusBadRequest)
			return
		}
		out, err := fn(r.Context(), in)
		if err != nil {
			c.logger.Error("control", slog.String("path", r.URL.Path), slog.String("error", err.Error()))
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}
}

func str(in map[string]any, k string) string { s, _ := in[k].(string); return s }

func num(in map[string]any, k string) float64 { f, _ := in[k].(float64); return f }

// registryValid makes the fake registry hold an operator number and a
// UAS serial as valid.
func (c *control) registryValid(_ context.Context, in map[string]any) (any, error) {
	if op := str(in, "operator"); op != "" {
		c.registry.SetOperator(op, "active", nil)
	}
	if sn := str(in, "serial"); sn != "" {
		c.registry.SetUAS(sn, "active", "", "")
	}
	return map[string]any{"ok": true}, nil
}

// call sends a JSON request to the api process.
func (c *control) call(ctx context.Context, method, path, bearer string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.apiURL+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %d %s", method, path, res.StatusCode, raw)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func (c *control) token(ctx context.Context, id, secret string) (string, error) {
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {id}, "client_secret": {secret}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = res.Body.Close() }()
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if res.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return "", fmt.Errorf("token: %d %s", res.StatusCode, raw)
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<16)).Decode(&tok); err != nil {
		return "", err
	}
	return tok.AccessToken, nil
}

// fly streams the client's serial at (lat, lng) for intent_id at 1 Hz over WS
// /v1/telemetry until the stack stops: the simulated operator client of
// internal/testfakes/operator, an ordinary client credential.
func (c *control) fly(ctx context.Context, in map[string]any) (any, error) {
	tok, err := c.token(ctx, str(in, "client_id"), str(in, "client_secret"))
	if err != nil {
		return nil, err
	}
	return c.start(tok, str(in, "serial"), str(in, "intent_id"), num(in, "lat"), num(in, "lng"))
}

// start flies serial at (lat, lng), each sample naming intentID (the
// operator client says which intent it flies; telemetry-ingest binds the
// flight to it when it may fly it).
func (c *control) start(tok, serial, intentID string, lat, lng float64) (any, error) {
	fctx, cancel := context.WithCancel(context.Background())
	op := operator.New(c.telemetryURL, tok, lat, lng, serial)
	if intentID != "" {
		op.IntentID = &intentID
	}
	if err := op.Up(fctx); err != nil {
		cancel()
		return nil, err
	}
	c.mu.Lock()
	c.flights = append(c.flights, func() { cancel(); op.Down() })
	c.mu.Unlock()
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-fctx.Done():
				return
			case now := <-t.C:
				if _, err := op.Tick(fctx, now); err != nil && fctx.Err() == nil {
					c.logger.Warn("telemetry tick", slog.String("serial", serial), slog.String("error", err.Error()))
				}
			}
		}
	}()
	return map[string]any{"flying": serial}, nil
}

// intruder registers a second operator through the API, gives it a
// client and a serial, files and activates an intent of its own 2 km
// east of (lat, lng) (so it does not meet the first operator's intent
// strategically), and flies its aircraft east_m metres east of (lat,
// lng), beside the first aircraft.
func (c *control) intruder(ctx context.Context, in map[string]any) (any, error) {
	u := strconv.FormatInt(time.Now().UnixNano()%1_000_000_000, 10)
	number, serial := "GEO-TEST-E2EB"+u, "TESTE2EB"+u
	c.registry.SetOperator(number, "active", nil)
	c.registry.SetUAS(serial, "active", "", "")
	user, pass := "intruder."+u, "intruder-password-"+u
	var op struct {
		ID string `json:"id"`
	}
	if err := c.call(ctx, http.MethodPost, "/v1/accounts/operators", "", map[string]any{"registration_number": number,
		"display_name": "E2E operator B " + u, "contact_email": "b" + u + "@example.test", "admin_username": user, "admin_password": pass}, &op); err != nil {
		return nil, err
	}
	var sess struct {
		Token string `json:"token"`
	}
	if err := c.call(ctx, http.MethodPost, "/v1/accounts/login", "", map[string]any{"realm": "portal", "username": user, "password": pass}, &sess); err != nil {
		return nil, err
	}
	var cl struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := c.call(ctx, http.MethodPost, "/v1/accounts/operators/"+op.ID+"/clients", sess.Token,
		map[string]any{"scopes": []string{"ussp.intents", "ussp.telemetry", "ussp.traffic"}}, &cl); err != nil {
		return nil, err
	}
	if err := c.call(ctx, http.MethodPost, "/v1/accounts/operators/"+op.ID+"/clients/"+cl.ClientID+"/serials", sess.Token,
		map[string]any{"serial": serial}, nil); err != nil {
		return nil, err
	}
	tok, err := c.token(ctx, cl.ClientID, cl.ClientSecret)
	if err != nil {
		return nil, err
	}
	at := core.LatLon{LatDeg: num(in, "lat"), LonDeg: num(in, "lng")}
	far := geodesy.Destination(at, 90, 2000)
	start := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
	d := 0.002
	var dec struct {
		IntentID string `json:"intent_id"`
		Decision string `json:"decision"`
	}
	if err := c.call(ctx, http.MethodPost, "/v1/intents", tok, map[string]any{
		"client_ref": "intruder-" + u, "uas_serial": serial, "mode": "VLOS", "flight_type": "normal", "category": "specific",
		"volumes": []any{map[string]any{
			"volume": map[string]any{
				"outline_polygon": map[string]any{"vertices": []map[string]float64{
					{"lat": far.LatDeg - d, "lng": far.LonDeg - d}, {"lat": far.LatDeg - d, "lng": far.LonDeg + d},
					{"lat": far.LatDeg + d, "lng": far.LonDeg + d}, {"lat": far.LatDeg + d, "lng": far.LonDeg - d}}},
				"altitude_lower": map[string]any{"value": 600, "reference": "W84", "units": "M"},
				"altitude_upper": map[string]any{"value": 700, "reference": "W84", "units": "M"},
			},
			"time_start": map[string]any{"value": start.Format(time.RFC3339), "format": "RFC3339"},
			"time_end":   map[string]any{"value": start.Add(40 * time.Minute).Format(time.RFC3339), "format": "RFC3339"},
		}},
		"identification_technology": "network", "connectivity_methods": []string{"lte"}, "endurance_s": 3000,
		"loss_of_c2_procedure": "return to the take-off point", "operator_reg": number,
		"contingency": map[string]any{"procedure": "land at the nearest landing site"}, "emergency_contact_ref": "EC-TEST-B" + u,
	}, &dec); err != nil {
		return nil, err
	}
	if dec.Decision != "authorised" {
		return nil, fmt.Errorf("the intruder's intent is %s", dec.Decision)
	}
	if err := c.call(ctx, http.MethodPatch, "/v1/intents/"+dec.IntentID, tok, map[string]any{"action": "activate"}, nil); err != nil {
		return nil, err
	}
	beside := geodesy.Destination(at, 90, num(in, "east_m"))
	if _, err := c.start(tok, serial, dec.IntentID, beside.LatDeg, beside.LonDeg); err != nil {
		return nil, err
	}
	return map[string]any{"intent_id": dec.IntentID, "serial": serial}, nil
}
