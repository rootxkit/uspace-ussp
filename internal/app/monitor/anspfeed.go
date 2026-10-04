package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// DepANSPFeed is the readiness dependency of the ANSP's manned-traffic
// stream (USSP_ANSP_STREAM_URL, spec 02 F4).
const DepANSPFeed = "ansp_feed"

// ScopeANSPTraffic is the scope of a token for the ANSP's stream (M23).
const ScopeANSPTraffic = "ansp.traffic"

// Bounds and periods of the stream reader.
const (
	// ANSPFrameBytes bounds one frame read from the ANSP (E-10).
	ANSPFrameBytes = 64 << 10
	// ANSPSilence is how long the stream may stay silent before it is
	// unavailable: the ANSP sends console/status/v1 every 2 s, so five
	// periods without a frame is a feed that is not there (WP-14's
	// manned_unavailable_s default).
	ANSPSilence     = 10 * time.Second
	ansPRetryMin    = time.Second
	ansPRetryMax    = 30 * time.Second
	ansPDialTimeout = 10 * time.Second
)

// Counters of the stream reader.
const (
	CounterANSPConnects      = "ansp_feed_connects"
	CounterANSPDisconnects   = "ansp_feed_disconnects"
	CounterANSPFrames        = "ansp_feed_frames"
	CounterANSPTracksHeld    = "ansp_feed_manned_tracks_held"
	CounterANSPStatusFrames  = "ansp_feed_status_frames"
	CounterANSPUnknownSchema = "ansp_feed_unknown_schema"
	CounterANSPUnreadable    = "ansp_feed_unreadable"
)

// TokenSource hands out an ecosystem token whose audience is the host of
// baseURL (auth.Outgoing; M18).
type TokenSource interface {
	Token(ctx context.Context, baseURL string, scopes ...string) (string, error)
}

// ANSPFeed reads the ANSP's manned-traffic stream (GET
// /v1/manned-traffic/stream, a WebSocket of envelope frames, M12, M29)
// with a token of scope ansp.traffic and mTLS per USSP_MTLS_MODE, and
// reconnects forever (B-08). It keeps what the feed says about itself:
// connected or not since when, the last frame, the ANSP's own degraded
// list from console/status/v1. The manned tracks it receives are counted
// and held: they reach man.v1, the CPA path and the products with WP-14,
// once its echo guard keeps one of this USSP's own flights heard back
// from being judged as a second aircraft (PLAN §15 Q23); until then no
// manned track is published, said on /readyz.
type ANSPFeed struct {
	URL      string
	Tokens   TokenSource
	HTTP     *http.Client
	Counters *core.Counters
	Logger   *slog.Logger
	Now      func() time.Time
	// Silence (ANSPSilence) and RetryMin (1 s) are what a test shortens.
	Silence  time.Duration
	RetryMin time.Duration

	mu        sync.Mutex
	connected bool
	since     time.Time
	lastFrame time.Time
	lastErr   string
	degraded  []string
	tracks    uint64
}

func (f *ANSPFeed) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func (f *ANSPFeed) silence() time.Duration {
	if f.Silence <= 0 {
		return ANSPSilence
	}
	return f.Silence
}

func (f *ANSPFeed) logger() *slog.Logger {
	if f.Logger == nil {
		return obs.Discard()
	}
	return f.Logger
}

func (f *ANSPFeed) count(name string) {
	if f.Counters != nil {
		f.Counters.Inc(name)
	}
}

// baseURL is the stream URL's origin (the ANSP's published base URL,
// whose host is the token's audience).
func (f *ANSPFeed) baseURL() string {
	u, err := url.Parse(f.URL)
	if err != nil {
		return f.URL
	}
	scheme := "https"
	if u.Scheme == "ws" {
		scheme = "http"
	}
	return scheme + "://" + u.Host
}

// set records the connection's state. A new connection counts as a
// frame (the silence bound runs from it) and forgets what the previous
// one said of the ANSP's feeds (its first console/status/v1 says it
// again).
func (f *ANSPFeed) set(connected bool, why string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.connected != connected || f.since.IsZero() {
		f.connected, f.since = connected, f.now()
		if connected {
			f.lastFrame, f.degraded = f.since, nil
		}
	}
	f.lastErr = why
}

// Run reads the stream until ctx ends, reconnecting with backoff.
func (f *ANSPFeed) Run(ctx context.Context) {
	f.set(false, "not connected yet")
	retryMin := f.RetryMin
	if retryMin <= 0 {
		retryMin = ansPRetryMin
	}
	retry := retryMin
	for ctx.Err() == nil {
		connected, err := f.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if connected {
			retry = retryMin
		}
		f.count(CounterANSPDisconnects)
		f.set(false, clipErr(err))
		f.logger().LogAttrs(ctx, slog.LevelWarn, "ANSP manned-traffic stream lost; reconnecting", slog.Duration("in", retry), obs.Err(err))
		t := time.NewTimer(retry)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		retry = min(retry*2, ansPRetryMax)
	}
}

func clipErr(err error) string {
	if err == nil {
		return "closed"
	}
	s := err.Error()
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// session is one connection: dial, read frames until it ends; connected
// is whether the dial succeeded.
func (f *ANSPFeed) session(ctx context.Context) (connected bool, err error) {
	dctx, cancel := context.WithTimeout(ctx, ansPDialTimeout)
	defer cancel()
	tok, err := f.Tokens.Token(dctx, f.baseURL(), ScopeANSPTraffic)
	if err != nil {
		return false, fmt.Errorf("no token for the ANSP: %w", err)
	}
	conn, resp, err := websocket.Dial(dctx, f.URL, &websocket.DialOptions{HTTPClient: f.HTTP,
		HTTPHeader: http.Header{"Authorization": {"Bearer " + tok}}})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return false, fmt.Errorf("dial: %w", err)
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(ANSPFrameBytes)
	f.count(CounterANSPConnects)
	f.set(true, "")
	f.logger().LogAttrs(ctx, slog.LevelInfo, "ANSP manned-traffic stream connected")
	for {
		rctx, rcancel := context.WithTimeout(ctx, f.silence())
		_, data, err := conn.Read(rctx)
		rcancel()
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				return true, fmt.Errorf("no frame for %s", f.silence())
			}
			return true, err
		}
		f.take(data)
	}
}

// take handles one frame: dispatched on its schema (M29); an unknown
// schema is counted and skipped.
func (f *ANSPFeed) take(data []byte) {
	var fr struct {
		Schema string `json:"schema"`
		Body   struct {
			Degraded []string `json:"degraded"`
		} `json:"body"`
	}
	if err := json.Unmarshal(data, &fr); err != nil || fr.Schema == "" {
		f.count(CounterANSPUnreadable)
		return
	}
	f.count(CounterANSPFrames)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastFrame = f.now()
	switch fr.Schema {
	case "track/manned/v1":
		f.tracks++
		f.count(CounterANSPTracksHeld)
	case "console/status/v1":
		f.degraded = append([]string(nil), fr.Body.Degraded...)
		f.count(CounterANSPStatusFrames)
	case "console/snapshot/v1":
	default:
		f.count(CounterANSPUnknownSchema)
	}
}

// Probe is the readiness of the stream: down while it is not connected
// or silent past ANSPSilence (unavailable since T), degraded while the
// ANSP says a feed of its own is degraded, up otherwise; every answer
// says that the manned tracks are held until WP-14.
func (f *ANSPFeed) Probe(context.Context) (obs.State, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	held := fmt.Sprintf("%d manned tracks received and held: not published to man.v1 until WP-14's echo guard (PLAN §15 Q23)", f.tracks)
	now := f.now()
	switch {
	case !f.connected:
		return obs.StateDown, fmt.Sprintf("unavailable since %s: %s; %s", f.since.UTC().Format(time.RFC3339), f.lastErr, held)
	case !f.lastFrame.IsZero() && now.Sub(f.lastFrame) > f.silence():
		return obs.StateDown, fmt.Sprintf("unavailable since %s: no frame for %.0f s; %s", f.lastFrame.UTC().Format(time.RFC3339), now.Sub(f.lastFrame).Seconds(), held)
	case len(f.degraded) > 0:
		return obs.StateDegraded, "the ANSP says degraded: " + strings.Join(f.degraded, ", ") + "; " + held
	}
	return obs.StateUp, "connected since " + f.since.UTC().Format(time.RFC3339) + "; " + held
}
