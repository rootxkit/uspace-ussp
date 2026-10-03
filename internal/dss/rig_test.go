package dss

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"
	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3548"

	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/dss/fakedss"
	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/intent"
	"github.com/rootxkit/uspace-ussp/internal/stdapi"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/peeruss"
)

// ourManager and peerManager are the subs of the two USSs' tokens.
const (
	ourManager  = "ussp-test-01"
	peerManager = "peer-uss-01"
)

// fakeJWT is a token whose payload names sub (the fake DSS and the test
// guard read it; nothing verifies it).
func fakeJWT(sub string, scopes ...string) string {
	h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	b, _ := json.Marshal(map[string]any{"sub": sub, "scope": strings.Join(scopes, " ")})
	return h + "." + base64.RawURLEncoding.EncodeToString(b) + ".sig"
}

// subOfHeader reads the sub of a fake JWT bearer.
func subOfHeader(h string) string {
	parts := strings.Split(strings.TrimPrefix(h, "Bearer "), ".")
	if len(parts) != 3 {
		return ""
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var c struct {
		Sub string `json:"sub"`
	}
	_ = json.Unmarshal(b, &c)
	return c.Sub
}

// tokens hands out fake JWTs of sub; the audiences asked are recorded.
type tokens struct {
	sub  string
	fail error
	mu   sync.Mutex
	auds []string
}

func (t *tokens) Token(_ context.Context, base string, scopes ...string) (string, error) {
	if t.fail != nil {
		return "", t.fail
	}
	t.mu.Lock()
	t.auds = append(t.auds, base)
	t.mu.Unlock()
	return fakeJWT(t.sub, scopes...), nil
}

// testGuard admits every request as the sub of its bearer.
func testGuard(httpx.Access) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sub := subOfHeader(r.Header.Get("Authorization"))
			if sub == "" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			ctx := auth.WithPrincipal(r.Context(), auth.Principal{Claims: coreauth.Claims{Subject: sub}})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// rig is the writer against the fake DSS, a fake peer, an in-memory
// store and a fake intent service, with our F3548 endpoints served so the
// peer can read our intents' details.
type rig struct {
	t        *testing.T
	dss      *fakedss.DSS
	peer     *peeruss.Fake
	st       *memStore
	in       *fakeIntents
	c        *Client
	w        *Writer
	srv      *Server
	us       *httptest.Server
	counters *core.Counters
	exlog    *ExchangeLog
}

func newRig(t *testing.T) *rig {
	t.Helper()
	g := &rig{t: t, dss: fakedss.New(), st: newMemStore(), counters: &core.Counters{}}
	t.Cleanup(g.dss.Close)
	g.in = newFakeIntents(g.st)
	g.exlog = NewExchangeLog(g.st, g.counters, nil)
	g.c = &Client{DSSBaseURL: g.dss.URL(), Tokens: &tokens{sub: ourManager}, Backoff: time.Millisecond,
		HTTP: &http.Client{Timeout: 5 * time.Second, Transport: &Transport{Log: g.exlog}}}
	g.srv = &Server{Intents: g.in, Store: g.st, Manager: ourManager, Client: g.c, Counters: g.counters}
	mux := http.NewServeMux()
	if err := stdapi.MountF3548(mux, g.srv, stdapi.Options{Guard: testGuard}); err != nil {
		t.Fatal(err)
	}
	g.us = httptest.NewServer(g.exlog.Middleware(mux))
	t.Cleanup(g.us.Close)
	g.srv.USSBaseURL = g.us.URL
	g.peer = peeruss.New(peerManager, g.dss.URL(), func(string, f3548.Scope) string { return fakeJWT(peerManager) })
	t.Cleanup(g.peer.Close)
	g.w = &Writer{Client: g.c, Store: g.st, Intents: g.in, USSBaseURL: g.us.URL, Manager: ourManager, Counters: g.counters,
		Every: 10 * time.Millisecond}
	return g
}

// volume is a 300 m circle at (lat, lng), 100 to 200 m W84, from 5 to
// 35 minutes from now.
func volume(lat, lng float64) f3548.Volume4D {
	now := time.Now().UTC().Truncate(time.Second)
	lo := f3548.Altitude{Reference: f3548.W84, Units: f3548.AltitudeUnitsM, Value: 100}
	hi := f3548.Altitude{Reference: f3548.W84, Units: f3548.AltitudeUnitsM, Value: 200}
	return f3548.Volume4D{
		Volume: f3548.Volume3D{OutlineCircle: &f3548.Circle{Center: &f3548.LatLngPoint{Lat: lat, Lng: lng},
			Radius: &f3548.Radius{Units: f3548.RadiusUnitsM, Value: 300}}, AltitudeLower: &lo, AltitudeUpper: &hi},
		TimeStart: &f3548.Time{Format: f3548.RFC3339, Value: now.Add(5 * time.Minute)},
		TimeEnd:   &f3548.Time{Format: f3548.RFC3339, Value: now.Add(35 * time.Minute)},
	}
}

// pending is a pending_dss intent inside U-space airspace.
func pending(id string, vols ...f3548.Volume4D) *intent.Record {
	return &intent.Record{ID: id, Version: 1, LocalState: intent.StatePendingDSS, Request: intent.Request{Volumes: vols},
		Decision:  intent.Decision{IntentID: id, Version: 1, Decision: intent.DecisionPendingDSS, State: intent.StatePendingDSS, InUSpaceAirspace: true},
		TimeStart: vols[0].TimeStart.Value, TimeEnd: vols[0].TimeEnd.Value}
}

// within waits for cond, polling, at most d (never a sleep standing in
// for a condition).
func within(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %s", d)
		}
		<-tick.C
	}
}

func (g *rig) count(name string) uint64 { return g.counters.Get(name) }
