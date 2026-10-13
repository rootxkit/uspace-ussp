package cis

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"

	"github.com/rootxkit/uspace-ussp/internal/testfakes/signer"
)

const ourHost = "ussp.test"

type receiverRig struct {
	t        *testing.T
	cisp     *signer.Signer
	ansp     *signer.Signer
	other    *signer.Signer
	store    *memStore
	rc       *Receiver
	mu       sync.Mutex
	triggers []Hint
	sets     []Dataset
}

func newReceiverRig(t *testing.T) *receiverRig {
	t.Helper()
	g := &receiverRig{t: t, store: newMemStore()}
	var err error
	if g.cisp, err = signer.New("https://cisp.test", "cisp-1"); err != nil {
		t.Fatal(err)
	}
	if g.ansp, err = signer.New("https://ansp.test", "ansp-1"); err != nil {
		t.Fatal(err)
	}
	// A key of neither: it claims to be the CISP.
	if g.other, err = signer.New("https://cisp.test", "cisp-1"); err != nil {
		t.Fatal(err)
	}
	v, err := coreauth.NewCompactVerifier(t.Context(), coreauth.CompactConfig{
		Issuers:   map[string]coreauth.IssuerConfig{g.cisp.Issuer: g.cisp.IssuerConfig(), g.ansp.Issuer: g.ansp.IssuerConfig()},
		Audiences: []string{ourHost, "ussp.lab"},
	})
	if err != nil {
		t.Fatal(err)
	}
	g.rc = NewReceiver(ReceiverConfig{
		Verifier: v,
		Senders: map[string]Sender{
			g.cisp.Issuer: {BaseHost: "cisp.test"},
			g.ansp.Issuer: {ANSP: true, BaseHost: "ansp.test"},
		},
		Store: g.store,
		Trigger: func(d Dataset, h Hint) {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.sets = append(g.sets, d)
			g.triggers = append(g.triggers, h)
		},
	})
	return g
}

func changeBody(dataset, reason string, version int64, pullURL string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"schema": "cis/change/v1", "msg_id": "42", "producer": "cisp/deliver-a", "dataset": dataset, "version": version,
		"etag": `"` + dataset + `:5"`, "feature_ids": []string{"TZP001"}, "removed_ids": []string{}, "reason": reason,
		"at": "2026-10-02T09:00:00Z", "pull_url": pullURL,
	})
	return b
}

func (g *receiverRig) post(token, contentType string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, NotificationsPath, strings.NewReader(token))
	r.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	g.rc.ServeHTTP(w, r)
	return w
}

func (g *receiverRig) send(s *signer.Signer, aud, jti string, body json.RawMessage) *httptest.ResponseRecorder {
	g.t.Helper()
	tok, err := s.SignFor(aud, "sub-1", jti, body, time.Now())
	if err != nil {
		g.t.Fatal(err)
	}
	return g.post(tok, "application/jose")
}

func (g *receiverRig) triggered() []Hint {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]Hint(nil), g.triggers...)
}

func (g *receiverRig) count(name string) uint64 { return g.rc.cfg.Counters.Get(name) }

const cispPull = "https://cisp.test/v1/zones?since_version=4"

func TestReceiverCISPNotificationPulls(t *testing.T) {
	g := newReceiverRig(t)
	w := g.send(g.cisp, ourHost, "d1", changeBody("zones", "publication", 5, cispPull))
	if w.Code != http.StatusNoContent {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	h := g.triggered()
	if len(h) != 1 || h[0].Version != 5 || h[0].PullURL != cispPull || h[0].Issuer != g.cisp.Issuer || g.sets[0] != Zones {
		t.Fatalf("trigger: %+v", h)
	}
	if len(g.store.notes) != 1 || g.store.notes[0].JTI != "d1" || g.store.notes[0].Subscription != "sub-1" || g.store.notes[0].Reason != "publication" {
		t.Fatalf("log: %+v", g.store.notes)
	}
	if g.count(CounterWebhooks) != 1 || g.count(CounterBadSignature) != 0 {
		t.Fatalf("counters: %v", g.rc.cfg.Counters.Snapshot())
	}
	// The lab alias is one of this receiver's audiences too.
	if w := g.send(g.cisp, "ussp.lab", "d2", changeBody("restrictions", "restriction_activated", 9, "https://cisp.test/v1/restrictions?since_version=8")); w.Code != http.StatusNoContent {
		t.Fatalf("lab alias: %d", w.Code)
	}
	if len(g.triggered()) != 2 {
		t.Fatal("restriction_activated did not pull")
	}
}

// M5, H-2: the ANSP's degraded direct path is accepted and its
// restriction is pulled from its own pull_url, by its ansp_version (the
// record's version member): the CISP is not asked, and a version below
// the CISP's never reads as a replay. Without a direct trigger (no
// cache) the CISP is read, with no version to skip on. A full queue is
// 503 and records nothing, so the ANSP's retry is taken.
func TestReceiverANSPNotificationPulls(t *testing.T) {
	g := newReceiverRig(t)
	var direct []DirectHint
	room := true
	g.rc.cfg.TriggerDirect = func(h DirectHint) bool {
		if room {
			direct = append(direct, h)
		}
		return room
	}
	w := g.send(g.ansp, ourHost, "a1", changeBody("restrictions", "restriction_activated", 9, "https://ansp.test/v1/restrictions/r1/direct"))
	if w.Code != http.StatusNoContent {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if len(g.triggered()) != 0 || len(direct) != 1 || direct[0].AnspVersion != 9 || direct[0].RestrictionID != "sub-1" ||
		direct[0].PullURL != "https://ansp.test/v1/restrictions/r1/direct" || direct[0].FeatureIDs[0] != "TZP001" || direct[0].Issuer != g.ansp.Issuer {
		t.Fatalf("direct %+v, CISP %+v", direct, g.triggered())
	}
	if g.count(CounterANSPDirect) != 1 || g.count(CounterPullURLMismatch) != 0 {
		t.Fatalf("counters: %v", g.rc.cfg.Counters.Snapshot())
	}
	room = false
	if w := g.send(g.ansp, ourHost, "a2", changeBody("restrictions", "restriction_ended", 10, "https://ansp.test/v1/restrictions/r1/direct")); w.Code != http.StatusServiceUnavailable || g.store.jtis[g.ansp.Issuer+" a2"] {
		t.Fatalf("full: %d", w.Code)
	}
	room = true
	if w := g.send(g.ansp, ourHost, "a2", changeBody("restrictions", "restriction_ended", 10, "https://ansp.test/v1/restrictions/r1/direct")); w.Code != http.StatusNoContent || len(direct) != 2 {
		t.Fatalf("the retry: %d", w.Code)
	}
	g.rc.cfg.TriggerDirect = nil
	if w := g.send(g.ansp, ourHost, "a3", changeBody("restrictions", "restriction_activated", 9, "https://ansp.test/v1/restrictions/r1/direct")); w.Code != http.StatusNoContent {
		t.Fatalf("status %d", w.Code)
	}
	if h := g.triggered(); len(h) != 1 || h[0].Version != 0 || h[0].PullURL != "" {
		t.Fatalf("without a direct trigger: %+v", h)
	}
}

// E-01 pair of the two above: a key of neither is 401, counted, and
// pulls nothing.
func TestReceiverRefusesAnotherKey(t *testing.T) {
	g := newReceiverRig(t)
	w := g.send(g.other, ourHost, "x1", changeBody("zones", "publication", 5, cispPull))
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "cis_webhook_bad_signature") {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if g.count(CounterBadSignature) != 1 || len(g.triggered()) != 0 || len(g.store.jtis) != 0 {
		t.Fatalf("counted %d, triggers %d, jtis %d", g.count(CounterBadSignature), len(g.triggered()), len(g.store.jtis))
	}
	// Another audience (M19: aud is our host, never anything else).
	if w := g.send(g.cisp, "elsewhere.test", "x2", changeBody("zones", "publication", 5, cispPull)); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong aud: %d", w.Code)
	}
	if g.count(CounterBadSignature) != 2 {
		t.Fatal("wrong aud not counted")
	}
	// Not a JWS at all.
	if w := g.post("not.a.jws", "application/jose"); w.Code != http.StatusUnauthorized {
		t.Fatalf("garbage: %d", w.Code)
	}
}

// M16: subscription_test, republished and an unknown reason are 204
// without a pull; publication pulls once (E-01 pair).
func TestReceiverAckOnlyReasons(t *testing.T) {
	g := newReceiverRig(t)
	for i, reason := range []string{"subscription_test", "republished", "zones_rearranged"} {
		w := g.send(g.cisp, ourHost, "r"+string(rune('a'+i)), changeBody("zones", reason, 0, cispPull))
		if w.Code != http.StatusNoContent {
			t.Fatalf("%s: %d", reason, w.Code)
		}
	}
	if len(g.triggered()) != 0 || len(g.store.notes) != 0 {
		t.Fatalf("an ack-only reason pulled: %+v", g.triggered())
	}
	if g.count(CounterWebhookAckOnly) != 3 || g.count(CounterWebhookUnknown) != 1 {
		t.Fatalf("counters: %v", g.rc.cfg.Counters.Snapshot())
	}
	if w := g.send(g.cisp, ourHost, "rp", changeBody("zones", "publication", 5, cispPull)); w.Code != http.StatusNoContent || len(g.triggered()) != 1 {
		t.Fatalf("publication: %d, %d triggers", w.Code, len(g.triggered()))
	}
}

// A replayed delivery (same jti) is 204, counted, and not acted on.
func TestReceiverReplay(t *testing.T) {
	g := newReceiverRig(t)
	tok, err := g.cisp.SignFor(ourHost, "sub-1", "same", changeBody("zones", "publication", 5, cispPull), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if w := g.post(tok, "application/jose"); w.Code != http.StatusNoContent {
			t.Fatalf("status %d", w.Code)
		}
	}
	if len(g.triggered()) != 1 || g.count(CounterWebhookReplayed) != 1 || g.count(CounterWebhooks) != 1 {
		t.Fatalf("triggers %d, counters %v", len(g.triggered()), g.rc.cfg.Counters.Snapshot())
	}
}

// The SSRF guard: a pull_url on another host is not followed, and the
// mismatch is counted; the dataset is still pulled (from the base URL).
func TestReceiverPullURLOnAnotherHost(t *testing.T) {
	g := newReceiverRig(t)
	for i, u := range []string{"https://evil.test/v1/zones?since_version=4", "http://169.254.169.254/latest", "not a url"} {
		w := g.send(g.cisp, ourHost, "m"+string(rune('a'+i)), changeBody("zones", "publication", int64(5+i), u))
		if w.Code != http.StatusNoContent {
			t.Fatalf("%s: %d", u, w.Code)
		}
	}
	h := g.triggered()
	if len(h) != 3 {
		t.Fatalf("triggers %d", len(h))
	}
	for _, x := range h {
		if x.PullURL != "" {
			t.Fatalf("followed %q", x.PullURL)
		}
	}
	if g.count(CounterPullURLMismatch) != 3 {
		t.Fatalf("mismatch counted %d", g.count(CounterPullURLMismatch))
	}
}

func TestReceiverRefusals(t *testing.T) {
	g := newReceiverRig(t)
	if w := g.post("x", "application/json"); w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("content type: %d", w.Code)
	}
	if w := g.post(strings.Repeat("a", MaxNotificationBytes+1), "application/jose"); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("too large: %d", w.Code)
	}
	for name, body := range map[string]json.RawMessage{
		"schema":  json.RawMessage(`{"schema":"cis/other/v1","dataset":"zones","version":1,"reason":"publication"}`),
		"dataset": json.RawMessage(`{"schema":"cis/change/v1","dataset":"weather","version":1,"reason":"publication"}`),
		"version": json.RawMessage(`{"schema":"cis/change/v1","dataset":"zones","version":-1,"reason":"publication"}`),
		"reason":  json.RawMessage(`{"schema":"cis/change/v1","dataset":"zones","version":1,"reason":""}`),
		"types":   json.RawMessage(`{"schema":"cis/change/v1","dataset":"zones","version":"one","reason":"publication"}`),
	} {
		if w := g.send(g.cisp, ourHost, "bad-"+name, body); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	if g.count(CounterWebhookMalformed) != 7 || len(g.store.jtis) != 0 {
		t.Fatalf("malformed %d, jtis %d", g.count(CounterWebhookMalformed), len(g.store.jtis))
	}
	// The replay store full: 503, counted, nothing pulled.
	g.store.full = true
	if w := g.send(g.cisp, ourHost, "f1", changeBody("zones", "publication", 5, cispPull)); w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
		t.Fatalf("full: %d", w.Code)
	}
	if g.count(CounterWebhookJTIFull) != 1 {
		t.Fatal("full not counted")
	}
	// The database down: 503 (the CISP retries), nothing pulled.
	g.store.full = false
	g.store.setDown(true)
	if w := g.send(g.cisp, ourHost, "s1", changeBody("zones", "publication", 5, cispPull)); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("store down: %d", w.Code)
	}
	if len(g.triggered()) != 0 || g.count(CounterWebhookStoreFailed) != 1 {
		t.Fatalf("store down: %d triggers", len(g.triggered()))
	}
}

// A verified issuer missing from Senders (a configuration gap) is
// refused, never trusted.
func TestReceiverSenderGap(t *testing.T) {
	g := newReceiverRig(t)
	delete(g.rc.cfg.Senders, g.ansp.Issuer)
	if w := g.send(g.ansp, ourHost, "g1", changeBody("restrictions", "restriction_activated", 9, "")); w.Code != http.StatusUnauthorized {
		t.Fatalf("status %d", w.Code)
	}
}

// The log write failing after the replay guard took the delivery still
// pulls: the pull matters more than the log.
func TestReceiverLogFailureStillPulls(t *testing.T) {
	g := newReceiverRig(t)
	g.rc.cfg.Store = logFails{g.store}
	if w := g.send(g.cisp, ourHost, "l1", changeBody("zones", "publication", 5, cispPull)); w.Code != http.StatusNoContent {
		t.Fatalf("status %d", w.Code)
	}
	if len(g.triggered()) != 1 || g.count(CounterWebhookStoreFailed) != 1 {
		t.Fatal("a failed log stopped the pull")
	}
}

type logFails struct{ *memStore }

func (logFails) InsertNotification(context.Context, Notification) error { return errDown }
