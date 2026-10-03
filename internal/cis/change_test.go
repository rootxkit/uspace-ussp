package cis

import (
	"context"
	"slices"
	"sync"
	"testing"
)

type changeLog struct {
	mu sync.Mutex
	cs []Change
}

func (l *changeLog) hook(_ context.Context, c Change) {
	l.mu.Lock()
	l.cs = append(l.cs, c)
	l.mu.Unlock()
}

func (l *changeLog) all() []Change {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.cs)
}

// An installed version tells the hook what it changed, after its
// projection and store; a pull that installs nothing (the same version
// again, a 304) tells it nothing (E-01 pair).
func TestCacheTellsTheHookWhatAnInstallChanged(t *testing.T) {
	g := newCacheRig(t, "")
	log := &changeLog{}
	g.cache.cfg.OnChange = log.hook
	ctx := t.Context()
	g.fake.Publish("zones", prohibited("TZP001").json(), prohibited("TZP002").json())
	if err := g.cache.Pull(ctx, Zones, nil, true); err != nil {
		t.Fatal(err)
	}
	cs := log.all()
	if len(cs) != 1 || cs[0].Dataset != Zones || cs[0].Version != 1 || cs[0].PreviousVersion != 0 || cs[0].Reason != ChangeInstalled ||
		!slices.Equal(cs[0].FeatureIDs, []string{"TZP001", "TZP002"}) || cs[0].Truncated {
		t.Fatalf("v1 change %+v", cs)
	}
	if _, n := g.proj.Last(); n != 1 || len(g.store.saved[Zones]) != 1 {
		t.Fatalf("told before the projection (%d) or the store (%d)", n, len(g.store.saved[Zones]))
	}
	// v2 changes TZP002, drops TZP001 and adds TZP003.
	changed := prohibited("TZP002")
	changed.upper = f64(60)
	g.fake.Publish("zones", changed.json(), prohibited("TZP003").json())
	if err := g.cache.Pull(ctx, Zones, nil, true); err != nil {
		t.Fatal(err)
	}
	cs = log.all()
	if len(cs) != 2 || cs[1].Version != 2 || cs[1].PreviousVersion != 1 || !slices.Equal(cs[1].FeatureIDs, []string{"TZP001", "TZP002", "TZP003"}) {
		t.Fatalf("v2 change %+v", cs)
	}
	// Nothing new: no change.
	if err := g.cache.Pull(ctx, Zones, nil, true); err != nil {
		t.Fatal(err)
	}
	if n := len(log.all()); n != 2 {
		t.Fatalf("a pull that installed nothing told the hook: %d changes", n)
	}
}

// Unchanged features are not listed, and a change listing more than
// MaxChangedFeatures says it is truncated (E-10).
func TestChangeOfListsOnlyWhatDiffersAndIsBounded(t *testing.T) {
	a := &Entry{Identifier: "A", Raw: []byte(`{"a":1}`)}
	b := &Entry{Identifier: "B", Raw: []byte(`{"b":1}`)}
	b2 := &Entry{Identifier: "B", Raw: []byte(`{"b":2}`)}
	if ids := changeOf([]*Entry{a, b}, []*Entry{a, b}); len(ids) != 0 {
		t.Fatalf("unchanged listed %v", ids)
	}
	if ids := changeOf([]*Entry{a, b}, []*Entry{a, b2}); !slices.Equal(ids, []string{"B"}) {
		t.Fatalf("changed %v", ids)
	}
	var next []*Entry
	for i := range MaxChangedFeatures + 1 {
		next = append(next, &Entry{Identifier: string(rune('a'+i%26)) + string(rune('0'+i/26%10)) + string(rune('0'+i/260)), Raw: []byte("{}")})
	}
	log := &changeLog{}
	c := NewCache(CacheConfig{Evaluator: NewEvaluator(EvaluatorConfig{}), OnChange: log.hook})
	c.notify(t.Context(), &Version{Dataset: Zones, Number: 1}, 0, nil, next, ChangeInstalled)
	cs := log.all()
	if len(cs) != 1 || !cs[0].Truncated || len(cs[0].FeatureIDs) != MaxChangedFeatures {
		t.Fatalf("not bounded: %d ids truncated %v", len(cs[0].FeatureIDs), cs[0].Truncated)
	}
}
