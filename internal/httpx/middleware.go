package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/rootxkit/uspace-core/core"
)

// Counter names of this package.
const (
	CounterHandlerPanics = "http_handler_panics"
	CounterBodyTooLarge  = "http_body_too_large"
)

// RequestIDHeader carries the request id in and out.
const RequestIDHeader = "X-Request-ID"

type ctxKey int

const (
	requestIDKey ctxKey = iota
	bodyLimitKey
)

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// RequestID accepts a well-formed incoming X-Request-ID (Caddy sets
// one) or makes a new one, puts it in the context and echoes it.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if !requestIDPattern.MatchString(id) {
			id = newRequestID()
		}
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

func newRequestID() string {
	var b [12]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error (Go 1.24+)
	return hex.EncodeToString(b[:])
}

// RequestIDFrom returns the request id of ctx, or "".
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// AccessLog writes one structured line per request after it completes.
// The route is the ServeMux pattern, never the raw path with ids in it.
func AccessLog(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rw := &recorder{ResponseWriter: w, code: http.StatusOK}
			next.ServeHTTP(rw, r)
			route := Route(r)
			logger.LogAttrs(r.Context(), slog.LevelInfo, "http request",
				slog.String("request_id", RequestIDFrom(r.Context())),
				slog.String("method", r.Method),
				slog.String("route", route),
				slog.Int("status", rw.code),
				slog.Int64("bytes_out", rw.n),
				slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
				slog.String("remote", r.RemoteAddr),
			)
		})
	}
}

type recorder struct {
	http.ResponseWriter
	code    int
	n       int64
	written bool
}

// WriteHeader records the first status code.
func (w *recorder) WriteHeader(code int) {
	if !w.written {
		w.code = code
		w.written = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *recorder) Write(b []byte) (int, error) {
	w.written = true
	n, err := w.ResponseWriter.Write(b)
	w.n += int64(n)
	return n, err
}

func (w *recorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Recover turns a handler panic into a 500 problem and a counted,
// logged event instead of a dropped connection.
func Recover(logger *slog.Logger, counters *core.Counters) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if v := recover(); v != nil {
					if v == http.ErrAbortHandler { //nolint:errorlint // re-raised sentinel, compared by identity as net/http does
						panic(v) //nolint:forbidigo // net/http's own abort protocol must reach the server
					}
					counters.Inc(CounterHandlerPanics)
					logger.LogAttrs(r.Context(), slog.LevelError, "handler panic",
						slog.String("request_id", RequestIDFrom(r.Context())),
						slog.String("route", Route(r)),
						slog.Any("panic", v))
					NewProblem(http.StatusInternalServerError, SlugInternal, "", "").Write(w, r)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// bodyLimit is a request body whose cap a route may change before the
// first read.
type bodyLimit struct {
	rc       io.ReadCloser
	limit    int64
	read     int64
	exceeded bool
	counters *core.Counters
}

func (b *bodyLimit) Read(p []byte) (int, error) {
	if b.exceeded {
		return 0, &http.MaxBytesError{Limit: b.limit}
	}
	// Read at most one byte past the cap, which is how an oversized
	// body is told apart from one of exactly the cap.
	if remaining := b.limit + 1 - b.read; int64(len(p)) > remaining {
		p = p[:int(remaining)]
	}
	n, err := b.rc.Read(p)
	b.read += int64(n)
	if b.read > b.limit {
		b.exceeded = true
		b.counters.Inc(CounterBodyTooLarge)
		return n - int(b.read-b.limit), &http.MaxBytesError{Limit: b.limit}
	}
	return n, err
}

// Close closes the underlying body.
func (b *bodyLimit) Close() error { return b.rc.Close() }

// BodyCap caps every request body at defaultBytes: a handler reading
// past it gets an *http.MaxBytesError, which WriteError turns into a
// 413 problem. A route changes its own cap with BodyLimit.
func BodyCap(defaultBytes int64, counters *core.Counters) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			bl := &bodyLimit{rc: r.Body, limit: defaultBytes, counters: counters}
			r.Body = bl
			ctx := context.WithValue(r.Context(), bodyLimitKey, bl)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// BodyLimit sets the body cap of one route (wrap the route's handler)
// and refuses a declared Content-Length above it before the handler
// runs. Inside BodyCap it replaces the default cap; outside it, it
// applies its own.
func BodyLimit(maxBytes int64, counters *core.Counters, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if bl, ok := r.Context().Value(bodyLimitKey).(*bodyLimit); ok && bl.read == 0 {
			bl.limit = maxBytes
		} else {
			r.Body = &bodyLimit{rc: r.Body, limit: maxBytes, counters: counters}
		}
		if r.ContentLength > maxBytes {
			counters.Inc(CounterBodyTooLarge)
			WriteError(w, r, &http.MaxBytesError{Limit: maxBytes})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Chain applies middlewares so the first listed is the outermost.
func Chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}
