// Package cisp is a fake CISP for tests: the executable reading of spec
// 02 F3 that internal/cis is tested against. It serves the subset of the
// pinned api/clients/cisp.yaml the cache calls (GET and HEAD
// /v1/{dataset} with ETag and since_version deltas, GET
// /v1/{dataset}/versions/{v}, GET /v1/changes, POST and GET
// /v1/subscriptions, GET /.well-known/jwks.json), publishes versions,
// and delivers signed cis/change/v1 notifications (a compact JWS, as the
// CISP's deliver does) to the subscribed callbacks. Down makes every
// request answer 503; Requests counts what was asked.
package cisp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"

	"github.com/rootxkit/uspace-ussp/internal/testfakes/signer"
)

// Token is the bearer the fake accepts.
const Token = "fake-cisp-token" //nolint:gosec // a test fake's fixed bearer, never a credential

type version struct {
	n        int64
	features []json.RawMessage // nil for a raw publication
	raw      []byte
	at       time.Time
}

// Subscription is one subscription the fake holds.
type Subscription struct {
	ID       string    `json:"id"`
	ClientID string    `json:"client_id"`
	Callback string    `json:"callback_url"`
	Datasets []string  `json:"datasets"`
	Bbox     []float64 `json:"bbox,omitempty"`
	Status   string    `json:"status"`
	Created  string    `json:"created_at"`
	Failures int       `json:"consecutive_failures"`
}

// Change is a cis/change/v1 record as the fake sends it.
type Change struct {
	Schema     string   `json:"schema"`
	MsgID      string   `json:"msg_id"`
	Producer   string   `json:"producer"`
	Dataset    string   `json:"dataset"`
	Version    int64    `json:"version"`
	ETag       string   `json:"etag"`
	FeatureIDs []string `json:"feature_ids"`
	RemovedIDs []string `json:"removed_ids"`
	Reason     string   `json:"reason"`
	At         string   `json:"at"`
	PullURL    string   `json:"pull_url"`
}

// Fake is a running fake CISP.
type Fake struct {
	Signer *signer.Signer
	srv    *httptest.Server
	// HTTPClient delivers the notifications.
	HTTPClient *http.Client

	mu       sync.Mutex
	down     bool
	versions map[string][]*version
	changes  []Change
	subs     []*Subscription
	requests map[string]int
	// Deliveries records every delivery: callback, status (0 on a
	// transport error).
	deliveries []Delivery
}

// Delivery is one notification POSTed by the fake.
type Delivery struct {
	Callback string
	Change   Change
	Status   int
	Err      string
}

// New starts a fake CISP over plain HTTP whose issuer is its own URL.
// Its pull_urls are http ones, which the client never follows (the
// dataset is read whole); NewTLS serves the delta path.
func New() (*Fake, error) { return start(httptest.NewServer) }

// NewTLS starts a fake CISP over HTTPS (a test certificate: Client
// trusts it) whose issuer is its own URL.
func NewTLS() (*Fake, error) { return start(httptest.NewTLSServer) }

func start(serve func(http.Handler) *httptest.Server) (*Fake, error) {
	f := &Fake{versions: map[string][]*version{}, requests: map[string]int{}, HTTPClient: &http.Client{Timeout: 2 * time.Second}}
	f.srv = serve(http.HandlerFunc(f.serve))
	s, err := signer.New(f.srv.URL, "fake-cisp-1")
	if err != nil {
		f.srv.Close()
		return nil, err
	}
	f.Signer = s
	return f, nil
}

// Close stops the server.
func (f *Fake) Close() { f.srv.Close() }

// Client is an HTTP client that trusts the fake's certificate (NewTLS)
// and, like the cis client's default, never follows a redirect.
func (f *Fake) Client() *http.Client {
	c := *f.srv.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

// URL is the fake's base URL.
func (f *Fake) URL() string { return f.srv.URL }

// Host is its host (no port).
func (f *Fake) Host() string { u, _ := url.Parse(f.srv.URL); return u.Hostname() }

// IssuerConfig is the static key set of its signing key.
func (f *Fake) IssuerConfig() coreauth.IssuerConfig { return f.Signer.IssuerConfig() }

// Down makes every request answer 503 until Up.
func (f *Fake) Down() { f.mu.Lock(); f.down = true; f.mu.Unlock() }

// Up ends Down.
func (f *Fake) Up() { f.mu.Lock(); f.down = false; f.mu.Unlock() }

// Requests is how many requests were served for key ("GET /v1/zones",
// "GET /v1/changes", "POST /v1/subscriptions", ...), Down ones included.
func (f *Fake) Requests(key string) int { f.mu.Lock(); defer f.mu.Unlock(); return f.requests[key] }

// TotalRequests is how many requests were served in all.
func (f *Fake) TotalRequests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, v := range f.requests {
		n += v
	}
	return n
}

// Deliveries is every delivery so far.
func (f *Fake) Deliveries() []Delivery {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.deliveries)
}

// Publish makes features the next version of dataset and returns its
// change record (not delivered: Deliver sends it).
func (f *Fake) Publish(dataset string, features ...json.RawMessage) Change {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.publishLocked(dataset, &version{features: append([]json.RawMessage{}, features...)})
}

// PublishRaw makes body (served as is, with the cis_* members the
// caller wrote, or none) the next version of dataset.
func (f *Fake) PublishRaw(dataset string, body []byte) Change {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.publishLocked(dataset, &version{raw: body})
}

func (f *Fake) publishLocked(dataset string, v *version) Change {
	vs := f.versions[dataset]
	v.n = int64(len(vs) + 1)
	v.at = time.Now().UTC()
	f.versions[dataset] = append(vs, v)
	ids, removed := []string{}, []string{}
	if len(vs) > 0 && v.features != nil && vs[len(vs)-1].features != nil {
		a, c, r := diff(vs[len(vs)-1].features, v.features)
		ids = append(append(append(ids, a...), c...), r...)
		removed = r
	}
	slices.Sort(ids)
	ch := Change{
		Schema: "cis/change/v1", MsgID: strconv.Itoa(len(f.changes) + 1), Producer: "cisp/deliver-fake",
		Dataset: dataset, Version: v.n, ETag: etag(dataset, v.n), FeatureIDs: ids, RemovedIDs: removed,
		Reason: "publication", At: v.at.Format(time.RFC3339Nano),
		PullURL: fmt.Sprintf("%s/v1/%s?since_version=%d", f.srv.URL, dataset, v.n-1),
	}
	f.changes = append(f.changes, ch)
	return ch
}

// Deliver POSTs ch, signed, to every subscription of its dataset.
func (f *Fake) Deliver(ctx context.Context, ch Change) {
	f.mu.Lock()
	subs := slices.Clone(f.subs)
	f.mu.Unlock()
	body, _ := json.Marshal(ch)
	for _, s := range subs {
		if !slices.Contains(s.Datasets, ch.Dataset) {
			continue
		}
		d := Delivery{Callback: s.Callback, Change: ch}
		tok, err := f.Signer.Sign(s.Callback, s.ID, signer.NewID(), body, time.Now())
		if err == nil {
			var req *http.Request
			req, err = http.NewRequestWithContext(ctx, http.MethodPost, s.Callback, strings.NewReader(tok))
			if err == nil {
				req.Header.Set("Content-Type", "application/jose")
				var resp *http.Response
				resp, err = f.HTTPClient.Do(req)
				if err == nil {
					d.Status = resp.StatusCode
					_ = resp.Body.Close()
				}
			}
		}
		if err != nil {
			d.Err = err.Error()
		}
		f.mu.Lock()
		f.deliveries = append(f.deliveries, d)
		f.mu.Unlock()
	}
}

func etag(dataset string, v int64) string {
	return fmt.Sprintf("%q", dataset+":"+strconv.FormatInt(v, 10))
}

// diff is what changed between two feature lists by identifier.
func diff(prev, next []json.RawMessage) (added, changed, removed []string) {
	p := byID(prev)
	n := byID(next)
	for id, b := range n {
		if old, ok := p[id]; !ok {
			added = append(added, id)
		} else if !bytes.Equal(old, b) {
			changed = append(changed, id)
		}
	}
	for id := range p {
		if _, ok := n[id]; !ok {
			removed = append(removed, id)
		}
	}
	slices.Sort(added)
	slices.Sort(changed)
	slices.Sort(removed)
	return added, changed, removed
}

func byID(fs []json.RawMessage) map[string]json.RawMessage {
	m := map[string]json.RawMessage{}
	for _, f := range fs {
		var x struct {
			Properties struct {
				Identifier string `json:"identifier"`
			} `json:"properties"`
		}
		_ = json.Unmarshal(f, &x)
		m[x.Properties.Identifier] = f
	}
	return m
}

func problem(w http.ResponseWriter, status int, slug, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "https://schemas.uspace.ge/problems/" + slug, "title": slug, "status": status, "detail": detail,
	})
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	f.mu.Lock()
	f.requests[r.Method+" "+path]++
	down := f.down
	f.mu.Unlock()
	if down {
		problem(w, http.StatusServiceUnavailable, "unavailable", "the fake CISP is down")
		return
	}
	if path == "/.well-known/jwks.json" {
		f.Signer.JWKSHandler(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+Token {
		problem(w, http.StatusUnauthorized, "unauthenticated", "no or wrong bearer")
		return
	}
	switch {
	case path == "/v1/subscriptions" && r.Method == http.MethodPost:
		f.createSubscription(w, r)
	case path == "/v1/subscriptions" && r.Method == http.MethodGet:
		f.mu.Lock()
		list := slices.Clone(f.subs)
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"subscriptions": list})
	case strings.HasPrefix(path, "/v1/subscriptions/") && r.Method == http.MethodPatch:
		f.patchSubscription(w, r, strings.TrimPrefix(path, "/v1/subscriptions/"))
	case path == "/v1/changes" && r.Method == http.MethodGet:
		f.listChanges(w, r)
	case strings.HasPrefix(path, "/v1/"):
		parts := strings.Split(strings.TrimPrefix(path, "/v1/"), "/")
		switch {
		case len(parts) == 1 && (r.Method == http.MethodGet || r.Method == http.MethodHead):
			f.getDataset(w, r, parts[0])
		case len(parts) == 3 && parts[1] == "versions" && r.Method == http.MethodGet:
			n, err := strconv.ParseInt(parts[2], 10, 64)
			if err != nil {
				problem(w, http.StatusBadRequest, "validation", "version")
				return
			}
			f.getVersion(w, parts[0], n)
		default:
			problem(w, http.StatusNotFound, "not_found", path)
		}
	default:
		problem(w, http.StatusNotFound, "not_found", path)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *Fake) createSubscription(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Callback string    `json:"callback_url"`
		Datasets []string  `json:"datasets"`
		Bbox     []float64 `json:"bbox"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Callback == "" || len(in.Datasets) == 0 {
		problem(w, http.StatusBadRequest, "validation", "body")
		return
	}
	f.mu.Lock()
	s := &Subscription{ID: "sub-" + strconv.Itoa(len(f.subs)+1), ClientID: "ussp-test-01", Callback: in.Callback,
		Datasets: in.Datasets, Bbox: in.Bbox, Status: "active", Created: time.Now().UTC().Format(time.RFC3339)}
	f.subs = append(f.subs, s)
	f.mu.Unlock()
	writeJSON(w, http.StatusCreated, s)
}

func (f *Fake) patchSubscription(w http.ResponseWriter, r *http.Request, id string) {
	var in struct {
		Datasets *[]string  `json:"datasets"`
		Bbox     *[]float64 `json:"bbox"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		problem(w, http.StatusBadRequest, "validation", "body")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.subs {
		if s.ID != id {
			continue
		}
		if in.Datasets != nil {
			s.Datasets = *in.Datasets
		}
		if in.Bbox != nil {
			s.Bbox = *in.Bbox
			if len(s.Bbox) == 0 {
				s.Bbox = nil
			}
		}
		s.Status = "active"
		writeJSON(w, http.StatusOK, s)
		return
	}
	problem(w, http.StatusNotFound, "not_found", "no such subscription")
}

// Suspend marks every subscription suspended (as 50 failures would).
func (f *Fake) Suspend() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.subs {
		s.Status = "suspended"
	}
}

// Subscriptions is a copy of the subscriptions.
func (f *Fake) Subscriptions() []Subscription {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Subscription, len(f.subs))
	for i, s := range f.subs {
		out[i] = *s
	}
	return out
}

func (f *Fake) listChanges(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.Atoi(r.URL.Query().Get("since"))
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []Change{}
	for i := since; i < len(f.changes); i++ {
		out = append(out, f.changes[i])
	}
	writeJSON(w, http.StatusOK, map[string]any{"changes": out, "next": len(f.changes)})
}

// served is the body of version v of dataset as the CISP serves it.
func served(dataset string, v *version) []byte {
	if v.raw != nil {
		return v.raw
	}
	b, _ := json.Marshal(map[string]any{
		"type": "FeatureCollection", "features": v.features,
		"metadata":    map[string]any{"issued": v.at.Format(time.RFC3339Nano)},
		"cis_dataset": dataset, "cis_version": v.n, "cis_updated_at": v.at.Format(time.RFC3339Nano),
	})
	return b
}

func (f *Fake) getDataset(w http.ResponseWriter, r *http.Request, dataset string) {
	f.mu.Lock()
	vs := f.versions[dataset]
	f.mu.Unlock()
	if len(vs) == 0 {
		problem(w, http.StatusNotFound, "no_version", "no version of "+dataset)
		return
	}
	cur := vs[len(vs)-1]
	tag := etag(dataset, cur.n)
	w.Header().Set("ETag", tag)
	w.Header().Set("X-CIS-Version", strconv.FormatInt(cur.n, 10))
	if sv := r.URL.Query().Get("since_version"); sv != "" {
		n, err := strconv.ParseInt(sv, 10, 64)
		if err != nil || n < 0 || n > cur.n {
			problem(w, http.StatusBadRequest, "validation", "since_version")
			return
		}
		f.delta(w, dataset, vs, n)
		return
	}
	if r.Header.Get("If-None-Match") == tag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/geo+json")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(served(dataset, cur))
}

func (f *Fake) delta(w http.ResponseWriter, dataset string, vs []*version, from int64) {
	cur := vs[len(vs)-1]
	var prev []json.RawMessage
	if from > 0 {
		prev = vs[from-1].features
		if prev == nil {
			problem(w, http.StatusGone, "delta_unavailable", "a raw version")
			return
		}
	}
	if cur.features == nil {
		problem(w, http.StatusGone, "delta_unavailable", "a raw version")
		return
	}
	a, c, rm := diff(prev, cur.features)
	n := byID(cur.features)
	pick := func(ids []string) []json.RawMessage {
		out := make([]json.RawMessage, 0, len(ids))
		for _, id := range ids {
			out = append(out, n[id])
		}
		return out
	}
	if rm == nil {
		rm = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"dataset": dataset, "from_version": from, "to_version": cur.n,
		"added":   map[string]any{"type": "FeatureCollection", "features": pick(a)},
		"changed": map[string]any{"type": "FeatureCollection", "features": pick(c)},
		"removed": rm,
	})
}

func (f *Fake) getVersion(w http.ResponseWriter, dataset string, n int64) {
	f.mu.Lock()
	vs := f.versions[dataset]
	f.mu.Unlock()
	if n < 1 || n > int64(len(vs)) {
		problem(w, http.StatusNotFound, "not_found", "no such version")
		return
	}
	w.Header().Set("ETag", etag(dataset, n))
	w.Header().Set("X-CIS-Version", strconv.FormatInt(n, 10))
	w.Header().Set("Content-Type", "application/geo+json")
	_, _ = w.Write(served(dataset, vs[n-1]))
}

// Tokens is a cis.TokenSource-shaped source that hands out Token.
type Tokens struct{}

// Token returns the fake's bearer.
func (Tokens) Token(context.Context, string, ...string) (string, error) { return Token, nil }
