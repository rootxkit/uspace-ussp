package cis

import (
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
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/cis/cispclient"
)

// ScopeCISRead is the scope of every call to the CISP (spec 06 §3).
const ScopeCISRead = "cis.read"

// Defaults of ClientConfig.
const (
	// DefaultTimeout is the deadline of one call.
	DefaultTimeout = 10 * time.Second
	// MaxErrorBodyBytes is how much of an error answer is read.
	MaxErrorBodyBytes = 8 << 10
)

// MaxBodyBytes is the most a dataset answer may hold once decoded: the
// most ed318.Parse reads with ProblemLimits, so nothing larger can be
// accepted anyway (E-10).
var MaxBodyBytes = int64(ProblemLimits.MaxBytes)

// TokenSource hands out an ecosystem token whose aud is the host of
// baseURL (M18) with the scopes; internal/auth.Outgoing implements it.
type TokenSource interface {
	Token(ctx context.Context, baseURL string, scopes ...string) (string, error)
}

// ErrNoTokenSource is returned by every call of a Client built without
// a token source: the CISP refuses an unauthenticated read, and the
// cache says so on /readyz instead of trying.
var ErrNoTokenSource = errors.New("no token client for the CISP (USSP_TOKEN_ISSUERS and USSP_TOKEN_CLIENT_SECRET_FILE)")

// ClientConfig configures a Client.
type ClientConfig struct {
	// BaseURL is USSP_CISP_BASE_URL.
	BaseURL string
	Tokens  TokenSource
	// HTTPClient is used for every call; nil is a client that never
	// follows a redirect (a 3xx is an error).
	HTTPClient *http.Client
	Timeout    time.Duration
	// MaxBodyBytes bounds a dataset answer (default MaxBodyBytes).
	MaxBodyBytes int64
}

// Client calls the CISP's F3 pull API through the generated client of
// the pinned api/clients/cisp.yaml.
type Client struct {
	cfg  ClientConfig
	base *url.URL
	host string
	gen  *cispclient.Client
}

// NewClient builds a Client; a base URL that is not an absolute http(s)
// URL is an error naming USSP_CISP_BASE_URL.
func NewClient(cfg ClientConfig) (*Client, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, core.Fieldf("USSP_CISP_BASE_URL", "not an absolute http(s) URL")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = MaxBodyBytes
	}
	c := &Client{cfg: cfg, base: u, host: u.Hostname()}
	c.gen, err = cispclient.NewClient(strings.TrimRight(cfg.BaseURL, "/"), cispclient.WithHTTPClient(cfg.HTTPClient),
		cispclient.WithRequestEditorFn(c.authorise))
	if err != nil {
		return nil, fmt.Errorf("CISP client: %w", err)
	}
	return c, nil
}

// Host is the CISP's configured host (the pull_url guard compares
// against it).
func (c *Client) Host() string { return c.host }

func (c *Client) authorise(ctx context.Context, req *http.Request) error {
	if c.cfg.Tokens == nil {
		return ErrNoTokenSource
	}
	tok, err := c.cfg.Tokens.Token(ctx, c.cfg.BaseURL, ScopeCISRead)
	if err != nil {
		return fmt.Errorf("token for the CISP: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	return nil
}

// StatusError is an answer of the CISP other than the ones a call
// expects: its status and the problem's type slug, when it sent one.
type StatusError struct {
	Status int
	Slug   string
	Detail string
}

func (e *StatusError) Error() string {
	s := "CISP answered " + strconv.Itoa(e.Status)
	if e.Slug != "" {
		s += " " + e.Slug
	}
	if e.Detail != "" {
		s += ": " + e.Detail
	}
	return s
}

func statusError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, MaxErrorBodyBytes))
	e := &StatusError{Status: resp.StatusCode}
	var p cispclient.Problem
	if json.Unmarshal(body, &p) == nil {
		if i := strings.LastIndex(p.Type, "/"); i >= 0 {
			e.Slug = short(p.Type[i+1:])
		}
		if p.Detail != nil {
			e.Detail = short(*p.Detail)
		}
	}
	return e
}

// The headers of GET /v1/{dataset}/versions/{v} that carry the
// publisher's detached JWS over the version's bytes, as the CISP
// received it (the pinned api/clients/cisp.yaml).
const (
	HeaderPublisherSignature = "X-Publisher-Signature"
	HeaderPublisherKID       = "X-Publisher-Kid"
)

// Fetched is one answer to a dataset read.
type Fetched struct {
	// Status is 200, 304 (If-None-Match named the current version) or
	// 404 (the dataset has no version yet: the problem no_version).
	Status int
	ETag   string
	// Version is X-CIS-Version (0 when absent).
	Version     int64
	ContentType string
	Body        []byte
	// PublisherSignature and PublisherKID are X-Publisher-Signature and
	// X-Publisher-Kid ("" when absent; only a version read sends them).
	PublisherSignature string
	PublisherKID       string
}

// GetDataset is GET /v1/{dataset}, unfiltered, with If-None-Match etag
// when it is not empty.
func (c *Client) GetDataset(ctx context.Context, d Dataset, etag string) (Fetched, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	p := &cispclient.GetDatasetParams{}
	if etag != "" {
		p.IfNoneMatch = &etag
	}
	resp, err := c.gen.GetDataset(ctx, cispclient.GetDatasetParamsDataset(d), p)
	if err != nil {
		return Fetched{}, err
	}
	return c.read(resp)
}

// GetVersion is GET /v1/{dataset}/versions/{v}: one version as
// published.
func (c *Client) GetVersion(ctx context.Context, d Dataset, v int64) (Fetched, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	resp, err := c.gen.GetDatasetVersion(ctx, cispclient.GetDatasetVersionParamsDataset(d), v, &cispclient.GetDatasetVersionParams{})
	if err != nil {
		return Fetched{}, err
	}
	return c.read(resp)
}

// ErrPullURLRefused is returned by GetURL for a pull_url that is not on
// the configured CISP: another scheme, host or port, user information,
// or plain http.
var ErrPullURLRefused = errors.New("pull_url refused")

// checkPullURL says whether raw may be followed: an absolute https URL
// with the scheme, host and port of USSP_CISP_BASE_URL (a missing port
// is the scheme's default) and no user information. Plain http is never
// followed, even when the base URL is http (a test or lab CISP): the
// dataset is then read whole from the base URL.
func (c *Client) checkPullURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	switch {
	case err != nil || !u.IsAbs() || u.Host == "":
		return nil, fmt.Errorf("%w: not an absolute URL", ErrPullURLRefused)
	case u.Scheme != "https":
		return nil, fmt.Errorf("%w: scheme %q, only https is followed", ErrPullURLRefused, short(u.Scheme))
	case u.Scheme != c.base.Scheme:
		return nil, fmt.Errorf("%w: scheme %q, the CISP's is %q", ErrPullURLRefused, short(u.Scheme), c.base.Scheme)
	case u.User != nil:
		return nil, fmt.Errorf("%w: it carries user information", ErrPullURLRefused)
	case !strings.EqualFold(u.Hostname(), c.base.Hostname()):
		return nil, fmt.Errorf("%w: not on the CISP's host", ErrPullURLRefused)
	case effectivePort(u) != effectivePort(c.base):
		return nil, fmt.Errorf("%w: port %s, the CISP's is %s", ErrPullURLRefused, short(effectivePort(u)), effectivePort(c.base))
	}
	return u, nil
}

// effectivePort is u's port, or its scheme's default.
func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if u.Scheme == "http" {
		return "80"
	}
	return "443"
}

// GetURL reads a pull_url; one checkPullURL refuses is never requested.
func (c *Client) GetURL(ctx context.Context, raw string) (Fetched, error) {
	u, err := c.checkPullURL(raw)
	if err != nil {
		return Fetched{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		return Fetched{}, err
	}
	if err := c.authorise(ctx, req); err != nil {
		return Fetched{}, err
	}
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return Fetched{}, err
	}
	return c.read(resp)
}

func (c *Client) read(resp *http.Response) (Fetched, error) {
	defer func() { _ = resp.Body.Close() }()
	f := Fetched{Status: resp.StatusCode, ETag: resp.Header.Get("ETag"), ContentType: resp.Header.Get("Content-Type"),
		PublisherSignature: resp.Header.Get(HeaderPublisherSignature), PublisherKID: resp.Header.Get(HeaderPublisherKID)}
	if v := resp.Header.Get("X-CIS-Version"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return Fetched{}, errors.New("X-CIS-Version is not a version")
		}
		f.Version = n
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotModified:
		return f, nil
	case http.StatusNotFound:
		err := statusError(resp)
		var se *StatusError
		if errors.As(err, &se) && se.Slug == "no_version" {
			return f, nil
		}
		return Fetched{}, err
	default:
		return Fetched{}, statusError(resp)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.cfg.MaxBodyBytes+1))
	if err != nil {
		return Fetched{}, fmt.Errorf("reading the answer: %w", err)
	}
	if int64(len(body)) > c.cfg.MaxBodyBytes {
		return Fetched{}, fmt.Errorf("the answer is longer than %d bytes", c.cfg.MaxBodyBytes)
	}
	f.Body = body
	return f, nil
}

// Changes is GET /v1/changes?since=, optionally for one dataset.
func (c *Client) Changes(ctx context.Context, since int64, d Dataset) (cispclient.ChangeList, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	p := &cispclient.ListChangesParams{Since: &since}
	if d != "" {
		ds := cispclient.ListChangesParamsDataset(d)
		p.Dataset = &ds
	}
	resp, err := c.gen.ListChanges(ctx, p)
	if err != nil {
		return cispclient.ChangeList{}, err
	}
	var out cispclient.ChangeList
	err = c.decode(resp, http.StatusOK, &out)
	return out, err
}

// RestrictionHeads is GET /v1/restrictions/heads?state=&limit=: the
// lifecycle heads the CISP holds in state, at most limit.
func (c *Client) RestrictionHeads(ctx context.Context, state string, limit int) (cispclient.RestrictionList, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	st := cispclient.ListRestrictionsParamsState(state)
	resp, err := c.gen.ListRestrictions(ctx, &cispclient.ListRestrictionsParams{State: &st, Limit: &limit})
	if err != nil {
		return cispclient.RestrictionList{}, err
	}
	var out cispclient.RestrictionList
	err = c.decode(resp, http.StatusOK, &out)
	return out, err
}

func (c *Client) decode(resp *http.Response, want int, v any) error {
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != want {
		return statusError(resp)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.cfg.MaxBodyBytes+1))
	if err != nil {
		return fmt.Errorf("reading the answer: %w", err)
	}
	if int64(len(body)) > c.cfg.MaxBodyBytes {
		return fmt.Errorf("the answer is longer than %d bytes", c.cfg.MaxBodyBytes)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("the answer does not decode: %s", short(err.Error()))
	}
	return nil
}

// Subscribe makes sure this USSP has one subscription with callback for
// datasets within bbox (nil: everywhere), and returns it. It is
// idempotent: it lists the caller's subscriptions first and reuses the
// one with the same callback, patching it when its datasets or box
// differ or it is suspended (a PATCH re-activates it); only when there
// is none does it create one.
func (c *Client) Subscribe(ctx context.Context, callback string, datasets []Dataset, bbox []float64) (cispclient.Subscription, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	resp, err := c.gen.ListSubscriptions(ctx)
	if err != nil {
		return cispclient.Subscription{}, err
	}
	var list cispclient.SubscriptionList
	if err := c.decode(resp, http.StatusOK, &list); err != nil {
		return cispclient.Subscription{}, err
	}
	want := make([]string, len(datasets))
	for i, d := range datasets {
		want[i] = string(d)
	}
	slices.Sort(want)
	for i := range list.Subscriptions {
		s := &list.Subscriptions[i]
		if s.CallbackUrl != callback || s.Status == "deleted" {
			continue
		}
		have := make([]string, len(s.Datasets))
		for i, d := range s.Datasets {
			have[i] = string(d)
		}
		slices.Sort(have)
		var hb []float64
		if s.Bbox != nil {
			hb = *s.Bbox
		}
		if slices.Equal(have, want) && slices.Equal(hb, bbox) && s.Status != "suspended" {
			return *s, nil
		}
		ds := make([]cispclient.SubscriptionPatchDatasets, len(datasets))
		for i, d := range datasets {
			ds[i] = cispclient.SubscriptionPatchDatasets(d)
		}
		b := bbox
		if b == nil {
			b = []float64{}
		}
		resp, err := c.gen.PatchSubscription(ctx, s.Id, cispclient.SubscriptionPatch{Datasets: &ds, Bbox: &b})
		if err != nil {
			return cispclient.Subscription{}, err
		}
		var out cispclient.Subscription
		err = c.decode(resp, http.StatusOK, &out)
		return out, err
	}
	ds := make([]cispclient.SubscriptionCreateDatasets, len(datasets))
	for i, d := range datasets {
		ds[i] = cispclient.SubscriptionCreateDatasets(d)
	}
	body := cispclient.SubscriptionCreate{CallbackUrl: callback, Datasets: ds}
	if bbox != nil {
		b := slices.Clone(bbox)
		body.Bbox = &b
	}
	resp, err = c.gen.CreateSubscription(ctx, body)
	if err != nil {
		return cispclient.Subscription{}, err
	}
	var out cispclient.Subscription
	err = c.decode(resp, http.StatusCreated, &out)
	return out, err
}

// sinceVersion is the since_version of a pull_url, or false.
func sinceVersion(raw string) (int64, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return 0, false
	}
	v := u.Query().Get("since_version")
	n, err := strconv.ParseInt(v, 10, 64)
	return n, err == nil && n >= 0
}

// hostOf is the host of an absolute URL without its port, or "".
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() {
		return ""
	}
	return u.Hostname()
}
