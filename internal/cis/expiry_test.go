package cis

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"

	"github.com/rootxkit/uspace-ussp/internal/testfakes/signer"
)

// The CISP's expiry versions (spec 02 F3 erratum 2026-10-16; brief
// WP-4): a restrictions version the CISP made when a restriction
// reached its end is installed by the expiry rule, every other version
// without a publisher signature is held as before. Each hold below is
// paired with the install it differs from (E-01).

// expirySkew is the USSP_CIS_EXPIRY_SKEW_S of these tests (its default).
const expirySkew = 30 * time.Second

func newExpiryRig(t *testing.T) *cacheRig {
	t.Helper()
	g := newCacheRig(t, "")
	g.cache.cfg.ExpirySkew = expirySkew
	return g
}

// at is the rig's clock (2026-10-02 09:00:00Z) plus d.
func at(d time.Duration) time.Time { return newClock().Now().Add(d) }

// dar is an active restriction over tbilisi whose single
// limitedApplicability period runs from 08:00 to end, with the CISP's
// cis_restriction.
func dar(id string, end time.Time) feat {
	f := prohibited(id)
	f.applicability = []any{map[string]any{"startDateTime": "2026-10-02T08:00:00Z", "endDateTime": end.UTC().Format(time.RFC3339)}}
	f.extended = map[string]any{RestrictionMember: map[string]any{"id": "r-" + id, "state": "active"}}
	return f
}

// expiryBody is the CISP's expiry record for featureID (the record
// format of the erratum; an empty featureID leaves feature_id and
// ends_at out, as a CISP from before the erratum writes it).
func expiryBody(op, featureID string, endsAt, when time.Time) []byte {
	m := map[string]any{"op": op, "restriction_id": "r-" + featureID, "at": when.UTC().Format(time.RFC3339Nano)}
	if featureID != "" {
		m["feature_id"], m["ends_at"] = featureID, endsAt.UTC().Format(time.RFC3339)
	}
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return b
}

// The set in use in most cases below: A ends at T1 (09:00), B at T2
// (10:00).
var (
	t1 = at(0)
	t2 = at(time.Hour)
)

// inUse publishes and installs v1 = {A, B}, signed by the ANSP, and a
// first zones and uspace_airspace version (so the CIS can be current).
func (g *cacheRig) inUse(t *testing.T) {
	t.Helper()
	g.fake.Publish("zones", prohibited("TZP001").json())
	g.fake.Publish("uspace_airspace", airspace("TSA001").json())
	g.fake.Publish("restrictions", dar("A", t1).json(), dar("B", t2).json())
	for _, d := range ED318Datasets {
		if err := g.cache.Pull(t.Context(), d, nil, false); err != nil || g.active(d) != 1 {
			t.Fatalf("%s v1: %v, active %d", d, err, g.active(d))
		}
	}
}

// reconcile pulls every ED-318 dataset as the reconciliation does.
func (g *cacheRig) reconcile(t *testing.T) {
	t.Helper()
	for _, d := range ED318Datasets {
		_ = g.cache.Pull(t.Context(), d, nil, true)
	}
}

func (g *cacheRig) ids(d Dataset) []string {
	var out []string
	for _, e := range g.eval.Snapshot().Entries(d) {
		out = append(out, e.Identifier)
	}
	slices.Sort(out)
	return out
}

func (g *cacheRig) heldNow(d Dataset) *UntrustedError {
	g.cache.mu.Lock()
	defer g.cache.mu.Unlock()
	return g.cache.held[d]
}

// pullInstalled pulls the restrictions and expects version v installed
// by the expiry rule: in use, stored with its kind, the hold gone,
// counted.
func (g *cacheRig) pullInstalled(t *testing.T, v int64) {
	t.Helper()
	before := g.count(CounterExpiryInstalled)
	if err := g.cache.Pull(t.Context(), Restrictions, nil, false); err != nil {
		t.Fatalf("pull: %v", err)
	}
	if g.active(Restrictions) != v {
		t.Fatalf("active %d, want %d", g.active(Restrictions), v)
	}
	if s := g.lastSaved(Restrictions); s == nil || s.Number != v || s.Provenance != ProvenanceCISPExpiry {
		t.Fatalf("stored: %+v", s)
	}
	if g.heldNow(Restrictions) != nil || g.count(CounterExpiryInstalled) != before+1 {
		t.Fatalf("held %v, counters %v", g.heldNow(Restrictions), g.cache.Counters().Snapshot())
	}
}

// pullExpiryHeld is pullHeld for an expiry version, and counts
// cis_expiry_held.
func (g *cacheRig) pullExpiryHeld(t *testing.T, v int64, why string) {
	t.Helper()
	before := g.count(CounterExpiryHeld)
	g.pullHeld(t, Restrictions, v, why)
	if g.count(CounterExpiryHeld) != before+1 {
		t.Fatalf("cis_expiry_held did not move: %v", g.cache.Counters().Snapshot())
	}
	if g.active(Restrictions) == v {
		t.Fatalf("held version %d is in use", v)
	}
}

// 1. Acceptance: v1 = {A end T1, B end T2}; the CISP serves v2 = {B},
// its bytes the expiry record of A at T1 + 5 s, signed by the CISP, no
// publisher signature: installed, A gone, the hold none, the age reset,
// /readyz up. Its pair: the ANSP's signed end of B, v3, installs on top
// of it as any version does (chaining).
func TestExpiryInstalled(t *testing.T) {
	g := newExpiryRig(t)
	ctx := t.Context()
	g.inUse(t)
	g.clk.advance(200 * time.Second) // aged; at stays within the skew
	g.fake.PublishExpiry("restrictions", expiryBody(OpExpire, "A", t1, t1.Add(5*time.Second)), dar("B", t2).json())
	g.pullInstalled(t, 2)
	if ids := g.ids(Restrictions); !slices.Equal(ids, []string{"B"}) {
		t.Fatalf("in use: %v", ids)
	}
	if v := g.eval.Snapshot().Version(Restrictions); v.Provenance != ProvenanceCISPExpiry {
		t.Fatalf("snapshot kind %q", v.Provenance)
	}
	for _, a := range g.eval.Ages() {
		if a.Dataset == Restrictions && (a.Version != 2 || a.AgeS != 0) {
			t.Fatalf("restrictions age: %+v", a)
		}
	}
	if _, detail := g.cache.Probe(ctx); !strings.HasPrefix(detail, "zones:1,uspace_airspace:1,restrictions:2, age 200 s") ||
		strings.Contains(detail, "held") {
		t.Fatalf("probe: %q", detail)
	}
	if g.count(CounterUntrusted) != 0 || g.count(CounterExpiryHeld) != 0 {
		t.Fatalf("counters %v", g.cache.Counters().Snapshot())
	}

	end, _ := json.Marshal(map[string]any{"op": "end", "ansp_ref": "R-B", "ansp_version": 2})
	g.fake.PublishSigned("restrictions", end)
	if err := g.cache.Pull(ctx, Restrictions, nil, false); err != nil || g.active(Restrictions) != 3 {
		t.Fatalf("v3: %v, active %d", err, g.active(Restrictions))
	}
	if s := g.lastSaved(Restrictions); s.Number != 3 || s.Provenance != ProvenancePublisher || len(g.ids(Restrictions)) != 0 {
		t.Fatalf("v3 stored %+v, in use %v", s, g.ids(Restrictions))
	}
}

// 2. The CISP's signature: missing, made by the ANSP's key, by another
// key under the CISP's kid, or over other bytes, holds the version; the
// same record correctly signed then installs it. Without a cisp key
// configured it is held too.
func TestExpiryCISPSignature(t *testing.T) {
	g := newExpiryRig(t)
	g.inUse(t)
	rec := expiryBody(OpExpire, "A", t1, t1)
	g.fake.PublishExpiry("restrictions", rec, dar("B", t2).json())

	g.fake.SetCISSignature("restrictions", 2, "")
	g.pullExpiryHeld(t, 2, "no X-CIS-Signature")

	sign := func(s *signer.Signer, b []byte) string {
		sig, err := s.SignDetached(b, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return sig
	}
	stranger, err := signer.New("stranger", "fake-cisp-1")
	if err != nil {
		t.Fatal(err)
	}
	tampered := []byte(strings.Replace(string(rec), `"op":"expire"`, `"op":"expirf"`, 1))
	for name, sig := range map[string]string{
		"the ANSP's key":          sign(g.fake.Publishers["ansp"], rec),
		"another key, CISP's kid": sign(stranger, rec),
		"over other bytes":        sign(g.fake.Signer, tampered),
	} {
		g.fake.SetCISSignature("restrictions", 2, sig)
		t.Run(name, func(t *testing.T) { g.pullExpiryHeld(t, 2, "X-CIS-Signature does not verify with the CISP's keys") })
	}

	// No cisp key configured: every expiry is held.
	g.fake.SetCISSignature("restrictions", 2, sign(g.fake.Signer, rec))
	keys := g.fake.PublisherKeys()
	delete(keys, "cisp")
	noCISP, err := coreauth.NewDetachedVerifier(t.Context(), coreauth.DetachedConfig{Publishers: keys, MaxAge: 366 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	withCISP := g.cache.cfg.Publishers
	g.cache.cfg.Publishers = noCISP
	g.pullExpiryHeld(t, 2, "X-CIS-Signature does not verify with the CISP's keys")

	g.cache.cfg.Publishers = withCISP
	g.pullInstalled(t, 2)
	if ids := g.ids(Restrictions); !slices.Equal(ids, []string{"B"}) {
		t.Fatalf("in use: %v", ids)
	}
}

// 3. A removal before its signed end is held: A ending after at; after
// an extend, an expiry at an instant between the original end and the
// extended one; an at later than the clock plus the skew. An at one
// second inside the skew installs.
func TestExpiryNotBeforeTheSignedEnd(t *testing.T) {
	t.Run("before its end", func(t *testing.T) {
		g := newExpiryRig(t)
		g.fake.Publish("restrictions", dar("A", t1.Add(10*time.Second)).json(), dar("B", t2).json())
		if err := g.cache.Pull(t.Context(), Restrictions, nil, false); err != nil {
			t.Fatal(err)
		}
		g.fake.PublishExpiry("restrictions", expiryBody(OpExpire, "A", t1.Add(10*time.Second), t1.Add(5*time.Second)), dar("B", t2).json())
		g.pullExpiryHeld(t, 2, `it removes "A" before its signed end 2026-10-02T09:00:10Z`)
	})
	t.Run("after an extend", func(t *testing.T) {
		g := newExpiryRig(t)
		g.inUse(t)
		extended := t1.Add(10 * time.Minute)
		g.fake.Publish("restrictions", dar("A", extended).json(), dar("B", t2).json())
		if err := g.cache.Pull(t.Context(), Restrictions, nil, false); err != nil || g.active(Restrictions) != 2 {
			t.Fatalf("extend: %v", err)
		}
		g.clk.advance(6 * time.Minute)
		// The expiry of the original end, at 09:05: between T1 and T1'.
		g.fake.PublishExpiry("restrictions", expiryBody(OpExpire, "A", t1, t1.Add(5*time.Minute)), dar("B", t2).json())
		g.pullExpiryHeld(t, 3, `it removes "A" before its signed end 2026-10-02T09:10:00Z`)
	})
	t.Run("at beyond the skew", func(t *testing.T) {
		g := newExpiryRig(t)
		g.inUse(t)
		g.fake.PublishExpiry("restrictions", expiryBody(OpExpire, "A", t1, g.clk.Now().Add(expirySkew+time.Second)), dar("B", t2).json())
		g.pullExpiryHeld(t, 2, "is later than this process's clock plus 30s")
	})
	t.Run("at within the skew", func(t *testing.T) {
		g := newExpiryRig(t)
		g.inUse(t)
		g.fake.PublishExpiry("restrictions", expiryBody(OpExpire, "A", t1, g.clk.Now().Add(expirySkew-time.Second)), dar("B", t2).json())
		g.pullInstalled(t, 2)
	})
}

// 4. Content: a version that adds a feature, changes one, removes
// none, or whose feature_id is not among the removed is held; so is
// the record of a CISP from before the erratum (no feature_id). The
// version that only removes A, named by feature_id, installs.
func TestExpiryContent(t *testing.T) {
	changed := dar("B", t2)
	changed.upper = f64(60)
	for _, c := range []struct {
		name   string
		record []byte
		fs     []feat
		why    string
	}{
		{"adds", expiryBody(OpExpire, "A", t1, t1), []feat{dar("B", t2), dar("C", t2)}, `it adds "C"`},
		{"changes", expiryBody(OpExpire, "A", t1, t1), []feat{changed}, `it changes "B"`},
		{"removes nothing", expiryBody(OpExpire, "A", t1, t1), []feat{dar("A", t1), dar("B", t2)}, "it removes nothing"},
		{"feature_id not removed", expiryBody(OpExpire, "B", t2, t1), []feat{dar("B", t2)}, `feature_id "B" is not among the features it removes`},
		{"older CISP", expiryBody(OpExpire, "", time.Time{}, t1), []feat{dar("B", t2)}, "names no feature_id"},
		{"installed", expiryBody(OpExpire, "A", t1, t1), []feat{dar("B", t2)}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newExpiryRig(t)
			g.inUse(t)
			fs := make([]json.RawMessage, len(c.fs))
			for i, f := range c.fs {
				fs[i] = f.json()
			}
			g.fake.PublishExpiry("restrictions", c.record, fs...)
			if c.why == "" {
				g.pullInstalled(t, 2)
				return
			}
			g.pullExpiryHeld(t, 2, c.why)
			if ids := g.ids(Restrictions); !slices.Equal(ids, []string{"A", "B"}) {
				t.Fatalf("in use: %v", ids)
			}
		})
	}
}

// 5. Replay and downgrade: once v2 is installed, the CISP serving v2
// again is a confirmation (no second read as published, nothing
// installed again); a notification of v1 is a counted replay; a version
// below the one in use is never installed.
func TestExpiryReplay(t *testing.T) {
	g := newExpiryRig(t)
	ctx := t.Context()
	g.inUse(t)
	g.fake.PublishExpiry("restrictions", expiryBody(OpExpire, "A", t1, t1), dar("B", t2).json())
	g.pullInstalled(t, 2)
	reads := g.fake.Requests("GET /v1/restrictions/versions/2")

	g.clk.advance(100 * time.Second)
	if err := g.cache.Pull(ctx, Restrictions, nil, true); err != nil {
		t.Fatal(err)
	}
	if g.count(CounterNotModified) != 1 || g.active(Restrictions) != 2 || g.count(CounterExpiryInstalled) != 1 ||
		g.fake.Requests("GET /v1/restrictions/versions/2") != reads {
		t.Fatalf("served again: %v", g.cache.Counters().Snapshot())
	}
	replays := g.count(CounterVersionReplays)
	if err := g.cache.Pull(ctx, Restrictions, &Hint{Version: 1, Issuer: "https://cisp.test", At: time.Now()}, false); err != nil {
		t.Fatal(err)
	}
	if g.count(CounterVersionReplays) != replays+1 || g.active(Restrictions) != 2 {
		t.Fatalf("v1 notified: %v, active %d", g.cache.Counters().Snapshot(), g.active(Restrictions))
	}
	// A version numbered below the one in use reaches accept (a cached
	// answer): counted, never installed, never read as published.
	old := mustVersion(t, Restrictions, 1, dar("B", t2).json())
	if err := g.cache.accept(ctx, old, g.eval.Snapshot().Version(Restrictions), false); err != nil {
		t.Fatal(err)
	}
	if g.count(CounterVersionReplays) != replays+2 || g.active(Restrictions) != 2 || g.fake.Requests("GET /v1/restrictions/versions/1") != 1 {
		t.Fatalf("older version: %v, active %d", g.cache.Counters().Snapshot(), g.active(Restrictions))
	}
}

// 6. The marker: a CISP-signed record with another op is held; the
// exception is the restrictions': a zones version without a publisher
// signature is held as before, its CISP signature notwithstanding, and
// is not an expiry hold.
func TestExpiryOnlyTheExpireOpOfRestrictions(t *testing.T) {
	g := newExpiryRig(t)
	g.inUse(t)
	g.fake.PublishExpiry("restrictions", expiryBody("end", "A", t1, t1), dar("B", t2).json())
	g.pullExpiryHeld(t, 2, `op "end" is not "expire"`)

	g.fake.Publish("zones", prohibited("TZP002").json())
	g.fake.SetPublisherSignature("zones", 2, "", "")
	held := g.count(CounterExpiryHeld)
	g.pullHeld(t, Zones, 2, "no X-Publisher-Signature")
	if ue := g.heldNow(Zones); ue == nil || ue.Reason != "no X-Publisher-Signature" || g.count(CounterExpiryHeld) != held {
		t.Fatalf("zones held as %v, cis_expiry_held %d", ue, g.count(CounterExpiryHeld))
	}
}

// A fresh start, no restrictions version of the CISP in use: there is
// nothing to compare an expiry with, and it is held as before. Its
// pair is TestExpiryInstalled, where one is in use.
func TestExpiryWithoutAVersionInUseHeld(t *testing.T) {
	g := newExpiryRig(t)
	g.fake.PublishExpiry("restrictions", expiryBody(OpExpire, "A", t1, t1), dar("B", t2).json())
	g.pullExpiryHeld(t, 1, "no restrictions version of the CISP is in use to compare it with")
	if g.active(Restrictions) != 0 {
		t.Fatal("an expiry was installed with nothing to compare it with")
	}
}

// The comparison is with the CISP's version in use, not with what the
// Evaluator holds: a restriction laid over it from the ANSP's direct
// path is absent from the CISP's expiry version, and is not a removal
// (compared with the merged set, D would be removed before its end and
// the expiry held). D stays in force after it.
func TestExpiryIgnoresTheDirectOverlay(t *testing.T) {
	g := newExpiryRig(t)
	ctx := t.Context()
	g.inUse(t)
	d := dar("D", t2)
	es, rf := buildEntries(mustVersion(t, Restrictions, 1, d.json()))
	if rf != nil {
		t.Fatal(rf)
	}
	g.cache.mu.Lock()
	g.cache.direct["D"] = &directHeld{d: DirectRestriction{Identifier: "D", AnspVersion: 1, State: "active", StartsAt: at(-time.Hour), EndsAt: t2},
		since: g.clk.Now(), entry: es[0]}
	g.cache.mu.Unlock()
	g.cache.reinstall(ctx)
	if ids := g.ids(Restrictions); !slices.Equal(ids, []string{"A", "B", "D"}) {
		t.Fatalf("with the overlay: %v", ids)
	}
	g.fake.PublishExpiry("restrictions", expiryBody(OpExpire, "A", t1, t1), dar("B", t2).json())
	g.pullInstalled(t, 2)
	if ids := g.ids(Restrictions); !slices.Equal(ids, []string{"B", "D"}) {
		t.Fatalf("after the expiry: %v", ids)
	}
}

// 7 (the cache's part). Before the erratum, the CISP's expiry version is
// held: the version in use is never confirmed again and its age passes
// the bound (Evaluator.Age stale). With the expiry installed, the same
// time later it is current. intent's TestDecideCISExpiry is the
// decision's part.
func TestExpiryStaleness(t *testing.T) {
	for _, installed := range []bool{false, true} {
		g := newExpiryRig(t)
		g.inUse(t)
		rec := expiryBody(OpExpire, "", time.Time{}, t1) // a CISP from before the erratum
		if installed {
			rec = expiryBody(OpExpire, "A", t1, t1)
		}
		g.fake.PublishExpiry("restrictions", rec, dar("B", t2).json())
		g.reconcile(t)
		g.clk.advance(301 * time.Second)
		g.reconcile(t) // past the bound
		_, age, stale := g.eval.Age()
		if stale == installed {
			t.Fatalf("installed %v: age %.0f s, stale %v", installed, age, stale)
		}
		if out := g.cache.Outdated(); (len(out) == 0) != installed {
			t.Fatalf("installed %v: outdated %v", installed, out)
		}
	}
}

// 8. Local applicability: a restriction past its signed end that the
// CISP has not yet expired does not apply in the Evaluator; before its
// end it does. The expiry version is bookkeeping, never what ends one.
func TestExpiryLocalApplicabilityEndsARestriction(t *testing.T) {
	g := newExpiryRig(t)
	g.inUse(t)
	var a *Entry
	for _, e := range g.eval.Snapshot().Entries(Restrictions) {
		if e.Identifier == "A" {
			a = e
		}
	}
	if a == nil {
		t.Fatal("A not in use")
	}
	if ap, err := g.eval.ApplicabilityAt(a, 0, t1.Add(-time.Second)); err != nil || ap != Applies {
		t.Fatalf("before its end: %v %v", ap, err)
	}
	if ap, err := g.eval.ApplicabilityAt(a, 0, t1.Add(time.Second)); err != nil || ap != NotApplicable {
		t.Fatalf("past its end: %v %v", ap, err)
	}
}

// A record that is not the erratum's, whole, is held, each with its
// reason: bytes that are not a record, a missing or unreadable ends_at
// or at. A removed feature whose end is not one signed period end is
// held too. Their pair is the "installed" case of TestExpiryContent.
func TestExpiryRecordMalformedHeld(t *testing.T) {
	good := map[string]any{"op": OpExpire, "restriction_id": "r-A", "feature_id": "A", "ends_at": "2026-10-02T09:00:00Z", "at": "2026-10-02T09:00:00Z"}
	with := func(k string, v any) []byte {
		m := map[string]any{}
		for key, val := range good {
			m[key] = val
		}
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
		b, _ := json.Marshal(m)
		return b
	}
	for _, c := range []struct {
		name   string
		record []byte
		why    string
	}{
		{"not a record", []byte(`["expire"]`), "its bytes are not a CISP expiry record"},
		{"no ends_at", with("ends_at", nil), "carries no ends_at"},
		{"ends_at not a time", with("ends_at", "soon"), `ends_at "soon" is not an RFC 3339 time`},
		{"no at", with("at", nil), "carries no at"},
		{"at not a time", with("at", "now"), `at "now" is not an RFC 3339 time`},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newExpiryRig(t)
			g.inUse(t)
			g.fake.PublishExpiry("restrictions", c.record, dar("B", t2).json())
			g.pullExpiryHeld(t, 2, c.why)
		})
	}
	t.Run("no single period", func(t *testing.T) {
		g := newExpiryRig(t)
		a := dar("A", t1)
		a.applicability = append(a.applicability, map[string]any{"startDateTime": "2026-10-03T08:00:00Z", "endDateTime": "2026-10-03T09:00:00Z"})
		g.fake.Publish("restrictions", a.json(), dar("B", t2).json())
		if err := g.cache.Pull(t.Context(), Restrictions, nil, false); err != nil {
			t.Fatal(err)
		}
		g.fake.PublishExpiry("restrictions", with("at", "2026-10-02T09:00:00Z"), dar("B", t2).json())
		g.pullExpiryHeld(t, 2, `it removes "A", which has no single limitedApplicability period with an endDateTime`)
	})
}

// The comparison's own failures, which a pulled version cannot reach
// (ed318.Parse refuses a repeated identifier, and every restrictions
// version holds a collection), are holds too, never an install: a
// version without a collection, and a set in use or a version whose
// features do not export.
func TestExpiryComparisonFailuresHeld(t *testing.T) {
	g := newExpiryRig(t)
	g.inUse(t)
	rec := expiryBody(OpExpire, "A", t1, t1)
	sig, err := g.fake.Signer.SignDetached(rec, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	f := Fetched{Status: 200, Body: rec, CISSignature: sig}
	good := mustVersion(t, Restrictions, 2, dar("B", t2).json())
	twice := mustVersion(t, Restrictions, 2, dar("B", t2).json())
	twice.Collection.Features = append(twice.Collection.Features, twice.Collection.Features[0])
	check := func(name string, v *Version, why string) {
		t.Helper()
		err := g.cache.expiryProvenance(t.Context(), v, f)
		var ue *UntrustedError
		if !errors.As(err, &ue) || !strings.Contains(ue.Reason, why) {
			t.Errorf("%s: %v, want held for %q", name, err, why)
		}
	}
	if err := g.cache.expiryProvenance(t.Context(), good, f); err != nil {
		t.Fatalf("the version as pulled: %v", err)
	}
	check("no collection", &Version{Dataset: Restrictions, Number: 2}, "the version holds no collection to compare")
	check("the version's features", twice, "the version's features do not export")
	g.cache.mu.Lock()
	base := *g.cache.restrBase
	fc := *base.Collection
	fc.Features = append(slices.Clone(fc.Features), fc.Features[0])
	base.Collection = &fc
	saved := g.cache.restrBase
	g.cache.restrBase = &base
	g.cache.mu.Unlock()
	check("the features in use", good, "the features in use do not export")
	g.cache.mu.Lock()
	g.cache.restrBase = saved
	g.cache.mu.Unlock()
}
