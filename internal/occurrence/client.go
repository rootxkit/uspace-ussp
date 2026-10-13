package occurrence

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/occurrence/authclient"
)

// ScopeOccurrences is the scope of POST /v1/occurrences (the
// authority's x-scope).
const ScopeOccurrences = "occurrences.write"

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

// Client is the authority's POST /v1/occurrences through the client
// generated from the pinned api/clients/authority.yaml: the Deliverer of
// the Service.
type Client struct {
	baseURL string
	tokens  TokenSource
	c       *authclient.Client
}

var _ Deliverer = (*Client)(nil)

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

// stored is a queued body as the store holds it. Kind is the class
// under the name reports queued before the authority's contract was
// pinned used (H-1); Category wins when both are present.
type stored struct {
	Payload
	Kind string `json:"kind"`
}

func strp(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// WireOf is the authority's OccurrenceReport of a queued body. It is a
// function of the bytes alone, so every try of one report sends the same
// report and a repeat after a lost answer is the authority's replay (200),
// never a 409. A body it cannot map is an error, found before any token
// is asked for or anything is sent.
func WireOf(body []byte) (authclient.OccurrenceReport, error) {
	var p stored
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&p); err != nil {
		return authclient.OccurrenceReport{}, fmt.Errorf("the queued body does not read as occurrence/v1: %w", err)
	}
	category := p.Category
	if category == "" {
		category = p.Kind
	}
	switch {
	case p.Schema != Schema:
		return authclient.OccurrenceReport{}, fmt.Errorf("the queued body's schema is %q, not %s", p.Schema, Schema)
	case p.ReportRef == "" || len(p.ReportRef) > 128:
		return authclient.OccurrenceReport{}, errors.New("the queued body's report_ref is empty or longer than 128")
	case !authclient.OccurrenceCategory(category).Valid():
		return authclient.OccurrenceReport{}, fmt.Errorf("the queued body's category %q is not one of the authority's", category)
	case !authclient.OccurrenceChannel(p.Channel).Valid():
		return authclient.OccurrenceReport{}, fmt.Errorf("the queued body's channel %q is not one of the authority's", p.Channel)
	case p.OccurredAt.IsZero() || p.BecameAwareAt.IsZero():
		return authclient.OccurrenceReport{}, errors.New("the queued body has no occurred_at or became_aware_at")
	}
	aircraft := make([]authclient.OccurrenceAircraftInput, 0, len(p.Aircraft))
	for _, a := range p.Aircraft {
		x := authclient.OccurrenceAircraftInput{Serial: strp(a.Serial), FlightId: strp(a.FlightID)}
		if a.OperatorReg != nil {
			x.OperatorReg = strp(*a.OperatorReg)
		}
		if a.AuthorisationNumber != nil {
			x.AuthorisationNumber = strp(*a.AuthorisationNumber)
		}
		aircraft = append(aircraft, x)
	}
	manned := make([]authclient.OccurrenceManned, 0, len(p.Manned))
	for _, m := range p.Manned {
		x := authclient.OccurrenceManned{Icao24: strp(m.ICAO24)}
		if m.Callsign != nil {
			x.Callsign = strp(*m.Callsign)
		}
		manned = append(manned, x)
	}
	intents := append([]string{}, p.IntentRefs...)
	evidence := append([]string{}, p.EvidenceURLs...)
	narrative := p.Narrative
	occurred, aware := p.OccurredAt.UTC(), p.BecameAwareAt.UTC()
	out := authclient.OccurrenceReport{Schema: authclient.Occurrencev1, ReportRef: p.ReportRef, Channel: authclient.OccurrenceChannel(p.Channel),
		Category: authclient.OccurrenceCategory(category), OccurredAt: occurred, BecameAwareAt: aware,
		Reporter: &authclient.OccurrenceReporterInput{Org: strp(p.Reporter.Org), PersonRef: strp(p.Reporter.PersonRef)},
		Aircraft: &aircraft, Manned: &manned, IntentRefs: &intents, EvidenceUrls: &evidence, Narrative: &narrative}
	if !p.ReportedAt.IsZero() {
		r := p.ReportedAt.UTC()
		out.ReportedAt = &r
	}
	if s := p.MinSeparation; s != nil {
		at := s.At.UTC()
		out.MinSeparation = &authclient.OccurrenceSeparation{HM: s.HM, VM: s.VM}
		if !s.At.IsZero() {
			out.MinSeparation.At = &at
		}
	}
	return out, nil
}

// Submit implements Deliverer: the queued body mapped to the authority's
// occurrence/v1 and posted. 201 (received) and 200 (received before:
// the same report again, idempotent on the token's sub and report_ref)
// give the authority's occurrence id. A 409 (report_ref_conflict: another
// report under this report_ref) is permanent and a conflict; any other
// 4xx but 408 and 429 is permanent. A timeout, a 408, a 429, a 5xx (one
// the authority answered after its commit too: the next try is its
// replay) or an answer that does not read as a receipt is an error the
// Service tries again, never a refusal.
func (c *Client) Submit(ctx context.Context, body []byte) (string, error) {
	rep, err := WireOf(body)
	if err != nil {
		return "", &PermanentError{Detail: err.Error()}
	}
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	tok, err := c.tokens.Token(ctx, c.baseURL, ScopeOccurrences)
	if err != nil {
		return "", fmt.Errorf("no token for the authority: %w", err)
	}
	resp, err := c.c.CreateOccurrence(ctx, rep, func(_ context.Context, r *http.Request) error {
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
		return "", fmt.Errorf("the authority's answer (%d) was not read: %w", resp.StatusCode, err)
	}
	if len(b) > MaxAnswerBytes {
		return "", fmt.Errorf("the authority's answer (%d) is longer than %d bytes", resp.StatusCode, MaxAnswerBytes)
	}
	switch s := resp.StatusCode; {
	case s == http.StatusOK || s == http.StatusCreated:
		var res authclient.OccurrenceReceipt
		if err := json.Unmarshal(b, &res); err != nil || res.OccurrenceId == "" || res.ReportRef != rep.ReportRef {
			return "", fmt.Errorf("the authority's answer (%d) does not read as the receipt of %s", s, rep.ReportRef)
		}
		return res.OccurrenceId, nil
	case s == http.StatusConflict:
		return "", &PermanentError{Status: s, Detail: fmt.Sprintf("409 %s", detailOf(b))}
	case s == http.StatusRequestTimeout || s == http.StatusTooManyRequests || s >= 500:
		return "", fmt.Errorf("the authority answered %d: %s", s, detailOf(b))
	default:
		return "", &PermanentError{Status: s, Detail: fmt.Sprintf("%d %s", s, detailOf(b))}
	}
}

// detailOf is the problem type's slug and detail of b, at most 200
// bytes; never the body itself.
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
