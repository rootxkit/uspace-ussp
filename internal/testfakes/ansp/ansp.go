// Package ansp is a fake ANSP for tests. WP-4 needs its degraded direct
// delivery of a restriction (spec 02 F2, M5): a cis/change/v1 record as
// a compact JWS signed with the ANSP's own key, POSTed to a subscriber's
// /v1/cis/notifications. WP-15 adds the coordination inbox of the
// ANSP's api/openapi.yaml (M2): POST /v1/coordination/notices answers
// 202 with a receipt, a repeat of a notice_ref with the same body 200
// with the first receipt, the same ref with another body 409
// notice_ref_reused; GET /v1/coordination/notices/{ack_id} answers the
// notice's state, acknowledged once a test calls Acknowledge. Down
// makes its server answer 503. WP-14 adds the manned-traffic service of
// 02 F4 (stream.go): GET /v1/manned-traffic/stream, a WebSocket of
// envelope frames (console/snapshot/v1 on connect, track/manned/v1 for
// every aircraft each TrackEvery, console/status/v1 each StatusEvery
// with the adapters' states), and GET /v1/manned-traffic/snapshot;
// CutStream closes every stream at once and refuses new ones.
package ansp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"

	"github.com/rootxkit/uspace-ussp/internal/testfakes/signer"
)

// Fake is a running fake ANSP.
type Fake struct {
	Signer     *signer.Signer
	HTTPClient *http.Client
	srv        *httptest.Server

	mu      sync.Mutex
	down    bool
	notices []*Notice
	posts   int
	gets    int
	// stream is the manned-traffic service's state (stream.go).
	stream streamState
}

// Notice is one notice the fake inbox received.
type Notice struct {
	AckID          string
	Ref            string
	Kind           string
	Body           []byte
	ReceivedAt     time.Time
	State          string
	AcknowledgedAt *time.Time
	AcknowledgedBy string
	Authorization  string
}

// New starts a fake ANSP whose issuer is its own URL.
func New() (*Fake, error) {
	f := &Fake{HTTPClient: &http.Client{Timeout: 2 * time.Second}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		down := f.down
		f.mu.Unlock()
		if down {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		switch {
		case r.URL.Path == "/.well-known/jwks.json":
			f.Signer.JWKSHandler(w, r)
		case r.URL.Path == "/v1/coordination/notices" && r.Method == http.MethodPost:
			f.submit(w, r)
		case strings.HasPrefix(r.URL.Path, "/v1/coordination/notices/") && r.Method == http.MethodGet:
			f.get(w, r, strings.TrimPrefix(r.URL.Path, "/v1/coordination/notices/"))
		case r.URL.Path == "/v1/manned-traffic/stream" && r.Method == http.MethodGet:
			f.serveStream(w, r)
		case r.URL.Path == "/v1/manned-traffic/snapshot" && r.Method == http.MethodGet:
			f.serveSnapshot(w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	s, err := signer.New(f.srv.URL, "fake-ansp-1")
	if err != nil {
		f.srv.Close()
		return nil, err
	}
	f.Signer = s
	return f, nil
}

// Close stops the server.
func (f *Fake) Close() { f.srv.Close() }

// URL is the fake's base URL.
func (f *Fake) URL() string { return f.srv.URL }

// Host is its host (no port).
func (f *Fake) Host() string { u, _ := url.Parse(f.srv.URL); return u.Hostname() }

// IssuerConfig is the static key set of its signing key.
func (f *Fake) IssuerConfig() coreauth.IssuerConfig { return f.Signer.IssuerConfig() }

// Down makes its server answer 503 until Up.
func (f *Fake) Down() { f.mu.Lock(); f.down = true; f.mu.Unlock() }

// Up ends Down.
func (f *Fake) Up() { f.mu.Lock(); f.down = false; f.mu.Unlock() }

// Notify POSTs change, signed by the ANSP (sub = the restriction id),
// to callback and returns the status.
func (f *Fake) Notify(ctx context.Context, callback, restrictionID string, change any) (int, error) {
	body, err := json.Marshal(change)
	if err != nil {
		return 0, err
	}
	tok, err := f.Signer.Sign(callback, restrictionID, signer.NewID(), body, time.Now())
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, callback, strings.NewReader(tok))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/jose")
	resp, err := f.HTTPClient.Do(req)
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

func problem(w http.ResponseWriter, status int, slug, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "https://schemas.uspace.ge/problems/" + slug, "title": slug, "status": status, "detail": detail})
}

func receipt(w http.ResponseWriter, status int, n *Notice) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"ack_id": n.AckID, "state": "received", "received_at": n.ReceivedAt.Format(time.RFC3339Nano)})
}

// submit is POST /v1/coordination/notices.
func (f *Fake) submit(w http.ResponseWriter, r *http.Request) {
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, "Bearer ") || len(authz) <= len("Bearer ") {
		problem(w, http.StatusUnauthorized, "unauthenticated", "no bearer token")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20+1))
	if err != nil || len(body) > 1<<20 {
		problem(w, http.StatusRequestEntityTooLarge, "too_large", "body over 1 MiB")
		return
	}
	var n struct {
		Schema    string            `json:"schema"`
		NoticeRef string            `json:"notice_ref"`
		Kind      string            `json:"kind"`
		Intents   []json.RawMessage `json:"intents"`
	}
	if err := json.Unmarshal(body, &n); err != nil || n.Schema != "coordination/annex_v/v1" || n.NoticeRef == "" || len(n.Intents) == 0 {
		problem(w, http.StatusBadRequest, "validation", "not a coordination/annex_v/v1 notice")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts++
	for _, old := range f.notices {
		if old.Ref != n.NoticeRef {
			continue
		}
		if !bytes.Equal(old.Body, body) {
			problem(w, http.StatusConflict, "notice_ref_reused", "this notice_ref was received with another body")
			return
		}
		receipt(w, http.StatusOK, old)
		return
	}
	x := &Notice{AckID: signer.NewID(), Ref: n.NoticeRef, Kind: n.Kind, Body: body, ReceivedAt: time.Now().UTC(), State: "received", Authorization: authz}
	f.notices = append(f.notices, x)
	receipt(w, http.StatusAccepted, x)
}

// get is GET /v1/coordination/notices/{ack_id}.
func (f *Fake) get(w http.ResponseWriter, r *http.Request, ackID string) {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		problem(w, http.StatusUnauthorized, "unauthenticated", "no bearer token")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	for _, n := range f.notices {
		if n.AckID != ackID {
			continue
		}
		var payload json.RawMessage = n.Body
		out := map[string]any{"ack_id": n.AckID, "kind": n.Kind, "sender_client_id": "fake", "ussp_id": "fake", "notice_ref": n.Ref,
			"intent_refs": []string{}, "authorisation_numbers": []string{}, "received_at": n.ReceivedAt.Format(time.RFC3339Nano),
			"state": n.State, "acknowledgement_required": n.Kind == "nonconformance" || n.Kind == "contingent", "payload": payload}
		if n.AcknowledgedAt != nil {
			out["acknowledged_at"], out["acknowledged_by"] = n.AcknowledgedAt.Format(time.RFC3339Nano), n.AcknowledgedBy
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	problem(w, http.StatusNotFound, "not_found", "no notice with this ack_id")
}

// Notices are copies of the notices received, in order.
func (f *Fake) Notices() []Notice {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Notice, 0, len(f.notices))
	for _, n := range f.notices {
		out = append(out, *n)
	}
	return out
}

// Requests are the POSTs and GETs the inbox answered (not while down).
func (f *Fake) Requests() (posts, gets int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.posts, f.gets
}

// Acknowledge marks the notice with ackID acknowledged by a person in
// role; false when there is none.
func (f *Fake) Acknowledge(ackID, role string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range f.notices {
		if n.AckID == ackID {
			t := time.Now().UTC()
			n.State, n.AcknowledgedAt, n.AcknowledgedBy = "acknowledged", &t, role
			return true
		}
	}
	return false
}
