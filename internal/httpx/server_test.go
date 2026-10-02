package httpx

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
)

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestNewServerAppliesTimeoutDefaults(t *testing.T) {
	s := NewServer(ServerOptions{Name: "t", Addr: "127.0.0.1:0", Logger: discard()}).HTTPServer()
	if s.ReadHeaderTimeout != DefaultReadHeaderTimeout || s.ReadTimeout != DefaultReadTimeout ||
		s.WriteTimeout != DefaultWriteTimeout || s.IdleTimeout != DefaultIdleTimeout || s.MaxHeaderBytes != DefaultMaxHeaderBytes {
		t.Errorf("defaults: %+v", s)
	}
	s = NewServer(ServerOptions{Logger: discard(), ReadHeaderTimeout: time.Second, NoWriteTimeout: true}).HTTPServer()
	if s.ReadHeaderTimeout != time.Second || s.WriteTimeout != 0 {
		t.Errorf("overrides: %+v", s)
	}
}

// The slow request is in flight when the context ends, and the drain
// waits for it. Two races made this test fail about one run in ten on
// Windows, both with "drain exceeded": a shared keep-alive transport
// could dial a spare connection for /slow while the /nowhere connection
// was still being returned to its pool, and that spare connection sat
// in StateNew, which Shutdown leaves alone for 5 s (net/http issue
// 22682), the whole drain bound; and fixed sleeps stood in for "the
// request reached the handler" and "the shutdown has begun". Each
// request now has its own connection, and both moments are observed.
func TestServeAnswersThenDrainsOnCancel(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /slow", func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		_, _ = io.WriteString(w, "done")
	})
	mux.HandleFunc("/", NotFound)
	s := NewServer(ServerOptions{Name: "public", Addr: "127.0.0.1:0", Logger: discard(),
		Handler: Baseline(mux, discard(), BaselineDeps{Counters: &core.Counters{}})})
	ln, err := s.Listen()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- s.Serve(ctx, ln, 5*time.Second) }()

	// One connection per request: no idle connection to race with.
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	url := "http://" + ln.Addr().String()
	resp, err := client.Get(url + "/nowhere")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 404 || resp.Header.Get("Content-Type") != ProblemContentType || resp.Header.Get(RequestIDHeader) == "" {
		t.Fatalf("unmatched path: %d %v", resp.StatusCode, resp.Header)
	}

	got := make(chan string, 1)
	go func() {
		resp, err := client.Get(url + "/slow")
		if err != nil {
			got <- err.Error()
			return
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		got <- string(b)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the slow request never reached its handler")
	}
	cancel()
	// The shutdown has begun once the listener refuses new connections.
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", ln.Addr().String(), 100*time.Millisecond)
		if err != nil {
			break
		}
		_ = c.Close()
		if time.Now().After(deadline) {
			t.Fatal("the listener still accepts after the context ended")
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	if body := <-got; body != "done" {
		t.Fatalf("in-flight request not drained: %q", body)
	}
	if err := <-served; err != nil {
		t.Fatalf("clean drain returned %v", err)
	}
}

func TestServeReportsADrainThatOverrunsItsBound(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /stuck", func(http.ResponseWriter, *http.Request) { <-block })
	s := NewServer(ServerOptions{Name: "public", Addr: "127.0.0.1:0", Logger: discard(), Handler: mux})
	ln, err := s.Listen()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- s.Serve(ctx, ln, 100*time.Millisecond) }()
	go func() {
		if resp, err := http.Get("http://" + ln.Addr().String() + "/stuck"); err == nil {
			_ = resp.Body.Close()
		}
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	err = <-served
	if err == nil || !strings.Contains(err.Error(), "drain exceeded") {
		t.Fatalf("got %v, want a drain overrun", err)
	}
}

func TestListenReportsABindError(t *testing.T) {
	a := NewServer(ServerOptions{Name: "a", Addr: "127.0.0.1:0", Logger: discard()})
	ln, err := a.Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	b := NewServer(ServerOptions{Name: "b", Addr: ln.Addr().String(), Logger: discard()})
	if _, err := b.Listen(); err == nil || !strings.Contains(err.Error(), "b listener") {
		t.Fatalf("got %v", err)
	}
}

func TestServeReturnsWhenTheListenerFails(t *testing.T) {
	s := NewServer(ServerOptions{Name: "x", Addr: "127.0.0.1:0", Logger: discard()})
	ln, err := s.Listen()
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()
	if err := s.Serve(context.Background(), ln, time.Second); err == nil {
		t.Fatal("Serve on a closed listener returned nil")
	}
}

func TestBaselineAppliesTheBodyCap(t *testing.T) {
	c := &core.Counters{}
	h := Baseline(http.HandlerFunc(readAll), discard(), BaselineDeps{Counters: c, MaxBodyBytes: 4})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("12345")))
	if rec.Code != 413 {
		t.Fatalf("body cap: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("1234")))
	if rec.Code != 200 {
		t.Fatalf("at the cap: %d", rec.Code)
	}
	// The default cap applies when none is given.
	h = Baseline(http.HandlerFunc(readAll), discard(), BaselineDeps{Counters: c})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("x", DefaultMaxBodyBytes+1))))
	if rec.Code != 413 {
		t.Fatalf("default cap: %d", rec.Code)
	}
}
