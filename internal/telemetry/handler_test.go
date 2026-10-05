package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	coreauth "github.com/rootxkit/uspace-core/auth"

	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/policy"
)

const ownIssuer = "https://ussp.test"

// verifier accepts a token that is a client id, as an operator token of
// this USSP granting ussp.telemetry; "geo:<id>" grants ussp.geo only,
// anything with a space is refused.
type verifier struct{}

func (verifier) Verify(_ context.Context, token string) (coreauth.Claims, error) {
	if strings.Contains(token, " ") || token == "" {
		return coreauth.Claims{}, &coreauth.TokenError{Counter: "rejected_signature", Claim: "signature", Reason: "bad"}
	}
	if id, ok := strings.CutPrefix(token, "geo:"); ok {
		return coreauth.Claims{Issuer: ownIssuer, Subject: id, Scopes: []string{auth.ScopeGeo}}, nil
	}
	return coreauth.Claims{Issuer: ownIssuer, Subject: token, Scopes: []string{auth.ScopeTelemetry}}, nil
}

type healthStub struct{}

func (healthStub) GetHealthz(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }
func (healthStub) GetReadyz(w http.ResponseWriter, _ *http.Request)  { w.WriteHeader(200) }

// server is the two operations of r behind the access table.
func server(t *testing.T, r *rig) (*httptest.Server, *Server) {
	t.Helper()
	guard := &auth.Guard{Verifier: verifier{}, OwnIssuer: ownIssuer}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := &Server{Health: healthStub{}, Ingest: r.in, WS: &auth.WSAuth{Guard: guard}, Sources: r.gate,
		Policy: r.policy, Ctx: ctx, StatusEvery: 20 * time.Millisecond, HandOver: 2 * time.Second, Counters: r.counters,
		Degraded: func() []string { return []string{"geoid", "geoid"} }}
	mux := http.NewServeMux()
	if err := Register(mux, s, guard.Require); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(httpx.TrackRoute(mux))
	t.Cleanup(srv.Close)
	return srv, s
}

// dialWS opens WS /v1/telemetry with the bearer token.
func dialWS(t *testing.T, srv *httptest.Server, token string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h := http.Header{}
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	return websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/telemetry", &websocket.DialOptions{HTTPHeader: h})
}

// readStatus reads frames until one satisfies cond; every frame must be
// a console/status/v1 frame (CLAUDE.md rule 1: nothing else is sent).
func readStatus(t *testing.T, c *websocket.Conn, cond func(ConsoleStatus) bool) ConsoleStatus {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if typ != websocket.MessageText {
			t.Fatalf("frame type %v", typ)
		}
		var st ConsoleStatus
		if err := json.Unmarshal(data, &st); err != nil {
			t.Fatal(err)
		}
		var raw map[string]any
		_ = json.Unmarshal(data, &raw)
		if raw["schema"] != SchemaConsoleStatus || len(raw) != 9 {
			t.Fatalf("the server sent something other than a status frame: %s", data)
		}
		if cond(st) {
			return st
		}
	}
}

func send(t *testing.T, c *websocket.Conn, msg string) {
	t.Helper()
	if err := c.Write(context.Background(), websocket.MessageText, []byte(msg)); err != nil {
		t.Fatal(err)
	}
}

func wsSample(sn string, seq int64, ts time.Time) string {
	s := strings.Replace(sampleJSON(""), `"serial":"TEST-SN-A","seq":1`, `"serial":"`+sn+`","seq":`+itoa(seq), 1)
	s = strings.Replace(s, `"2026-10-02T09:00:00.000Z"`, `"`+ts.UTC().Format("2006-01-02T15:04:05.000Z")+`"`, 1)
	return `{"schema":"telemetry/v1","body":` + s + `}`
}

func itoa(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// The socket: status on connect; a sample accepted and acknowledged by
// acked_seq; an unbound serial and an unreadable message refused while
// the socket stays up; only status frames ever come back.
func TestWebSocketStream(t *testing.T) {
	r := newRig(t, rigOpts{})
	r.clk.set(time.Now())
	r.in.cfg.Now = time.Now
	srv, _ := server(t, r)
	c, _, err := dialWS(t, srv, clientA)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.CloseNow() }()
	first := readStatus(t, c, func(ConsoleStatus) bool { return true })
	if first.Body.PolicyVersion != "7" || first.Body.StaleAfterS != 5 || len(first.Body.Degraded) != 1 || first.Body.Sources[0].Source != SourceOperatorWS {
		t.Fatalf("first status %+v", first.Body)
	}
	now := time.Now()
	send(t, c, wsSample(snA, 1, now))
	st := readStatus(t, c, func(s ConsoleStatus) bool { return s.Body.AckedSeq[snA] == 1 })
	if st.Body.Accepted != 1 || st.Body.Outcomes[OutcomeAccepted] != 1 {
		t.Fatalf("status %+v", st.Body)
	}
	send(t, c, wsSample("TEST-SN-UNBOUND", 1, now))
	send(t, c, `{"schema":"telemetry/v1","body":{"trust":"simulated"}}`)
	send(t, c, `not json`)
	st = readStatus(t, c, func(s ConsoleStatus) bool {
		return s.Body.Outcomes[RefusedInvalid] == 2 && s.Body.Outcomes[RefusedUnbound] == 1
	})
	if st.Body.Refused != 3 {
		t.Fatalf("refused %d", st.Body.Refused)
	}
	send(t, c, wsSample(snA, 2, now.Add(time.Second)))
	readStatus(t, c, func(s ConsoleStatus) bool { return s.Body.AckedSeq[snA] == 2 })
	if n := len(r.pub.tracks(t)); n != 2 {
		t.Fatalf("%d tracks", n)
	}
}

// E-01 pair (B-10): a disabled source is refused before the upgrade with
// 503 and Retry-After; an open socket is closed with 1013 when it is
// switched off; switched on, the same client connects.
func TestWebSocketDisabledSource(t *testing.T) {
	r := newRig(t, rigOpts{})
	srv, _ := server(t, r)
	r.gate.set(clientA, true)
	_, resp, err := dialWS(t, srv, clientA)
	if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("disabled connect: %v %+v", err, resp)
	}
	r.gate.set(clientA, false)
	c, resp, err := dialWS(t, srv, clientA)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("enabled connect: %v", err)
	}
	defer func() { _ = c.CloseNow() }()
	readStatus(t, c, func(ConsoleStatus) bool { return true })
	r.gate.set(clientA, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, _, err := c.Read(ctx)
		if err != nil {
			if code := websocket.CloseStatus(err); code != websocket.StatusTryAgainLater {
				t.Fatalf("closed with %d (%v), want 1013", code, err)
			}
			break
		}
	}
	if r.counters.Get(CounterConnectRefusedDisabled) != 1 || r.counters.Get(CounterClosedDisabled) != 1 {
		t.Fatalf("counters %v", r.counters.Snapshot())
	}
}

// Conformance C8 as a presence/absence pair: an upgrade without a
// credential, with a refused token, or with a token without the scope is
// answered with its status and problem body before any upgrade (401,
// 401, 403), never 101, and no session is opened; the same upgrade with
// a token granting ussp.telemetry is 101 and streams.
func TestWebSocketRefusedBeforeTheUpgrade(t *testing.T) {
	r := newRig(t, rigOpts{})
	srv, _ := server(t, r)
	for _, c := range []struct {
		tok    string
		status int
		slug   string
	}{
		{"", http.StatusUnauthorized, httpx.SlugUnauthenticated},
		{"bad token", http.StatusUnauthorized, "rejected_signature"},
		{"geo:" + clientA, http.StatusForbidden, httpx.SlugForbidden},
	} {
		conn, resp, err := dialWS(t, srv, c.tok)
		if err == nil {
			_ = conn.CloseNow()
			t.Fatalf("%q: upgraded", c.tok)
		}
		if resp == nil || resp.StatusCode != c.status {
			t.Fatalf("%q: %v %v, want %d", c.tok, resp, err, c.status)
		}
		var p struct{ Type string }
		if err := json.NewDecoder(resp.Body).Decode(&p); err != nil || p.Type != httpx.ProblemTypeBase+c.slug ||
			!strings.HasPrefix(resp.Header.Get("Content-Type"), "application/problem+json") {
			t.Errorf("%q: %s %q %v", c.tok, resp.Header.Get("Content-Type"), p.Type, err)
		}
		if c.status == http.StatusUnauthorized && resp.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("%q: 401 without WWW-Authenticate", c.tok)
		}
	}
	if n := r.counters.Get(CounterSessions); n != 0 {
		t.Fatalf("%d sessions opened by refused upgrades", n)
	}
	conn, resp, err := dialWS(t, srv, clientA)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("valid token: %v %v", resp, err)
	}
	defer func() { _ = conn.CloseNow() }()
	readStatus(t, conn, func(ConsoleStatus) bool { return true })
	if n := r.counters.Get(CounterSessions); n != 1 {
		t.Fatalf("%d sessions, want 1", n)
	}
}

// E-01 pair (B-14) on the wire: a second socket for the aircraft takes
// it; the first socket's next sample for it is refused_replaced.
func TestWebSocketSecondSessionReplacesTheFirst(t *testing.T) {
	r := newRig(t, rigOpts{})
	r.in.cfg.Now = time.Now
	srv, _ := server(t, r)
	first, _, err := dialWS(t, srv, clientA)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.CloseNow() }()
	second, _, err := dialWS(t, srv, clientA)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.CloseNow() }()
	now := time.Now()
	send(t, first, wsSample(snA, 1, now))
	readStatus(t, first, func(s ConsoleStatus) bool { return s.Body.AckedSeq[snA] == 1 })
	send(t, second, wsSample(snA, 2, now.Add(time.Second)))
	readStatus(t, second, func(s ConsoleStatus) bool { return s.Body.AckedSeq[snA] == 2 })
	send(t, first, wsSample(snA, 3, now.Add(2*time.Second)))
	readStatus(t, first, func(s ConsoleStatus) bool { return s.Body.Outcomes[RefusedReplaced] == 1 })
}

func postBatch(t *testing.T, srv *httptest.Server, token, body string) (int, BatchResult, http.Header) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/telemetry/batch", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var res BatchResult
	if resp.StatusCode == http.StatusAccepted {
		if err := json.Unmarshal(raw, &res); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
	}
	return resp.StatusCode, res, resp.Header
}

func batchOf(sentAt string, samples ...string) string {
	var b bytes.Buffer
	b.WriteString("{")
	if sentAt != "" {
		b.WriteString(`"sent_at":"` + sentAt + `",`)
	}
	b.WriteString(`"frames":[` + strings.Join(samples, ",") + `]}`)
	return b.String()
}

func batchSample(sn string, seq int64, ts time.Time, backlog bool) string {
	m := wsSample(sn, seq, ts)
	body := strings.TrimSuffix(strings.TrimPrefix(m, `{"schema":"telemetry/v1","body":`), `}`)
	if backlog {
		body = strings.Replace(body, `{`, `{"backlog":true,`, 1)
	}
	return body
}

// POST /v1/telemetry/batch: 202 with the accepted samples counted only
// once handed (B-05), every other one listed with its outcome; a source
// switched off 503 with Retry-After and nothing stored; no token 401; a
// token without the scope 403; a body that is no batch 400.
func TestBatch(t *testing.T) {
	r := newRig(t, rigOpts{})
	r.pol.Values.TelemetryBacklogRateHz = 100
	srv, s := server(t, r)
	s.Now = r.clk.now
	r.clk.set(t0.Add(time.Minute))
	sent := t0.Add(time.Minute).Format(time.RFC3339)
	body := batchOf(sent,
		batchSample(snA, 1, t0, true), batchSample(snA, 2, t0.Add(500*time.Millisecond), true),
		`{"serial":"x"}`, batchSample("TEST-SN-UNBOUND", 1, t0, true),
		batchSample(snB, 1, t0.Add(-10*time.Second), true), batchSample(snB, 2, t0, true),
	)
	code, res, _ := postBatch(t, srv, clientA, body)
	if code != http.StatusAccepted {
		t.Fatalf("status %d", code)
	}
	if res.Accepted != 3 || res.Refused != 3 || len(res.Outcomes) != 3 {
		t.Fatalf("result %+v", res)
	}
	byIndex := map[int]string{}
	for _, o := range res.Outcomes {
		byIndex[o.Index] = o.Outcome
	}
	if byIndex[2] != RefusedInvalid || byIndex[3] != RefusedUnbound || byIndex[4] != RefusedBatchSpan {
		t.Fatalf("outcomes %+v", res.Outcomes)
	}
	trs := r.pub.tracks(t)
	if len(trs) != 3 || !trs[0].Backlog || !trs[0].CapturedAt.Equal(t0) {
		t.Fatalf("tracks %d %+v", len(trs), trs[0].Envelope)
	}
	// The same batch again: duplicates, acknowledged, nothing twice.
	code, res, _ = postBatch(t, srv, clientA, batchOf(sent, batchSample(snA, 1, t0, true)))
	if code != http.StatusAccepted || res.Duplicate != 1 || len(r.pub.tracks(t)) != 3 {
		t.Fatalf("replay %d %+v", code, res)
	}
	r.gate.set(clientA, true)
	code, _, h := postBatch(t, srv, clientA, body)
	if code != http.StatusServiceUnavailable || h.Get("Retry-After") == "" {
		t.Fatalf("disabled: %d", code)
	}
	r.gate.set(clientA, false)
	if code, _, _ := postBatch(t, srv, "", body); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", code)
	}
	if code, _, _ := postBatch(t, srv, "geo:"+clientA, body); code != http.StatusForbidden {
		t.Fatalf("no scope: %d", code)
	}
	for _, bad := range []string{`{}`, `nope`, `{"frames":[]}`} {
		if code, _, _ := postBatch(t, srv, clientA, bad); code != http.StatusBadRequest {
			t.Errorf("%s: %d", bad, code)
		}
	}
	if code, _, _ := postBatch(t, srv, clientA, strings.Repeat(" ", MaxBatchBytes+1)); code != http.StatusRequestEntityTooLarge {
		t.Errorf("too large: %d", code)
	}
}

// A sample whose hand-over does not complete within the bound is
// reported not_acknowledged (sent again), never counted accepted.
func TestBatchNotAcknowledgedWhenNothingHands(t *testing.T) {
	r := newRig(t, rigOpts{noRun: true, queueFrames: 4})
	srv, s := server(t, r)
	s.HandOver = 50 * time.Millisecond
	code, res, _ := postBatch(t, srv, clientA, batchOf("", batchSample(snA, 1, t0, false)))
	if code != http.StatusAccepted || res.Accepted != 0 || res.NotAcknowledged != 1 || res.Outcomes[0].Outcome != OutcomeNotAcknowledged {
		t.Fatalf("%d %+v", code, res)
	}
}

// policy is the rig's policy for the server.
func (r *rig) policy() policy.Record { return r.pol }
