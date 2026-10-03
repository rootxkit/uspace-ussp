package httpx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"time"

	"github.com/rootxkit/uspace-core/core"
)

// Defaults of a Server.
const (
	DefaultMaxBodyBytes      = 1 << 20
	DefaultReadHeaderTimeout = 5 * time.Second
	DefaultReadTimeout       = 30 * time.Second
	DefaultWriteTimeout      = 30 * time.Second
	DefaultIdleTimeout       = 120 * time.Second
	DefaultMaxHeaderBytes    = 32 << 10
)

// Server is a net/http server with the baseline every listener of this
// system has: header read timeout, read, write and idle timeouts, a
// header size cap, and a Shutdown bounded by a deadline.
type Server struct {
	Name   string // "public", "admin": names the listener in logs
	Logger *slog.Logger
	srv    *http.Server
}

// ServerOptions configures NewServer; zero values take the defaults.
type ServerOptions struct {
	Name              string
	Addr              string
	Handler           http.Handler
	Logger            *slog.Logger
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	// NoWriteTimeout is for a listener that serves WebSockets, which
	// manage their own deadlines.
	NoWriteTimeout bool
	IdleTimeout    time.Duration
}

// NewServer returns a Server for opts.
func NewServer(opts ServerOptions) *Server {
	def := func(v, d time.Duration) time.Duration {
		if v <= 0 {
			return d
		}
		return v
	}
	writeTimeout := def(opts.WriteTimeout, DefaultWriteTimeout)
	if opts.NoWriteTimeout {
		writeTimeout = 0
	}
	return &Server{
		Name:   opts.Name,
		Logger: opts.Logger,
		srv: &http.Server{
			Addr:              opts.Addr,
			Handler:           opts.Handler,
			ReadHeaderTimeout: def(opts.ReadHeaderTimeout, DefaultReadHeaderTimeout),
			ReadTimeout:       def(opts.ReadTimeout, DefaultReadTimeout),
			WriteTimeout:      writeTimeout,
			IdleTimeout:       def(opts.IdleTimeout, DefaultIdleTimeout),
			MaxHeaderBytes:    DefaultMaxHeaderBytes,
			ErrorLog:          slog.NewLogLogger(opts.Logger.Handler(), slog.LevelWarn),
		},
	}
}

// HTTPServer exposes the underlying server (tests read its timeouts).
func (s *Server) HTTPServer() *http.Server { return s.srv }

// Listen opens the listener, so a bind error is reported before the
// process says it started.
func (s *Server) Listen() (net.Listener, error) {
	ln, err := net.Listen("tcp", s.srv.Addr)
	if err != nil {
		return nil, fmt.Errorf("%s listener on %s: %w", s.Name, s.srv.Addr, err)
	}
	return ln, nil
}

// Serve serves on ln until ctx is done, then shuts down: in-flight
// requests get until drainTimeout to finish; past it the remaining
// connections are closed and Serve returns an error naming the overrun.
func (s *Server) Serve(ctx context.Context, ln net.Listener, drainTimeout time.Duration) error {
	errc := make(chan error, 1)
	go func() { errc <- s.srv.Serve(ln) }()
	select {
	case err := <-errc:
		return fmt.Errorf("%s server: %w", s.Name, err)
	case <-ctx.Done():
	}
	return s.Shutdown(drainTimeout, errc)
}

// Shutdown drains within timeout; errc, if not nil, receives the Serve
// result.
func (s *Server) Shutdown(timeout time.Duration, errc <-chan error) error {
	sctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	err := s.srv.Shutdown(sctx)
	if err != nil {
		_ = s.srv.Close()
		return fmt.Errorf("%s server drain exceeded %s: %w", s.Name, timeout, err)
	}
	if errc != nil {
		if serr := <-errc; serr != nil && !errors.Is(serr, http.ErrServerClosed) {
			return fmt.Errorf("%s server: %w", s.Name, serr)
		}
	}
	return nil
}

// BaselineDeps are the shared parts Baseline needs.
type BaselineDeps struct {
	Counters     *core.Counters
	MaxBodyBytes int64 // <= 0: DefaultMaxBodyBytes
	// TrustedProxies are the proxies whose X-Forwarded-For RealIP
	// believes (USSP_TRUSTED_PROXIES); empty: the peer is the client.
	TrustedProxies []netip.Prefix
	// ProxyWatch, when set, is told of every X-Forwarded-For from a peer
	// that is not a trusted proxy (audit S7).
	ProxyWatch *ProxyWatch
}

// Baseline wraps h with the middleware every listener uses, in order:
// route tracking, request id, the client address behind the trusted
// proxies (RealIP), access log, panic recovery and the default body
// cap. Rate limits are per route, keyed on RemoteIP or on the client.
func Baseline(h http.Handler, logger *slog.Logger, deps BaselineDeps) http.Handler {
	maxBody := deps.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = DefaultMaxBodyBytes
	}
	return Chain(captureRoute(h),
		TrackRoute,
		RequestID,
		RealIP(deps.TrustedProxies, deps.ProxyWatch),
		AccessLog(logger),
		Recover(logger, deps.Counters),
		BodyCap(maxBody, deps.Counters),
	)
}

// NotFound answers every request no route matched with a problem, so
// even a 404 has the contract's shape.
func NotFound(w http.ResponseWriter, r *http.Request) {
	NewProblem(http.StatusNotFound, SlugNotFound, "", "no such endpoint").Write(w, r)
}
