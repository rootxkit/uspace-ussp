package testif

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/national/client"
	"github.com/rootxkit/uspace-ussp/internal/telemetry"
)

const (
	fakeOperatorID = "6f1c2b8e-3d4a-4f5b-9c6d-7e8f9a0b1c2d"
	fakeToken      = "op-token"
)

// fakeUSSP is this USSP's published API as the test operator sees it,
// with the behaviours the tests switch: the token service, the portal's
// me and binding routes, and WS /v1/telemetry deciding each frame as
// telemetry-ingest would (bound serial: accepted; otherwise
// refused_unbound) and answering a console/status/v1 frame after each,
// its body telemetry-ingest's own type (the wire names are the ingest's).
type fakeUSSP struct {
	t   *testing.T
	srv *httptest.Server

	mu             sync.Mutex
	bound          map[string]bool
	elsewhere      map[string]bool // bound to another client: 409
	sourceDisabled bool
	refuseAll      string // an outcome every frame gets, when set
	refuseFromSeq  int64  // with refuseFrom, a sample from this seq on is refused_invalid (a teleport)
	refuseFrom     bool
	frames         []client.TelemetryFrame
	bindings       []string
	sockets        int
	tokens         int
	intents        *fakeIntents
	product        []byte // the snapshot's answer
	bboxes         []string
	operatorStatus string            // of the registered test operator
	registered     map[string]string // admin username -> password
	clientsMade    int
}

// snapshot answers the staff session's bbox product.
func (f *fakeUSSP) snapshot(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer console-session" {
		problem(w, http.StatusForbidden, "forbidden", "an operator client subscribes by intent_id")
		return
	}
	f.mu.Lock()
	f.bboxes = append(f.bboxes, r.URL.Query().Get("bbox"))
	b := f.product
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

func newFakeUSSP(t *testing.T) *fakeUSSP {
	t.Helper()
	f := &fakeUSSP{t: t, bound: map[string]bool{}, elsewhere: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.tokens++
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"access_token": fakeToken, "token_type": "Bearer", "expires_in": 3600, "scope": r.FormValue("scope")})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ready", "checked_at": time.Now().UTC(), "dependencies": map[string]any{}, "degraded": []string{}})
	})
	mux.HandleFunc("GET /v1/accounts/me", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer portal-session" {
			problem(w, http.StatusUnauthorized, "unauthenticated", "no session")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"account_id": "a1", "operator_id": fakeOperatorID, "realm": "portal",
			"roles": []string{"operator_admin"}, "session_expires_at": time.Now().Add(time.Hour).UTC(), "username": "wp21.test.admin"})
	})
	mux.HandleFunc("POST /v1/accounts/operators/{op}/clients/{client}/serials", func(w http.ResponseWriter, r *http.Request) {
		var req client.SerialBindingRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.PathValue("op") != fakeOperatorID || r.PathValue("client") != "op-test" {
			problem(w, http.StatusNotFound, "not_found", "no such client")
			return
		}
		if f.elsewhere[req.Serial] {
			problem(w, http.StatusConflict, "serial_bound_elsewhere", "the serial is bound to another client")
			return
		}
		f.bound[req.Serial] = true
		f.bindings = append(f.bindings, req.Serial)
		writeJSON(w, http.StatusCreated, client.SerialBinding{ClientId: "op-test", Serial: req.Serial, SerialFold: req.Serial, BoundAt: time.Now().UTC()})
	})
	mux.HandleFunc("GET /v1/telemetry", f.telemetry)
	f.registered = map[string]string{"staff.support": "staff-password-1"}
	f.operatorStatus = "active"
	mux.HandleFunc("POST /v1/accounts/operators", func(w http.ResponseWriter, r *http.Request) {
		var reg client.OperatorRegistration
		_ = json.NewDecoder(r.Body).Decode(&reg)
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, ok := f.registered[reg.AdminUsername]; ok {
			problem(w, http.StatusConflict, "operator_exists", "registered")
			return
		}
		f.registered[reg.AdminUsername] = reg.AdminPassword
		writeJSON(w, http.StatusCreated, client.Operator{Id: fakeOperatorID, RegistrationNumber: reg.RegistrationNumber, Status: client.OperatorStatus(f.operatorStatus)})
	})
	mux.HandleFunc("POST /v1/accounts/login", func(w http.ResponseWriter, r *http.Request) {
		var l client.LoginRequest
		_ = json.NewDecoder(r.Body).Decode(&l)
		f.mu.Lock()
		pw, ok := f.registered[l.Username]
		f.mu.Unlock()
		if !ok || pw != l.Password {
			problem(w, http.StatusUnauthorized, "unauthenticated", "no such user or password")
			return
		}
		tok, op := "portal-session", fakeOperatorID
		s := client.Session{Token: tok, Realm: l.Realm, Roles: []string{"operator_admin"}, OperatorId: &op, ExpiresAt: time.Now().Add(time.Hour)}
		if l.Realm == client.Console {
			s = client.Session{Token: "console-session", Realm: l.Realm, Roles: []string{"support"}, ExpiresAt: time.Now().Add(time.Hour)}
		}
		writeJSON(w, http.StatusOK, s)
	})
	mux.HandleFunc("GET /v1/accounts/operators/{op}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer portal-session" || r.PathValue("op") != fakeOperatorID {
			problem(w, http.StatusNotFound, "not_found", "no operator")
			return
		}
		f.mu.Lock()
		st := f.operatorStatus
		f.mu.Unlock()
		v := "valid"
		if st != "active" {
			v = "unknown"
		}
		writeJSON(w, http.StatusOK, client.Operator{Id: fakeOperatorID, Status: client.OperatorStatus(st), ValidationStatus: &v})
	})
	mux.HandleFunc("POST /v1/accounts/operators/{op}/clients", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.operatorStatus != "active" {
			problem(w, http.StatusForbidden, "operator_not_active", "no client before the operator is active")
			return
		}
		f.clientsMade++
		writeJSON(w, http.StatusCreated, client.ClientSecret{ClientId: "op-test", ClientSecret: "fresh-secret", Status: "active"})
	})
	mux.HandleFunc("GET /v1/traffic/snapshot", f.snapshot)
	f.intents = newFakeIntents()
	f.intents.routes(mux)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func problem(w http.ResponseWriter, status int, slug, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "https://schemas.uspace.ge/problems/" + slug, "title": slug, "status": status,
		"detail": detail, "errors": []any{}})
}

func (f *fakeUSSP) telemetry(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+fakeToken {
		problem(w, http.StatusUnauthorized, "unauthenticated", "no token")
		return
	}
	f.mu.Lock()
	disabled := f.sourceDisabled
	f.sockets++
	f.mu.Unlock()
	if disabled {
		w.Header().Set("Retry-After", "5")
		problem(w, http.StatusServiceUnavailable, "source_disabled", "the operator_ws source of this client is switched off")
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()
	body := telemetry.ConsoleStatusBody{ConnectionID: "c1", PolicyVersion: "1", StaleAfterS: 5, LiveMaxAgeS: 2,
		Degraded: []string{}, Sources: []telemetry.SourceStatusBody{}, Outcomes: map[string]uint64{}, AckedSeq: map[string]int64{}}
	send := func() bool {
		b, _ := json.Marshal(map[string]any{"schema": "console/status/v1", "body": body})
		return conn.Write(r.Context(), websocket.MessageText, b) == nil
	}
	if !send() {
		return
	}
	for {
		_, b, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		var msg struct {
			Schema string                `json:"schema"`
			Body   client.TelemetryFrame `json:"body"`
		}
		if err := json.Unmarshal(b, &msg); err != nil || msg.Schema != "telemetry/v1" {
			f.t.Errorf("not a telemetry/v1 message: %s", b)
			return
		}
		f.mu.Lock()
		f.frames = append(f.frames, msg.Body)
		outcome := "accepted"
		switch {
		case f.refuseAll != "":
			outcome = f.refuseAll
		case f.refuseFrom && msg.Body.Seq >= f.refuseFromSeq && (msg.Body.End == nil || !*msg.Body.End):
			outcome = "refused_invalid"
		case !f.bound[msg.Body.Serial]:
			outcome = "refused_unbound"
		}
		f.mu.Unlock()
		body.Outcomes[outcome]++
		if outcome == "accepted" {
			body.Accepted++
			body.AckedSeq[msg.Body.Serial] = msg.Body.Seq
		} else {
			body.Refused++
		}
		if !send() {
			return
		}
	}
}

func (f *fakeUSSP) received() []client.TelemetryFrame {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]client.TelemetryFrame(nil), f.frames...)
}

// testServer is a test listener against f with fx's fixture and opts.
type testServer struct {
	*Server
	h        http.Handler
	counters *core.Counters
	cancel   context.CancelFunc
	done     chan struct{}
}

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// profileFixture is the conformance profile's fixture with the open
// subcategory the test operator declares (annex_iv.open_subcategory, A3,
// pending GCAA) when the file does not name one yet.
func profileFixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "deploy", "conformance", "test-operator.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, "open_subcategory:") {
		if !strings.Contains(s, "\nannex_iv:\n") {
			t.Fatal("the fixture has no annex_iv block")
		}
		s = strings.Replace(s, "\nannex_iv:\n", "\nannex_iv:\n  open_subcategory: A3\n", 1)
	}
	return writeFile(t, "test-operator.yaml", s)
}

func newTestServer(t *testing.T, f *fakeUSSP, edit func(*Config)) *testServer {
	t.Helper()
	cfg, err := Load(env(map[string]string{
		"USSP_TEST_INTERFACES_USSP_BASE_URL":        f.srv.URL,
		"USSP_TEST_INTERFACES_OPERATOR_CLIENT_ID":   "op-test",
		"USSP_TEST_INTERFACES_OPERATOR_SECRET_FILE": writeFile(t, "secret", "s3cret"),
		"USSP_TEST_INTERFACES_SESSION_FILE":         writeFile(t, "session", "portal-session"),
		"USSP_TEST_INTERFACES_OPERATOR_FILE":        profileFixture(t),
		"USSP_TEST_INTERFACES_ACK_TIMEOUT_S":        "5",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(&cfg)
	}
	u, err := NewUSSP(cfg)
	if err != nil {
		t.Fatal(err)
	}
	counters := &core.Counters{}
	s, err := NewServer(cfg, &Guard{Verifier: fakeVerifier{}, Counters: counters}, u, counters, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	if err := s.Mount(mux); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ts := &testServer{Server: s, h: mux, counters: counters, cancel: cancel, done: make(chan struct{})}
	go func() { defer close(ts.done); s.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-ts.done })
	return ts
}

// do calls the test listener with a token granting scope.
func (ts *testServer) do(t *testing.T, method, path, scope string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader = http.NoBody
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		if s, ok := body.(string); ok {
			b = []byte(s)
		}
		rd = strings.NewReader(string(b))
	}
	r := httptest.NewRequest(method, path, rd)
	if scope != "" {
		r.Header.Set("Authorization", "Bearer tok-"+scope)
	}
	w := httptest.NewRecorder()
	ts.h.ServeHTTP(w, r)
	return w
}
