package testif

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/httpx"
	inj "github.com/rootxkit/uspace-ussp/internal/stdapi/interuss/ridinjection"
)

// Counters of the RID injection (E-09).
const (
	CounterInjectionAccepted   = "testif_injection_accepted"
	CounterInjectionRefused    = "testif_injection_refused"
	CounterInjectionFailed     = "testif_injection_failed"
	CounterInjectionConflict   = "testif_injection_conflict"
	CounterInjectionFull       = "testif_injection_full"
	CounterInjectionDeleted    = "testif_injection_deleted"
	CounterFramesSent          = "testif_frames_sent"
	CounterFramesBacklog       = "testif_frames_backlog"
	CounterFramesRefused       = "testif_frames_refused"
	CounterFramesSendFailed    = "testif_frames_send_failed"
	CounterEndNotAcknowledged  = "testif_end_not_acknowledged"
	CounterBindingsMade        = "testif_bindings_made"
	CounterUserNotifications   = "testif_user_notifications_answered"
	CounterInjectionBadRequest = "testif_injection_bad_request"
	CounterInjectionFailedLive = "testif_injection_failed_in_flight"
	CounterFramesNotAcked      = "testif_frames_not_acknowledged"
)

// maxTestIDBytes bounds a test id.
const maxTestIDBytes = 255

// Clock is the time the injection sends by: Now, and Until, which
// returns when the wall clock reaches t or ctx ends.
type Clock interface {
	Now() time.Time
	Until(ctx context.Context, t time.Time) error
}

type wallClock struct{}

// Now is the wall clock.
func (wallClock) Now() time.Time { return time.Now() }

// Until waits for the wall clock to reach t.
func (wallClock) Until(ctx context.Context, t time.Time) error {
	d := time.Until(t)
	if d <= 0 {
		return ctx.Err()
	}
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-tm.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// injector is the RID injection interface: the tests it holds, each
// flight a socket of the test operator's client streaming its frames.
type injector struct {
	s     *Server
	clock Clock

	mu    sync.Mutex
	tests map[string]*injTest
	// held is the serialKey of every serial a test flies, from its PUT
	// until its streams have ended after its delete: each test its own.
	held map[string]bool
	ctx  context.Context // the server's lifetime: a test's streams outlive its PUT
	work sync.WaitGroup
	seq  uint64
}

type injTest struct {
	id, version string
	flights     []inj.TestFlight
	runs        []*flightRun
	cancel      context.CancelFunc
	reserved    bool     // a PUT is still building it
	serials     []string // serialKey of every serial it flies (in held)
	failure     error    // the ingest's refusal of a frame in flight (under injector.mu)
}

// flightRun is one injected flight on its socket.
type flightRun struct {
	p    planned
	t    *injTest
	sess *session
	done chan struct{} // closed when the stream goroutine returns

	mu     sync.Mutex
	sent   int  // frames sent
	ok     int  // frames sent before the first the ingest refused
	ended  bool // its end frame was sent
	closed bool
}

func newInjector(s *Server) *injector {
	return &injector{s: s, clock: wallClock{}, tests: map[string]*injTest{}, held: map[string]bool{}, ctx: context.Background()}
}

func (in *injector) iface() iface {
	return iface{name: "rid_injection", access: InjectionAccess(), register: func(g *httpx.GuardedMux) {
		g.HandleFunc("PUT "+BaseInjection+"/tests/{test_id}", in.createTest)
		g.HandleFunc("DELETE "+BaseInjection+"/tests/{test_id}/{version}", in.deleteTest)
		g.HandleFunc("GET "+BaseInjection+"/user_notifications", in.userNotifications)
	}}
}

func (in *injector) count(name string) { in.s.Counters.Inc(name) }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// refuse answers the injection's failure with the USSP's reason: 400 for
// a refusal of the request or on the real path, 503 when the USSP could
// not be reached or answered 5xx.
func (in *injector) refuse(w http.ResponseWriter, r *http.Request, err error) {
	var ref *RefusalError
	var fe *core.FieldError
	switch {
	case errors.As(err, &fe):
		in.count(CounterInjectionBadRequest)
		httpx.NewProblem(http.StatusBadRequest, httpx.SlugValidation, "", "the injection is not one the file allows", fe).Write(w, r)
	case errors.As(err, &ref) && ref.Status < 500:
		in.count(CounterInjectionRefused)
		httpx.NewProblem(http.StatusBadRequest, "ussp_refused", "", "the USSP refused the injection on its real path: "+ref.Error()).Write(w, r)
	default:
		in.count(CounterInjectionFailed)
		httpx.RetryAfter(w, retryAfter)
		httpx.NewProblem(http.StatusServiceUnavailable, "ussp_unavailable", "", clip(err.Error(), 512)).Write(w, r)
	}
}

// createTest is PUT /tests/{test_id}: validate, plan every frame, bind the
// serials, open one socket per flight, send each flight's first frame at
// its time and answer once the ingest accepted every first frame due
// within the acknowledgement bound (brief WP-21, RID injection).
func (in *injector) createTest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("test_id")
	if id == "" || len(id) > maxTestIDBytes {
		in.refuse(w, r, core.Fieldf("test_id", "1 to %d bytes", maxTestIDBytes))
		return
	}
	var params inj.CreateTestParameters
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			httpx.WriteError(w, r, err)
			return
		}
		in.refuse(w, r, core.Fieldf("body", "not a CreateTestParameters object"))
		return
	}
	cfg := in.s.cfg
	in.mu.Lock()
	if _, ok := in.tests[id]; ok {
		in.mu.Unlock()
		in.count(CounterInjectionConflict)
		httpx.NewProblem(http.StatusConflict, "test_exists", "", "a test with this id exists").Write(w, r)
		return
	}
	if len(in.tests) >= cfg.MaxTests {
		in.mu.Unlock()
		in.count(CounterInjectionFull)
		httpx.RetryAfter(w, retryAfter)
		httpx.NewProblem(http.StatusTooManyRequests, "too_many_tests", "", fmt.Sprintf("%d tests are held; delete one first", cfg.MaxTests)).Write(w, r)
		return
	}
	in.seq++
	version := strconv.FormatInt(in.clock.Now().UnixMilli(), 36) + strconv.FormatUint(in.seq, 36)
	// The serials are chosen under the lock against every held test's,
	// so two tests never fly one serial (each its own of the pool).
	plans, err := planFlights(params, version, in.s.fixture.Serials, in.held, cfg.MaxFlightsPerTest, cfg.MaxStatesFlight)
	if err != nil {
		in.mu.Unlock()
		in.refuse(w, r, err)
		return
	}
	t := &injTest{id: id, version: version, flights: params.RequestedFlights, reserved: true}
	for _, p := range plans {
		k := serialKey(p.serial)
		t.serials = append(t.serials, k)
		in.held[k] = true
	}
	in.tests[id] = t
	in.mu.Unlock()
	release := func() {
		in.mu.Lock()
		delete(in.tests, id)
		in.mu.Unlock()
		in.unhold(t)
	}

	if err := in.start(r.Context(), t, plans); err != nil {
		release()
		in.refuse(w, r, err)
		return
	}
	in.mu.Lock()
	t.reserved = false
	failure := t.failure
	in.mu.Unlock()
	if failure != nil {
		// Refused in flight after start had read the failures: the PUT
		// still fails with it.
		in.stop(context.WithoutCancel(r.Context()), t)
		release()
		in.refuse(w, r, failure)
		return
	}
	in.count(CounterInjectionAccepted)
	in.s.Logger.LogAttrs(r.Context(), slog.LevelInfo, "rid injection test created", slog.String("test_id", id),
		slog.String("version", version), slog.Int("flights", len(plans)))
	writeJSON(w, http.StatusOK, inj.ChangeTestResponse{InjectedFlights: params.RequestedFlights, Version: version})
}

// start binds the serials, opens the sockets and starts the streams; it
// returns once every first frame due within the acknowledgement bound
// has been decided, and on any refusal ends what it started.
func (in *injector) start(ctx context.Context, t *injTest, plans []planned) error {
	token, err := in.s.USSP.OperatorToken(ctx)
	if err != nil {
		return fmt.Errorf("the test operator's token: %w", err)
	}
	if err := in.s.bind(ctx, plans); err != nil {
		return err
	}
	in.mu.Lock()
	parent := in.ctx
	in.mu.Unlock()
	streamCtx, cancel := context.WithCancel(parent)
	t.cancel = cancel
	ack := time.Duration(in.s.cfg.AckTimeoutS) * time.Second
	type first struct {
		i   int
		err error
	}
	results := make(chan first, len(plans))
	for i, p := range plans {
		sess, err := dial(ctx, in.s.USSP, token)
		if err != nil {
			cancel()
			in.stop(context.WithoutCancel(ctx), t)
			return err
		}
		run := &flightRun{p: p, t: t, sess: sess, done: make(chan struct{})}
		t.runs = append(t.runs, run)
		in.work.Add(1)
		go func() {
			defer in.work.Done()
			defer close(run.done)
			in.stream(streamCtx, run, ack, func(err error) { results <- first{i, err} })
		}()
	}
	// Wait for every first frame due within the bound: a flight starting
	// later is accepted as scheduled, its refusals counted and logged.
	due := in.clock.Now().Add(ack)
	var errs []error
	for _, p := range plans {
		if p.frames[0].Ts.After(due) {
			continue
		}
		select {
		case r := <-results:
			if r.err != nil {
				errs = append(errs, fmt.Errorf("flight %s (%s): %w", plans[r.i].injectionID, plans[r.i].serial, r.err))
			}
		case <-ctx.Done():
			errs = append(errs, ctx.Err())
		}
		if len(errs) > 0 {
			break
		}
	}
	in.mu.Lock()
	failure := t.failure
	in.mu.Unlock()
	if failure != nil {
		// A flight's later frame was refused while the PUT waited for the
		// others' first: the injection fails with that refusal.
		errs = append([]error{failure}, errs...)
	}
	if len(errs) > 0 {
		cancel()
		in.stop(context.WithoutCancel(ctx), t)
		var ref *RefusalError
		if errors.As(errs[0], &ref) {
			return &RefusalError{Status: ref.Status, Reason: ref.Reason, Detail: errs[0].Error()}
		}
		return errors.Join(errs...)
	}
	return nil
}

// stream sends a flight's frames at their times: a frame whose time has
// passed goes at once with backlog true. The first frame's decision is
// reported through first; the frame after the last one is the end frame.
// Every later frame's decision is read before the next frame goes: a
// frame the ingest refuses fails the test (fail), and nothing is sent
// past the refusal but the flight's end frame.
func (in *injector) stream(ctx context.Context, run *flightRun, ack time.Duration, first func(error)) {
	reported := false
	report := func(err error) {
		if !reported {
			reported = true
			first(err)
		}
	}
	defer report(context.Canceled)
	var base ingestStatus
	for j := range run.p.frames {
		f := run.p.frames[j]
		if err := in.clock.Until(ctx, f.Ts); err != nil {
			return
		}
		if f.Ts.Before(in.clock.Now()) {
			b := true
			f.Backlog = &b
			in.count(CounterFramesBacklog)
		}
		if j == 0 {
			actx, cancel := context.WithTimeout(ctx, ack)
			err := run.sess.sendAndWait(actx, f)
			cancel()
			report(err)
			if err != nil {
				return
			}
			// A refusal counted from here on is one in flight.
			base, _, _ = run.sess.status()
		} else {
			if _, err := run.sess.send(ctx, f); err != nil {
				in.count(CounterFramesSendFailed)
				return
			}
			run.mu.Lock()
			run.sent = j + 1
			run.mu.Unlock()
			actx, cancel := context.WithTimeout(ctx, ack)
			err := run.sess.settle(actx, base, f.Serial)
			cancel()
			var ref *RefusalError
			switch {
			case errors.As(err, &ref):
				in.fail(ctx, run, ref)
				return
			case errors.Is(err, errNotDecided):
				// Not decided within the bound: the next frame's (or the
				// end frame's) wait reads its decision with its own.
				in.count(CounterFramesNotAcked)
			case err != nil:
				in.count(CounterFramesSendFailed)
				return
			}
		}
		in.count(CounterFramesSent)
		run.mu.Lock()
		run.sent, run.ok = j+1, j+1
		run.mu.Unlock()
	}
	in.end(ctx, run, ack)
}

// fail fails run's test on the ingest's refusal of a frame in flight:
// the refusal is counted and logged at error with its reason, kept as the
// test's failure, and every flight of the test is ended (an end frame
// each, then the close), as a delete would; the test stays held until
// its delete. While its PUT is still building it, the PUT fails with the
// refusal and ends the flights itself.
func (in *injector) fail(ctx context.Context, run *flightRun, ref *RefusalError) {
	t := run.t
	in.count(CounterFramesRefused)
	in.mu.Lock()
	first := t.failure == nil
	if first {
		t.failure = ref
	}
	building := t.reserved
	in.mu.Unlock()
	if !first {
		return
	}
	in.count(CounterInjectionFailedLive)
	in.s.Logger.LogAttrs(ctx, slog.LevelError, "rid injection test failed: a frame was refused in flight",
		slog.String("test_id", t.id), slog.String("version", t.version), slog.String("serial", run.p.serial),
		slog.String("injection_id", run.p.injectionID), slog.String("reason", ref.Reason))
	if building {
		t.cancel() // the PUT reads the failure and stops the test
		return
	}
	// stop waits for every stream of the test, this one included, so it
	// runs beside it; a later delete's stop finds the flights ended.
	in.work.Add(1)
	go func() {
		defer in.work.Done()
		in.stop(context.WithoutCancel(ctx), t)
	}()
}

// end sends a flight's end frame once: its last accepted sample at the
// current time (after the last state), end true, the next seq; and waits
// for the ingest to decide on it.
func (in *injector) end(ctx context.Context, run *flightRun, ack time.Duration) {
	run.mu.Lock()
	if run.ended || run.ok == 0 {
		run.mu.Unlock()
		return
	}
	run.ended = true
	// At the last accepted sample, numbered after the last sent one (a
	// refused frame's position is not repeated).
	f := run.p.frames[run.ok-1]
	f.Seq = run.p.frames[run.sent-1].Seq
	run.mu.Unlock()
	t := in.clock.Now().UTC()
	if !t.After(f.Ts) {
		t = f.Ts.Add(time.Millisecond)
	}
	yes := true
	f.Ts, f.End, f.Seq, f.Backlog = t, &yes, f.Seq+1, nil
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ack)
	defer cancel()
	if err := run.sess.sendAndWait(actx, f); err != nil {
		var ref *RefusalError
		if errors.As(err, &ref) {
			in.count(CounterFramesRefused)
		} else {
			in.count(CounterEndNotAcknowledged)
		}
		in.s.Logger.LogAttrs(ctx, slog.LevelWarn, "end frame not accepted", slog.String("serial", f.Serial), slog.String("error", err.Error()))
	}
}

// stop ends every stream of t: the stream goroutines first, then an end
// frame on each socket that sent a frame and has not ended, then the
// close.
func (in *injector) stop(ctx context.Context, t *injTest) {
	if t.cancel != nil {
		t.cancel()
	}
	ack := time.Duration(in.s.cfg.AckTimeoutS) * time.Second
	for _, run := range t.runs {
		<-run.done
		in.end(ctx, run, ack)
		run.mu.Lock()
		closed := run.closed
		run.closed = true
		run.mu.Unlock()
		if !closed {
			run.sess.close()
		}
	}
}

// deleteTest is DELETE /tests/{test_id}/{version}: end every flight's
// stream (an end frame each, acknowledged by the ingest), then forget the
// test. The ISA's deletion is the ISA worker's, not done here.
func (in *injector) deleteTest(w http.ResponseWriter, r *http.Request) {
	id, version := r.PathValue("test_id"), r.PathValue("version")
	in.mu.Lock()
	t, ok := in.tests[id]
	if !ok || t.reserved || t.version != version {
		in.mu.Unlock()
		httpx.NewProblem(http.StatusNotFound, httpx.SlugNotFound, "", "no test with this id and version").Write(w, r)
		return
	}
	delete(in.tests, id)
	in.mu.Unlock()
	in.stop(r.Context(), t)
	in.unhold(t)
	in.count(CounterInjectionDeleted)
	writeJSON(w, http.StatusOK, inj.DeleteTestResponse{InjectedFlights: t.flights})
}

// userNotifications is GET /user_notifications: the empty list the file
// allows; this USSP shows the virtual user no notification of its own.
func (in *injector) userNotifications(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	after, err := time.Parse(time.RFC3339Nano, q.Get("after"))
	if err != nil {
		httpx.NewProblem(http.StatusBadRequest, httpx.SlugValidation, "", "after is required (RFC 3339)", core.Fieldf("after", "missing or not RFC 3339")).Write(w, r)
		return
	}
	if b := q.Get("before"); b != "" {
		before, err := time.Parse(time.RFC3339Nano, b)
		if err != nil || !before.After(after) {
			httpx.NewProblem(http.StatusBadRequest, httpx.SlugValidation, "", "before must be RFC 3339 and after after", core.Fieldf("before", "malformed or not after after")).Write(w, r)
			return
		}
	}
	in.count(CounterUserNotifications)
	writeJSON(w, http.StatusOK, inj.QueryUserNotificationsResponse{UserNotifications: []inj.UserNotification{}})
}

// run holds the streams to the server's lifetime: when ctx ends every
// test is stopped (end frames sent) before Run returns.
func (in *injector) run(ctx context.Context) {
	in.mu.Lock()
	in.ctx = ctx
	in.mu.Unlock()
	<-ctx.Done()
	in.mu.Lock()
	tests := make([]*injTest, 0, len(in.tests))
	for id, t := range in.tests {
		if !t.reserved {
			tests = append(tests, t)
			delete(in.tests, id)
		}
	}
	in.mu.Unlock()
	for _, t := range tests {
		in.stop(context.WithoutCancel(ctx), t)
		in.unhold(t)
	}
	in.work.Wait()
}

// unhold frees t's serials once its streams have ended.
func (in *injector) unhold(t *injTest) {
	in.mu.Lock()
	defer in.mu.Unlock()
	for _, k := range t.serials {
		delete(in.held, k)
	}
}
