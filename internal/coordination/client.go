package coordination

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

	"github.com/rootxkit/uspace-ussp/internal/coordination/anspclient"
)

// ScopeCoordination is the scope of a token for the ANSP's coordination
// inbox (cross-plan M23).
const ScopeCoordination = "ansp.coordination"

// TokenSource hands out an ecosystem token whose audience is the host
// of baseURL (auth.Outgoing; M18).
type TokenSource interface {
	Token(ctx context.Context, baseURL string, scopes ...string) (string, error)
}

// Bounds of one call.
const (
	// DefaultCallTimeout bounds one call to the ANSP.
	DefaultCallTimeout = 5 * time.Second
	// MaxAnswerBytes bounds an answer read from the ANSP (E-10).
	MaxAnswerBytes = 64 << 10
)

// Client is the ANSP's coordination inbox through the client generated
// from the pinned api/clients/ansp.yaml (never a hand-built one). It
// never follows a redirect: the answer is the ANSP's or an error.
type Client struct {
	baseURL string
	tokens  TokenSource
	c       *anspclient.Client
	timeout time.Duration
}

// NewClient is the client of the ANSP at baseURL with tokens and hc (a
// plain client when nil; its redirects are never followed).
func NewClient(baseURL string, tokens TokenSource, hc *http.Client) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("ANSP base URL %q is not an absolute http(s) URL without userinfo, query or fragment", baseURL)
	}
	if tokens == nil {
		return nil, errors.New("no token source for the ANSP (USSP_TOKEN_ISSUERS, USSP_TOKEN_CLIENT_SECRET_FILE)")
	}
	h := &http.Client{}
	if hc != nil {
		*h = *hc
	}
	h.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c, err := anspclient.NewClient(strings.TrimRight(baseURL, "/"), anspclient.WithHTTPClient(h))
	if err != nil {
		return nil, err
	}
	return &Client{baseURL: baseURL, tokens: tokens, c: c, timeout: DefaultCallTimeout}, nil
}

func (c *Client) bearer(ctx context.Context) (anspclient.RequestEditorFn, error) {
	tok, err := c.tokens.Token(ctx, c.baseURL, ScopeCoordination)
	if err != nil {
		return nil, &RetryableError{Detail: "no token for the ANSP: " + err.Error()}
	}
	return func(_ context.Context, r *http.Request) error {
		r.Header.Set("Authorization", "Bearer "+tok)
		r.Header.Set("Accept", "application/json")
		return nil
	}, nil
}

// read reads at most MaxAnswerBytes of resp's body; more is an error.
func read(resp *http.Response) ([]byte, error) {
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxAnswerBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxAnswerBytes {
		return nil, fmt.Errorf("answer longer than %d bytes", MaxAnswerBytes)
	}
	return b, nil
}

// problemDetail is the type slug and detail of a problem body, clipped.
func problemDetail(b []byte) string {
	var p struct {
		Type   string `json:"type"`
		Detail string `json:"detail"`
	}
	if json.Unmarshal(b, &p) != nil || (p.Type == "" && p.Detail == "") {
		return "no problem body"
	}
	slug := p.Type[strings.LastIndex(p.Type, "/")+1:]
	return clip(strings.TrimSpace(slug + " " + p.Detail))
}

func retryAfter(h http.Header) time.Duration {
	s, err := strconv.Atoi(strings.TrimSpace(h.Get("Retry-After")))
	if err != nil || s <= 0 {
		return 0
	}
	return time.Duration(s) * time.Second
}

// classify is the error of a status that is not the success: a refusal
// of the request itself (400, 403, 404, 409, 413, 415, 422) is
// permanent; anything else (401 while a token renews, 408, 429, 5xx, a
// redirect) is tried again.
func classify(resp *http.Response, body []byte) error {
	d := problemDetail(body)
	switch resp.StatusCode {
	case http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict,
		http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity:
		return &PermanentError{Status: resp.StatusCode, Detail: d}
	}
	return &RetryableError{Status: resp.StatusCode, Detail: d, RetryAfter: retryAfter(resp.Header)}
}

// Submit implements ANSP: POST /v1/coordination/notices with body as it
// is. 202 is the receipt, 200 the first receipt of a repeat.
func (c *Client) Submit(ctx context.Context, body []byte) (Receipt, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	auth, err := c.bearer(ctx)
	if err != nil {
		return Receipt{}, err
	}
	resp, err := c.c.SubmitCoordinationNoticeWithBody(ctx, "application/json", bytes.NewReader(body), auth)
	if err != nil {
		return Receipt{}, &RetryableError{Detail: clip(err.Error())}
	}
	b, err := read(resp)
	if err != nil {
		return Receipt{}, &RetryableError{Status: resp.StatusCode, Detail: err.Error()}
	}
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		return Receipt{}, classify(resp, b)
	}
	var r anspclient.NoticeReceipt
	if err := json.Unmarshal(b, &r); err != nil || r.AckId == "" || len(r.AckId) > 64 || r.State != anspclient.NoticeReceiptStateReceived {
		return Receipt{}, &RetryableError{Status: resp.StatusCode, Detail: "the receipt does not read as NoticeReceipt"}
	}
	return Receipt{AckID: r.AckId, ReceivedAt: r.ReceivedAt.UTC(), Repeat: resp.StatusCode == http.StatusOK}, nil
}

// Get implements ANSP: GET /v1/coordination/notices/{ack_id}.
func (c *Client) Get(ctx context.Context, ackID string) (NoticeState, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	auth, err := c.bearer(ctx)
	if err != nil {
		return NoticeState{}, err
	}
	resp, err := c.c.GetCoordinationNotice(ctx, ackID, auth)
	if err != nil {
		return NoticeState{}, &RetryableError{Detail: clip(err.Error())}
	}
	b, err := read(resp)
	if err != nil {
		return NoticeState{}, &RetryableError{Status: resp.StatusCode, Detail: err.Error()}
	}
	if resp.StatusCode != http.StatusOK {
		return NoticeState{}, classify(resp, b)
	}
	var n anspclient.CoordinationNotice
	if err := json.Unmarshal(b, &n); err != nil || !n.State.Valid() {
		return NoticeState{}, &RetryableError{Status: resp.StatusCode, Detail: "the notice does not read as CoordinationNotice"}
	}
	out := NoticeState{State: string(n.State)}
	if n.AcknowledgedAt != nil {
		t := n.AcknowledgedAt.UTC()
		out.AcknowledgedAt = &t
	}
	if n.AcknowledgedBy != nil {
		out.AcknowledgedBy = clip(string(*n.AcknowledgedBy))
	}
	return out, nil
}
