package httpx

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/core"
)

// CounterXFFUntrusted counts requests that carried X-Forwarded-For from
// a peer that is not a trusted proxy (audit S7).
const CounterXFFUntrusted = "xff_from_untrusted_peer"

// proxyWarnEvery is the shortest interval between two warnings of a
// ProxyWatch.
const proxyWarnEvery = 10 * time.Minute

// ProxyWatch notices X-Forwarded-For sent by a peer that is not a
// trusted proxy. The header is not believed (the peer is the client),
// which is right; but behind a reverse proxy with USSP_TRUSTED_PROXIES
// unset it means every client is keyed on the proxy's address: one
// address carries every sign-in's rate limit and every audit row's
// remote_ip. A ProxyWatch counts it, warns once per period and keeps
// the last peer for the readiness probe. Safe for concurrent use.
type ProxyWatch struct {
	Counters *core.Counters
	Logger   *slog.Logger

	mu       sync.Mutex
	lastPeer string
	lastAt   time.Time
	lastWarn time.Time
	n        uint64
}

func (w *ProxyWatch) saw(peer string) {
	if w == nil {
		return
	}
	now := time.Now()
	w.mu.Lock()
	w.lastPeer, w.lastAt = peer, now
	w.n++
	warn := now.Sub(w.lastWarn) >= proxyWarnEvery
	if warn {
		w.lastWarn = now
	}
	n := w.n
	w.mu.Unlock()
	if w.Counters != nil {
		w.Counters.Inc(CounterXFFUntrusted)
	}
	if warn && w.Logger != nil {
		w.Logger.Warn("X-Forwarded-For from a peer that is not a trusted proxy; the peer is taken as the client: "+
			"behind a reverse proxy set USSP_TRUSTED_PROXIES, or every client shares its address for rate limits and audit rows",
			slog.String("peer", peer), slog.Uint64("since_start", n))
	}
}

// Last is the last peer that sent X-Forwarded-For untrusted, when, and
// how many requests did since the start ("", zero, 0: none).
func (w *ProxyWatch) Last() (peer string, at time.Time, n uint64) {
	if w == nil {
		return "", time.Time{}, 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastPeer, w.lastAt, w.n
}

// MaxForwardedHops bounds how many X-Forwarded-For entries are read: a
// longer chain is cut to its rightmost entries, the ones the trusted
// proxies appended (E-10).
const MaxForwardedHops = 32

// ParseTrustedProxies parses USSP_TRUSTED_PROXIES: CIDRs or single
// addresses of the reverse proxies (Caddy) whose X-Forwarded-For is
// believed. Nothing else's is.
func ParseTrustedProxies(list []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(list))
	for _, s := range list {
		s = strings.TrimSpace(s)
		if p, err := netip.ParsePrefix(s); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(s)
		if err != nil {
			return nil, fmt.Errorf("%q is neither a CIDR nor an address", s)
		}
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

func trusted(a netip.Addr, proxies []netip.Prefix) bool {
	a = a.Unmap()
	return slices.ContainsFunc(proxies, func(p netip.Prefix) bool { return p.Contains(a) })
}

// ClientIP is the address of the client behind trusted proxies: the
// peer itself unless the peer is a trusted proxy; then, walking
// X-Forwarded-For from the right, the first hop that is not a trusted
// proxy. The left part of the header is written by whoever sent the
// request and is never believed, so a client cannot choose the address
// its rate limits and audit rows are keyed on. A hop that does not
// parse ends the walk at the last trusted hop (the proxy that appended
// it). With no trusted proxy configured the peer is always the client.
func ClientIP(peer string, forwarded []string, proxies []netip.Prefix) string {
	host, _, err := net.SplitHostPort(peer)
	if err != nil {
		host = peer
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || len(proxies) == 0 || !trusted(addr, proxies) {
		return host
	}
	var hops []string
	for _, h := range forwarded {
		for p := range strings.SplitSeq(h, ",") {
			hops = append(hops, strings.TrimSpace(p))
		}
	}
	if len(hops) > MaxForwardedHops {
		hops = hops[len(hops)-MaxForwardedHops:]
	}
	client := addr.Unmap()
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(hops[i])
		if err != nil {
			break
		}
		client = a.Unmap()
		if !trusted(client, proxies) {
			break
		}
	}
	return client.String()
}

type clientIPKey struct{}

// RealIP puts the client address (ClientIP) on the request for
// RemoteIP: every rate limiter, every audit row and the access log read
// it from there, never from r.RemoteAddr, which behind Caddy is Caddy.
// watch (may be nil) is told of every X-Forwarded-For from a peer that
// is not a trusted proxy.
func RealIP(proxies []netip.Prefix, watch ...*ProxyWatch) func(http.Handler) http.Handler {
	var pw *ProxyWatch
	if len(watch) > 0 {
		pw = watch[0]
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			xff := r.Header.Values("X-Forwarded-For")
			ip := ClientIP(r.RemoteAddr, xff, proxies)
			if len(xff) > 0 && pw != nil {
				if peer, ok := untrustedPeer(r.RemoteAddr, proxies); ok {
					pw.saw(peer)
				}
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientIPKey{}, ip)))
		})
	}
}

// untrustedPeer is the host of peer when it is not a trusted proxy.
func untrustedPeer(peer string, proxies []netip.Prefix) (string, bool) {
	host, _, err := net.SplitHostPort(peer)
	if err != nil {
		host = peer
	}
	addr, err := netip.ParseAddr(host)
	if err == nil && trusted(addr, proxies) {
		return "", false
	}
	return host, true
}

// RemoteIP is the client address RealIP derived; outside RealIP (a
// unit test, a handler mounted without Baseline) it is the host part of
// r.RemoteAddr.
func RemoteIP(r *http.Request) string {
	if ip, ok := r.Context().Value(clientIPKey{}).(string); ok && ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
