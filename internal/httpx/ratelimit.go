package httpx

import (
	"container/list"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/core"
)

// Counter names of the rate limiters.
const (
	CounterRateLimited  = "http_rate_limited"
	CounterLimiterEvict = "http_rate_limiter_evicted"
)

// SlugRateLimited is the problem type of a spent budget.
const SlugRateLimited = "rate_limited"

// RateLimiter is a token bucket per key (a client address, a client
// id) with a bounded key map (E-10): beyond MaxKeys the least recently
// seen key is evicted (counted) and starts again with a full bucket. It
// is per process: a budget that must hold across replicas (the login
// lockout) lives in the database instead.
type RateLimiter struct {
	rate     float64 // tokens per second
	burst    float64
	max      int
	counters *core.Counters
	now      func() time.Time

	mu    sync.Mutex
	keys  map[string]*list.Element
	order *list.List // front = most recently seen
}

type bucket struct {
	key    string
	tokens float64
	last   time.Time
}

// NewRateLimiter returns a limiter of ratePerS sustained requests per
// second and burst per key, remembering at most maxKeys.
func NewRateLimiter(ratePerS float64, burst, maxKeys int, counters *core.Counters) *RateLimiter {
	if counters == nil {
		counters = &core.Counters{}
	}
	return &RateLimiter{
		rate: ratePerS, burst: float64(burst), max: max(1, maxKeys), counters: counters, now: time.Now,
		keys: map[string]*list.Element{}, order: list.New(),
	}
}

// SetClock replaces the clock (tests).
func (l *RateLimiter) SetClock(now func() time.Time) { l.now = now }

// Len is the number of keys remembered.
func (l *RateLimiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.keys)
}

// Allow takes one token from key's bucket. When it is empty it returns
// false and how long until a token is available.
func (l *RateLimiter) Allow(key string) (bool, time.Duration) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	var b *bucket
	if el, ok := l.keys[key]; ok {
		l.order.MoveToFront(el)
		b = el.Value.(*bucket)
		b.tokens = math.Min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
		b.last = now
	} else {
		if len(l.keys) >= l.max {
			oldest := l.order.Back()
			l.order.Remove(oldest)
			delete(l.keys, oldest.Value.(*bucket).key)
			l.counters.Inc(CounterLimiterEvict)
		}
		b = &bucket{key: key, tokens: l.burst, last: now}
		l.keys[key] = l.order.PushFront(b)
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	l.counters.Inc(CounterRateLimited)
	return false, time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
}

// RetryAfter writes the Retry-After header for wait: whole seconds,
// at least 1.
func RetryAfter(w http.ResponseWriter, wait time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(wait.Seconds())))))
}

// LimitByIP refuses a request over its client address's budget
// (RemoteIP) with a 429 problem and Retry-After.
func (l *RateLimiter) LimitByIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ok, wait := l.Allow("ip:" + RemoteIP(r)); !ok {
			RetryAfter(w, wait)
			NewProblem(http.StatusTooManyRequests, SlugRateLimited, "Too many requests", "the request budget of this address is spent; wait and try again").Write(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}
