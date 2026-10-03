package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// tokenService is a fake token service that counts requests, records
// the last form and can be slowed or made to refuse.
type tokenService struct {
	srv      *httptest.Server
	calls    atomic.Int64
	mu       sync.Mutex
	last     map[string]string
	ttl      int
	status   int
	body     string
	gate     chan struct{}
	sawBasic bool
}

func newTokenService(t *testing.T) *tokenService {
	t.Helper()
	s := &tokenService{ttl: 3600, status: 200}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := s.calls.Add(1)
		if s.gate != nil {
			<-s.gate
		}
		_ = r.ParseForm()
		s.mu.Lock()
		s.last = map[string]string{}
		for k := range r.PostForm {
			s.last[k] = r.PostForm.Get(k)
		}
		_, _, s.sawBasic = r.BasicAuth()
		status, body, ttl := s.status, s.body, s.ttl
		s.mu.Unlock()
		w.WriteHeader(status)
		if body != "" {
			_, _ = w.Write([]byte(body))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("tok-%d", n), "token_type": "Bearer", "expires_in": ttl})
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func newOutgoing(t *testing.T, s *tokenService, c *clock) *Outgoing {
	t.Helper()
	o, err := NewOutgoing(OutgoingConfig{TokenURL: s.srv.URL + "/oauth/token", ClientID: ClientIDFor("USSP-DEV"), ClientSecret: "s3cret", Now: c.fn(), MaxEntries: 2})
	mustNoErr(t, err)
	return o
}

// One token per (audience, scope set), aud = the target's host (M18),
// kept until 60 s before exp, then fetched again.
func TestOutgoingCachesUntilSixtySecondsBeforeExp(t *testing.T) {
	c := newClock(t0())
	s := newTokenService(t)
	o := newOutgoing(t, s, c)
	ctx := context.Background()
	tok, err := o.Token(ctx, "https://uspace-cisp.test/v1", "cis.read")
	if err != nil || tok != "tok-1" {
		t.Fatalf("%q %v", tok, err)
	}
	if s.last["audience"] != "uspace-cisp.test" || s.last["client_id"] != "ussp-USSP-DEV-01" || s.last["scope"] != "cis.read" || s.last["grant_type"] != "client_credentials" {
		t.Fatalf("form %v", s.last)
	}
	// Same key, scope order and duplicates aside: cached.
	if tok, _ := o.TokenFor(ctx, "uspace-cisp.test", []string{"cis.read", "cis.read"}); tok != "tok-1" || s.calls.Load() != 1 {
		t.Fatalf("not cached: %q", tok)
	}
	c.Add(time.Hour - RefreshBefore - time.Second)
	if tok, _ := o.TokenFor(ctx, "uspace-cisp.test", []string{"cis.read"}); tok != "tok-1" {
		t.Fatalf("refreshed too early: %q", tok)
	}
	c.Add(time.Second)
	if tok, _ := o.TokenFor(ctx, "uspace-cisp.test", []string{"cis.read"}); tok != "tok-2" {
		t.Fatalf("not refreshed at 60 s before exp: %q", tok)
	}
	// Another audience is another token.
	if tok, _ := o.Token(ctx, "https://dss.test", "utm.strategic_coordination"); tok != "tok-3" {
		t.Fatalf("another audience: %q", tok)
	}
}

// E-14: concurrent callers share one request; a caller that gives up
// does not fail it for the others.
func TestOutgoingSingleFlightSurvivesACancelledCaller(t *testing.T) {
	c := newClock(t0())
	s := newTokenService(t)
	s.gate = make(chan struct{})
	o := newOutgoing(t, s, c)
	cctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { _, err := o.TokenFor(cctx, "dss.test", []string{"utm.strategic_coordination"}); errc <- err }()
	var wg sync.WaitGroup
	got := make([]string, 5)
	for i := range got {
		wg.Go(func() {
			got[i], _ = o.TokenFor(context.Background(), "dss.test", []string{"utm.strategic_coordination"})
		})
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled caller: %v", err)
	}
	close(s.gate)
	wg.Wait()
	for _, g := range got {
		if g != "tok-1" {
			t.Fatalf("got %v", got)
		}
	}
	if s.calls.Load() != 1 {
		t.Fatalf("%d requests", s.calls.Load())
	}
}

// E-10: beyond MaxEntries the least recently used entry is evicted.
func TestOutgoingEvictsBeyondItsBound(t *testing.T) {
	c := newClock(t0())
	s := newTokenService(t)
	o := newOutgoing(t, s, c)
	for _, aud := range []string{"a.test", "b.test", "c.test"} {
		_, err := o.TokenFor(context.Background(), aud, []string{"cis.read"})
		mustNoErr(t, err)
	}
	if o.Len() != 2 || o.Counters().Get(CounterOutgoingEvicted) != 1 || o.Counters().Get(CounterOutgoingFetched) != 3 {
		t.Fatalf("len %d counters %v", o.Len(), o.Counters().Snapshot())
	}
}

// A refusal of the token service is a TokenServiceError without the
// secret; a token already within 60 s of its end is no token.
func TestOutgoingErrors(t *testing.T) {
	c := newClock(t0())
	s := newTokenService(t)
	o := newOutgoing(t, s, c)
	ctx := context.Background()
	s.status, s.body = 401, `{"error":"invalid_client","error_description":"no"}`
	_, err := o.TokenFor(ctx, "a.test", []string{"cis.read"})
	var tse *TokenServiceError
	if !errors.As(err, &tse) || tse.Code != "invalid_client" || strings.Contains(err.Error(), "s3cret") || o.Counters().Get(CounterOutgoingFetchFailed) != 1 {
		t.Fatalf("refusal: %v", err)
	}
	s.status, s.body, s.ttl = 200, "", 30
	if _, err := o.TokenFor(ctx, "a.test", []string{"cis.read"}); err == nil {
		t.Fatal("a 30 s token handed out")
	}
	if _, err := o.TokenFor(ctx, "a.test", nil); err == nil {
		t.Fatal("no scope accepted")
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := o.TokenFor(cctx, "a.test", []string{"cis.read"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
	if _, err := o.Token(ctx, "not a url", "cis.read"); err == nil {
		t.Fatal("a malformed base URL accepted")
	}
	s.srv.Close()
	if _, err := o.TokenFor(ctx, "b.test", []string{"cis.read"}); err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("service down: %v", err)
	}
}

func TestNewOutgoingRefusesAnIncompleteConfiguration(t *testing.T) {
	for name, c := range map[string]OutgoingConfig{
		"no url":    {ClientID: "x", ClientSecret: "y"},
		"ftp url":   {TokenURL: "ftp://a/b", ClientID: "x", ClientSecret: "y"},
		"no secret": {TokenURL: "https://a/oauth/token", ClientID: "x"},
	} {
		if _, err := NewOutgoing(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	o, err := NewOutgoing(OutgoingConfig{TokenURL: "https://a/oauth/token", ClientID: "x", ClientSecret: "y"})
	if err != nil || o.cfg.MaxEntries != DefaultOutgoingMaxEntries || o.cfg.FetchTimeout != DefaultOutgoingFetchTimeout {
		t.Fatalf("defaults: %v %+v", err, o.cfg)
	}
}

func TestParseTokenResponse(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
		ok     bool
	}{
		"good":          {200, `{"access_token":"t","token_type":"bearer","expires_in":60}`, true},
		"not json":      {200, `x`, false},
		"no token":      {200, `{"token_type":"Bearer","expires_in":60}`, false},
		"not bearer":    {200, `{"access_token":"t","token_type":"mac","expires_in":60}`, false},
		"zero ttl":      {200, `{"access_token":"t","token_type":"Bearer","expires_in":0}`, false},
		"two hours":     {200, `{"access_token":"t","token_type":"Bearer","expires_in":7200}`, false},
		"string ttl":    {200, `{"access_token":"t","token_type":"Bearer","expires_in":"60"}`, false},
		"refused bare":  {503, ``, false},
		"refused oauth": {400, `{"error":"invalid_scope"}`, false},
	}
	for name, c := range cases {
		tok, ttl, err := ParseTokenResponse(c.status, []byte(c.body))
		if (err == nil) != c.ok || (c.ok && (tok != "t" || ttl != time.Minute)) {
			t.Errorf("%s: %q %s %v", name, tok, ttl, err)
		}
	}
	_, _, err := ParseTokenResponse(503, nil)
	var tse *TokenServiceError
	if !errors.As(err, &tse) || tse.Code != "http_503" {
		t.Fatalf("%v", err)
	}
}

func FuzzParseTokenResponse(f *testing.F) {
	f.Add(200, `{"access_token":"t","token_type":"Bearer","expires_in":60}`)
	f.Add(400, `{"error":"x"}`)
	f.Fuzz(func(t *testing.T, status int, body string) {
		tok, ttl, err := ParseTokenResponse(status, []byte(body))
		if err == nil && (tok == "" || ttl <= 0 || ttl > time.Hour || status != 200) {
			t.Fatalf("accepted %d %q", status, body)
		}
	})
}

func TestAudienceOfAndClientID(t *testing.T) {
	for in, want := range map[string]string{"https://USSP.example:8443/x": "ussp.example", "http://peer.test": "peer.test"} {
		if got, err := AudienceOf(in); err != nil || got != want {
			t.Errorf("%s: %q %v", in, got, err)
		}
	}
	// The code keeps its case: the authority registers ussp-<CODE>-<nn>
	// with an upper-case code only, and the lab issuer ussp-USSP-DEV-01.
	for code, want := range map[string]string{"ABC1": "ussp-ABC1-01", "USSP-DEV": "ussp-USSP-DEV-01"} {
		if got := ClientIDFor(code); got != want {
			t.Errorf("ClientIDFor(%q) = %q, want %q", code, got, want)
		}
	}
}
