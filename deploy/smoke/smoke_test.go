package smoke

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/geodesy"

	"github.com/rootxkit/uspace-ussp/internal/testfakes/operator"
)

// Where the simulated operator flies: Tbilisi, inside the traffic input
// box the peers write, 3 km south of their zone.
const (
	smokeLat = 41.70
	smokeLng = 44.80
)

// smokeState is what the first phase leaves for the one after the
// restart (STAGING_SMOKE_STATE_DIR/state.json, a temporary directory of
// run.sh's).
type smokeState struct {
	OperatorID   string `json:"operator_id"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	IntentID     string `json:"intent_id"`
	Authorised   string `json:"authorisation_number"`
	State        string `json:"state"`
}

// driver talks to the stack only through its public host, as an
// operator on the internet does: STAGING_SMOKE_URL (https://<USSP_HOST>)
// reached at STAGING_SMOKE_CONNECT (the edge Caddy's published address),
// trusting STAGING_SMOKE_CA_FILE (the smoke's throwaway root).
type driver struct {
	t      *testing.T
	base   string
	client *http.Client
}

func newDriver(t *testing.T) *driver {
	t.Helper()
	base, connect, caFile := need(t, "STAGING_SMOKE_URL"), need(t, "STAGING_SMOKE_CONNECT"), need(t, "STAGING_SMOKE_CA_FILE")
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		t.Fatalf("STAGING_SMOKE_URL %q is not an https URL", base)
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatalf("%s holds no certificate", caFile)
	}
	host := u.Hostname()
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	tr := &http.Transport{
		TLSClientConfig:     &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: 5 * time.Second,
		// Every connection to the public host goes to the edge Caddy's
		// published address; nothing else is dialled.
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			h, _, err := net.SplitHostPort(addr)
			if err != nil || h != host {
				return nil, fmt.Errorf("the smoke dials only %s, not %s", host, addr)
			}
			return dialer.DialContext(ctx, network, connect)
		},
	}
	// The simulated operator (internal/testfakes/operator) dials its
	// WebSocket with the default client.
	http.DefaultTransport = tr
	http.DefaultClient = &http.Client{Transport: tr, Timeout: 20 * time.Second}
	return &driver{t: t, base: strings.TrimSuffix(base, "/"), client: &http.Client{Transport: tr, Timeout: 20 * time.Second}}
}

// call sends one request and returns the status and body; it never
// retries (a refusal is an answer, and a 409 is permanent).
func (d *driver) call(ctx context.Context, method, path, bearer string, body any) (int, []byte) {
	d.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			d.t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, d.base+path, rd)
	if err != nil {
		d.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	res, err := d.client.Do(req)
	if err != nil {
		d.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		d.t.Fatalf("%s %s: %v", method, path, err)
	}
	return res.StatusCode, raw
}

// must is call with the status it wants, decoding the body into out.
func (d *driver) must(ctx context.Context, want int, method, path, bearer string, body, out any) {
	d.t.Helper()
	code, raw := d.call(ctx, method, path, bearer, body)
	if code != want {
		d.t.Fatalf("%s %s: %d, want %d: %s", method, path, code, want, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			d.t.Fatalf("%s %s: %v: %s", method, path, err, raw)
		}
	}
}

func (d *driver) token(ctx context.Context, id, secret string) string {
	d.t.Helper()
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {id}, "client_secret": {secret}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.base+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		d.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := d.client.Do(req)
	if err != nil {
		d.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if res.StatusCode != http.StatusOK || json.Unmarshal(raw, &tok) != nil || tok.AccessToken == "" {
		d.t.Fatalf("POST /oauth/token: %d %s", res.StatusCode, raw)
	}
	return tok.AccessToken
}

// control asks the peers (their loopback control listener) for something.
func control(ctx context.Context, t *testing.T, path string, body any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+need(t, "STAGING_SMOKE_CONTROL")+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	// Its own transport: the default one dials only the public host.
	res, err := (&http.Client{Transport: &http.Transport{}, Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("peers' control %s: %v", path, err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("peers' control %s: %d", path, res.StatusCode)
	}
}

func statePath(t *testing.T) string {
	return filepath.Join(need(t, "STAGING_SMOKE_STATE_DIR"), "state.json")
}

// TestStagingSmoke is the simulated operator (doc.go).
func TestStagingSmoke(t *testing.T) {
	if os.Getenv("STAGING_SMOKE") != "1" {
		t.Skip("STAGING_SMOKE is not 1: run by deploy/smoke/run.sh")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	d := newDriver(t)
	if os.Getenv("STAGING_SMOKE_PHASE") == "after-restart" {
		afterRestart(ctx, t, d)
		return
	}

	// The public surface: api's /healthz, the JWKS, and nothing internal.
	d.must(ctx, http.StatusOK, http.MethodGet, "/healthz", "", nil, nil)
	var jwks struct {
		Keys []json.RawMessage `json:"keys"`
	}
	d.must(ctx, http.StatusOK, http.MethodGet, "/.well-known/jwks.json", "", nil, &jwks)
	if len(jwks.Keys) == 0 {
		t.Fatal("the JWKS has no key")
	}
	for _, p := range []string{"/metrics", "/readyz"} {
		if code, _ := d.call(ctx, http.MethodGet, p, "", nil); code != http.StatusNotFound {
			t.Fatalf("GET %s through the public host: %d, want 404", p, code)
		}
	}
	// No credential, no intent: the refusal before the presence below.
	if code, raw := d.call(ctx, http.MethodPost, "/v1/intents", "", map[string]any{}); code != http.StatusUnauthorized {
		t.Fatalf("POST /v1/intents without a credential: %d, want 401: %s", code, raw)
	}
	t.Logf("public surface: /healthz 200, JWKS %d key(s), /metrics and /readyz 404, an intent without a credential 401", len(jwks.Keys))

	// An operator the registry holds valid.
	u := strconv.FormatInt(time.Now().UnixNano()%1_000_000_000, 10)
	number, serial := "GEO-TEST-SMK"+u, "TESTSMK"+u
	control(ctx, t, "/registry", map[string]string{"operator": number, "serial": serial})
	user, pass := "smoke."+u, "smoke-password-"+u
	var op struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	d.must(ctx, http.StatusCreated, http.MethodPost, "/v1/accounts/operators", "", map[string]any{
		"registration_number": number, "display_name": "Smoke operator " + u, "contact_email": "smoke" + u + "@example.test",
		"admin_username": user, "admin_password": pass,
	}, &op)
	if op.Status != "active" {
		t.Fatalf("the operator is %q after the registry said valid, want active", op.Status)
	}
	var sess struct {
		Token string `json:"token"`
	}
	d.must(ctx, http.StatusOK, http.MethodPost, "/v1/accounts/login", "", map[string]any{"realm": "portal", "username": user, "password": pass}, &sess)
	var cl struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	d.must(ctx, http.StatusCreated, http.MethodPost, "/v1/accounts/operators/"+op.ID+"/clients", sess.Token,
		map[string]any{"scopes": []string{"ussp.intents", "ussp.telemetry", "ussp.traffic"}}, &cl)
	code, raw := d.call(ctx, http.MethodPost, "/v1/accounts/operators/"+op.ID+"/clients/"+cl.ClientID+"/serials", sess.Token, map[string]any{"serial": serial})
	if code/100 != 2 {
		t.Fatalf("binding %s: %d %s", serial, code, raw)
	}
	tok := d.token(ctx, cl.ClientID, cl.ClientSecret)
	t.Logf("operator %s (%s) active, client %s, serial %s bound, token issued", op.ID, number, cl.ClientID, serial)

	// One intent, authorised and activated.
	at := core.LatLon{LatDeg: smokeLat, LonDeg: smokeLng}
	start := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
	const half = 0.002
	var dec struct {
		IntentID   string  `json:"intent_id"`
		Decision   string  `json:"decision"`
		State      string  `json:"state"`
		Authorised *string `json:"authorisation_number"`
	}
	d.must(ctx, http.StatusCreated, http.MethodPost, "/v1/intents", tok, map[string]any{
		"client_ref": "smoke-" + u, "uas_serial": serial, "mode": "VLOS", "flight_type": "normal", "category": "specific",
		"volumes": []any{map[string]any{
			"volume": map[string]any{
				"outline_polygon": map[string]any{"vertices": []map[string]float64{
					{"lat": at.LatDeg - half, "lng": at.LonDeg - half}, {"lat": at.LatDeg - half, "lng": at.LonDeg + half},
					{"lat": at.LatDeg + half, "lng": at.LonDeg + half}, {"lat": at.LatDeg + half, "lng": at.LonDeg - half}}},
				"altitude_lower": map[string]any{"value": 600, "reference": "W84", "units": "M"},
				"altitude_upper": map[string]any{"value": 700, "reference": "W84", "units": "M"},
			},
			"time_start": map[string]any{"value": start.Format(time.RFC3339), "format": "RFC3339"},
			"time_end":   map[string]any{"value": start.Add(40 * time.Minute).Format(time.RFC3339), "format": "RFC3339"},
		}},
		"identification_technology": "network", "connectivity_methods": []string{"lte"}, "endurance_s": 3000,
		"loss_of_c2_procedure": "return to the take-off point", "operator_reg": number,
		"contingency": map[string]any{"procedure": "land at the nearest landing site"}, "emergency_contact_ref": "EC-SMOKE-" + u,
	}, &dec)
	if dec.Decision != "authorised" || dec.Authorised == nil {
		t.Fatalf("the intent is %q (authorisation %v), want authorised with a number", dec.Decision, dec.Authorised)
	}
	code, raw = d.call(ctx, http.MethodPatch, "/v1/intents/"+dec.IntentID, tok, map[string]any{"action": "activate"})
	if code != http.StatusOK {
		t.Fatalf("activating %s: %d %s", dec.IntentID, code, raw)
	}
	t.Logf("intent %s authorised (%s) and activated", dec.IntentID, *dec.Authorised)

	// Telemetry over the operator WebSocket, through Caddy, until every
	// sample is acknowledged by telemetry-ingest's status frames.
	sim := operator.New(d.base, tok, geodesy.Destination(at, 0, 50).LatDeg, at.LonDeg, serial)
	sim.IntentID = &dec.IntentID
	if err := sim.Up(ctx); err != nil {
		t.Fatalf("the operator WebSocket: %v", err)
	}
	defer sim.Down()
	sent := 0
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for sent < 5 {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case now := <-tick.C:
			n, err := sim.Tick(ctx, now)
			if err != nil {
				t.Fatalf("telemetry: %v", err)
			}
			sent += n
		}
	}
	if err := sim.WaitAcked(ctx, 30*time.Second); err != nil {
		pending, dropped := sim.Pending()
		t.Fatalf("telemetry not acknowledged: %v (%d pending, %d dropped, %d status frames)", err, pending, dropped, sim.Statuses())
	}
	t.Logf("telemetry: %d samples sent over WS /v1/telemetry, every one acknowledged (%d status frames)", sent, sim.Statuses())

	var got struct {
		State string `json:"state"`
	}
	d.must(ctx, http.StatusOK, http.MethodGet, "/v1/intents/"+dec.IntentID, tok, nil, &got)
	st := smokeState{OperatorID: op.ID, ClientID: cl.ClientID, ClientSecret: cl.ClientSecret, IntentID: dec.IntentID, Authorised: *dec.Authorised, State: got.State}
	b, _ := json.Marshal(st)
	if err := os.WriteFile(statePath(t), b, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("intent %s is %s; state written for the phase after the restart", dec.IntentID, got.State)
}

// afterRestart reads back, through the public host, what the first phase
// made: the client still signs in and the intent is still there with its
// authorisation number.
func afterRestart(ctx context.Context, t *testing.T, d *driver) {
	raw, err := os.ReadFile(statePath(t))
	if err != nil {
		t.Fatalf("no state from the first phase: %v", err)
	}
	var st smokeState
	if err := json.Unmarshal(raw, &st); err != nil || st.IntentID == "" {
		t.Fatalf("the first phase's state does not read: %v", err)
	}
	tok := d.token(ctx, st.ClientID, st.ClientSecret)
	var got struct {
		IntentID   string  `json:"intent_id"`
		State      string  `json:"state"`
		Authorised *string `json:"authorisation_number"`
	}
	d.must(ctx, http.StatusOK, http.MethodGet, "/v1/intents/"+st.IntentID, tok, nil, &got)
	if got.IntentID != st.IntentID || got.Authorised == nil || *got.Authorised != st.Authorised {
		t.Fatalf("after the restart the intent reads %+v, want %s with %s", got, st.IntentID, st.Authorised)
	}
	t.Logf("after the restart: client %s signs in, intent %s still %s with %s", st.ClientID, got.IntentID, got.State, *got.Authorised)
}
