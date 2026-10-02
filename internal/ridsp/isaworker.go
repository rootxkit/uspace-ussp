package ridsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3411"

	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	stdf3411 "github.com/rootxkit/uspace-ussp/internal/stdapi/f3411"
	"github.com/rootxkit/uspace-ussp/internal/store"
)

// TokenSource hands out ecosystem tokens whose aud is the host of the
// target's base URL (internal/auth.Outgoing, M18).
type TokenSource interface {
	Token(ctx context.Context, baseURL string, scopes ...string) (string, error)
}

// Bounds of the worker (E-10).
const (
	// MaxSubscribers bounds the subscribers one DSS answer may ask us
	// to notify; more is refused as a malformed answer.
	MaxSubscribers = 100
	// maxAnswerBytes bounds a DSS or subscriber answer read.
	maxAnswerBytes = f3411.MaxMessageBytes
	// DefaultCallTimeout bounds one DSS or subscriber call.
	DefaultCallTimeout = 5 * time.Second
	// DefaultMaxBackoff caps the wait before an item is retried.
	DefaultMaxBackoff = 30 * time.Second
	// DefaultNotifyBudget bounds one pass of the notification loop: it
	// takes no further notification once the budget is spent, and the
	// call under way ends with it.
	DefaultNotifyBudget = 10 * time.Second
	// MaxNotifyAttempts bounds the attempts of one notification; after
	// the last it is dropped, counted and logged.
	MaxNotifyAttempts = 3
	// notifyRetry is the wait before a notification is tried again.
	notifyRetry = time.Second
	// DefaultMaxRefusals bounds the DSS refusals in a row of the writes
	// of one ISA (a 4xx, an answer that cannot be used, a version
	// conflict): at the bound the ISA is given up, marked refused,
	// counted, logged and reported on /readyz. A DSS that does not
	// answer is not a refusal; its items are kept through any outage.
	DefaultMaxRefusals = 8
	// renewBatch bounds the session ISAs renewed per tick.
	renewBatch = 100
)

// ISAKinds are the outbox kinds the ISA worker's write loop takes.
var ISAKinds = []string{store.OutboxISAPut, store.OutboxISADelete}

// NotifyKinds are the outbox kinds its notification loop takes.
var NotifyKinds = []string{store.OutboxISANotify}

// errDSSDown marks a failure that says the DSS is unreachable (no
// answer, a 5xx or a 429), as opposed to an answer refusing the call.
var errDSSDown = errors.New("DSS unreachable")

// refusedError is a DSS answer that refused a write or could not be
// used; the refusals of one ISA in a row are bounded (MaxRefusals).
type refusedError struct{ error }

func (r refusedError) Unwrap() error { return r.error }

// refused marks err as a refusal.
func refused(err error) error { return refusedError{err} }

// DSSState is what the worker last learnt of the DSS.
type DSSState struct {
	// Known is false before the first call.
	Known bool
	Up    bool
	// Since is when the DSS entered its state.
	Since  time.Time
	Reason string
}

// ISAWorker writes the planned ISAs to the DSS and notifies the
// subscribers the DSS lists (see the package documentation). The ISA
// writes and the notifications are separate outbox kinds worked by
// separate loops, so a subscriber that does not answer never holds an
// ISA write. Safe for one Run per process; several processes may run it
// at once (the outbox leases each item to one of them).
type ISAWorker struct {
	// Store is the relational database (internal/ridsp/pgstore in api).
	Store WorkStore
	// DSSBaseURL is USSP_DSS_BASE_URL; the F3411 DSS operations are at
	// its /rid/v2. USSBaseURL is ours (uss_base_url of the ISA).
	DSSBaseURL string
	USSBaseURL string
	Tokens     TokenSource
	HTTP       *http.Client
	Policy     func() policy.Values
	Counters   *core.Counters
	Logger     *slog.Logger
	Now        func() time.Time
	// Batch (16) items per claim, a claim every Every (1 s), the session
	// renewal every RenewEvery (60 s), retries no later than MaxBackoff;
	// a notification pass spends at most NotifyBudget (10 s).
	Batch        int
	Every        time.Duration
	RenewEvery   time.Duration
	MaxBackoff   time.Duration
	NotifyBudget time.Duration
	// MaxRefusals bounds the refusals in a row of one ISA's writes
	// (DefaultMaxRefusals).
	MaxRefusals int

	once   sync.Once
	dss    *stdf3411.StdClient
	dssErr error

	mu    sync.Mutex
	state DSSState
}

func (w *ISAWorker) count(name string) {
	if w.Counters != nil {
		w.Counters.Inc(name)
	}
}

func (w *ISAWorker) logger() *slog.Logger {
	if w.Logger == nil {
		return obs.Discard()
	}
	return w.Logger
}

func (w *ISAWorker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *ISAWorker) policy() policy.Values {
	if w.Policy != nil {
		return w.Policy()
	}
	return policy.Defaults()
}

func (w *ISAWorker) httpClient() *http.Client {
	if w.HTTP != nil {
		return w.HTTP
	}
	return &http.Client{Timeout: DefaultCallTimeout}
}

// State is the DSS as the worker last saw it.
func (w *ISAWorker) State() DSSState {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.state
}

func (w *ISAWorker) set(up bool, reason string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.state.Known || w.state.Up != up {
		w.state.Since = w.now().UTC()
	}
	w.state.Known, w.state.Up, w.state.Reason = true, up, reason
}

// Probe is the readiness entry dss: up once the DSS answered, no ISA
// work waits and no ISA was given up; degraded while ISA work waits on a
// DSS that answers or while an ISA the DSS refused is given up; down
// since T while the DSS does not answer; unknown before the first call.
// The outbox depth, the oldest item's age and the ISAs given up (the
// newest named with its error) are in the detail.
func (w *ISAWorker) Probe() obs.Probe {
	return func(ctx context.Context) (obs.State, string) {
		s := w.State()
		backlog := ""
		if w.Store != nil {
			n, age, err := w.Store.Backlog(ctx)
			if err != nil {
				backlog = "; ISA outbox depth unknown"
			} else if n > 0 {
				backlog = fmt.Sprintf("; %d ISA writes waiting, the oldest %.0f s", n, age)
			}
			n, id, msg, err := w.Store.RefusedISAs(ctx)
			if err != nil {
				backlog += "; ISAs given up unknown"
			} else if n > 0 {
				backlog += fmt.Sprintf("; %d ISA refused by the DSS and given up in 24 h, the newest %s: %s", n, id, msg)
			}
		}
		switch {
		case !s.Known:
			return obs.StateUnknown, "no DSS call yet" + backlog
		case !s.Up:
			return obs.StateDown, "down since " + s.Since.Format(time.RFC3339) + ": " + s.Reason + backlog
		case s.Reason != "" || backlog != "":
			return obs.StateDegraded, strings.TrimPrefix(s.Reason+backlog, "; ")
		}
		return obs.StateUp, ""
	}
}

func (w *ISAWorker) client() (*stdf3411.StdClient, error) {
	w.once.Do(func() {
		base := strings.TrimRight(w.DSSBaseURL, "/") + "/rid/v2"
		w.dss, w.dssErr = stdf3411.NewClient(base, stdf3411.WithHTTPClient(w.httpClient()))
	})
	return w.dss, w.dssErr
}

// bearer is the request editor that adds a token for target with scope
// rid.service_provider.
func (w *ISAWorker) bearer(target string) stdf3411.RequestEditorFn {
	return func(ctx context.Context, req *http.Request) error {
		tok, err := w.Tokens.Token(ctx, target, string(f3411.ScopeServiceProvider))
		if err != nil {
			return fmt.Errorf("token for %s: %w", target, err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		return nil
	}
}

// pingID is the ISA a reachability check reads: the nil UUID's
// version 4 form, which no Service Provider writes.
const pingID = "00000000-0000-4000-8000-000000000000"

// Ping reads one ISA that does not exist: any answer tells whether the
// DSS is reachable and takes our token, so /readyz knows the DSS before
// the first flight.
func (w *ISAWorker) Ping(ctx context.Context) error {
	c, err := w.client()
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, DefaultCallTimeout)
	defer cancel()
	res, err := c.GetIdentificationServiceArea(cctx, pingID, w.bearer(w.DSSBaseURL))
	if err := w.classify(res, err); err != nil {
		return err
	}
	_, _ = readAnswer(res)
	return nil
}

// Run checks the DSS, then works through the ISA items until ctx ends;
// every RenewEvery it queues the renewal of session ISAs and checks the
// DSS again. The notifications run in their own loop beside it.
func (w *ISAWorker) Run(ctx context.Context) {
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Go(func() { w.runNotify(ctx) })
	_ = w.Ping(ctx)
	every, renew := w.every(), w.RenewEvery
	if renew <= 0 {
		renew = time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	r := time.NewTicker(renew)
	defer r.Stop()
	for {
		if _, err := w.Once(ctx); err != nil && ctx.Err() == nil {
			w.logger().LogAttrs(ctx, slog.LevelWarn, "ISA outbox not read; retried", obs.Err(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-r.C:
			if err := w.Renew(ctx); err != nil && ctx.Err() == nil {
				w.logger().LogAttrs(ctx, slog.LevelWarn, "session ISA renewal not queued; retried", obs.Err(err))
			}
			_ = w.Ping(ctx)
		case <-t.C:
		}
	}
}

func (w *ISAWorker) every() time.Duration {
	if w.Every <= 0 {
		return time.Second
	}
	return w.Every
}

// runNotify posts the queued notifications every Every until ctx ends.
func (w *ISAWorker) runNotify(ctx context.Context) {
	t := time.NewTicker(w.every())
	defer t.Stop()
	for {
		if _, err := w.NotifyOnce(ctx); err != nil && ctx.Err() == nil {
			w.logger().LogAttrs(ctx, slog.LevelWarn, "ISA notification outbox not read; retried", obs.Err(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Once claims and handles one batch of ISA writes; it returns how many
// items it took.
func (w *ISAWorker) Once(ctx context.Context) (int, error) {
	n := w.Batch
	if n <= 0 {
		n = 16
	}
	items, err := w.Store.Claim(ctx, ISAKinds, n)
	if err != nil {
		return 0, err
	}
	ob := w.Store
	for i := range items {
		it := &items[i]
		err := w.handle(ctx, *it)
		if err == nil {
			if derr := ob.Done(ctx, it.ID); derr != nil {
				w.logger().LogAttrs(ctx, slog.LevelWarn, "ISA item done but not marked; it will be taken again", obs.Err(derr))
			}
			continue
		}
		w.count(CounterISAFailed)
		backoff := w.backoff(it.Attempts)
		var r *retryNowError
		if errors.As(err, &r) {
			backoff = 0
		}
		if w.givenUp(ctx, *it, err) {
			continue
		}
		w.logger().LogAttrs(ctx, slog.LevelWarn, "ISA write failed; retried from the outbox",
			slog.String("isa_id", it.EntityID), slog.String("kind", it.Kind), slog.Int("attempts", int(it.Attempts)),
			slog.Duration("retry_in", backoff), obs.Err(err))
		if !isRefusal(err) {
			_ = w.Store.Failed(ctx, it.EntityID, clipErr(err))
		}
		if ferr := ob.Fail(ctx, it.ID, err, backoff); ferr != nil {
			w.logger().LogAttrs(ctx, slog.LevelWarn, "ISA item failure not recorded; it is taken again after its lease", obs.Err(ferr))
		}
	}
	return len(items), nil
}

func isRefusal(err error) bool {
	var r refusedError
	var now *retryNowError
	return errors.As(err, &r) || errors.As(err, &now)
}

// givenUp counts a refusal of the item's ISA; at MaxRefusals in a row it
// gives the ISA up: the item is done, the refusal counted and logged,
// and the ISA stays marked refused (on /readyz) until it is written or
// deleted. Anything but a refusal is retried without bound.
func (w *ISAWorker) givenUp(ctx context.Context, it store.OutboxItem, err error) bool {
	if !isRefusal(err) {
		return false
	}
	limit := w.MaxRefusals
	if limit <= 0 {
		limit = DefaultMaxRefusals
	}
	gave, rerr := w.Store.Refused(ctx, it.EntityID, clipErr(err), limit)
	if rerr != nil {
		w.logger().LogAttrs(ctx, slog.LevelWarn, "ISA refusal not recorded; the item is retried", slog.String("isa_id", it.EntityID), obs.Err(rerr))
		return false
	}
	if !gave {
		return false
	}
	w.count(CounterISAGivenUp)
	w.logger().LogAttrs(ctx, slog.LevelError, "the DSS kept refusing the ISA write; given up",
		slog.String("isa_id", it.EntityID), slog.String("kind", it.Kind), slog.Int("max_refusals", limit), obs.Err(err))
	if derr := w.Store.Done(ctx, it.ID); derr != nil {
		w.logger().LogAttrs(ctx, slog.LevelWarn, "ISA item given up but not marked; it will be taken again", obs.Err(derr))
	}
	return true
}

// backoff is 1 s doubled per earlier attempt, at most MaxBackoff.
func (w *ISAWorker) backoff(attempts int32) time.Duration {
	limit := w.MaxBackoff
	if limit <= 0 {
		limit = DefaultMaxBackoff
	}
	d := time.Second
	for i := int32(1); i < attempts && d < limit; i++ {
		d *= 2
	}
	return min(d, limit)
}

// retryNowError is a failure after which the item is due again at once: the
// worker learnt the version it needed.
type retryNowError struct{ cause string }

func (r *retryNowError) Error() string { return r.cause }

// handle handles one item holding its ISA's lock, so a put and a delete
// of one ISA taken by two workers (two api replicas) run one after the
// other: each reads the ISA as the other left it.
func (w *ISAWorker) handle(ctx context.Context, it store.OutboxItem) error {
	return w.Store.Lock(ctx, it.EntityID, func() error { return w.handleLocked(ctx, it) })
}

func (w *ISAWorker) handleLocked(ctx context.Context, it store.OutboxItem) error {
	switch it.Kind {
	case store.OutboxISAPut:
		var p ISAPut
		if err := json.Unmarshal(it.Payload, &p); err != nil || p.ISAID != it.EntityID {
			return w.unreadable(ctx, it)
		}
		return w.put(ctx, p)
	case store.OutboxISADelete:
		var d ISADelete
		if err := json.Unmarshal(it.Payload, &d); err != nil || d.ISAID != it.EntityID {
			return w.unreadable(ctx, it)
		}
		return w.delete(ctx, d)
	}
	return w.unreadable(ctx, it)
}

// unreadable logs an item that can never be handled; it is marked done
// so it does not block the others, and the log says so (never silent).
func (w *ISAWorker) unreadable(ctx context.Context, it store.OutboxItem) error {
	w.count(CounterISAFailed)
	w.logger().LogAttrs(ctx, slog.LevelError, "ISA outbox item is not readable; it is dropped",
		slog.Int64("outbox_id", it.ID), slog.String("kind", it.Kind), slog.String("isa_id", it.EntityID))
	return nil
}

// readAnswer reads at most maxAnswerBytes of res and closes it.
func readAnswer(res *http.Response) ([]byte, error) {
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(res.Body, maxAnswerBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxAnswerBytes {
		return nil, core.Fieldf("answer", "larger than %d bytes", maxAnswerBytes)
	}
	return b, nil
}

// classify turns a DSS call's outcome into an error: nil for 200, a
// down error (and the DSS marked down) without an answer or on a 5xx or
// 429, an answer error otherwise (the DSS is up).
func (w *ISAWorker) classify(res *http.Response, err error) error {
	if err != nil {
		w.set(false, "no answer: "+clipErr(err))
		return fmt.Errorf("%w: %w", errDSSDown, err)
	}
	if res.StatusCode >= 500 || res.StatusCode == http.StatusTooManyRequests {
		_, _ = readAnswer(res)
		w.set(false, fmt.Sprintf("answers %d", res.StatusCode))
		return fmt.Errorf("%w: status %d", errDSSDown, res.StatusCode)
	}
	if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
		w.set(true, fmt.Sprintf("answers %d to our token", res.StatusCode))
		return nil
	}
	w.set(true, "")
	return nil
}

func clipErr(err error) string {
	s := err.Error()
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// put creates or updates the ISA: nothing for an ISA of an ended flight
// or one already deleted (the delete follows or followed), nothing for
// a window that has passed; otherwise PUT with the window from now (the
// database clock), the version recorded, the subscribers notified.
func (w *ISAWorker) put(ctx context.Context, p ISAPut) error {
	row, found, err := w.Store.ISA(ctx, p.ISAID)
	if err != nil || !found {
		return err
	}
	if row.DeletedAt != nil || row.FlightEndedAt != nil {
		w.count(CounterISASkippedEnded)
		return nil
	}
	now, err := w.Store.Now(ctx)
	if err != nil {
		return err
	}
	start, end := planned(p, now)
	if !end.After(start.Add(time.Second)) {
		w.count(CounterISAExpired)
		w.logger().LogAttrs(ctx, slog.LevelWarn, "ISA window passed before it was written", slog.String("isa_id", p.ISAID),
			slog.String("flight_id", p.FlightID))
		return nil
	}
	ext := f3411.Volume4D{Volume: p.Volume, TimeStart: &f3411.Time{Format: f3411.RFC3339, Value: start.UTC()},
		TimeEnd: &f3411.Time{Format: f3411.RFC3339, Value: end.UTC()}}
	c, err := w.client()
	if err != nil {
		return err
	}
	var res *http.Response
	if row.Version == nil {
		res, err = c.CreateIdentificationServiceArea(ctx, p.ISAID,
			f3411.CreateIdentificationServiceAreaParameters{Extents: ext, UssBaseUrl: w.USSBaseURL}, w.bearer(w.DSSBaseURL))
	} else {
		res, err = c.UpdateIdentificationServiceArea(ctx, p.ISAID, *row.Version,
			f3411.UpdateIdentificationServiceAreaParameters{Extents: ext, UssBaseUrl: w.USSBaseURL}, w.bearer(w.DSSBaseURL))
	}
	if err := w.classify(res, err); err != nil {
		return err
	}
	body, err := readAnswer(res)
	if err != nil {
		return err
	}
	if res.StatusCode == http.StatusConflict {
		return w.refreshVersion(ctx, p.ISAID)
	}
	if res.StatusCode != http.StatusOK {
		return refused(fmt.Errorf("DSS refused the ISA: %d %s", res.StatusCode, clip(body)))
	}
	var ans f3411.PutIdentificationServiceAreaResponse
	if err := json.Unmarshal(body, &ans); err != nil {
		return refused(core.Fieldf("answer", "not a PutIdentificationServiceAreaResponse"))
	}
	if err := checkArea(ans.ServiceArea, p.ISAID); err != nil {
		return refused(err)
	}
	raw, err := json.Marshal(ext)
	if err != nil {
		return err
	}
	// The DSS holds the ISA at this version whatever it says of the
	// subscribers: record it first, or the next attempt creates it again,
	// meets 409 and loops.
	subs, serr := checkSubscribers(ans.Subscribers)
	v := ans.ServiceArea.Version
	sa := ans.ServiceArea
	notes := notifications(p.ISAID, subs, &sa, &ext)
	if err := w.Store.Written(ctx, ISARecord{ISAID: p.ISAID, FlightID: p.FlightID, Version: &v, TimeStart: start, TimeEnd: end, Extents: raw}, notes); err != nil {
		return err
	}
	w.count(CounterISAWrites)
	w.logger().LogAttrs(ctx, slog.LevelInfo, "ISA written in the DSS", slog.String("isa_id", p.ISAID),
		slog.String("flight_id", p.FlightID), slog.Int("subscribers", len(subs)))
	w.subscribersRefused(ctx, p.ISAID, serr)
	return nil
}

// subscribersRefused counts and logs a subscriber list of a DSS answer
// that was refused (checkSubscribers): the ISA write is recorded, and no
// subscriber is notified of it.
func (w *ISAWorker) subscribersRefused(ctx context.Context, isaID string, err error) {
	if err == nil {
		return
	}
	w.count(CounterSubscribersRefused)
	w.logger().LogAttrs(ctx, slog.LevelWarn, "the DSS listed subscribers that cannot be notified; the ISA write is recorded and none is notified",
		slog.String("isa_id", isaID), obs.Err(err))
}

// refreshVersion reads the ISA the DSS holds after a 409 and records its
// version, so the next attempt updates it: our own ISA whose create
// answer was lost, or one whose version moved. Every outcome but an
// unreachable DSS or a database error is a refusal, so a DSS that
// answers 409 again and again is given up at MaxRefusals; an ISA the
// DSS does not hold under our base URL is a refusal too.
func (w *ISAWorker) refreshVersion(ctx context.Context, isaID string) error {
	c, err := w.client()
	if err != nil {
		return err
	}
	res, err := c.GetIdentificationServiceArea(ctx, isaID, w.bearer(w.DSSBaseURL))
	if err := w.classify(res, err); err != nil {
		return err
	}
	body, err := readAnswer(res)
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		return refused(fmt.Errorf("DSS answered 409, then %d to the read of the ISA", res.StatusCode))
	}
	var ans f3411.GetIdentificationServiceAreaResponse
	if err := json.Unmarshal(body, &ans); err != nil {
		return refused(core.Fieldf("answer", "not a GetIdentificationServiceAreaResponse"))
	}
	if err := checkArea(ans.ServiceArea, isaID); err != nil {
		return refused(err)
	}
	if strings.TrimRight(ans.ServiceArea.UssBaseUrl, "/") != strings.TrimRight(w.USSBaseURL, "/") {
		return refused(fmt.Errorf("the DSS holds ISA %s for another Service Provider", isaID))
	}
	v := ans.ServiceArea.Version
	if err := w.Store.Refreshed(ctx, isaID, v); err != nil {
		return err
	}
	return &retryNowError{cause: "the DSS answered 409; its version " + v + " is recorded and the write is retried"}
}

// delete removes the ISA from the DSS with its version: nothing to do
// for one never written or already deleted; a 404 means the DSS no
// longer holds it.
func (w *ISAWorker) delete(ctx context.Context, d ISADelete) error {
	row, found, err := w.Store.ISA(ctx, d.ISAID)
	if err != nil || !found || row.DeletedAt != nil {
		return err
	}
	if row.Version == nil {
		return w.Store.Deleted(ctx, d.ISAID, nil)
	}
	c, err := w.client()
	if err != nil {
		return err
	}
	res, err := c.DeleteIdentificationServiceArea(ctx, d.ISAID, *row.Version, w.bearer(w.DSSBaseURL))
	if err := w.classify(res, err); err != nil {
		return err
	}
	body, err := readAnswer(res)
	if err != nil {
		return err
	}
	switch res.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		w.logger().LogAttrs(ctx, slog.LevelWarn, "the DSS no longer holds the ISA; recorded as deleted", slog.String("isa_id", d.ISAID))
		return w.Store.Deleted(ctx, d.ISAID, nil)
	case http.StatusConflict:
		return w.refreshVersion(ctx, d.ISAID)
	default:
		return refused(fmt.Errorf("DSS refused the ISA delete: %d %s", res.StatusCode, clip(body)))
	}
	var ans f3411.DeleteIdentificationServiceAreaResponse
	if err := json.Unmarshal(body, &ans); err != nil {
		return refused(core.Fieldf("answer", "not a DeleteIdentificationServiceAreaResponse"))
	}
	// The DSS no longer holds the ISA whatever it says of the
	// subscribers: record that first.
	subs, serr := checkSubscribers(ans.Subscribers)
	// A deletion is notified without service_area and extents (the file:
	// "If this field is not populated, the ISA was deleted").
	if err := w.Store.Deleted(ctx, d.ISAID, notifications(d.ISAID, subs, nil, nil)); err != nil {
		return err
	}
	w.count(CounterISADeletes)
	w.logger().LogAttrs(ctx, slog.LevelInfo, "ISA deleted from the DSS", slog.String("isa_id", d.ISAID),
		slog.String("flight_id", d.FlightID), slog.Int("subscribers", len(subs)))
	w.subscribersRefused(ctx, d.ISAID, serr)
	return nil
}

func clip(b []byte) string {
	if len(b) > 200 {
		b = b[:200]
	}
	return strings.ToValidUTF8(string(b), "?")
}

// checkArea refuses an answer that is not about the ISA asked or has no
// version.
func checkArea(a f3411.IdentificationServiceArea, isaID string) error {
	switch {
	case a.Id != isaID:
		return core.Fieldf("answer.service_area.id", "is not the ISA written")
	case a.Version == "" || len(a.Version) > 256:
		return core.Fieldf("answer.service_area.version", "missing or longer than 256 bytes")
	}
	return nil
}

// checkSubscribers refuses more than MaxSubscribers and a subscriber
// whose url is not an absolute http(s) URL or that names no
// subscription.
func checkSubscribers(subs *[]f3411.SubscriberToNotify) ([]f3411.SubscriberToNotify, error) {
	if subs == nil {
		return nil, nil
	}
	if len(*subs) > MaxSubscribers {
		return nil, core.Fieldf("answer.subscribers", "more than %d", MaxSubscribers)
	}
	for i, s := range *subs {
		u, err := url.Parse(s.Url)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || len(s.Subscriptions) == 0 {
			return nil, core.Fieldf(fmt.Sprintf("answer.subscribers[%d]", i), "not an absolute http(s) url with a subscription")
		}
	}
	return *subs, nil
}

// ISANotify is the payload of a dss_outbox isa_notify item: the
// notification of one ISA write to one subscriber the DSS listed.
type ISANotify struct {
	ISAID string                                                   `json:"isa_id"`
	URL   string                                                   `json:"url"`
	Body  f3411.PutIdentificationServiceAreaNotificationParameters `json:"body"`
}

// Key is the item's idempotency key (entity id and version): the ISA
// and the subscriber's first subscription, at its notification index,
// which the DSS raises with every change it notifies.
func (n ISANotify) Key() (string, int64) {
	id, idx := n.ISAID, int64(0)
	if len(n.Body.Subscriptions) > 0 {
		s := n.Body.Subscriptions[0]
		id += "/" + s.SubscriptionId
		if s.NotificationIndex != nil {
			idx = int64(*s.NotificationIndex)
		}
	}
	return id, idx
}

// notifications are the notifications of an ISA write, one a subscriber.
func notifications(isaID string, subs []f3411.SubscriberToNotify, sa *f3411.IdentificationServiceArea, ext *f3411.Volume4D) []ISANotify {
	out := make([]ISANotify, 0, len(subs))
	for _, s := range subs {
		out = append(out, ISANotify{ISAID: isaID, URL: s.Url,
			Body: f3411.PutIdentificationServiceAreaNotificationParameters{Subscriptions: s.Subscriptions, ServiceArea: sa, Extents: ext}})
	}
	return out
}

func (w *ISAWorker) notifyBudget() time.Duration {
	if w.NotifyBudget <= 0 {
		return DefaultNotifyBudget
	}
	return w.NotifyBudget
}

// NotifyOnce posts queued notifications, one claimed at a time, until
// none is due or NotifyBudget is spent; it returns how many it took.
// The call under way ends with the budget at the latest. A notification
// a subscriber does not take is tried again after notifyRetry, and after
// MaxNotifyAttempts it is dropped, counted and logged; the others are
// posted regardless.
func (w *ISAWorker) NotifyOnce(ctx context.Context) (int, error) {
	bctx, cancel := context.WithTimeout(ctx, w.notifyBudget())
	defer cancel()
	taken := 0
	for bctx.Err() == nil {
		items, err := w.Store.Claim(ctx, NotifyKinds, 1)
		if err != nil {
			return taken, err
		}
		if len(items) == 0 {
			return taken, nil
		}
		taken++
		w.notifyItem(ctx, bctx, items[0])
	}
	return taken, nil
}

// notifyItem posts one notification within bctx and records the outcome
// with ctx.
func (w *ISAWorker) notifyItem(ctx, bctx context.Context, it store.OutboxItem) {
	var n ISANotify
	err := json.Unmarshal(it.Payload, &n)
	if err == nil {
		err = w.notifyOne(bctx, n.URL, n.ISAID, n.Body)
	} else {
		it.Attempts = MaxNotifyAttempts // unreadable: never tried again
	}
	if err == nil {
		w.count(CounterSubscriberNotified)
		if derr := w.Store.Done(ctx, it.ID); derr != nil {
			w.logger().LogAttrs(ctx, slog.LevelWarn, "ISA notification sent but not marked; it may be sent again", obs.Err(derr))
		}
		return
	}
	if it.Attempts < MaxNotifyAttempts {
		if ferr := w.Store.Fail(ctx, it.ID, err, notifyRetry); ferr != nil {
			w.logger().LogAttrs(ctx, slog.LevelWarn, "ISA notification failure not recorded; it is taken again after its lease", obs.Err(ferr))
		}
		return
	}
	w.count(CounterSubscriberNotifyErr)
	w.logger().LogAttrs(ctx, slog.LevelWarn, "ISA notification not taken by a subscriber; dropped",
		slog.String("isa_id", n.ISAID), slog.String("subscriber", n.URL), slog.Int("attempts", int(it.Attempts)), obs.Err(err))
	if derr := w.Store.Done(ctx, it.ID); derr != nil {
		w.logger().LogAttrs(ctx, slog.LevelWarn, "ISA notification dropped but not marked; it is taken again", obs.Err(derr))
	}
}

func (w *ISAWorker) notifyOne(ctx context.Context, base, isaID string, body f3411.PutIdentificationServiceAreaNotificationParameters) error {
	c, err := stdf3411.NewClient(strings.TrimRight(base, "/"), stdf3411.WithHTTPClient(w.httpClient()))
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, DefaultCallTimeout)
	defer cancel()
	res, err := c.PostIdentificationServiceArea(cctx, isaID, body, w.bearer(base))
	if err != nil {
		return err
	}
	b, _ := readAnswer(res)
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("answered %d %s", res.StatusCode, clip(b))
	}
	return nil
}

// Renew queues an update of every session ISA whose flight goes on and
// whose time_end is within half the horizon, at most renewBatch a tick.
func (w *ISAWorker) Renew(ctx context.Context) error {
	pol := w.policy()
	n, err := w.Store.Renew(ctx, pol.SessionISAHorizonS/2, renewBatch, func(r ISARecord) (ISAPut, error) {
		var ext f3411.Volume4D
		if err := json.Unmarshal(r.Extents, &ext); err != nil {
			return ISAPut{}, fmt.Errorf("ISA %s extents: %w", r.ISAID, err)
		}
		return ISAPut{ISAID: r.ISAID, FlightID: r.FlightID, Volume: ext.Volume, HorizonS: pol.SessionISAHorizonS}, nil
	})
	if w.Counters != nil && n > 0 {
		w.Counters.Add(CounterISARenewed, uint64(n))
	}
	return err
}
