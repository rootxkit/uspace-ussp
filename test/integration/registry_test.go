//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/registry"
	"github.com/rootxkit/uspace-ussp/internal/registry/pgstore"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/authority"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/cisp"
)

// withRegistry adds a fake F8 registry as USSP_AUTHORITY_BASE_URL; it
// accepts the bearer the integration token service hands out.
func withRegistry(t *testing.T, vars map[string]string) *authority.Fake {
	t.Helper()
	fake := authority.New()
	t.Cleanup(fake.Close)
	fake.AcceptBearer(cisp.Token)
	vars["USSP_AUTHORITY_BASE_URL"] = fake.URL()
	return fake
}

// cleanRegistry empties the registry cache tables, before and after.
func cleanRegistry(t *testing.T) {
	t.Helper()
	ensureSchemas(t)
	clean := func() {
		if _, err := relOwner(t).Exec(context.Background(),
			"DELETE FROM registry_validity; DELETE FROM registry_invalidations; DELETE FROM registry_feed"); err != nil {
			t.Fatal(err)
		}
	}
	clean()
	t.Cleanup(clean)
}

func noProjection(context.Context, []registry.Entry) error { return nil }

func noDeletion(context.Context, []registry.Key) error { return nil }

// pgstore on PostgreSQL as ussp_app: ages on the database clock, the
// invalidation guard both ways, invalidation by fold key with the
// cursor (never backwards), the rollback of a refused projection, and
// the bound on a competency set (E-10).
func TestIntegrationRegistryStore(t *testing.T) {
	cleanRegistry(t)
	ctx := context.Background()
	st := pgstore.Store{S: appStore(t)}
	until := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	es := []registry.Entry{
		{Key: registry.Key{Entity: registry.EntityOperator, Key: "GEOTESTOP0001"}, KeyFold: "GEOTESTOP0001", Status: registry.StatusValid, ValidUntil: &until},
		{Key: registry.Key{Entity: registry.EntityUAS, Key: "test-sn-a"}, KeyFold: "TEST-SN-A", Status: registry.StatusSuspended, ClassLabel: "C1", MTOMBand: "under_900g"},
		{Key: registry.Key{Entity: registry.EntityUAS, Key: "TEST-SN-A"}, KeyFold: "TEST-SN-A", Status: registry.StatusValid},
		{Key: registry.Key{Entity: registry.EntityPilot, Key: "GEO-TEST-PILOT-1"}, KeyFold: "GEO-TEST-PILOT-1", Status: registry.StatusUnknown},
	}
	written, err := st.Save(ctx, es, 0, noProjection)
	if err != nil || len(written) != 4 || written[0].FetchedAt.IsZero() {
		t.Fatalf("save: %d %v", len(written), err)
	}
	keys := []registry.Key{es[0].Key, es[1].Key, es[3].Key, {Entity: registry.EntityUAS, Key: "TEST-NOBODY"}}
	got, since, err := st.Entries(ctx, keys)
	if err != nil || len(got) != 3 || since != 0 {
		t.Fatalf("entries %+v %d %v", got, since, err)
	}
	for _, c := range got {
		if c.AgeS < 0 || c.AgeS > 5 {
			t.Errorf("%v age %v", c.Key, c.AgeS)
		}
		if c.Key == es[1].Key && (c.ClassLabel != "C1" || c.Status != registry.StatusSuspended) {
			t.Errorf("uas %+v", c)
		}
		if c.Key == es[0].Key && (c.ValidUntil == nil || !c.ValidUntil.Equal(until)) {
			t.Errorf("operator %+v", c)
		}
		if c.Key == es[3].Key && (c.Competencies == nil || len(c.Competencies) != 0) {
			t.Errorf("pilot competencies %+v", c.Competencies)
		}
	}
	// The age is the database clock's: a row written 25 h ago is 25 h old.
	if _, err := relOwner(t).Exec(ctx, "UPDATE registry_validity SET fetched_at = now() - interval '25 hours' WHERE key = 'GEOTESTOP0001'"); err != nil {
		t.Fatal(err)
	}
	got, _, _ = st.Entries(ctx, keys[:1])
	if len(got) != 1 || got[0].AgeS < 25*3600 || got[0].AgeS > 25*3600+5 {
		t.Fatalf("aged %+v", got)
	}

	// The feed invalidates the fold key: both spellings go; the cursor
	// moves; the projection sees what was deleted.
	var projected []registry.Key
	deleted, err := st.Invalidate(ctx, []registry.Invalidation{{Entity: registry.EntityUAS, KeyFold: "TEST-SN-A", Seq: 5}}, 5, `"e5"`, time.Hour,
		func(_ context.Context, del []registry.Key) error { projected = del; return nil })
	if err != nil || len(deleted) != 2 || len(projected) != 2 {
		t.Fatalf("invalidate %v %v %v", deleted, projected, err)
	}
	cur, err := st.Cursor(ctx)
	if err != nil || cur.Since != 5 || cur.ETag != `"e5"` || cur.AgeS == nil || *cur.AgeS > 5 {
		t.Fatalf("cursor %+v %v", cur, err)
	}
	// A fetch that started under cursor 4 raced it and is not written;
	// one under cursor 5 is (E-01 pair).
	again := []registry.Entry{es[2]}
	if w, err := st.Save(ctx, again, 4, noProjection); err != nil || len(w) != 0 {
		t.Fatalf("a raced write: %v %v", w, err)
	}
	if w, err := st.Save(ctx, again, 5, noProjection); err != nil || len(w) != 1 {
		t.Fatalf("a write after the invalidation: %v %v", w, err)
	}
	// A refused projection rolls the invalidation and the cursor back.
	refused := errors.New("bucket down")
	if _, err := st.Invalidate(ctx, []registry.Invalidation{{Entity: registry.EntityUAS, KeyFold: "TEST-SN-A", Seq: 6}}, 6, "", time.Hour,
		func(context.Context, []registry.Key) error { return refused }); !errors.Is(err, refused) {
		t.Fatalf("got %v", err)
	}
	if cur, _ := st.Cursor(ctx); cur.Since != 5 {
		t.Fatalf("the cursor moved with a refused projection: %d", cur.Since)
	}
	if got, _, _ := st.Entries(ctx, []registry.Key{es[2].Key}); len(got) != 1 {
		t.Fatal("the row went with a refused projection")
	}
	if _, err := st.Save(ctx, []registry.Entry{es[1]}, 5, func(context.Context, []registry.Entry) error { return refused }); !errors.Is(err, refused) {
		t.Fatalf("got %v", err)
	}
	if got, _, _ := st.Entries(ctx, []registry.Key{es[1].Key}); len(got) != 0 {
		t.Fatal("a row was written with a refused projection")
	}
	// The cursor never moves backwards.
	if _, err := st.Invalidate(ctx, nil, 3, "", time.Hour, noDeletion); err != nil {
		t.Fatal(err)
	}
	if cur, _ := st.Cursor(ctx); cur.Since != 5 {
		t.Fatalf("the cursor moved back to %d", cur.Since)
	}
	all, err := st.All(ctx, 10)
	if err != nil || len(all) != 3 {
		t.Fatalf("all %d %v", len(all), err)
	}
	if one, _ := st.All(ctx, 1); len(one) != 1 {
		t.Fatal("All ignored its limit")
	}
	// E-10: a competency set beyond the bound is refused by the table.
	big := registry.Entry{Key: registry.Key{Entity: registry.EntityPilot, Key: "GEO-TEST-PILOT-2"}, KeyFold: "GEO-TEST-PILOT-2", Status: registry.StatusValid}
	for i := 0; i <= registry.MaxCompetencies; i++ {
		big.Competencies = append(big.Competencies, registry.Competency{Competency: "c", ValidUntil: until})
	}
	if _, err := st.Save(ctx, []registry.Entry{big}, 5, noProjection); err == nil {
		t.Fatalf("%d competencies were stored", len(big.Competencies))
	}
	big.Competencies = big.Competencies[:registry.MaxCompetencies]
	if _, err := st.Save(ctx, []registry.Entry{big}, 5, noProjection); err != nil {
		t.Fatalf("%d competencies refused: %v", len(big.Competencies), err)
	}
}

// The cache on PostgreSQL against the fake authority (done-when, E-02
// both ways): an answer is cached and served with its age; aged beyond
// its TTL on the database clock it is asked again; with the authority
// down an uncached key is unknown with registry_unavailable; the feed
// invalidates a key; an operator lookup is an events row with the
// client and the purpose.
func TestIntegrationRegistryCacheOnPostgres(t *testing.T) {
	cleanRegistry(t)
	ctx := context.Background()
	st := pgstore.Store{S: appStore(t)}
	fake := authority.New()
	defer fake.Close()
	client, err := registry.NewClient(registry.ClientConfig{BaseURL: fake.URL(), Tokens: authority.Tokens{}})
	if err != nil {
		t.Fatal(err)
	}
	proj := registry.NewMemoryProjector()
	cache := registry.NewCache(registry.CacheConfig{Client: client, Store: st, Projector: proj, Audit: st})
	feed := registry.NewFeed(registry.FeedConfig{Client: client, Store: st, Projector: proj, Counters: cache.Counters()})
	fake.SetUAS("TEST-SN-INT", "active", "C2", "")
	if err := feed.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	ask := func(sn string) *registry.Answer {
		t.Helper()
		rs, err := cache.Validate(ctx, []registry.Query{{Serial: sn}}, registry.PurposeIdentification)
		if err != nil {
			t.Fatal(err)
		}
		return rs[0].UAS
	}
	if a := ask("TEST-SN-INT"); a.Status != registry.StatusValid || *a.CacheAgeS != 0 {
		t.Fatalf("first %+v", a)
	}
	if es, _ := proj.Snapshot(); len(es) != 1 || es[0].FetchedAt.IsZero() {
		t.Fatalf("projection %+v", es)
	}
	if a := ask("TEST-SN-INT"); a.Status != registry.StatusValid || fake.Requests("validate") != 1 {
		t.Fatalf("cached %+v after %d requests", a, fake.Requests("validate"))
	}
	if _, err := relOwner(t).Exec(ctx, "UPDATE registry_validity SET fetched_at = now() - interval '25 hours'"); err != nil {
		t.Fatal(err)
	}
	if a := ask("TEST-SN-INT"); *a.CacheAgeS != 0 || fake.Requests("validate") != 2 {
		t.Fatalf("at 25 h %+v after %d requests", a, fake.Requests("validate"))
	}
	fake.Down()
	if a := ask("TEST-SN-INT"); a.Status != registry.StatusValid {
		t.Fatalf("cached while down %+v", a)
	}
	if a := ask("TEST-SN-OTHER"); a.Status != registry.StatusUnknown || a.Reason != registry.ReasonRegistryUnavailable {
		t.Fatalf("uncached while down %+v", a)
	}
	if cache.Counters().Get(registry.CounterUnavailable) != 1 {
		t.Fatal("not counted")
	}
	fake.Up()
	fake.SetUAS("TEST-SN-INT", "revoked", "", "")
	if err := feed.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if a := ask("TEST-SN-INT"); a.Status != registry.StatusRevoked {
		t.Fatalf("after the change %+v", a)
	}
	if _, err := cache.ValidateAudited(ctx, "client", "client-int-1", []registry.Query{{Operator: "GEOTESTOP0007-xyz"}}, registry.PurposeAuthorisation); err != nil {
		t.Fatal(err)
	}
	n := count(t, relOwner(t), `SELECT count(*) FROM events WHERE event_type = 'registry_validated' AND actor_id = 'client-int-1'
	    AND purpose = 'authorisation' AND payload->'operators'->>0 = 'GEOTESTOP0007' AND (payload->>'unknown')::int = 1`)
	if n != 1 {
		t.Fatalf("%d events rows for the lookup", n)
	}
	if count(t, relOwner(t), "SELECT count(*) FROM events WHERE event_type = 'registry_validated' AND payload::text LIKE '%xyz%'") != 0 {
		t.Fatal("the secret part was recorded")
	}
}
