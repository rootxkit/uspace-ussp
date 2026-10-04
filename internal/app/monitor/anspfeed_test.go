package monitor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/obs"
)

type staticTokens struct {
	tok string
	err error
	got atomic.Value
}

func (s *staticTokens) Token(_ context.Context, baseURL string, scopes ...string) (string, error) {
	s.got.Store(baseURL + "|" + strings.Join(scopes, " "))
	return s.tok, s.err
}

// fakeStream is the ANSP's stream: it accepts only "Bearer tok" and sends
// what the test queues; a frame "drop" ends the connection.
type fakeStream struct {
	srv    *httptest.Server
	frames chan string
}

func newFakeStream(t *testing.T) *fakeStream {
	t.Helper()
	f := &fakeStream{frames: make(chan string, 16)}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		// Ends with the connection, so a closed one takes no frame.
		ctx := c.CloseRead(r.Context())
		for {
			select {
			case <-ctx.Done():
				return
			case fr := <-f.frames:
				if fr == "drop" {
					return
				}
				if err := c.Write(ctx, websocket.MessageText, []byte(fr)); err != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeStream) wsURL() string {
	return "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/v1/manned-traffic/stream"
}

// waitState polls the probe (a condition, never a fixed sleep) until it
// answers want with contains in its detail.
func waitState(t *testing.T, f *ANSPFeed, want obs.State, contains string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s, d := f.Probe(context.Background())
		if s == want && strings.Contains(d, contains) {
			return d
		}
		if time.Now().After(deadline) {
			t.Fatalf("probe %s %q, want %s with %q", s, d, want, contains)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func runFeed(t *testing.T, f *ANSPFeed) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

// The stream read end to end: connected with a bearer of scope
// ansp.traffic for the ANSP's origin; the ANSP's degraded list shown; the
// manned tracks counted and held (never published, and said so);
// unknown and unreadable frames counted; silence makes it unavailable
// since T, and a new connection brings it back (E-02).
func TestANSPFeed(t *testing.T) {
	fs := newFakeStream(t)
	tokens := &staticTokens{tok: "tok"}
	f := &ANSPFeed{URL: fs.wsURL(), Tokens: tokens, Counters: &core.Counters{}, Silence: 300 * time.Millisecond, RetryMin: 20 * time.Millisecond}
	if s, d := f.Probe(context.Background()); s != obs.StateDown || !strings.Contains(d, "unavailable since") {
		t.Fatalf("before Run: %s %s", s, d)
	}
	runFeed(t, f)
	waitState(t, f, obs.StateUp, "connected since")
	if got, _ := tokens.got.Load().(string); !strings.HasPrefix(got, "http://127.0.0.1:") || !strings.HasSuffix(got, "|ansp.traffic") {
		t.Fatalf("token asked for %q", got)
	}
	fs.frames <- `{"schema":"console/status/v1","body":{"degraded":["ads_b_stale"]}}`
	fs.frames <- `{"schema":"track/manned/v1","body":{"icao24":"4ca123"}}`
	fs.frames <- `{"schema":"console/snapshot/v1","body":{}}`
	fs.frames <- `{"schema":"weather/v1","body":{}}`
	fs.frames <- `not json`
	d := waitState(t, f, obs.StateDegraded, "1 manned tracks received and held")
	if !strings.Contains(d, "ads_b_stale") || !strings.Contains(d, "Q23") {
		t.Fatalf("detail %q", d)
	}
	for name, want := range map[string]uint64{CounterANSPTracksHeld: 1, CounterANSPStatusFrames: 1, CounterANSPUnknownSchema: 1,
		CounterANSPUnreadable: 1, CounterANSPFrames: 4} {
		if got := f.Counters.Get(name); got != want {
			t.Errorf("%s = %d, want %d", name, got, want)
		}
	}
	// Silence past the bound: unavailable; the reader reconnects and the
	// new connection starts without the old degraded list.
	waitState(t, f, obs.StateDown, "unavailable since")
	waitState(t, f, obs.StateUp, "connected since")
	if f.Counters.Get(CounterANSPConnects) < 2 || f.Counters.Get(CounterANSPDisconnects) < 1 {
		t.Errorf("counters %v", f.Counters.Snapshot())
	}
}

// Refused (E-01 pair with the connection above): no token, a bearer the
// ANSP refuses; never connected, said with the reason.
func TestANSPFeedRefused(t *testing.T) {
	fs := newFakeStream(t)
	noToken := &ANSPFeed{URL: fs.wsURL(), Tokens: &staticTokens{err: errors.New("token service down")}, RetryMin: 10 * time.Millisecond}
	runFeed(t, noToken)
	waitState(t, noToken, obs.StateDown, "no token for the ANSP")
	refused := &ANSPFeed{URL: fs.wsURL(), Tokens: &staticTokens{tok: "wrong"}, RetryMin: 10 * time.Millisecond}
	runFeed(t, refused)
	waitState(t, refused, obs.StateDown, "dial")
	if (&ANSPFeed{URL: "wss://ansp.example/v1/manned-traffic/stream"}).baseURL() != "https://ansp.example" {
		t.Error("baseURL")
	}
}
