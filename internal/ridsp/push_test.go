package ridsp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	coreauth "github.com/rootxkit/uspace-core/auth"
	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3411"

	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/policy"
)

// scopeVerifier admits the token "dp" with rid.display_provider and
// "other" with another scope, from an ecosystem issuer.
type scopeVerifier struct{}

func (scopeVerifier) Verify(_ context.Context, tok string) (coreauth.Claims, error) {
	switch tok {
	case "dp":
		return coreauth.Claims{Issuer: "https://authority.test", Subject: "authority-dp", Scopes: []string{"rid.display_provider"}}, nil
	case "other":
		return coreauth.Claims{Issuer: "https://authority.test", Subject: "x", Scopes: []string{"rid.service_provider"}}, nil
	}
	return coreauth.Claims{}, errors.New("bad token")
}

func newPush(t *testing.T, c *clock, w *Window) (*Push, string) {
	t.Helper()
	g := &auth.Guard{Verifier: scopeVerifier{}}
	p := &Push{Window: w, WS: &auth.WSAuth{Guard: g}, Now: c.now, StatusEvery: time.Hour, Counters: &core.Counters{},
		Policy: func() policy.Record { return policy.Record{Version: 7, Values: policy.Defaults()} }}
	mux := http.NewServeMux()
	if err := RegisterPush(mux, p, g.Require); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return p, "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/authority/flights"
}

func dial(t *testing.T, url, tok string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + tok}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

type frame struct {
	Schema  string          `json:"schema"`
	Backlog bool            `json:"backlog"`
	Body    json.RawMessage `json:"body"`
}

func read(t *testing.T, conn *websocket.Conn) frame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var f frame
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

// A frame per airborne flight whose state is new: a flight on the ground
// makes none, a state already pushed makes none, the next state does
// (E-01 both ways).
func TestPushTick(t *testing.T) {
	c := &clock{at: t0}
	w := newWindow(c)
	w.Add(sample(1, origin, t0))
	ground := sample(2, origin, t0)
	gs := string(f3411.Ground)
	ground.Body.Status = &gs
	w.Add(ground)
	p := &Push{Window: w, Now: c.now, Counters: &core.Counters{}}
	if n := p.Tick(); n != 1 {
		t.Fatalf("frames %d, want the airborne flight only", n)
	}
	c.add(time.Second)
	if n := p.Tick(); n != 0 {
		t.Fatalf("a state pushed twice: %d", n)
	}
	w.Add(sample(1, origin, c.now()))
	if n := p.Tick(); n != 1 || p.Buffered() != 2 || p.Counters.Get(CounterPushFrames) != 2 {
		t.Fatalf("the next state: %d buffered %d", n, p.Buffered())
	}
}

// E-10 and the gap both ways: while nobody is connected the buffer
// holds at most MaxFrames and sheds the oldest, which is counted; the
// next client is told (dropped_frames, authority_push_gap) and then gets
// what is still buffered, marked backlog. A client connected while
// frames are made gets them live, without a gap.
func TestPushBufferAndGap(t *testing.T) {
	c := &clock{at: t0}
	w := newWindow(c)
	p, url := newPush(t, c, w)
	p.MaxFrames = 3
	for i := range 5 {
		w.Add(sample(1, origin, c.now()))
		if p.Tick() != 1 {
			t.Fatalf("tick %d", i)
		}
		c.add(time.Second)
	}
	if p.Buffered() != 3 || p.Counters.Get(CounterPushShed) != 2 {
		t.Fatalf("buffered %d %v", p.Buffered(), p.Counters.Snapshot())
	}
	conn := dial(t, url, "dp")
	st := read(t, conn)
	var body PushStatusBody
	_ = json.Unmarshal(st.Body, &body)
	if st.Schema != SchemaConsoleStatus || body.DroppedFrames != 2 || !slices.Contains(body.Degraded, SlugPushGap) ||
		body.PolicyVersion != "7" || body.Buffered != 3 || p.Counters.Get(CounterPushGaps) != 1 {
		t.Fatalf("status %s %+v %v", st.Schema, body, p.Counters.Snapshot())
	}
	for range 3 {
		f := read(t, conn)
		got, err := f3411.UnmarshalRIDFlight(f.Body)
		if f.Schema != SchemaAuthorityFlight || !f.Backlog || err != nil || got.Id != flightN(1) {
			t.Fatalf("buffered frame %+v %v", f, err)
		}
		if strings.Contains(string(f.Body), "operator_location") {
			t.Fatal("operator_location in a push frame")
		}
	}
	w.Add(sample(1, origin, c.now()))
	p.Tick()
	f := read(t, conn)
	if f.Schema != SchemaAuthorityFlight || f.Backlog {
		t.Fatalf("live frame %+v", f)
	}
	// The server counts a frame once its write returned, which may be
	// after the client read it.
	for deadline := time.Now().Add(5 * time.Second); p.Counters.Get(CounterPushSent) < 4 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	if p.Counters.Get(CounterPushSent) != 4 || p.Counters.Get(CounterPushSessions) != 1 {
		t.Fatalf("%v", p.Counters.Snapshot())
	}
	// A frame shed while a client is connected and behind is that
	// client's gap, not the next one's.
	p.mu.Lock()
	if p.lost != 0 {
		t.Errorf("lost %d with nothing shed unseen", p.lost)
	}
	p.mu.Unlock()
}

// The upgrade needs rid.display_provider: another scope or no token is
// closed with 4401 and gets no frame.
func TestPushNeedsTheDisplayProviderScope(t *testing.T) {
	c := &clock{at: t0}
	w := newWindow(c)
	_, url := newPush(t, c, w)
	for _, tok := range []string{"other", "nope"} {
		conn := dial(t, url, tok)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _, err := conn.Read(ctx)
		cancel()
		if websocket.CloseStatus(err) != auth.CloseRelogin {
			t.Errorf("%s: %v", tok, err)
		}
	}
}

// A client that falls behind loses what is shed under it: the gap is
// its own (its cursor moves to the oldest frame held); a client that
// kept up has none (E-01 pair).
func TestPushSlowClientGap(t *testing.T) {
	c := &clock{at: t0}
	w := newWindow(c)
	p := &Push{Window: w, Now: c.now, MaxFrames: 2, Counters: &core.Counters{}}
	p.conns = 1 // a client is connected: shedding is its gap, not lost
	for range 4 {
		w.Add(sample(1, origin, c.now()))
		p.Tick()
		c.add(time.Second)
	}
	behind := &session{cursor: 1}
	batch, _ := p.nextBatch(behind)
	if behind.gap != 2 || behind.cursor != 3 || len(batch) != 2 || batch[0].seq != 3 {
		t.Fatalf("behind: gap %d cursor %d batch %d", behind.gap, behind.cursor, len(batch))
	}
	upToDate := &session{cursor: 4}
	batch, _ = p.nextBatch(upToDate)
	if upToDate.gap != 0 || len(batch) != 1 || batch[0].seq != 4 {
		t.Fatalf("up to date: gap %d batch %d", upToDate.gap, len(batch))
	}
	if p.lost != 0 {
		t.Fatalf("shed with a client connected counted as lost: %d", p.lost)
	}
	none := &Push{Window: newWindow(c)}
	if b, wake := none.nextBatch(&session{cursor: 1}); b != nil || wake == nil {
		t.Fatal("an empty buffer")
	}
}

// Run makes frames every Every until its context ends.
func TestPushRun(t *testing.T) {
	c := &clock{at: t0}
	w := newWindow(c)
	w.Add(sample(1, origin, t0))
	p := &Push{Window: w, Now: c.now, Every: 5 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for p.Buffered() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if p.Buffered() != 1 {
		t.Fatalf("buffered %d", p.Buffered())
	}
}
