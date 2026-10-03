package cis

import (
	"bytes"
	"context"
	"slices"
	"time"
)

// MaxChangedFeatures bounds the feature ids one Change lists (E-10); a
// version that changes more lists the first ones and says Truncated:
// a reader then takes every feature of the dataset as changed.
const MaxChangedFeatures = 1000

// The reasons of a Change.
const (
	// ChangeInstalled: a version newer than the one held was installed.
	ChangeInstalled = "installed"
	// ChangeWarm: the version held in the database was installed at the
	// start (Warm); nothing is known about what changed, so every
	// feature is listed.
	ChangeWarm = "warm_start"
)

// Change is what one installed version changed: the dataset, its new
// and previous version, the identifiers of the features added, removed
// or changed (by their published form), and why.
type Change struct {
	Dataset         Dataset   `json:"dataset"`
	Version         int64     `json:"version"`
	PreviousVersion int64     `json:"previous_version"`
	FeatureIDs      []string  `json:"feature_ids"`
	Truncated       bool      `json:"truncated"`
	Reason          string    `json:"reason"`
	CISVersion      string    `json:"cis_version"`
	At              time.Time `json:"at"`
}

// ChangeHook is told every installed version, after it was projected
// and stored (the api process publishes it on cis.v1.<dataset> and runs
// the standing re-check of the active intents, WP-12). It runs on the
// dataset's worker and must not block for long; ctx is the cache's.
type ChangeHook func(ctx context.Context, c Change)

// changeOf compares the entries of a dataset before and after an
// install: a feature is changed when it is new, gone, or published in
// another form.
func changeOf(prev, next []*Entry) []string {
	before := make(map[string][]byte, len(prev))
	for _, e := range prev {
		before[e.Identifier] = e.Raw
	}
	seen := make(map[string]bool, len(next))
	var ids []string
	for _, e := range next {
		seen[e.Identifier] = true
		if old, ok := before[e.Identifier]; !ok || !bytes.Equal(old, e.Raw) {
			ids = append(ids, e.Identifier)
		}
	}
	for id := range before {
		if !seen[id] {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

// notify calls the hook with what installing v over prev changed.
func (c *Cache) notify(ctx context.Context, v *Version, prevVersion int64, prev, next []*Entry, reason string) {
	if c.cfg.OnChange == nil {
		return
	}
	ch := Change{Dataset: v.Dataset, Version: v.Number, PreviousVersion: prevVersion, Reason: reason, At: c.cfg.Now().UTC()}
	ch.CISVersion, _, _ = c.cfg.Evaluator.Age()
	if v.Dataset.ED318() {
		ids := changeOf(prev, next)
		if len(ids) > MaxChangedFeatures {
			ids, ch.Truncated = ids[:MaxChangedFeatures], true
		}
		ch.FeatureIDs = ids
	}
	if ch.FeatureIDs == nil {
		ch.FeatureIDs = []string{}
	}
	c.cfg.OnChange(ctx, ch)
}
