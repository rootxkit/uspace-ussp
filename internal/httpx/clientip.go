package httpx

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
)

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
func RealIP(proxies []netip.Prefix) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := ClientIP(r.RemoteAddr, r.Header.Values("X-Forwarded-For"), proxies)
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientIPKey{}, ip)))
		})
	}
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
