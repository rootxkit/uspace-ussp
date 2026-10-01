package httpx

import (
	"context"
	"net/http"
)

type routeHolder struct{ pattern string }

const routeKey ctxKey = 100

// TrackRoute lets middleware outside a context copy see the ServeMux
// pattern that served the request (http.Request.Pattern is set only on
// the copy the mux receives). Install it outermost; it is idempotent.
func TrackRoute(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Context().Value(routeKey).(*routeHolder); ok {
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), routeKey, &routeHolder{})))
	})
}

// captureRoute wraps the mux and records the pattern it matched.
func captureRoute(mux http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
		if h, ok := r.Context().Value(routeKey).(*routeHolder); ok {
			h.pattern = r.Pattern
		}
	})
}

// Route is the ServeMux pattern that served r ("GET /v1/x/{id}"), never
// the raw path, or "unmatched".
func Route(r *http.Request) string {
	if r.Pattern != "" {
		return r.Pattern
	}
	if h, ok := r.Context().Value(routeKey).(*routeHolder); ok && h.pattern != "" {
		return h.pattern
	}
	return "unmatched"
}
