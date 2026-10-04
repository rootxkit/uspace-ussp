package dss

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// The exchange log (GET /uss/v1/log_sets): every DSS and peer request this
// USSP makes and every F3548 request it serves, with the answer, bodies
// clipped. Recording never holds a call up: exchanges go through a bounded
// queue to one writer, and one that does not fit is dropped and counted
// (E-10). No header is recorded: a bearer token never reaches the log.

// Bounds of the exchange log.
const (
	// MaxExchangeBodyBytes clips a recorded request or answer body.
	MaxExchangeBodyBytes = 16 << 10
	// ExchangeQueue bounds the exchanges waiting for the writer.
	ExchangeQueue = 1024
	// MaxExchangeRows bounds the rows the purge keeps.
	MaxExchangeRows = 200_000
	// MaxLogSetMessages bounds the messages of one USSLogSet.
	MaxLogSetMessages = 1000
)

// Exchange recorder roles (F3548 ExchangeRecordRecorderRole).
const (
	RoleClient = "Client"
	RoleServer = "Server"
)

// Exchange is one recorded request and its answer.
type Exchange struct {
	EntityID     string
	Role         string
	Method       string
	URL          string
	RequestBody  string
	RequestTime  time.Time
	ResponseCode int
	ResponseBody string
	ResponseTime time.Time
	Problem      string
}

// ExchangeStore is where the log's writer puts exchanges.
type ExchangeStore interface {
	InsertExchange(ctx context.Context, e Exchange) error
}

// clockStore is a store with the database clock (Store.Now).
type clockStore interface {
	Now(ctx context.Context) (time.Time, error)
}

// ClockSyncEvery is how often the log reads the database clock again.
const ClockSyncEvery = time.Minute

// ExchangeLog queues exchanges for one writer. The zero value is not
// usable; use NewExchangeLog. A nil *ExchangeLog records nothing.
//
// The times of an exchange are the database clock, as every other time
// the purge and the log sets compare them with: this host's clock plus
// the offset to the database's, read by Sync (at Run's start and every
// ClockSyncEvery), so stamping a request costs no query.
type ExchangeLog struct {
	ch       chan Exchange
	store    ExchangeStore
	counters *core.Counters
	logger   *slog.Logger
	offset   atomic.Int64
}

// Sync reads the database clock (when the store has one) and keeps its
// offset from this host's.
func (l *ExchangeLog) Sync(ctx context.Context) error {
	c, ok := l.store.(clockStore)
	if !ok {
		return nil
	}
	before := time.Now()
	db, err := c.Now(ctx)
	if err != nil {
		return err
	}
	after := time.Now()
	mid := before.Add(after.Sub(before) / 2)
	l.offset.Store(int64(db.Sub(mid)))
	return nil
}

// Now is the database clock as last synced (this host's before the
// first Sync).
func (l *ExchangeLog) Now() time.Time {
	if l == nil {
		return time.Now().UTC()
	}
	return time.Now().Add(time.Duration(l.offset.Load())).UTC()
}

func (l *ExchangeLog) sync(ctx context.Context) {
	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := l.Sync(sctx); err != nil {
		l.logger.LogAttrs(ctx, slog.LevelWarn, "database clock not read; exchanges keep the last offset", obs.Err(err))
	}
}

// Counter names of the exchange log.
const (
	CounterExchangeRecorded = "dss_exchange_recorded"
	CounterExchangeDropped  = "dss_exchange_dropped"
	CounterExchangeFailed   = "dss_exchange_write_failed"
)

// NewExchangeLog is a log writing to s.
func NewExchangeLog(s ExchangeStore, counters *core.Counters, logger *slog.Logger) *ExchangeLog {
	if logger == nil {
		logger = obs.Discard()
	}
	return &ExchangeLog{ch: make(chan Exchange, ExchangeQueue), store: s, counters: counters, logger: logger}
}

func (l *ExchangeLog) count(name string) {
	if l.counters != nil {
		l.counters.Inc(name)
	}
}

// Record queues e; a full queue drops it and counts the drop.
func (l *ExchangeLog) Record(e Exchange) {
	if l == nil {
		return
	}
	select {
	case l.ch <- e:
	default:
		l.count(CounterExchangeDropped)
	}
}

// Run writes queued exchanges until ctx ends, then the ones still queued
// within a short grace; it syncs the database clock at its start and
// every ClockSyncEvery.
func (l *ExchangeLog) Run(ctx context.Context) {
	l.sync(ctx)
	t := time.NewTicker(ClockSyncEvery)
	defer t.Stop()
	for {
		select {
		case e := <-l.ch:
			l.write(context.WithoutCancel(ctx), e)
		case <-t.C:
			l.sync(ctx)
		case <-ctx.Done():
			l.drain(ctx)
			return
		}
	}
}

// drain writes what is still queued at the end, within a short grace.
func (l *ExchangeLog) drain(ctx context.Context) {
	grace, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	for grace.Err() == nil {
		select {
		case e := <-l.ch:
			l.write(grace, e)
		default:
			return
		}
	}
}

func (l *ExchangeLog) write(ctx context.Context, e Exchange) {
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := l.store.InsertExchange(wctx, e); err != nil {
		l.count(CounterExchangeFailed)
		l.logger.LogAttrs(ctx, slog.LevelWarn, "DSS exchange not recorded", obs.Err(err))
		return
	}
	l.count(CounterExchangeRecorded)
}

type entityKey struct{}

// withEntity names the entity a call is about, for the exchange log.
func withEntity(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, entityKey{}, id)
}

// uuidRE finds an entity id in a path when the caller named none.
var uuidRE = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

func entityOf(ctx context.Context, path string) string {
	if id, ok := ctx.Value(entityKey{}).(string); ok {
		return id
	}
	return uuidRE.FindString(strings.ToLower(path))
}

func clipBody(b []byte) string {
	if len(b) > MaxExchangeBodyBytes {
		b = b[:MaxExchangeBodyBytes]
	}
	return strings.ToValidUTF8(string(b), "?")
}

// Transport records every request made through it and its answer in the
// log; Base makes the requests (nil is http.DefaultTransport).
type Transport struct {
	Base http.RoundTripper
	Log  *ExchangeLog
	Now  func() time.Time
}

func (t *Transport) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	if t.Log != nil {
		return t.Log.Now()
	}
	return time.Now()
}

// RoundTrip implements http.RoundTripper. The answer's body is read here
// (at most MaxAnswerBytes + 1, which the client refuses anyway) so that it
// is recorded, and handed on unchanged.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	e := Exchange{Role: RoleClient, Method: req.Method, URL: req.URL.Redacted(), RequestTime: t.now().UTC(),
		EntityID: entityOf(req.Context(), req.URL.Path)}
	if req.GetBody != nil {
		if rc, err := req.GetBody(); err == nil {
			b, _ := io.ReadAll(io.LimitReader(rc, MaxExchangeBodyBytes))
			_ = rc.Close()
			e.RequestBody = clipBody(b)
		}
	}
	res, err := base.RoundTrip(req)
	e.ResponseTime = t.now().UTC()
	if err != nil {
		e.Problem = clipErr(err)
		t.Log.Record(e)
		return nil, err
	}
	b, rerr := io.ReadAll(io.LimitReader(res.Body, MaxAnswerBytes+1))
	_ = res.Body.Close()
	res.Body = io.NopCloser(bytes.NewReader(b))
	e.ResponseCode, e.ResponseBody = res.StatusCode, clipBody(b)
	if rerr != nil {
		e.Problem = clipErr(rerr)
	}
	t.Log.Record(e)
	return res, nil
}

// Middleware records every request next serves and its answer, as the
// server of the exchange (the F3548 USS endpoints). The request body is
// read once, clipped for the log, and handed on whole.
func (l *ExchangeLog) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if l == nil {
			next.ServeHTTP(w, r)
			return
		}
		e := Exchange{Role: RoleServer, Method: r.Method, URL: r.URL.RequestURI(), RequestTime: l.Now(),
			EntityID: entityOf(r.Context(), r.URL.Path)}
		if r.Body != nil && r.Body != http.NoBody {
			r.Body = &teeBody{rc: r.Body, max: MaxExchangeBodyBytes}
		}
		rec := &recordingWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if tb, ok := r.Body.(*teeBody); ok {
			e.RequestBody = clipBody(tb.buf.Bytes())
			if e.EntityID == "" {
				e.EntityID = uuidRE.FindString(strings.ToLower(e.RequestBody))
			}
		}
		e.ResponseTime, e.ResponseCode, e.ResponseBody = l.Now(), rec.status, clipBody(rec.buf.Bytes())
		l.Record(e)
	})
}

// Behind is guard with the log inside it: a request the guard refuses
// (no token, a bad one, a scope missing) is answered by the guard and
// never recorded, so no caller without a valid token can fill the
// exchange log's queue or its table; every request it admits is
// recorded with its answer (Middleware).
func (l *ExchangeLog) Behind(guard httpx.Guard) httpx.Guard {
	return func(a httpx.Access) func(http.Handler) http.Handler {
		inner := guard(a)
		return func(next http.Handler) http.Handler { return inner(l.Middleware(next)) }
	}
}

// teeBody keeps the first max bytes read from rc.
type teeBody struct {
	rc  io.ReadCloser
	buf bytes.Buffer
	max int
}

func (t *teeBody) Read(p []byte) (int, error) {
	n, err := t.rc.Read(p)
	if room := t.max - t.buf.Len(); room > 0 && n > 0 {
		t.buf.Write(p[:min(n, room)])
	}
	return n, err
}

// Close implements io.Closer.
func (t *teeBody) Close() error { return t.rc.Close() }

// recordingWriter keeps the status and the first bytes of the answer.
type recordingWriter struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
}

// WriteHeader implements http.ResponseWriter, keeping the status.
func (w *recordingWriter) WriteHeader(s int) {
	w.status = s
	w.ResponseWriter.WriteHeader(s)
}

// Write implements http.ResponseWriter, keeping the first bytes.
func (w *recordingWriter) Write(p []byte) (int, error) {
	if room := MaxExchangeBodyBytes - w.buf.Len(); room > 0 {
		w.buf.Write(p[:min(len(p), room)])
	}
	return w.ResponseWriter.Write(p)
}
