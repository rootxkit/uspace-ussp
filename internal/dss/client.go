package dss

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3548"

	stdf3548 "github.com/rootxkit/uspace-ussp/internal/stdapi/f3548"
)

// TokenSource hands out ecosystem tokens whose aud is the host of the
// target's base URL (internal/auth.Outgoing, M18).
type TokenSource interface {
	Token(ctx context.Context, baseURL string, scopes ...string) (string, error)
}

// Bounds and timing of the client (E-10).
const (
	// MaxAnswerBytes bounds a DSS or peer answer read.
	MaxAnswerBytes = f3548.MaxMessageBytes
	// DefaultCallTimeout bounds one attempt of one DSS or peer call.
	DefaultCallTimeout = 5 * time.Second
	// DefaultAttempts bounds the attempts of one call: a 5xx, a 429 or no
	// answer is tried again after a backoff, a 4xx never.
	DefaultAttempts = 3
	// DefaultRetryBackoff is the wait before the second attempt, doubled
	// before each further one.
	DefaultRetryBackoff = 200 * time.Millisecond
	// MaxReferences bounds the references one DSS query answer may hold;
	// more is refused as an answer that cannot be judged.
	MaxReferences = 1000
	// MaxSubscribers bounds the subscribers one DSS answer may list.
	MaxSubscribers = 100
)

// ErrDSSDown marks a failure that says the DSS (or a peer) is
// unreachable: no answer, a 5xx or a 429, after every attempt.
var ErrDSSDown = errors.New("unreachable")

// ErrNotFound is a 404 answer.
var ErrNotFound = errors.New("not found")

// RefusedError is a 4xx answer other than 404 and 409: the call was
// refused and is never retried as it is.
type RefusedError struct {
	Status int
	Msg    string
}

func (e *RefusedError) Error() string { return fmt.Sprintf("refused with %d: %s", e.Status, e.Msg) }

// ConflictError is the DSS's 409 to a write of an operational intent
// reference (AirspaceConflictResponse): the references whose ovns the key
// lacked. A 409 that is not an AirspaceConflictResponse has no members.
type ConflictError struct {
	Message                   string
	MissingOperationalIntents []f3548.OperationalIntentReference
	MissingConstraints        []f3548.ConstraintReference
}

func (e *ConflictError) Error() string {
	ids := make([]string, 0, len(e.MissingOperationalIntents)+len(e.MissingConstraints))
	for i := range e.MissingOperationalIntents {
		ids = append(ids, e.MissingOperationalIntents[i].Id)
	}
	for i := range e.MissingConstraints {
		ids = append(ids, e.MissingConstraints[i].Id)
	}
	return fmt.Sprintf("the DSS answered 409 (%s), missing the ovns of %s", e.Message, strings.Join(ids, ", "))
}

// Reach is what the client last learnt of the DSS.
type Reach struct {
	// Known is false before the first DSS call.
	Known bool
	Up    bool
	Since time.Time
	// Reason is why it is down, or what is wrong while it answers.
	Reason string
	// DriftS is the difference between the DSS's Date header and this
	// host's clock, minus half the round trip, when an answer carried
	// one (F3548 TimeSyncMaxDifferentialSeconds); DriftKnown says so.
	DriftS     float64
	DriftKnown bool
}

// Client is the F3548 DSS and USS-to-USS client: the operations of
// docs/PLAN.md §6.2 over the generated client of internal/stdapi/f3548,
// every call with an ecosystem token for the scope the standard names and
// aud = the host of the target (the DSS of DSSBaseURL, a peer's
// uss_base_url), a deadline, a bounded answer, and a retry on a 5xx, a 429
// or no answer, never on a 4xx. Safe for concurrent use.
type Client struct {
	DSSBaseURL string
	Tokens     TokenSource
	// HTTP is the transport of every call (internal/dss wraps it with
	// the exchange log); nil is a client with DefaultCallTimeout.
	HTTP        *http.Client
	Attempts    int
	Backoff     time.Duration
	CallTimeout time.Duration
	Now         func() time.Time

	once   sync.Once
	dss    *stdf3548.StdClient
	dssErr error

	mu    sync.Mutex
	reach Reach
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) attempts() int {
	if c.Attempts <= 0 {
		return DefaultAttempts
	}
	return c.Attempts
}

func (c *Client) backoff() time.Duration {
	if c.Backoff <= 0 {
		return DefaultRetryBackoff
	}
	return c.Backoff
}

func (c *Client) callTimeout() time.Duration {
	if c.CallTimeout <= 0 {
		return DefaultCallTimeout
	}
	return c.CallTimeout
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: c.callTimeout()}
}

// Reach is the DSS as the client last saw it.
func (c *Client) Reach() Reach {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reach
}

func (c *Client) setReach(up bool, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.reach.Known || c.reach.Up != up {
		c.reach.Since = c.now().UTC()
	}
	c.reach.Known, c.reach.Up, c.reach.Reason = true, up, reason
}

func (c *Client) setDrift(res *http.Response, sent, got time.Time) {
	d, err := http.ParseTime(res.Header.Get("Date"))
	if err != nil {
		return
	}
	// The Date header has a resolution of one second: the drift is the
	// header against the middle of the round trip.
	mid := sent.Add(got.Sub(sent) / 2)
	c.mu.Lock()
	c.reach.DriftS, c.reach.DriftKnown = d.Sub(mid.Truncate(time.Second)).Seconds(), true
	c.mu.Unlock()
}

func (c *Client) client() (*stdf3548.StdClient, error) {
	c.once.Do(func() {
		if c.DSSBaseURL == "" {
			c.dssErr = errors.New("no DSS base URL")
			return
		}
		c.dss, c.dssErr = stdf3548.NewClient(strings.TrimRight(c.DSSBaseURL, "/"), stdf3548.WithHTTPClient(c.httpClient()))
	})
	return c.dss, c.dssErr
}

// bearer adds a token for the target's base URL with scope.
func (c *Client) bearer(target string, scope f3548.Scope) stdf3548.RequestEditorFn {
	return func(ctx context.Context, req *http.Request) error {
		if c.Tokens == nil {
			return errors.New("no outgoing token client")
		}
		tok, err := c.Tokens.Token(ctx, target, string(scope))
		if err != nil {
			return fmt.Errorf("token for %s: %w", target, err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		return nil
	}
}

// answer is one call's outcome: the status and the body read.
type answer struct {
	status int
	body   []byte
}

// callKind says how a call is retried and whether it tells the DSS's
// reachability.
type callKind int

const (
	// readDSS: a DSS read, retried on no answer, a 5xx or a 429.
	readDSS callKind = iota
	// writeDSS: a DSS write, retried only on a 429 or a 503, the answers
	// that say the write was not made: a write whose answer was lost, or
	// a 500, may have been made, and is recovered by the writer reading
	// the DSS, never by writing again blind.
	writeDSS
	// readPeer: a read of a peer USS, retried like readDSS; it says
	// nothing of the DSS.
	readPeer
)

func (k callKind) retry(status int) bool {
	if k == writeDSS {
		return status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable
	}
	return status == 0 || status >= 500 || status == http.StatusTooManyRequests
}

// do runs call with retries: each attempt within callTimeout, an attempt
// the kind retries tried again after the backoff until attempts are
// spent or ctx ends.
func (c *Client) do(ctx context.Context, kind callKind, call func(ctx context.Context) (*http.Response, error)) (answer, error) {
	dss := kind != readPeer
	wait := c.backoff()
	var last error
	for i := 0; i < c.attempts(); i++ {
		if i > 0 {
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return answer{}, fmt.Errorf("%w: %w (last: %w)", ErrDSSDown, ctx.Err(), last)
			case <-t.C:
			}
			wait *= 2
		}
		actx, cancel := context.WithTimeout(ctx, c.callTimeout())
		sent := c.now()
		res, err := call(actx)
		if err != nil {
			cancel()
			last = err
			if ctx.Err() != nil || !kind.retry(0) {
				break
			}
			continue
		}
		body, rerr := readBounded(res.Body)
		cancel()
		if dss {
			c.setDrift(res, sent, c.now())
		}
		if rerr != nil {
			last = rerr
			if !kind.retry(0) {
				break
			}
			continue
		}
		if res.StatusCode >= 500 || res.StatusCode == http.StatusTooManyRequests {
			last = fmt.Errorf("answered %d %s", res.StatusCode, clip(body))
			if !kind.retry(res.StatusCode) {
				break
			}
			continue
		}
		if dss {
			c.setReach(true, "")
		}
		return answer{status: res.StatusCode, body: body}, nil
	}
	if last == nil {
		last = ctx.Err()
	}
	if dss {
		c.setReach(false, clipErr(last))
	}
	return answer{}, fmt.Errorf("%w: %w", ErrDSSDown, last)
}

// readBounded reads at most MaxAnswerBytes and closes the body.
func readBounded(r io.ReadCloser) ([]byte, error) {
	defer func() { _ = r.Close() }()
	b, err := io.ReadAll(io.LimitReader(r, MaxAnswerBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxAnswerBytes {
		return nil, core.Fieldf("answer", "larger than %d bytes", MaxAnswerBytes)
	}
	return b, nil
}

func clip(b []byte) string {
	if len(b) > 200 {
		b = b[:200]
	}
	return strings.ToValidUTF8(string(b), "?")
}

func clipErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// outcome turns an answer other than 200 into its error: ErrNotFound for
// 404, a *ConflictError for 409, a *RefusedError for any other 4xx.
func outcome(a answer) error {
	switch a.status {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrNotFound, clip(a.body))
	case http.StatusConflict:
		var r f3548.AirspaceConflictResponse
		ce := &ConflictError{Message: clip(a.body)}
		if json.Unmarshal(a.body, &r) == nil {
			if r.Message != nil {
				ce.Message = *r.Message
			}
			if r.MissingOperationalIntents != nil {
				ce.MissingOperationalIntents = *r.MissingOperationalIntents
			}
			if r.MissingConstraints != nil {
				ce.MissingConstraints = *r.MissingConstraints
			}
		}
		if len(ce.MissingOperationalIntents) > MaxReferences || len(ce.MissingConstraints) > MaxReferences {
			return &RefusedError{Status: a.status, Msg: "a conflict answer naming more than the references bound"}
		}
		return ce
	}
	return &RefusedError{Status: a.status, Msg: clip(a.body)}
}

func decode[T any](a answer, what string) (*T, error) {
	var v T
	if err := json.Unmarshal(a.body, &v); err != nil {
		return nil, &RefusedError{Status: a.status, Msg: "the answer is not a " + what}
	}
	return &v, nil
}

// QueryOperationalIntents is POST /dss/v1/operational_intent_references/query.
func (c *Client) QueryOperationalIntents(ctx context.Context, aoi f3548.Volume4D) ([]f3548.OperationalIntentReference, error) {
	dc, err := c.client()
	if err != nil {
		return nil, err
	}
	a, err := c.do(ctx, readDSS, func(ctx context.Context) (*http.Response, error) {
		return dc.QueryOperationalIntentReferences(ctx, f3548.QueryOperationalIntentReferenceParameters{AreaOfInterest: &aoi},
			c.bearer(c.DSSBaseURL, f3548.ScopeStrategicCoordination))
	})
	if err != nil {
		return nil, err
	}
	if err := outcome(a); err != nil {
		return nil, err
	}
	r, err := decode[f3548.QueryOperationalIntentReferenceResponse](a, "QueryOperationalIntentReferenceResponse")
	if err != nil {
		return nil, err
	}
	if len(r.OperationalIntentReferences) > MaxReferences {
		return nil, &RefusedError{Status: a.status, Msg: fmt.Sprintf("more than %d operational intent references", MaxReferences)}
	}
	return r.OperationalIntentReferences, nil
}

// QueryConstraints is POST /dss/v1/constraint_references/query.
func (c *Client) QueryConstraints(ctx context.Context, aoi f3548.Volume4D) ([]f3548.ConstraintReference, error) {
	dc, err := c.client()
	if err != nil {
		return nil, err
	}
	a, err := c.do(ctx, readDSS, func(ctx context.Context) (*http.Response, error) {
		return dc.QueryConstraintReferences(ctx, f3548.QueryConstraintReferenceParameters{AreaOfInterest: &aoi},
			c.bearer(c.DSSBaseURL, f3548.ScopeStrategicCoordination))
	})
	if err != nil {
		return nil, err
	}
	if err := outcome(a); err != nil {
		return nil, err
	}
	r, err := decode[f3548.QueryConstraintReferencesResponse](a, "QueryConstraintReferencesResponse")
	if err != nil {
		return nil, err
	}
	if len(r.ConstraintReferences) > MaxReferences {
		return nil, &RefusedError{Status: a.status, Msg: fmt.Sprintf("more than %d constraint references", MaxReferences)}
	}
	return r.ConstraintReferences, nil
}

// PutOperationalIntent creates the reference (ovn "") or updates it at
// ovn: PUT /dss/v1/operational_intent_references/{id}[/{ovn}]. A state
// beyond Accepted and Activated needs utm.conformance_monitoring_sa too
// (the DSS's SCD0100), which the token then carries.
func (c *Client) PutOperationalIntent(ctx context.Context, id, ovn string, p f3548.PutOperationalIntentReferenceParameters) (*f3548.ChangeOperationalIntentReferenceResponse, error) {
	dc, err := c.client()
	if err != nil {
		return nil, err
	}
	edit := c.bearer(c.DSSBaseURL, f3548.ScopeStrategicCoordination)
	if p.State == f3548.Nonconforming || p.State == f3548.Contingent {
		edit = c.bearerScopes(c.DSSBaseURL, f3548.ScopeStrategicCoordination, f3548.ScopeConformanceMonitoringForSituationalAwareness)
	}
	ctx = withEntity(ctx, id)
	a, err := c.do(ctx, writeDSS, func(ctx context.Context) (*http.Response, error) {
		if ovn == "" {
			return dc.CreateOperationalIntentReference(ctx, id, p, edit)
		}
		return dc.UpdateOperationalIntentReference(ctx, id, ovn, p, edit)
	})
	if err != nil {
		return nil, err
	}
	if a.status == http.StatusCreated {
		a.status = http.StatusOK
	}
	if err := outcome(a); err != nil {
		return nil, err
	}
	r, err := decode[f3548.ChangeOperationalIntentReferenceResponse](a, "ChangeOperationalIntentReferenceResponse")
	if err != nil {
		return nil, err
	}
	return r, checkChange(r, id)
}

// GetOperationalIntent is GET
// /dss/v1/operational_intent_references/{id}: the reference the DSS
// holds (its ovn only when we manage it).
func (c *Client) GetOperationalIntent(ctx context.Context, id string) (*f3548.OperationalIntentReference, error) {
	dc, err := c.client()
	if err != nil {
		return nil, err
	}
	ctx = withEntity(ctx, id)
	a, err := c.do(ctx, readDSS, func(ctx context.Context) (*http.Response, error) {
		return dc.GetOperationalIntentReference(ctx, id, c.bearer(c.DSSBaseURL, f3548.ScopeStrategicCoordination))
	})
	if err != nil {
		return nil, err
	}
	if err := outcome(a); err != nil {
		return nil, err
	}
	r, err := decode[f3548.GetOperationalIntentReferenceResponse](a, "GetOperationalIntentReferenceResponse")
	if err != nil {
		return nil, err
	}
	if r.OperationalIntentReference.Id != id {
		return nil, &RefusedError{Status: a.status, Msg: "the answer is about another operational intent reference"}
	}
	return &r.OperationalIntentReference, nil
}

// DeleteOperationalIntent is DELETE
// /dss/v1/operational_intent_references/{id}/{ovn}.
func (c *Client) DeleteOperationalIntent(ctx context.Context, id, ovn string) (*f3548.ChangeOperationalIntentReferenceResponse, error) {
	dc, err := c.client()
	if err != nil {
		return nil, err
	}
	ctx = withEntity(ctx, id)
	a, err := c.do(ctx, writeDSS, func(ctx context.Context) (*http.Response, error) {
		return dc.DeleteOperationalIntentReference(ctx, id, ovn, c.bearer(c.DSSBaseURL, f3548.ScopeStrategicCoordination))
	})
	if err != nil {
		return nil, err
	}
	if err := outcome(a); err != nil {
		return nil, err
	}
	r, err := decode[f3548.ChangeOperationalIntentReferenceResponse](a, "ChangeOperationalIntentReferenceResponse")
	if err != nil {
		return nil, err
	}
	return r, checkChange(r, id)
}

// checkChange refuses an answer about another reference, without an ovn,
// or listing more subscribers than MaxSubscribers or one that is not an
// absolute http(s) URL with a subscription.
func checkChange(r *f3548.ChangeOperationalIntentReferenceResponse, id string) error {
	ref := r.OperationalIntentReference
	switch {
	case ref.Id != id:
		return &RefusedError{Status: http.StatusOK, Msg: "the answer is about another operational intent reference"}
	case ref.Ovn == nil || *ref.Ovn == "" || len(*ref.Ovn) > 256:
		return &RefusedError{Status: http.StatusOK, Msg: "the answer has no usable ovn"}
	case !validState(ref.State):
		return &RefusedError{Status: http.StatusOK, Msg: "the answer's state is not an F3548 state"}
	}
	return checkSubscribers(r.Subscribers)
}

func validState(s f3548.OperationalIntentState) bool {
	for _, x := range f3548.DSSStates {
		if s == x {
			return true
		}
	}
	return false
}

func checkSubscribers(subs []f3548.SubscriberToNotify) error {
	if len(subs) > MaxSubscribers {
		return &RefusedError{Status: http.StatusOK, Msg: fmt.Sprintf("more than %d subscribers", MaxSubscribers)}
	}
	for i, s := range subs {
		if !absoluteURL(s.UssBaseUrl) || len(s.Subscriptions) == 0 {
			return &RefusedError{Status: http.StatusOK, Msg: fmt.Sprintf("subscriber %d is not an absolute http(s) url with a subscription", i)}
		}
	}
	return nil
}

func absoluteURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Host != "" && (u.Scheme == "https" || u.Scheme == "http")
}

// PutSubscription creates (version "") or updates the subscription:
// PUT /dss/v1/subscriptions/{id}[/{version}].
func (c *Client) PutSubscription(ctx context.Context, id, version string, p f3548.PutSubscriptionParameters) (*f3548.PutSubscriptionResponse, error) {
	dc, err := c.client()
	if err != nil {
		return nil, err
	}
	a, err := c.do(ctx, writeDSS, func(ctx context.Context) (*http.Response, error) {
		if version == "" {
			return dc.CreateSubscription(ctx, id, p, c.bearer(c.DSSBaseURL, f3548.ScopeStrategicCoordination))
		}
		return dc.UpdateSubscription(ctx, id, version, p, c.bearer(c.DSSBaseURL, f3548.ScopeStrategicCoordination))
	})
	if err != nil {
		return nil, err
	}
	if err := outcome(a); err != nil {
		return nil, err
	}
	r, err := decode[f3548.PutSubscriptionResponse](a, "PutSubscriptionResponse")
	if err != nil {
		return nil, err
	}
	if r.Subscription.Id != id || r.Subscription.Version == "" {
		return nil, &RefusedError{Status: a.status, Msg: "the answer is about another subscription or has no version"}
	}
	return r, nil
}

// GetSubscription is GET /dss/v1/subscriptions/{id}.
func (c *Client) GetSubscription(ctx context.Context, id string) (*f3548.Subscription, error) {
	dc, err := c.client()
	if err != nil {
		return nil, err
	}
	a, err := c.do(ctx, readDSS, func(ctx context.Context) (*http.Response, error) {
		return dc.GetSubscription(ctx, id, c.bearer(c.DSSBaseURL, f3548.ScopeStrategicCoordination))
	})
	if err != nil {
		return nil, err
	}
	if err := outcome(a); err != nil {
		return nil, err
	}
	r, err := decode[f3548.GetSubscriptionResponse](a, "GetSubscriptionResponse")
	if err != nil {
		return nil, err
	}
	return &r.Subscription, nil
}

// DeleteSubscription is DELETE /dss/v1/subscriptions/{id}/{version}.
func (c *Client) DeleteSubscription(ctx context.Context, id, version string) error {
	dc, err := c.client()
	if err != nil {
		return err
	}
	a, err := c.do(ctx, writeDSS, func(ctx context.Context) (*http.Response, error) {
		return dc.DeleteSubscription(ctx, id, version, c.bearer(c.DSSBaseURL, f3548.ScopeStrategicCoordination))
	})
	if err != nil {
		return err
	}
	return outcome(a)
}

// Availability is GET /dss/v1/uss_availability/{uss_id}.
func (c *Client) Availability(ctx context.Context, ussID string) (*f3548.UssAvailabilityStatusResponse, error) {
	dc, err := c.client()
	if err != nil {
		return nil, err
	}
	a, err := c.do(ctx, readDSS, func(ctx context.Context) (*http.Response, error) {
		return dc.GetUssAvailability(ctx, ussID, c.bearer(c.DSSBaseURL, f3548.ScopeStrategicCoordination))
	})
	if err != nil {
		return nil, err
	}
	if err := outcome(a); err != nil {
		return nil, err
	}
	r, err := decode[f3548.UssAvailabilityStatusResponse](a, "UssAvailabilityStatusResponse")
	if err != nil {
		return nil, err
	}
	switch r.Status.Availability {
	case f3548.Normal, f3548.Down, f3548.Unknown:
	default:
		return nil, &RefusedError{Status: a.status, Msg: "the availability is not an F3548 UssAvailabilityState"}
	}
	return r, nil
}

func (c *Client) peer(base string) (*stdf3548.StdClient, error) {
	if !absoluteURL(base) {
		return nil, &RefusedError{Status: 0, Msg: "the peer's uss_base_url is not an absolute http(s) URL"}
	}
	return stdf3548.NewClient(strings.TrimRight(base, "/"), stdf3548.WithHTTPClient(c.httpClient()))
}

// PeerDetails is GET {uss_base_url}/uss/v1/operational_intents/{id} at
// the managing USS: the operational intent as its manager gives it
// (trust provider), read by uspace-core's UnmarshalOperationalIntent
// (bounded, every member a judgement rests on checked) and refused when
// it is about another intent or has no ovn.
func (c *Client) PeerDetails(ctx context.Context, base, id string) (*f3548.OperationalIntent, error) {
	pc, err := c.peer(base)
	if err != nil {
		return nil, err
	}
	ctx = withEntity(ctx, id)
	a, err := c.do(ctx, readPeer, func(ctx context.Context) (*http.Response, error) {
		return pc.GetOperationalIntentDetails(ctx, id, nil, c.bearer(base, f3548.ScopeStrategicCoordination))
	})
	if err != nil {
		return nil, err
	}
	if err := outcome(a); err != nil {
		return nil, err
	}
	var env struct {
		OperationalIntent json.RawMessage `json:"operational_intent"`
	}
	if err := json.Unmarshal(a.body, &env); err != nil || len(env.OperationalIntent) == 0 {
		return nil, &RefusedError{Status: a.status, Msg: "the answer is not a GetOperationalIntentDetailsResponse"}
	}
	oi, err := f3548.UnmarshalOperationalIntent(env.OperationalIntent)
	if err != nil {
		return nil, &RefusedError{Status: a.status, Msg: "the operational intent is not usable: " + err.Error()}
	}
	if oi.Reference.Id != id || oi.Reference.Ovn == nil || *oi.Reference.Ovn == "" {
		return nil, &RefusedError{Status: a.status, Msg: "the operational intent is another one or has no ovn"}
	}
	return oi, nil
}

// PeerConstraint is GET {uss_base_url}/uss/v1/constraints/{id} at the
// constraint's manager (utm.constraint_processing).
func (c *Client) PeerConstraint(ctx context.Context, base, id string) (*f3548.Constraint, error) {
	pc, err := c.peer(base)
	if err != nil {
		return nil, err
	}
	ctx = withEntity(ctx, id)
	a, err := c.do(ctx, readPeer, func(ctx context.Context) (*http.Response, error) {
		return pc.GetConstraintDetails(ctx, id, c.bearer(base, f3548.ScopeConstraintProcessing))
	})
	if err != nil {
		return nil, err
	}
	if err := outcome(a); err != nil {
		return nil, err
	}
	r, err := decode[f3548.GetConstraintDetailsResponse](a, "GetConstraintDetailsResponse")
	if err != nil {
		return nil, err
	}
	ref := r.Constraint.Reference
	if ref.Id != id || ref.Ovn == nil || *ref.Ovn == "" || len(r.Constraint.Details.Volumes) == 0 {
		return nil, &RefusedError{Status: a.status, Msg: "the constraint is another one, has no ovn or no volume"}
	}
	return &r.Constraint, nil
}

// Notify is POST {uss_base_url}/uss/v1/operational_intents at a
// subscriber: the details of a change to one of our intents (or its
// deletion, OperationalIntent nil). One attempt: the outbox retries.
func (c *Client) Notify(ctx context.Context, base string, body f3548.PutOperationalIntentDetailsParameters) error {
	pc, err := c.peer(base)
	if err != nil {
		return err
	}
	ctx = withEntity(ctx, body.OperationalIntentId)
	actx, cancel := context.WithTimeout(ctx, c.callTimeout())
	defer cancel()
	res, err := pc.NotifyOperationalIntentDetailsChanged(actx, body, c.bearer(base, f3548.ScopeStrategicCoordination))
	if err != nil {
		return fmt.Errorf("%w: %w", ErrDSSDown, err)
	}
	b, err := readBounded(res.Body)
	if err != nil {
		return err
	}
	if res.StatusCode/100 != 2 {
		if res.StatusCode >= 500 || res.StatusCode == http.StatusTooManyRequests {
			return fmt.Errorf("%w: answered %d %s", ErrDSSDown, res.StatusCode, clip(b))
		}
		return &RefusedError{Status: res.StatusCode, Msg: clip(b)}
	}
	return nil
}

// bearerScopes adds a token for the target with several scopes.
func (c *Client) bearerScopes(target string, scopes ...f3548.Scope) stdf3548.RequestEditorFn {
	return func(ctx context.Context, req *http.Request) error {
		if c.Tokens == nil {
			return errors.New("no outgoing token client")
		}
		s := make([]string, len(scopes))
		for i, x := range scopes {
			s[i] = string(x)
		}
		tok, err := c.Tokens.Token(ctx, target, s...)
		if err != nil {
			return fmt.Errorf("token for %s: %w", target, err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		return nil
	}
}
