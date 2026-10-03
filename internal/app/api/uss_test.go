package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/dss"
	"github.com/rootxkit/uspace-ussp/internal/httpx"
)

// exchanges records what the exchange log writes.
type exchanges struct {
	mu  sync.Mutex
	got []dss.Exchange
}

func (e *exchanges) InsertExchange(_ context.Context, x dss.Exchange) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.got = append(e.got, x)
	return nil
}

func (e *exchanges) n() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.got)
}

// bearerGuard admits a request with the bearer "good" and answers any
// other 401, as auth.Guard does for a missing or invalid token.
func bearerGuard(httpx.Access) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer good" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// The USS endpoints record an exchange only once the guard admitted it:
// requests without a valid token are refused and never reach the
// exchange log (its queue and its table); an admitted one is recorded
// with its answer (E-01 both).
func TestUSSExchangesAreRecordedBehindTheGuard(t *testing.T) {
	st := &exchanges{}
	exlog := dss.NewExchangeLog(st, &core.Counters{}, nil)
	h, err := ussHandler(&dss.Server{}, bearerGuard, exlog, &core.Counters{})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { exlog.Run(ctx); close(done) }()
	call := func(method, path, bearer, body string) int {
		req, err := http.NewRequestWithContext(context.Background(), method, srv.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		return res.StatusCode
	}
	const path = "/uss/v1/constraints/11111111-1111-4111-8111-111111111111"
	for i := range 50 {
		bearer := ""
		if i%2 == 1 {
			bearer = "forged"
		}
		if st := call(http.MethodGet, path, bearer, ""); st != http.StatusUnauthorized {
			t.Fatalf("refused request answered %d", st)
		}
		if st := call(http.MethodPost, "/uss/v1/reports", bearer, strings.Repeat("x", 4096)); st != http.StatusUnauthorized {
			t.Fatalf("refused report answered %d", st)
		}
	}
	if st := call(http.MethodGet, path, "good", ""); st != http.StatusNotFound {
		t.Fatalf("admitted request answered %d", st)
	}
	deadline := time.Now().Add(5 * time.Second)
	for st.n() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if n := st.n(); n != 1 {
		t.Fatalf("%d exchanges recorded; only the admitted one may be", n)
	}
	if e := st.got[0]; e.ResponseCode != http.StatusNotFound || e.Role != dss.RoleServer {
		t.Fatalf("recorded %+v", e)
	}
}
