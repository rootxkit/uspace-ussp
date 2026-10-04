package status

import (
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

	"github.com/rootxkit/uspace-ussp/internal/status/authclient"
)

// ScopeStatus is the scope of the operating-status notice (cross-plan
// M23).
const ScopeStatus = "certificates.status"

// TokenSource hands out an ecosystem token whose audience is the host
// of baseURL (auth.Outgoing; M18).
type TokenSource interface {
	Token(ctx context.Context, baseURL string, scopes ...string) (string, error)
}

// Bounds of one call.
const (
	CallTimeout    = 10 * time.Second
	MaxAnswerBytes = 64 << 10
)

// Client is the authority's POST /v1/certificates/{id}/status through
// the client generated from the pinned api/clients/authority.yaml.
type Client struct {
	baseURL string
	tokens  TokenSource
	c       *authclient.Client
}

// NewClient is the client of the authority at baseURL (redirects are not
// followed).
func NewClient(baseURL string, tokens TokenSource, hc *http.Client) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("authority base URL %q is not an absolute http(s) URL without userinfo, query or fragment", baseURL)
	}
	if tokens == nil {
		return nil, errors.New("no token source for the authority")
	}
	h := &http.Client{Timeout: CallTimeout}
	if hc != nil {
		*h = *hc
	}
	h.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c, err := authclient.NewClient(strings.TrimRight(baseURL, "/"), authclient.WithHTTPClient(h))
	if err != nil {
		return nil, err
	}
	return &Client{baseURL: baseURL, tokens: tokens, c: c}, nil
}

// Post implements Authority: the notice with this USSP's reference; 201
// (recorded) and 200 (recorded before) give the authority's notice id;
// a refusal of the request (400, 403, 404, 409) is permanent; anything
// else is tried again.
func (c *Client) Post(ctx context.Context, q Queued) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	tok, err := c.tokens.Token(ctx, c.baseURL, ScopeStatus)
	if err != nil {
		return "", fmt.Errorf("no token for the authority: %w", err)
	}
	ref := q.Reference
	body := authclient.CertificateStatusNotice{State: authclient.CertificateStatusNoticeState(AuthorityState(q.Kind)), At: q.At.UTC(), Reference: &ref}
	resp, err := c.c.PostCertificateStatus(ctx, q.CertificateID, body, func(_ context.Context, r *http.Request) error {
		r.Header.Set("Authorization", "Bearer "+tok)
		r.Header.Set("Accept", "application/json")
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("the authority was not reached: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxAnswerBytes+1))
	if err != nil {
		return "", err
	}
	if len(b) > MaxAnswerBytes {
		return "", fmt.Errorf("answer longer than %d bytes", MaxAnswerBytes)
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		var res authclient.CertificateNoticeResult
		if err := json.Unmarshal(b, &res); err != nil || res.Notice.Id <= 0 {
			return "", errors.New("the answer does not read as CertificateNoticeResult")
		}
		return strconv.FormatInt(res.Notice.Id, 10), nil
	case http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict:
		return "", &PermanentError{Status: resp.StatusCode, Detail: detailOf(b)}
	}
	return "", fmt.Errorf("the authority answered %d: %s", resp.StatusCode, detailOf(b))
}

func detailOf(b []byte) string {
	var p struct {
		Type   string `json:"type"`
		Detail string `json:"detail"`
	}
	if json.Unmarshal(b, &p) != nil {
		return "no problem body"
	}
	s := strings.TrimSpace(p.Type[strings.LastIndex(p.Type, "/")+1:] + " " + p.Detail)
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
