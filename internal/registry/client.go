package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/regnum"
	"github.com/rootxkit/uspace-core/serial"

	"github.com/rootxkit/uspace-ussp/internal/registry/authclient"
)

// ScopeRegistryValidate is the scope of every F8 call (spec 06 §3).
const ScopeRegistryValidate = "registry.validate"

// Defaults of ClientConfig.
const (
	// DefaultTimeout is the deadline of one F8 call (brief WP-5).
	DefaultTimeout = 2 * time.Second
	// DefaultMaxBodyBytes bounds an answer: a full batch of MaxQueries
	// or a change page of MaxChangesLimit is far below it (E-10).
	DefaultMaxBodyBytes = 1 << 20
	// MaxErrorBodyBytes is how much of an error answer is read.
	MaxErrorBodyBytes = 8 << 10
	// MaxChangesLimit is the largest change page the contract allows.
	MaxChangesLimit = 1000
	// MaxPublicKeyLen bounds the key a change names.
	MaxPublicKeyLen = 128
	// maxETagLen bounds an ETag kept as the feed's cursor.
	maxETagLen = 128
)

// TokenSource hands out an ecosystem token whose aud is the host of
// baseURL (M18) with the scopes; internal/auth.Outgoing implements it.
type TokenSource interface {
	Token(ctx context.Context, baseURL string, scopes ...string) (string, error)
}

// ErrNoTokenSource is returned by every call of a Client built without a
// token source: the authority refuses an unauthenticated lookup.
var ErrNoTokenSource = errors.New("no token client for the authority (USSP_TOKEN_ISSUERS and USSP_TOKEN_CLIENT_SECRET_FILE)")

// ClientConfig configures a Client.
type ClientConfig struct {
	// BaseURL is USSP_AUTHORITY_BASE_URL.
	BaseURL string
	Tokens  TokenSource
	// HTTPClient is used for every call; nil is a client that never
	// follows a redirect (a 3xx is an error).
	HTTPClient *http.Client
	// Timeout is the deadline of one call (DefaultTimeout).
	Timeout time.Duration
	// MaxBodyBytes bounds an answer (DefaultMaxBodyBytes).
	MaxBodyBytes int64
}

// Client calls the authority's F8 API through the generated client of
// the pinned api/clients/authority.yaml.
type Client struct {
	cfg ClientConfig
	gen *authclient.Client
}

// NewClient builds a Client; a base URL that is not an absolute http(s)
// URL is an error naming USSP_AUTHORITY_BASE_URL.
func NewClient(cfg ClientConfig) (*Client, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, core.Fieldf("USSP_AUTHORITY_BASE_URL", "not an absolute http(s) URL")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = DefaultMaxBodyBytes
	}
	c := &Client{cfg: cfg}
	c.gen, err = authclient.NewClient(strings.TrimRight(cfg.BaseURL, "/"), authclient.WithHTTPClient(cfg.HTTPClient),
		authclient.WithRequestEditorFn(c.authorise))
	if err != nil {
		return nil, fmt.Errorf("authority client: %w", err)
	}
	return c, nil
}

func (c *Client) authorise(ctx context.Context, req *http.Request) error {
	if c.cfg.Tokens == nil {
		return ErrNoTokenSource
	}
	tok, err := c.cfg.Tokens.Token(ctx, c.cfg.BaseURL, ScopeRegistryValidate)
	if err != nil {
		return fmt.Errorf("token for the authority: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	return nil
}

// StatusError is an answer of the authority other than the one a call
// expects: its status and the problem's type slug, when it sent one.
type StatusError struct {
	Status int
	Slug   string
}

func (e *StatusError) Error() string {
	s := "authority answered " + strconv.Itoa(e.Status)
	if e.Slug != "" {
		s += " " + e.Slug
	}
	return s
}

func statusError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, MaxErrorBodyBytes))
	e := &StatusError{Status: resp.StatusCode}
	var p authclient.Problem
	if json.Unmarshal(body, &p) == nil {
		if i := strings.LastIndex(p.Type, "/"); i >= 0 {
			e.Slug = clip(p.Type[i+1:])
		}
	}
	return e
}

// RefusedError is an answer this USSP refuses: Counter is the counter
// it is counted under (CounterPIIRefused, CounterAnswerRefused,
// CounterFeedRefused) and Detail says why without repeating a value.
type RefusedError struct {
	Counter string
	Detail  string
}

func (e *RefusedError) Error() string {
	return "authority answer refused (" + e.Counter + "): " + e.Detail
}

func refusedPII(format string, args ...any) error {
	return &RefusedError{Counter: CounterPIIRefused, Detail: fmt.Sprintf(format, args...)}
}

func refusedAnswer(format string, args ...any) error {
	return &RefusedError{Counter: CounterAnswerRefused, Detail: fmt.Sprintf(format, args...)}
}

// readBody reads at most MaxBodyBytes of an expected answer.
func (c *Client) readBody(resp *http.Response) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.cfg.MaxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading the answer: %w", err)
	}
	if int64(len(body)) > c.cfg.MaxBodyBytes {
		return nil, refusedAnswer("the answer is longer than %d bytes", c.cfg.MaxBodyBytes)
	}
	return body, nil
}

// decodeStrict decodes body into v refusing any field the contract does
// not name: the response schemas of F8 have no name, address, phone or
// e-mail, so a field outside them is personal data this USSP did not
// ask for and never stores (CLAUDE.md rule 8). The field's name is
// reported, never its value.
func decodeStrict(body []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if name, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
			return refusedPII("the answer carries the field %s, which F8 does not define", clip(name))
		}
		return refusedAnswer("the answer does not decode")
	}
	if dec.More() {
		return refusedAnswer("trailing data after the answer")
	}
	return nil
}

// Validate asks the authority about the queries: one GET for a single
// query, else one POST batch (at most MaxQueries). The answers are in
// the order of qs and each one is checked against what was asked
// (checkAnswer); one refused answer refuses the call.
func (c *Client) Validate(ctx context.Context, p Purpose, qs []normalised) ([]authclient.RegistryValidity, error) {
	if len(qs) == 0 || len(qs) > MaxQueries {
		return nil, fmt.Errorf("validate: %d queries, want 1 to %d", len(qs), MaxQueries)
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	purpose := authclient.RegistryPurpose(p)
	var out []authclient.RegistryValidity
	if len(qs) == 1 {
		q := qs[0]
		params := &authclient.ValidateRegistryParams{Purpose: purpose, Operator: opt(q.operatorPublic), Serial: opt(q.serial), Pilot: opt(q.pilot)}
		resp, err := c.gen.ValidateRegistry(ctx, params)
		if err != nil {
			return nil, err
		}
		var v authclient.RegistryValidity
		if err := c.decode(resp, &v); err != nil {
			return nil, err
		}
		out = []authclient.RegistryValidity{v}
	} else {
		body := authclient.RegistryValidateBatch{Items: make([]authclient.RegistryValidateItem, len(qs))}
		for i, q := range qs {
			body.Items[i] = authclient.RegistryValidateItem{Operator: opt(q.operatorPublic), Serial: opt(q.serial), Pilot: opt(q.pilot)}
		}
		resp, err := c.gen.ValidateRegistryBatch(ctx, &authclient.ValidateRegistryBatchParams{Purpose: purpose}, body)
		if err != nil {
			return nil, err
		}
		var list authclient.RegistryValidityList
		if err := c.decode(resp, &list); err != nil {
			return nil, err
		}
		if len(list.Results) != len(qs) {
			return nil, refusedAnswer("%d answers to %d queries", len(list.Results), len(qs))
		}
		out = list.Results
	}
	for i := range out {
		if err := checkAnswer(qs[i], out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (c *Client) decode(resp *http.Response, v any) error {
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return statusError(resp)
	}
	body, err := c.readBody(resp)
	if err != nil {
		return err
	}
	return decodeStrict(body, v)
}

func opt(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// checkAnswer refuses an answer that is not the answer to q: a part for
// a key not asked or none for a key asked, another key echoed, a status
// F8 does not define, a field beyond its bound, and an operator number
// echoed with a secret part (personal data, spec 06 §5).
func checkAnswer(q normalised, v authclient.RegistryValidity) error {
	if (q.operatorKey != "") != (v.Operator != nil) || (q.serial != "") != (v.Uas != nil) || (q.pilot != "") != (v.Pilot != nil) {
		return refusedAnswer("the answer's parts are not the keys asked")
	}
	if o := v.Operator; o != nil {
		switch {
		case regnum.CompareKey(o.RegistrationNumber) != q.operatorKey:
			return refusedAnswer("the operator answered is not the one asked")
		case regnum.PublicPart(o.RegistrationNumber) != strings.TrimSpace(o.RegistrationNumber):
			return refusedPII("the operator number is echoed with a secret part")
		case !Status(o.Status).Valid():
			return refusedAnswer("operator status %s is not an F8 status", clip(string(o.Status)))
		}
	}
	if u := v.Uas; u != nil {
		switch {
		case serial.Normalize(u.Serial) != q.serial:
			return refusedAnswer("the serial answered is not the one asked")
		case !Status(u.Status).Valid():
			return refusedAnswer("UAS status %s is not an F8 status", clip(string(u.Status)))
		case u.ClassLabel != nil && len(*u.ClassLabel) > MaxFieldLen, u.MtomBand != nil && len(*u.MtomBand) > MaxFieldLen:
			return refusedAnswer("a UAS field is longer than %d bytes", MaxFieldLen)
		}
	}
	if p := v.Pilot; p != nil {
		switch {
		case strings.TrimSpace(p.Pilot) != q.pilot:
			return refusedAnswer("the pilot answered is not the one asked")
		case !Status(p.Status).Valid():
			return refusedAnswer("pilot status %s is not an F8 status", clip(string(p.Status)))
		case len(p.Competencies) > MaxCompetencies:
			return refusedAnswer("%d competencies, at most %d", len(p.Competencies), MaxCompetencies)
		}
		for _, c := range p.Competencies {
			if c.Competency == "" || len(c.Competency) > MaxFieldLen {
				return refusedAnswer("a competency name is empty or longer than %d bytes", MaxFieldLen)
			}
		}
	}
	return nil
}

// ChangesPage is one answer of the change feed.
type ChangesPage struct {
	// NotModified is a 304: nothing newer than the page ETag named.
	NotModified bool
	ETag        string
	Changes     []authclient.RegistryChange
	NextSince   int64
}

// Changes is GET /v1/registry/changes?since=&limit=, with If-None-Match
// etag when it is not empty. A page is checked: every change after
// since and at most next_since, next_since never before since, at most
// limit changes, an entity type F8 defines and a key within its bound.
func (c *Client) Changes(ctx context.Context, since int64, limit int, etag string) (ChangesPage, error) {
	if limit <= 0 || limit > MaxChangesLimit {
		limit = MaxChangesLimit
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	p := &authclient.ListRegistryChangesParams{Since: &since, Limit: &limit}
	if etag != "" {
		p.IfNoneMatch = &etag
	}
	resp, err := c.gen.ListRegistryChanges(ctx, p)
	if err != nil {
		return ChangesPage{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	out := ChangesPage{ETag: resp.Header.Get("ETag")}
	if len(out.ETag) > maxETagLen {
		out.ETag = ""
	}
	switch resp.StatusCode {
	case http.StatusNotModified:
		out.NotModified, out.NextSince = true, since
		return out, nil
	case http.StatusOK:
	default:
		return ChangesPage{}, statusError(resp)
	}
	body, err := c.readBody(resp)
	if err != nil {
		return ChangesPage{}, err
	}
	var page authclient.RegistryChangePage
	if err := decodeStrict(body, &page); err != nil {
		return ChangesPage{}, err
	}
	if err := checkPage(page, since, limit); err != nil {
		return ChangesPage{}, err
	}
	out.Changes, out.NextSince = page.Changes, page.NextSince
	return out, nil
}

func refusedFeed(format string, args ...any) error {
	return &RefusedError{Counter: CounterFeedRefused, Detail: fmt.Sprintf(format, args...)}
}

func checkPage(page authclient.RegistryChangePage, since int64, limit int) error {
	switch {
	case page.NextSince < since:
		return refusedFeed("next_since %d is before since %d", page.NextSince, since)
	case len(page.Changes) > limit:
		return refusedFeed("%d changes on a page of %d", len(page.Changes), limit)
	}
	for _, ch := range page.Changes {
		switch {
		case ch.Seq <= since || ch.Seq > page.NextSince:
			return refusedFeed("change %d is outside (%d, %d]", ch.Seq, since, page.NextSince)
		case ch.EntityType != authclient.RegistryChangeEntityType(EntityOperator) && ch.EntityType != authclient.RegistryChangeEntityType(EntityUAS) &&
			ch.EntityType != authclient.RegistryChangeEntityType(EntityPilot):
			return refusedFeed("change %d names the entity type %s", ch.Seq, clip(string(ch.EntityType)))
		case strings.TrimSpace(ch.PublicKey) == "" || len(ch.PublicKey) > MaxPublicKeyLen:
			return refusedFeed("change %d has an empty key or one longer than %d bytes", ch.Seq, MaxPublicKeyLen)
		}
	}
	return nil
}

// clip bounds a value repeated in a message.
func clip(s string) string {
	const n = 64
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
