package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/telemetry/gen"
)

// Health serves the two health operations (the process supplies them).
type Health interface {
	GetHealthz(w http.ResponseWriter, r *http.Request)
	GetReadyz(w http.ResponseWriter, r *http.Request)
}

// The access of the two operations: an operator machine token of this
// USSP granting ussp.telemetry (06 §3). No session realm streams
// telemetry; the WebSocket authenticates its own upgrade (M22).
var (
	StreamAccess = httpx.Access{WebSocket: true, Scopes: []string{auth.ScopeTelemetry}}
	BatchAccess  = httpx.Access{Scopes: []string{auth.ScopeTelemetry}}
)

// AccessTable is the access entry of every operation telemetry-ingest
// serves, by ServeMux pattern (the GuardedMux fails closed).
func AccessTable() map[string]httpx.Access {
	public := httpx.Access{Public: true}
	return map[string]httpx.Access{
		"GET /healthz":             public,
		"GET /readyz":              public,
		"GET /v1/telemetry":        StreamAccess,
		"POST /v1/telemetry/batch": BatchAccess,
	}
}

// Counters of the handlers.
const (
	CounterConnectRefusedDisabled = "ws_refused_source_disabled"
	CounterClosedDisabled         = "ws_closed_source_disabled"
	CounterSessions               = "ws_sessions"
	CounterStatusFrames           = "ws_status_frames"
	CounterBatchRefusedDisabled   = "batch_refused_source_disabled"
	CounterBatchNotAcknowledged   = "batch_not_acknowledged"
)

// OutcomeNotAcknowledged is a batch sample whose hand-over did not
// complete before the answer: it is sent again (a duplicate then).
const OutcomeNotAcknowledged = "not_acknowledged"

// Server serves WS /v1/telemetry and POST /v1/telemetry/batch
// (gen.ServerInterface).
type Server struct {
	Health
	Ingest *Ingestor
	WS     *auth.WSAuth
	// Sources gates connections (B-10); nil admits everything.
	Sources SourceGate
	Policy  func() policy.Record
	// Degraded lists what is degraded now, for the status frames.
	Degraded func() []string
	// Ctx ends every open socket when the process stops (the server's
	// shutdown does not reach hijacked connections).
	Ctx context.Context
	// StatusEvery is the status frame period (2 s, M29).
	StatusEvery time.Duration
	// HandOver bounds how long a batch answer waits for its samples to
	// reach the bus (10 s).
	HandOver time.Duration
	// RetryAfter is what a refused connection is told (5 s).
	RetryAfter time.Duration
	Counters   *core.Counters
	Logger     *slog.Logger
	Now        func() time.Time
}

var _ gen.ServerInterface = (*Server)(nil)

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) policy() policy.Record {
	if s.Policy != nil {
		return s.Policy()
	}
	return policy.Record{Values: policy.Defaults()}
}

func (s *Server) count(name string) {
	if s.Counters != nil {
		s.Counters.Inc(name)
	}
}

func (s *Server) retryAfter() time.Duration {
	if s.RetryAfter > 0 {
		return s.RetryAfter
	}
	return 5 * time.Second
}

// disabled is the source-control decision for client; "" when enabled.
func (s *Server) disabled(clientID string) string {
	if s.Sources == nil {
		return ""
	}
	if dec := s.Sources.Query(SourceOperatorWS, &clientID); !dec.Enabled {
		return disabledBy(dec)
	}
	return ""
}

// refuseDisabled answers 503 with Retry-After (B-10): a client retries
// it, where a 401 or 403 is final to it.
func (s *Server) refuseDisabled(w http.ResponseWriter, r *http.Request, why string) {
	httpx.RetryAfter(w, s.retryAfter())
	httpx.NewProblem(http.StatusServiceUnavailable, "source_disabled", "Source disabled",
		"the operator_ws source of this client is "+why+"; retry later").Write(w, r)
}

// Register registers every operation on mux behind guard (the batch;
// the WebSocket authenticates its own upgrade); the error lists every
// route without a valid access entry and every entry without a route.
func Register(mux *http.ServeMux, s *Server, guard httpx.Guard) error {
	g := httpx.NewGuardedMux(mux, AccessTable(), guard, auth.ValidateAccess)
	gen.HandlerWithOptions(s, gen.StdHTTPServerOptions{BaseRouter: g})
	return g.Err()
}

// OpenTelemetryStream implements gen.ServerInterface: the WebSocket.
func (s *Server) OpenTelemetryStream(w http.ResponseWriter, r *http.Request) {
	ws := *s.WS
	ws.Admit = func(w http.ResponseWriter, r *http.Request, p auth.Principal) bool {
		if why := s.disabled(p.Claims.Subject); why != "" {
			s.count(CounterConnectRefusedDisabled)
			s.refuseDisabled(w, r, why)
			return false
		}
		return true
	}
	conn, p, err := ws.AcceptWS(w, r, StreamAccess)
	if err != nil {
		return
	}
	s.count(CounterSessions)
	conn.SetReadLimit(MaxMessageBytes)
	clientID := p.Claims.Subject
	sess := s.Ingest.NewSession(clientID, s.now())
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	if s.Ctx != nil {
		stop := context.AfterFunc(s.Ctx, cancel)
		defer stop()
	}
	defer s.Ingest.Release(sess)
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Go(func() { s.writeStatus(ctx, cancel, conn, sess) })
	s.read(ctx, conn, sess)
	cancel()
}

// read takes every message until the socket ends.
func (s *Server) read(ctx context.Context, conn *websocket.Conn, sess *Session) {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		rx := s.now()
		f, err := DecodeMessage(data)
		if err != nil {
			r := Result{Index: 0, Reason: RefusedInvalid, Detail: clipErr(err)}
			s.Ingest.record(Delivery{ClientID: sess.ClientID, Session: sess, RxTS: rx}, []Result{r})
			continue
		}
		s.Ingest.Take(ctx, Delivery{ClientID: sess.ClientID, Session: sess, RxTS: rx, Frames: []Frame{f}})
	}
}

// writeStatus sends the status frame on connect, every period and after
// a refusal, and closes the socket with 1013 when the client's source is
// switched off. It is the only writer of the socket, and status frames
// are all it ever writes.
func (s *Server) writeStatus(ctx context.Context, cancel context.CancelFunc, conn *websocket.Conn, sess *Session) {
	every := s.StatusEvery
	if every <= 0 {
		every = 2 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if why := s.disabled(sess.ClientID); why != "" {
			s.count(CounterClosedDisabled)
			_ = s.sendStatus(ctx, conn, sess)
			_ = conn.Close(websocket.StatusTryAgainLater, "operator_ws source "+why)
			cancel()
			return
		}
		if err := s.sendStatus(ctx, conn, sess); err != nil {
			cancel()
			return
		}
		select {
		case <-ctx.Done():
			_ = conn.Close(websocket.StatusNormalClosure, "")
			return
		case <-t.C:
		case <-sess.Poke():
		}
	}
}

// ConsoleStatusBody is console/status/v1 (M29) with the extras of this
// socket.
type ConsoleStatusBody struct {
	ConnectionID  string             `json:"connection_id"`
	ServerTS      bus.Stamp          `json:"server_ts"`
	PolicyVersion string             `json:"policy_version"`
	StaleAfterS   float64            `json:"stale_after_s"`
	LiveMaxAgeS   float64            `json:"live_max_age_s"`
	DroppedFrames uint64             `json:"dropped_frames"`
	Degraded      []string           `json:"degraded"`
	Sources       []SourceStatusBody `json:"sources"`
	Accepted      uint64             `json:"accepted"`
	Refused       uint64             `json:"refused"`
	Dropped       uint64             `json:"dropped"`
	Backlog       uint64             `json:"backlog"`
	Rate          float64            `json:"rate"`
	Outcomes      map[string]uint64  `json:"outcomes"`
	AckedSeq      map[string]int64   `json:"acked_seq"`
}

// ConsoleStatus is one console/status/v1 frame.
type ConsoleStatus struct {
	bus.Envelope
	Body ConsoleStatusBody `json:"body"`
}

// StatusFrame is the status frame of sess at now.
func (s *Server) StatusFrame(sess *Session, src SourceStatusBody) *ConsoleStatus {
	now := s.now()
	pol := s.policy()
	st := sess.Status(now)
	degraded := []string{}
	if s.Degraded != nil {
		degraded = append(degraded, s.Degraded()...)
	}
	slices.Sort(degraded)
	degraded = slices.Compact(degraded)
	body := ConsoleStatusBody{
		ConnectionID: sess.connectionID(), ServerTS: bus.Stamp{Time: now.UTC()},
		PolicyVersion: strconv.FormatInt(pol.Version, 10),
		StaleAfterS:   pol.Values.TelemetryLostS, LiveMaxAgeS: pol.Values.BacklogAfterS,
		DroppedFrames: st.Dropped, Degraded: degraded, Sources: []SourceStatusBody{src},
		Accepted: st.Accepted, Refused: st.Refused, Dropped: st.Dropped, Backlog: st.Backlog, Rate: st.Rate,
		Outcomes: st.Outcomes, AckedSeq: st.AckedSeq,
	}
	return &ConsoleStatus{Envelope: bus.SystemEnvelope(SchemaConsoleStatus, Producer, now), Body: body}
}

func (s *Server) sendStatus(ctx context.Context, conn *websocket.Conn, sess *Session) error {
	st := &Status{Ingest: s.Ingest, Sources: s.Sources, Policy: s.Policy, Now: s.Now}
	data, err := json.Marshal(s.StatusFrame(sess, st.Body(sess.ClientID)))
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := conn.Write(wctx, websocket.MessageText, data); err != nil {
		return err
	}
	s.count(CounterStatusFrames)
	return nil
}

// TelemetryOutcome is one sample of a batch that was not accepted.
type TelemetryOutcome struct {
	Index   int    `json:"index"`
	Serial  string `json:"serial,omitempty"`
	Seq     *int64 `json:"seq,omitempty"`
	Outcome string `json:"outcome"`
	Detail  string `json:"detail,omitempty"`
}

// BatchResult is the 202 answer of POST /v1/telemetry/batch.
type BatchResult struct {
	Accepted        int                `json:"accepted"`
	Refused         int                `json:"refused"`
	Dropped         int                `json:"dropped"`
	Duplicate       int                `json:"duplicate"`
	NotAcknowledged int                `json:"not_acknowledged"`
	Outcomes        []TelemetryOutcome `json:"outcomes"`
}

// PostTelemetryBatch implements gen.ServerInterface.
func (s *Server) PostTelemetryBatch(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.NewProblem(http.StatusUnauthorized, httpx.SlugUnauthenticated, "", "no caller").Write(w, r)
		return
	}
	clientID := p.Claims.Subject
	if why := s.disabled(clientID); why != "" {
		// B-10: neither stored nor acknowledged.
		s.count(CounterBatchRefusedDisabled)
		s.refuseDisabled(w, r, why)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, MaxBatchBytes+1))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if len(raw) > MaxBatchBytes {
		httpx.WriteError(w, r, &http.MaxBytesError{Limit: MaxBatchBytes})
		return
	}
	rx := s.now()
	req, err := DecodeBatch(raw)
	if err != nil {
		httpx.NewProblem(http.StatusBadRequest, httpx.SlugValidation, "Invalid telemetry batch", "", fieldErrors(err)...).Write(w, r)
		return
	}
	res := BatchResult{Outcomes: []TelemetryOutcome{}}
	for i := 0; i < len(req.Invalid)+len(req.Frames); i++ {
		if err, bad := req.Invalid[i]; bad {
			res.Refused++
			res.Outcomes = append(res.Outcomes, TelemetryOutcome{Index: i, Outcome: RefusedInvalid, Detail: clipErr(err)})
			s.Ingest.count(RefusedInvalid)
			s.Ingest.stats.get(clientID).add(RefusedInvalid, rx)
		}
	}
	frames, index, spanRefused := s.withinSpan(req)
	for _, o := range spanRefused {
		res.Refused++
		res.Outcomes = append(res.Outcomes, o)
		s.Ingest.count(RefusedBatchSpan)
		s.Ingest.stats.get(clientID).add(RefusedBatchSpan, rx)
	}
	var mu sync.Mutex
	handed := map[int]bool{}
	notify := make(chan struct{}, 1)
	results := s.Ingest.Take(r.Context(), Delivery{ClientID: clientID, RxTS: rx, SentAt: req.SentAt, BatchRule: req.SentAt == nil,
		Frames: frames, Index: index,
		Handed: func(i int, _ *Frame, ok bool) {
			mu.Lock()
			handed[i] = ok
			mu.Unlock()
			select {
			case notify <- struct{}{}:
			default:
			}
		}})
	wait := s.HandOver
	if wait <= 0 {
		wait = 10 * time.Second
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for !allHanded(&mu, handed, results) {
		select {
		case <-notify:
			continue
		case <-timer.C:
		case <-r.Context().Done():
		}
		break
	}
	mu.Lock()
	defer mu.Unlock()
	for _, x := range results {
		seq := x.Seq
		o := TelemetryOutcome{Index: x.Index, Serial: x.Serial, Seq: &seq, Outcome: x.Reason, Detail: x.Detail}
		switch {
		case x.Reason == OutcomeAccepted:
			if ok, landed := handed[x.Index]; landed && ok {
				res.Accepted++
				continue
			}
			o.Outcome = OutcomeNotAcknowledged
			res.NotAcknowledged++
			s.count(CounterBatchNotAcknowledged)
		case x.Reason == OutcomeDuplicate:
			res.Duplicate++
		case x.Reason == OutcomeDuplicatePending || x.Reason == DroppedQueueFull:
			res.NotAcknowledged++
		case isDropped(x.Reason):
			res.Dropped++
		default:
			res.Refused++
		}
		res.Outcomes = append(res.Outcomes, o)
	}
	slices.SortFunc(res.Outcomes, func(a, b TelemetryOutcome) int { return a.Index - b.Index })
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(res)
}

// allHanded reports whether every accepted sample of results was
// settled.
func allHanded(mu *sync.Mutex, handed map[int]bool, results []Result) bool {
	mu.Lock()
	defer mu.Unlock()
	for _, x := range results {
		if x.Reason == OutcomeAccepted {
			if _, ok := handed[x.Index]; !ok {
				return false
			}
		}
	}
	return true
}

// withinSpan keeps the samples of each serial whose own times span at
// most the policy's telemetry_batch_span_s from the serial's newest
// (02 F5: a batch holds at most 1 s of samples); the older ones are
// refused_batch_span.
func (s *Server) withinSpan(req BatchRequest) ([]Frame, []int, []TelemetryOutcome) {
	span := seconds(s.policy().Values.TelemetryBatchSpanS)
	newest := map[string]time.Time{}
	for i := range req.Frames {
		k := keyOf("", req.Frames[i].Serial).fold
		if t := req.Frames[i].TS; t.After(newest[k]) {
			newest[k] = t
		}
	}
	var frames []Frame
	var index []int
	var refused []TelemetryOutcome
	for i := range req.Frames {
		f := req.Frames[i]
		if newest[keyOf("", f.Serial).fold].Sub(f.TS) > span {
			seq := f.Seq
			refused = append(refused, TelemetryOutcome{Index: req.Index[i], Serial: f.Serial, Seq: &seq, Outcome: RefusedBatchSpan,
				Detail: "more than telemetry_batch_span_s older than the newest sample of its serial in this batch"})
			continue
		}
		frames = append(frames, f)
		index = append(index, req.Index[i])
	}
	return frames, index, refused
}

// fieldErrors are the field errors joined in err.
func fieldErrors(err error) []*core.FieldError {
	var out []*core.FieldError
	var j interface{ Unwrap() []error }
	if errors.As(err, &j) {
		for _, e := range j.Unwrap() {
			out = append(out, fieldErrors(e)...)
		}
		return out
	}
	var fe *core.FieldError
	if errors.As(err, &fe) {
		return []*core.FieldError{fe}
	}
	return []*core.FieldError{{Field: "body", Reason: clipErr(err)}}
}
