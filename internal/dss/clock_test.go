package dss

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/f3548"
)

// near reports whether a is within a minute of b.
func near(a, b time.Time) bool {
	d := a.Sub(b)
	return d < time.Minute && d > -time.Minute
}

// The availability's read time and the exchanges' times are the
// database clock (the memStore's, a day off this host's), as every other
// time the store records: an exchange served, one made, and the poll.
// E-01: the store's clock moved is followed at the next sync.
func TestTimesAreTheDatabaseClock(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	dbNow, _ := g.st.Now(ctx)
	if near(dbNow, time.Now()) {
		t.Fatal("the store's clock must differ from this host's for the test to mean anything")
	}

	a := &Availability{Client: g.c, Store: g.st, USSID: ourManager}
	g.dss.SetAvailability(ourManager, f3548.Normal)
	if err := a.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, at, _ := a.State(); !near(at, dbNow) {
		t.Fatalf("availability read at %s, the database clock is %s", at, dbNow)
	}

	st := newMemStore()
	l := NewExchangeLog(st, nil, nil)
	if err := l.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})))
	t.Cleanup(srv.Close)
	c := &http.Client{Transport: &Transport{Log: l}}
	res, err := c.Get(srv.URL + "/uss/v1/x")
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { l.Run(rctx); close(done) }()
	within(t, 5*time.Second, func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return len(st.exchanges) == 2
	})
	cancel()
	<-done
	st.mu.Lock()
	got := append([]Exchange(nil), st.exchanges...)
	st.mu.Unlock()
	for _, e := range got {
		if !near(e.RequestTime, st.now) || !near(e.ResponseTime, st.now) {
			t.Fatalf("%s exchange at %s / %s, the database clock is %s", e.Role, e.RequestTime, e.ResponseTime, st.now)
		}
	}
	st.advance(48 * time.Hour)
	if err := l.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if !near(l.Now(), st.now) {
		t.Fatalf("after a sync %s, the database clock is %s", l.Now(), st.now)
	}
}
