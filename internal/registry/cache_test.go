package registry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/authority"
)

type fixture struct {
	fake  *authority.Fake
	clock *clock
	store *memStore
	proj  *MemoryProjector
	audit *auditRecorder
	cache *Cache
	feed  *Feed
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{fake: authority.New(), clock: newClock(), proj: NewMemoryProjector(), audit: &auditRecorder{}}
	t.Cleanup(f.fake.Close)
	f.store = newMemStore(f.clock)
	client, err := NewClient(ClientConfig{BaseURL: f.fake.URL(), Tokens: authority.Tokens{}})
	if err != nil {
		t.Fatal(err)
	}
	counters := &core.Counters{}
	f.cache = NewCache(CacheConfig{Client: client, Store: f.store, Projector: f.proj, Audit: f.audit, Counters: counters, Now: f.clock.Now})
	f.feed = NewFeed(FeedConfig{Client: client, Store: f.store, Projector: f.proj, Counters: counters, Now: f.clock.Now})
	return f
}

func (f *fixture) uas(t *testing.T, sn string) *Answer {
	t.Helper()
	rs, err := f.cache.Validate(t.Context(), []Query{{Serial: sn}}, PurposeIdentification)
	if err != nil {
		t.Fatal(err)
	}
	return rs[0].UAS
}

func (f *fixture) count(name string) uint64 { return f.cache.Counters().Get(name) }

func ageOf(t *testing.T, a *Answer) float64 {
	t.Helper()
	if a.CacheAgeS == nil {
		t.Fatalf("no cache_age_s on %+v", a)
	}
	return *a.CacheAgeS
}

// Done-when: a positive answer is served at 23 h and refetched at 25 h
// (policy's 24 h), with its cache_age_s.
func TestPositiveAnswerServedAt23hRefetchedAt25h(t *testing.T) {
	f := newFixture(t)
	f.fake.SetUAS(snA, "active", "C1", "")
	if a := f.uas(t, snA); a.Status != StatusValid || ageOf(t, a) != 0 || a.Reason != "" {
		t.Fatalf("first %+v", a)
	}
	f.clock.Advance(23 * time.Hour)
	a := f.uas(t, snA)
	if a.Status != StatusValid || ageOf(t, a) != 23*3600 || f.fake.Requests("validate") != 1 {
		t.Fatalf("at 23 h: %+v after %d requests", a, f.fake.Requests("validate"))
	}
	f.clock.Advance(2 * time.Hour)
	if a := f.uas(t, snA); ageOf(t, a) != 0 || f.fake.Requests("validate") != 2 {
		t.Fatalf("at 25 h: %+v after %d requests", a, f.fake.Requests("validate"))
	}
	if f.count(CounterCacheHit) != 1 || f.count(CounterCacheMiss) != 2 || f.count(CounterFetched) != 2 {
		t.Fatalf("hit %d miss %d fetched %d", f.count(CounterCacheHit), f.count(CounterCacheMiss), f.count(CounterFetched))
	}
}

// Done-when: a negative answer (unknown) is served at 4 min and
// refetched at 6 min (policy's 5 min).
func TestNegativeAnswerServedAt4minRefetchedAt6min(t *testing.T) {
	f := newFixture(t)
	if a := f.uas(t, "TEST-NOBODY"); a.Status != StatusUnknown || a.Reason != "" {
		t.Fatalf("the registry's unknown has no reason: %+v", a)
	}
	f.clock.Advance(4 * time.Minute)
	if a := f.uas(t, "TEST-NOBODY"); ageOf(t, a) != 240 || f.fake.Requests("validate") != 1 {
		t.Fatalf("at 4 min: %+v", a)
	}
	f.clock.Advance(2 * time.Minute)
	f.fake.SetUAS("TEST-NOBODY", "active", "", "")
	if a := f.uas(t, "TEST-NOBODY"); a.Status != StatusValid || f.fake.Requests("validate") != 2 {
		t.Fatalf("at 6 min: %+v", a)
	}
}

// The TTLs are the policy's, read on every call (INV-03).
func TestTTLFollowsThePolicy(t *testing.T) {
	f := newFixture(t)
	v := policy.Defaults()
	v.RegistryPositiveTTLS = 60
	f.cache.cfg.TTL = func() TTL { return TTLFromPolicy(v) }
	f.fake.SetUAS(snA, "active", "", "")
	f.uas(t, snA)
	f.clock.Advance(61 * time.Second)
	f.uas(t, snA)
	if f.fake.Requests("validate") != 2 {
		t.Fatal("a 60 s policy TTL did not refetch at 61 s")
	}
}

// Done-when (SC-17 step 2 in unit form): a status change at the
// authority reaches the feed, which invalidates the key in the table and
// the projection; the next lookup refetches and sees it. Without the
// poll the cached answer is still served (E-01 pair).
func TestChangeFeedInvalidatesAndTheNextLookupRefetches(t *testing.T) {
	f := newFixture(t)
	f.fake.SetUAS(snA, "active", "", "")
	if err := f.feed.Poll(t.Context()); err != nil { // consume the registration
		t.Fatal(err)
	}
	f.uas(t, snA)
	if es, _ := f.proj.Snapshot(); len(es) != 1 {
		t.Fatalf("projection %v", es)
	}
	f.fake.SetUAS(snA, "suspended", "", "")
	if a := f.uas(t, snA); a.Status != StatusValid {
		t.Fatalf("before the poll the cached answer stands: %+v", a)
	}
	if err := f.feed.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.store.row(Key{EntityUAS, snA}); ok {
		t.Fatal("the row survived the change")
	}
	if es, _ := f.proj.Snapshot(); len(es) != 0 {
		t.Fatalf("the projection kept %v", es)
	}
	if a := f.uas(t, snA); a.Status != StatusSuspended || ageOf(t, a) != 0 || f.fake.Requests("validate") != 2 {
		t.Fatalf("after the poll: %+v", a)
	}
	if f.count(CounterFeedInvalidated) != 1 || f.count(CounterFeedPolled) != 2 {
		t.Fatalf("invalidated %d polled %d", f.count(CounterFeedInvalidated), f.count(CounterFeedPolled))
	}
}

// Done-when (E-02 both ways): with the authority down a cached answer is
// served with its age; an uncached key is unknown with
// registry_unavailable, counted; back up, the key is answered fresh.
func TestAuthorityDownAndBackUp(t *testing.T) {
	f := newFixture(t)
	f.fake.SetUAS(snA, "active", "", "")
	f.fake.SetUAS("TEST-SN-B", "active", "", "")
	f.uas(t, snA)
	f.clock.Advance(time.Hour)
	f.fake.Down()
	if a := f.uas(t, snA); a.Status != StatusValid || ageOf(t, a) != 3600 {
		t.Fatalf("cached while down: %+v", a)
	}
	b := f.uas(t, "TEST-SN-B")
	if b.Status != StatusUnknown || b.Reason != ReasonRegistryUnavailable || b.CacheAgeS != nil || f.count(CounterUnavailable) != 1 {
		t.Fatalf("uncached while down: %+v, counted %d", b, f.count(CounterUnavailable))
	}
	if _, ok := f.store.row(Key{EntityUAS, "TEST-SN-B"}); ok {
		t.Fatal("an unavailable answer was cached")
	}
	f.fake.Up()
	if b := f.uas(t, "TEST-SN-B"); b.Status != StatusValid || b.Reason != "" || ageOf(t, b) != 0 {
		t.Fatalf("back up: %+v", b)
	}
	if f.count(CounterUnavailable) != 1 {
		t.Fatal("a fresh answer was counted unavailable")
	}
}

// An expired cached answer is not served while the authority is down:
// unknown, never the stale valid (spec 02 F8: up to 24 h).
func TestExpiredAnswerIsNotServedWhileDown(t *testing.T) {
	f := newFixture(t)
	f.fake.SetUAS(snA, "active", "", "")
	f.uas(t, snA)
	f.clock.Advance(25 * time.Hour)
	f.fake.Down()
	if a := f.uas(t, snA); a.Status != StatusUnknown || a.Reason != ReasonRegistryUnavailable {
		t.Fatalf("a 25 h old valid answer was served: %+v", a)
	}
}

// Done-when: a lookup answer carrying a name is refused and counted
// registry_pii_refused, and nothing of it is stored or projected; the
// same answer without the name is stored (E-01).
func TestAnswerWithANameIsRefusedAndNotStored(t *testing.T) {
	f := newFixture(t)
	f.fake.SetOperator(opNumber, "active", nil)
	f.fake.Misbehave("name", "Test Person")
	rs, err := f.cache.Validate(t.Context(), []Query{{Operator: opNumber}}, PurposeAuthorisation)
	if err != nil {
		t.Fatal(err)
	}
	if a := rs[0].Operator; a.Status != StatusUnknown || a.Reason != ReasonAnswerRefused {
		t.Fatalf("answer %+v", a)
	}
	if f.count(CounterPIIRefused) != 1 || f.count(CounterUnavailable) != 0 {
		t.Fatalf("pii %d unavailable %d", f.count(CounterPIIRefused), f.count(CounterUnavailable))
	}
	if _, ok := f.store.row(Key{EntityOperator, opNumber}); ok {
		t.Fatal("a refused answer was stored")
	}
	if es, _ := f.proj.Snapshot(); len(es) != 0 {
		t.Fatal("a refused answer was projected")
	}
	f.fake.Misbehave("name", nil)
	rs, _ = f.cache.Validate(t.Context(), []Query{{Operator: opNumber}}, PurposeAuthorisation)
	if rs[0].Operator.Status != StatusValid {
		t.Fatalf("the clean answer: %+v", rs[0].Operator)
	}
	if _, ok := f.store.row(Key{EntityOperator, opNumber}); !ok {
		t.Fatal("the clean answer was not stored")
	}
}

// An answer whose fetch raced the feed invalidating its key is answered
// (it is the authority's) but not cached, so the invalidation stands;
// an answer that raced nothing is cached (E-01).
func TestWriteRacingAnInvalidationIsSkipped(t *testing.T) {
	f := newFixture(t)
	f.fake.SetUAS(snA, "active", "", "")
	f.store.beforeSave = func() {
		_, _ = f.store.Invalidate(context.Background(), []Invalidation{{Entity: EntityUAS, KeyFold: snA, Seq: 7}}, 7, "", time.Hour,
			func(context.Context, []Key) error { return nil })
	}
	if a := f.uas(t, snA); a.Status != StatusValid {
		t.Fatalf("answer %+v", a)
	}
	if _, ok := f.store.row(Key{EntityUAS, snA}); ok || f.count(CounterWriteSkipped) != 1 {
		t.Fatalf("an answer that raced the invalidation was cached (skipped %d)", f.count(CounterWriteSkipped))
	}
	f.store.beforeSave = nil
	f.uas(t, snA)
	if _, ok := f.store.row(Key{EntityUAS, snA}); !ok || f.count(CounterWriteSkipped) != 1 {
		t.Fatal("an answer fetched after the invalidation was not cached")
	}
}

// The table and the projection are written in one step (G-08): a
// projection that refuses rolls the row back; the answer stands.
func TestProjectionFailureLeavesNothingCached(t *testing.T) {
	f := newFixture(t)
	f.fake.SetUAS(snA, "active", "", "")
	f.proj.Fail = errors.New("bucket down")
	if a := f.uas(t, snA); a.Status != StatusValid {
		t.Fatalf("answer %+v", a)
	}
	if _, ok := f.store.row(Key{EntityUAS, snA}); ok || f.count(CounterCacheWriteFailed) != 1 {
		t.Fatal("the row was kept although the projection refused it")
	}
	f.proj.Fail = nil
	f.uas(t, snA)
	if _, ok := f.store.row(Key{EntityUAS, snA}); !ok {
		t.Fatal("not cached once the projection takes it")
	}
	if es, _ := f.proj.Snapshot(); len(es) != 1 || es[0].Key.Key != snA {
		t.Fatalf("projection %v", es)
	}
}

// A table that cannot be read: every key is asked, nothing written.
func TestUnreadableCacheAsksTheAuthority(t *testing.T) {
	f := newFixture(t)
	f.fake.SetUAS(snA, "active", "", "")
	f.uas(t, snA)
	f.store.failRead = errDown
	if a := f.uas(t, snA); a.Status != StatusValid || ageOf(t, a) != 0 || f.fake.Requests("validate") != 2 {
		t.Fatalf("answer %+v", a)
	}
	if f.count(CounterCacheReadFailed) != 1 {
		t.Fatal("not counted")
	}
}

func TestValidateRefusesWhatF8DoesNotDefine(t *testing.T) {
	f := newFixture(t)
	many := make([]Query, MaxQueries+1)
	for i := range many {
		many[i] = Query{Serial: fmt.Sprintf("TEST-%d", i)}
	}
	for name, tc := range map[string]struct {
		qs    []Query
		p     Purpose
		field string
	}{
		"purpose":      {[]Query{{Serial: snA}}, "billing", "purpose"},
		"no query":     {nil, PurposeAuthorisation, "items"},
		"no key":       {[]Query{{Serial: "  "}}, PurposeAuthorisation, "query"},
		"no key batch": {[]Query{{Serial: snA}, {}}, PurposeAuthorisation, "items[1]"},
		"too many":     {many, PurposeAuthorisation, "items"},
		"long serial":  {[]Query{{Serial: strings.Repeat("S", MaxSerialLen+1)}}, PurposeAuthorisation, "serial"},
		"long op":      {[]Query{{Operator: strings.Repeat("O", MaxOperatorLen+1)}}, PurposeAuthorisation, "operator"},
		"long pilot":   {[]Query{{Pilot: strings.Repeat("P", MaxPilotLen+1)}}, PurposeAuthorisation, "pilot"},
		"control":      {[]Query{{Serial: "TEST\x00A"}}, PurposeAuthorisation, "serial"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := f.cache.Validate(t.Context(), tc.qs, tc.p)
			var fe *core.FieldError
			if !errors.As(err, &fe) || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("got %v, want a field error on %s", err, tc.field)
			}
		})
	}
	if f.fake.Requests("validate")+f.fake.Requests("validate_batch") != 0 {
		t.Fatal("a refused lookup reached the authority")
	}
	if _, err := f.cache.Validate(t.Context(), many[:MaxQueries], PurposeAuthorisation); err != nil {
		t.Fatalf("%d queries refused: %v", MaxQueries, err)
	}
}

// Only the keys without a fresh answer are asked; several queries are
// one batch; a key repeated is asked once; the secret part of an
// operator number is answered as its public part.
func TestOnlyTheMissingKeysAreAsked(t *testing.T) {
	f := newFixture(t)
	f.fake.SetOperator(opNumber, "active", nil)
	f.fake.SetUAS(snA, "revoked", "", "")
	f.fake.SetPilot(pilotA, "suspended")
	rs, err := f.cache.Validate(t.Context(), []Query{{Operator: opSecret}}, PurposeAuthorisation)
	if err != nil || rs[0].Operator.Key != opNumber || rs[0].Operator.Status != StatusValid {
		t.Fatalf("%+v %v", rs, err)
	}
	f.clock.Advance(time.Minute)
	rs, err = f.cache.Validate(t.Context(), []Query{{Operator: opNumber, Serial: snA}, {Serial: snA, Pilot: pilotA}, {Operator: "geotestop0001"}}, PurposeAuthorisation)
	if err != nil {
		t.Fatal(err)
	}
	if ageOf(t, rs[0].Operator) != 60 || ageOf(t, rs[0].UAS) != 0 || rs[1].UAS.Status != StatusRevoked || rs[1].Pilot.Status != StatusSuspended ||
		rs[2].Operator.Status != StatusValid || ageOf(t, rs[2].Operator) != 60 {
		t.Fatalf("%+v %+v %+v", rs[0], rs[1], rs[2])
	}
	if f.fake.Requests("validate") != 1 || f.fake.Requests("validate_batch") != 1 {
		t.Fatalf("requests %d/%d", f.fake.Requests("validate"), f.fake.Requests("validate_batch"))
	}
	if f.count(CounterCacheMiss) != 3 || f.count(CounterCacheHit) != 1 {
		t.Fatalf("miss %d hit %d", f.count(CounterCacheMiss), f.count(CounterCacheHit))
	}
}

func TestWithoutAnAuthorityEveryUncachedKeyIsUnavailable(t *testing.T) {
	c := NewCache(CacheConfig{})
	rs, err := c.Validate(t.Context(), []Query{{Operator: opNumber, Pilot: pilotA}}, PurposeAuthorisation)
	if err != nil {
		t.Fatal(err)
	}
	if rs[0].Operator.Status != StatusUnknown || rs[0].Operator.Reason != ReasonRegistryUnavailable || rs[0].Pilot.Reason != ReasonRegistryUnavailable {
		t.Fatalf("%+v", rs[0])
	}
	if c.Counters().Get(CounterUnavailable) != 2 {
		t.Fatal("not counted")
	}
	if st, detail := ReadinessProbe(c, nil)(t.Context()); st != obs.StateDown || !strings.Contains(detail, "USSP_AUTHORITY_BASE_URL") {
		t.Fatalf("%s %s", st, detail)
	}
	client, _ := NewClient(ClientConfig{BaseURL: "https://authority.test"})
	c = NewCache(CacheConfig{Client: client})
	if st, detail := ReadinessProbe(c, nil)(t.Context()); st != obs.StateDown || !strings.Contains(detail, "token") {
		t.Fatalf("%s %s", st, detail)
	}
}

// No answer leaves without its events row: an audit that fails withholds
// the answer (503), no auditor refuses, and a recorded lookup names the
// public parts only, with how many were unknown.
func TestValidateAuditedRecordsOrWithholds(t *testing.T) {
	f := newFixture(t)
	f.fake.SetOperator(opNumber, "active", nil)
	rs, err := f.cache.ValidateAudited(t.Context(), "client", "client-1", []Query{{Operator: opSecret, Serial: "TEST-NOBODY"}}, PurposeAuthorisation)
	if err != nil || rs[0].Operator.Status != StatusValid {
		t.Fatalf("%+v %v", rs, err)
	}
	if len(f.audit.got) != 1 {
		t.Fatal("not recorded")
	}
	got := f.audit.got[0]
	if got.ActorID != "client-1" || got.Purpose != PurposeAuthorisation || got.Unknown != 1 || got.Operators[0] != opNumber || got.Serials[0] != "TEST-NOBODY" || len(got.Pilots) != 0 {
		t.Fatalf("recorded %+v", got)
	}
	f.audit.fail = errDown
	_, err = f.cache.ValidateAudited(t.Context(), "client", "client-1", []Query{{Operator: opNumber}}, PurposeAuthorisation)
	var ae *AuditError
	if !errors.As(err, &ae) || ae.HTTPStatus() != 503 || ae.ProblemSlug() != "audit_unavailable" || ae.ProblemDetail() == "" || !errors.Is(err, errDown) {
		t.Fatalf("got %v", err)
	}
	f.cache.cfg.Audit = nil
	if _, err := f.cache.ValidateAudited(t.Context(), "client", "c", []Query{{Operator: opNumber}}, PurposeAuthorisation); !errors.As(err, &ae) {
		t.Fatalf("no auditor: %v", err)
	}
	if _, err := f.cache.ValidateAudited(t.Context(), "client", "c", nil, PurposeAuthorisation); err == nil || errors.As(err, &ae) {
		t.Fatalf("an undefined lookup: %v", err)
	}
}

// accounts' RegistryChecker: the operator's F8 status; unknown, never an
// error, for a registry that cannot answer.
func TestOperatorStatus(t *testing.T) {
	f := newFixture(t)
	f.fake.SetOperator(opNumber, "suspended", nil)
	if s, err := f.cache.OperatorStatus(t.Context(), opNumber); err != nil || s != "suspended" {
		t.Fatalf("%q %v", s, err)
	}
	f.fake.Down()
	if s, err := f.cache.OperatorStatus(t.Context(), "GEOTESTOP0009"); err != nil || s != "unknown" {
		t.Fatalf("%q %v", s, err)
	}
	if _, err := f.cache.OperatorStatus(t.Context(), ""); err == nil {
		t.Fatal("an empty number was looked up")
	}
}

// /readyz registry: unknown before the authority answered, up once it
// has (with last_success_age_s and the feed's cursor), degraded after a
// failed lookup and while the feed is not answering (E-02).
func TestReadinessProbe(t *testing.T) {
	f := newFixture(t)
	probe := ReadinessProbe(f.cache, f.feed)
	if st, detail := probe(t.Context()); st != obs.StateUnknown || !strings.Contains(detail, "last_success_age_s=none") {
		t.Fatalf("before: %s %s", st, detail)
	}
	f.fake.SetUAS(snA, "active", "", "")
	if err := f.feed.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.uas(t, snA)
	f.clock.Advance(10 * time.Second)
	st, detail := probe(t.Context())
	if st != obs.StateUp || !strings.Contains(detail, "last_success_age_s=10") || !strings.Contains(detail, "feed_cursor=1") || !strings.Contains(detail, "feed_age_s=10") {
		t.Fatalf("up: %s %s", st, detail)
	}
	f.fake.Down()
	f.uas(t, "TEST-SN-B")
	if st, detail := probe(t.Context()); st != obs.StateDegraded || !strings.Contains(detail, "last lookup failed") {
		t.Fatalf("lookup failed: %s %s", st, detail)
	}
	f.fake.Up()
	f.uas(t, "TEST-SN-B")
	if st, _ := probe(t.Context()); st != obs.StateUp {
		t.Fatalf("recovered: %s", st)
	}
	f.clock.Advance(2 * time.Minute) // beyond three feed periods
	if st, detail := probe(t.Context()); st != obs.StateDegraded || !strings.Contains(detail, "no answer within 90 s") {
		t.Fatalf("feed quiet: %s %s", st, detail)
	}
	f.fake.Down()
	if err := f.feed.Poll(t.Context()); err == nil {
		t.Fatal("a down feed polled")
	}
	if st, detail := probe(t.Context()); st != obs.StateDegraded || !strings.Contains(detail, "last poll failed") {
		t.Fatalf("feed failed: %s %s", st, detail)
	}
}

// A transport failure of a lookup names the operation and the
// authority's host, never the request's query: neither the error the
// log line carries nor the readiness detail (public on /readyz) holds
// the serial, the pilot id or the operator asked (audit S2). The
// failure itself is still said (E-01): the detail says the lookup failed.
func TestLookupFailureCarriesNoKeys(t *testing.T) {
	var logs strings.Builder
	client, err := NewClient(ClientConfig{BaseURL: "http://127.0.0.1:1", Tokens: authority.Tokens{}, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	c := NewCache(CacheConfig{Client: client, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	q := []Query{{Serial: "TEST-SN-SECRET", Pilot: "GEO-PILOT-SECRET", Operator: "GEOOPSECRET01"}}
	if _, err := c.Validate(t.Context(), q, PurposeIdentification); err != nil {
		t.Fatal(err)
	}
	_, detail := ReadinessProbe(c, nil)(t.Context())
	if !strings.Contains(detail, "last lookup failed") || !strings.Contains(detail, "127.0.0.1") {
		t.Fatalf("the failure is not said: %q", detail)
	}
	for _, where := range []string{detail, logs.String()} {
		for _, k := range []string{"TEST-SN-SECRET", "GEO-PILOT-SECRET", "GEOOPSECRET", "serial=", "pilot="} {
			if strings.Contains(where, k) {
				t.Fatalf("%q carries %s", where, k)
			}
		}
	}
	if _, err := client.Validate(t.Context(), PurposeIdentification, []normalised{{serial: "TEST-SN-SECRET", pilot: "GEO-PILOT-SECRET"}}); err == nil ||
		strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("client error: %v", err)
	}
}

// A lookup the caller gave up on (its context cancelled) is neither a
// failure of the authority nor a readiness event: /readyz stays as it
// was and no error is logged; it is counted as cancelled (audit N4). A
// lookup that fails while the caller waits still degrades (E-01,
// TestReadinessProbe).
func TestCallerCancelledLookupIsNotARegistryFailure(t *testing.T) {
	f := newFixture(t)
	f.fake.SetUAS(snA, "active", "", "")
	f.uas(t, snA) // the authority answered once
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := f.cache.Validate(ctx, []Query{{Serial: "TEST-SN-OTHER"}}, PurposeIdentification); err != nil {
		t.Fatal(err)
	}
	if _, detail := ReadinessProbe(f.cache, nil)(t.Context()); strings.Contains(detail, "last lookup failed") {
		t.Fatalf("a cancelled caller degraded readiness: %q", detail)
	}
	if f.count(CounterLookupCancelled) != 1 {
		t.Fatalf("not counted: %v", f.cache.Counters().Snapshot())
	}
}
