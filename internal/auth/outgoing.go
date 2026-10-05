package auth

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"
	"github.com/rootxkit/uspace-core/core"
)

// Defaults of OutgoingConfig.
const (
	DefaultOutgoingMaxEntries   = 64
	DefaultOutgoingFetchTimeout = 10 * time.Second
	DefaultOutgoingMaxBodyBytes = 64 << 10
	// RefreshBefore is how long before exp a cached token stops being
	// handed out (brief WP-2: until 60 s before exp).
	RefreshBefore = 60 * time.Second
)

// Counters of the outgoing token client.
const (
	CounterOutgoingFetched     = "token_client_fetched"
	CounterOutgoingFetchFailed = "token_client_fetch_failed"
	CounterOutgoingEvicted     = "token_client_evicted"
)

// OutgoingConfig configures an Outgoing client.
type OutgoingConfig struct {
	// TokenURL is POST /oauth/token of the first USSP_TOKEN_ISSUERS
	// entry (the authority's token service).
	TokenURL string
	// ClientID is this USSP's client at the token service,
	// ussp-<code>-01 (M24); ClientSecret its secret.
	ClientID     string
	ClientSecret string
	HTTPClient   *http.Client
	Now          func() time.Time
	// MaxEntries bounds the cached tokens, one per audience and scope
	// set (E-10).
	MaxEntries   int
	FetchTimeout time.Duration
	MaxBodyBytes int64
}

// ClientIDFor is this USSP's client id at the authority for the USSP
// code (USSP_SYSTEM_ID): "ussp-" + code + "-01" (M24), the code as the
// certificate has it (M8: upper-case alphanumerics). The authority's
// token service registers only that form (^ussp-[A-Z0-9]{1,8}-[0-9]{2}$,
// its migration 00004_tokens) and the lab issuer lists ussp-DEV01-01,
// so a lower-cased code names a client no issuer can hold.
func ClientIDFor(systemID string) string { return "ussp-" + systemID + "-01" }

// Outgoing is the token client for the calls this USSP makes (the CISP,
// the authority, the ANSP, the DSS, a peer USSP): it asks the token
// service for a token whose aud is the host of the target's base URL
// (M18), caches one per (audience, scope set) and hands it out until
// RefreshBefore before its exp. A call without a usable token joins the
// one request in flight for its key, or starts it, and waits for it or
// for its own context; the request runs on a context of its own, so a
// cancelled caller never fails it for the others (E-14). Safe for
// concurrent use.
type Outgoing struct {
	cfg      OutgoingConfig
	counters core.Counters

	mu      sync.Mutex
	entries map[string]*list.Element
	order   *list.List // front = most recently used
}

type outEntry struct {
	key      string
	audience string
	scopes   []string
	token    string
	expires  time.Time
	inflight chan struct{}
	err      error
}

// NewOutgoing validates c and applies the defaults.
func NewOutgoing(c OutgoingConfig) (*Outgoing, error) {
	u, err := url.Parse(c.TokenURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, core.Fieldf("token_url", "not an absolute http(s) URL")
	}
	if c.ClientID == "" || c.ClientSecret == "" {
		return nil, core.Fieldf("USSP_TOKEN_CLIENT_SECRET_FILE", "the client id and secret are required for outgoing calls")
	}
	if c.HTTPClient == nil {
		c.HTTPClient = &http.Client{Timeout: DefaultOutgoingFetchTimeout}
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.MaxEntries <= 0 {
		c.MaxEntries = DefaultOutgoingMaxEntries
	}
	if c.FetchTimeout <= 0 {
		c.FetchTimeout = DefaultOutgoingFetchTimeout
	}
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = DefaultOutgoingMaxBodyBytes
	}
	return &Outgoing{cfg: c, entries: map[string]*list.Element{}, order: list.New()}, nil
}

// Counters are the client's counters.
func (o *Outgoing) Counters() *core.Counters { return &o.counters }

// Len is the number of cached entries.
func (o *Outgoing) Len() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.order.Len()
}

// AudienceOf is the audience of a target: the host of its base URL
// (M18), without a port's default and lower-case.
func AudienceOf(baseURL string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", core.Fieldf("base_url", "%s is not an absolute http(s) URL", quote(baseURL))
	}
	return strings.ToLower(u.Hostname()), nil
}

// Token returns a token to call the system published at baseURL with
// scopes.
func (o *Outgoing) Token(ctx context.Context, baseURL string, scopes ...string) (string, error) {
	aud, err := AudienceOf(baseURL)
	if err != nil {
		return "", err
	}
	return o.TokenFor(ctx, aud, scopes)
}

func (e *outEntry) usable(now time.Time) bool {
	return e.token != "" && now.Before(e.expires.Add(-RefreshBefore))
}

// TokenFor returns a token for audience (a host) and scopes.
func (o *Outgoing) TokenFor(ctx context.Context, audience string, scopes []string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	sorted := slices.Clone(scopes)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)
	if len(sorted) == 0 || slices.Contains(sorted, "") {
		return "", core.Fieldf("scope", "at least one non-empty scope")
	}
	key := audience + " " + strings.Join(sorted, " ")

	o.mu.Lock()
	e := o.lookup(key, audience, sorted)
	if e.usable(o.cfg.Now()) {
		tok := e.token
		o.mu.Unlock()
		return tok, nil
	}
	done := e.inflight
	if done == nil {
		done = o.start(e)
	}
	o.mu.Unlock()

	select {
	case <-done:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if e.usable(o.cfg.Now()) {
		return e.token, nil
	}
	if e.err != nil {
		return "", e.err
	}
	return "", errors.New("the token service answered a token that is already within 60 s of its expiry")
}

// lookup returns the entry of key, creating it and evicting the least
// recently used entry with no request in flight beyond the bound. The
// caller holds mu.
func (o *Outgoing) lookup(key, audience string, scopes []string) *outEntry {
	if el, ok := o.entries[key]; ok {
		o.order.MoveToFront(el)
		return el.Value.(*outEntry)
	}
	for o.order.Len() >= o.cfg.MaxEntries {
		victim := o.order.Back()
		for victim != nil && victim.Value.(*outEntry).inflight != nil {
			victim = victim.Prev()
		}
		if victim == nil {
			break
		}
		o.order.Remove(victim)
		delete(o.entries, victim.Value.(*outEntry).key)
		o.counters.Inc(CounterOutgoingEvicted)
	}
	e := &outEntry{key: key, audience: audience, scopes: scopes}
	o.entries[key] = o.order.PushFront(e)
	return e
}

// start launches the request for e on a context of its own; the caller
// holds mu.
func (o *Outgoing) start(e *outEntry) chan struct{} {
	done := make(chan struct{})
	e.inflight = done
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), o.cfg.FetchTimeout)
		defer cancel()
		tok, ttl, err := o.fetch(ctx, e.audience, e.scopes)
		now := o.cfg.Now()
		o.mu.Lock()
		if err != nil {
			o.counters.Inc(CounterOutgoingFetchFailed)
			e.err = err
		} else {
			o.counters.Inc(CounterOutgoingFetched)
			e.token, e.expires, e.err = tok, now.Add(ttl), nil
		}
		e.inflight = nil
		o.mu.Unlock()
		close(done)
	}()
	return done
}

// TokenServiceError is a refused token request: the issuer's RFC 6749
// error. It never carries a token or the secret.
type TokenServiceError struct {
	Status      int
	Code        string
	Description string
}

func (e *TokenServiceError) Error() string {
	return fmt.Sprintf("token request refused (%d %s): %s", e.Status, e.Code, e.Description)
}

func (o *Outgoing) fetch(ctx context.Context, audience string, scopes []string) (string, time.Duration, error) {
	form := url.Values{"grant_type": {"client_credentials"}, "scope": {strings.Join(scopes, " ")}, "audience": {audience},
		"client_id": {o.cfg.ClientID}, "client_secret": {o.cfg.ClientSecret}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := o.cfg.HTTPClient.Do(req)
	if err != nil {
		return "", 0, errors.New("token request: the token service did not answer")
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, o.cfg.MaxBodyBytes+1))
	if err != nil {
		return "", 0, errors.New("token response: cannot be read")
	}
	if int64(len(body)) > o.cfg.MaxBodyBytes {
		return "", 0, fmt.Errorf("token response larger than %d bytes", o.cfg.MaxBodyBytes)
	}
	return ParseTokenResponse(resp.StatusCode, body)
}

// ParseTokenResponse reads an RFC 6749 §5.1 answer (or a §5.2 refusal):
// a Bearer token and a positive integer expires_in of at most one hour.
// The returned errors never contain the token.
func ParseTokenResponse(status int, body []byte) (string, time.Duration, error) {
	if status != http.StatusOK {
		var e struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		_ = json.Unmarshal(body, &e)
		if e.Error == "" {
			e.Error = "http_" + strconv.Itoa(status)
		}
		return "", 0, &TokenServiceError{Status: status, Code: clip(e.Error), Description: clip(e.Description)}
	}
	var r struct {
		AccessToken string          `json:"access_token"`
		TokenType   string          `json:"token_type"`
		ExpiresIn   json.RawMessage `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return "", 0, errors.New("token response is not a JSON object")
	}
	if r.AccessToken == "" || len(r.AccessToken) > coreauth.DefaultMaxTokenBytes {
		return "", 0, errors.New("token response has no usable access_token")
	}
	if !strings.EqualFold(r.TokenType, "Bearer") {
		return "", 0, errors.New("token response is not a Bearer token")
	}
	secs, err := strconv.ParseInt(string(r.ExpiresIn), 10, 64)
	if err != nil || secs <= 0 || secs > int64(time.Hour/time.Second) {
		return "", 0, errors.New("token response expires_in is not a positive integer of at most 3600")
	}
	return r.AccessToken, time.Duration(secs) * time.Second, nil
}
