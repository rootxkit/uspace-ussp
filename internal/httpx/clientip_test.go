package httpx

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
)

func mustProxies(t *testing.T, s ...string) []netip.Prefix {
	t.Helper()
	p, err := ParseTrustedProxies(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// The client is the rightmost untrusted X-Forwarded-For hop, and only
// when the peer is a trusted proxy; each case beside its twin (E-01).
func TestClientIP(t *testing.T) {
	caddy := mustProxies(t, "10.0.0.0/8", "::1")
	cases := []struct {
		name, peer string
		xff        []string
		proxies    []netip.Prefix
		want       string
	}{
		{"no proxies: peer, header ignored", "198.51.100.7:4000", []string{"203.0.113.9"}, nil, "198.51.100.7"},
		{"untrusted peer: header ignored", "198.51.100.7:4000", []string{"203.0.113.9"}, caddy, "198.51.100.7"},
		{"trusted peer: the forwarded client", "10.0.0.2:4000", []string{"203.0.113.9"}, caddy, "203.0.113.9"},
		{"spoofed left part ignored", "10.0.0.2:4000", []string{"1.2.3.4, 203.0.113.9"}, caddy, "203.0.113.9"},
		{"chain of trusted proxies", "10.0.0.2:4000", []string{"203.0.113.9, 10.1.1.1", "10.2.2.2"}, caddy, "203.0.113.9"},
		{"all hops trusted: leftmost", "10.0.0.2:4000", []string{"10.9.9.9"}, caddy, "10.9.9.9"},
		{"unparsable hop: the proxy that wrote it", "10.0.0.2:4000", []string{"garbage, 10.3.3.3"}, caddy, "10.3.3.3"},
		{"unparsable last hop: the peer", "10.0.0.2:4000", []string{"garbage"}, caddy, "10.0.0.2"},
		{"no header: the peer", "10.0.0.2:4000", nil, caddy, "10.0.0.2"},
		{"ipv6 loopback proxy", "[::1]:4000", []string{"2001:db8::7"}, caddy, "2001:db8::7"},
		{"peer without a port", "10.0.0.2", []string{"203.0.113.9"}, caddy, "203.0.113.9"},
		{"unparsable peer: as given", "caddy:80", []string{"203.0.113.9"}, caddy, "caddy"},
	}
	for _, c := range cases {
		if got := ClientIP(c.peer, c.xff, c.proxies); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	// E-10: past MaxForwardedHops only the rightmost hops are read.
	long := strings.Repeat("10.5.5.5, ", 100) + "203.0.113.1"
	if got := ClientIP("10.0.0.2:1", []string{"198.51.100.1, " + long}, caddy); got != "203.0.113.1" {
		t.Fatalf("long chain: %q", got)
	}
	allTrusted := strings.TrimSuffix(strings.Repeat("10.5.5.5, ", MaxForwardedHops+5), ", ")
	if got := ClientIP("10.0.0.2:1", []string{"198.51.100.1, " + allTrusted}, caddy); got != "10.5.5.5" {
		t.Fatalf("the walk read past the hop bound: %q", got)
	}
	if _, err := ParseTrustedProxies([]string{"caddy"}); err == nil {
		t.Fatal("a name accepted as a proxy")
	}
	if p := mustProxies(t, "192.0.2.7", " 10.1.2.3/8 "); p[0].Bits() != 32 || p[1].String() != "10.0.0.0/8" {
		t.Fatalf("parsed %v", p)
	}
}

// Behind a trusted proxy every client gets its own bucket and its own
// access-log address; from an untrusted peer X-Forwarded-For buys
// nothing (authority WP-2 review lesson: never key on RemoteAddr behind
// Caddy, never believe a header from anyone else).
func TestBaselineKeysLimitsOnTheForwardedClient(t *testing.T) {
	c := &core.Counters{}
	l := NewRateLimiter(0.001, 1, 10, c)
	h := Baseline(l.LimitByIP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(RemoteIP(r))) })),
		discard(), BaselineDeps{Counters: c, TrustedProxies: mustProxies(t, "10.0.0.0/8")})
	do := func(peer, xff string) (int, string) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = peer
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code, rec.Body.String()
	}
	if code, ip := do("10.0.0.2:1", "203.0.113.1"); code != 200 || ip != "203.0.113.1" {
		t.Fatalf("first client: %d %q", code, ip)
	}
	if code, _ := do("10.0.0.2:1", "203.0.113.2"); code != 200 {
		t.Fatal("a second client behind the proxy shared the first one's bucket")
	}
	if code, _ := do("10.0.0.2:1", "203.0.113.1"); code != 429 {
		t.Fatal("the first client's bucket was not its own")
	}
	if code, _ := do("198.51.100.5:1", "203.0.113.3"); code != 200 {
		t.Fatal("untrusted peer, first request")
	}
	if code, _ := do("198.51.100.5:1", "203.0.113.4"); code != 429 {
		t.Fatal("an untrusted peer escaped its bucket by changing X-Forwarded-For")
	}
}

func TestRemoteIPOutsideRealIP(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "192.0.2.1:5"
	if RemoteIP(r) != "192.0.2.1" {
		t.Fatal(RemoteIP(r))
	}
	r.RemoteAddr = "pipe"
	if RemoteIP(r) != "pipe" {
		t.Fatal(RemoteIP(r))
	}
}

func TestRateLimiterRefusesOverBudgetAndRefills(t *testing.T) {
	now := time.Unix(1000, 0)
	c := &core.Counters{}
	l := NewRateLimiter(1, 2, 10, c)
	l.SetClock(func() time.Time { return now })
	for i := range 2 {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatalf("request %d within the burst refused", i)
		}
	}
	ok, wait := l.Allow("a")
	if ok || wait <= 0 || wait > time.Second {
		t.Fatalf("over budget: ok=%v wait=%s", ok, wait)
	}
	if c.Get(CounterRateLimited) != 1 {
		t.Fatal("refusal not counted")
	}
	now = now.Add(time.Second)
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("not refilled after a second")
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Fatal("another key shares a's bucket")
	}
}

// E-10: the key map holds at most maxKeys; one more evicts the least
// recently seen, which starts again with a full bucket.
func TestRateLimiterEvictsBeyondMaxKeys(t *testing.T) {
	c := &core.Counters{}
	l := NewRateLimiter(0.001, 1, 3, c)
	for _, k := range []string{"a", "b", "c"} {
		l.Allow(k)
	}
	if ok, _ := l.Allow("a"); ok {
		t.Fatal("a's bucket was not spent")
	}
	l.Allow("d") // evicts b, the least recently seen
	if l.Len() != 3 || c.Get(CounterLimiterEvict) != 1 {
		t.Fatalf("len %d evictions %d", l.Len(), c.Get(CounterLimiterEvict))
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Fatal("an evicted key did not start with a full bucket")
	}
}

func TestLimitByIPAnswers429WithRetryAfter(t *testing.T) {
	l := NewRateLimiter(0.5, 1, 10, nil)
	h := l.LimitByIP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	do := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))
		return rec
	}
	if rec := do(); rec.Code != 204 {
		t.Fatalf("first: %d", rec.Code)
	}
	rec := do()
	if p := decodeProblem(t, rec); rec.Code != 429 || p.Slug() != SlugRateLimited || rec.Header().Get("Retry-After") != "2" {
		t.Fatalf("second: %d %s Retry-After %q", rec.Code, p.Slug(), rec.Header().Get("Retry-After"))
	}
}

func FuzzClientIP(f *testing.F) {
	proxies := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	f.Add("10.0.0.2:1", "203.0.113.9, 10.1.1.1")
	f.Add("x", ",,,")
	f.Add("[::ffff:10.0.0.1]:3", "::ffff:203.0.113.4")
	f.Fuzz(func(t *testing.T, peer, xff string) {
		got := ClientIP(peer, []string{xff}, proxies)
		host, _, err := net.SplitHostPort(peer)
		if err != nil {
			host = peer
		}
		a, err := netip.ParseAddr(host)
		if err != nil || !proxies[0].Contains(a.Unmap()) {
			if got != host {
				t.Fatalf("untrusted peer %q yielded %q", peer, got)
			}
			return
		}
		// Behind the trusted proxy the answer is always an address.
		if _, err := netip.ParseAddr(got); err != nil {
			t.Fatalf("trusted peer %q, header %q yielded %q", peer, xff, got)
		}
	})
}
