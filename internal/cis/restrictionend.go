package cis

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// Counters of the restriction end lookup (WP-12).
const (
	// CounterInconsistency counts the restrictions found gone from a
	// restrictions version while the CISP holds their head neither
	// ended nor cancelled, once per restriction and version: the
	// cis_inconsistency alarm.
	CounterInconsistency = "cis_inconsistency"
	// CounterHeadsFailed counts the lookups of the ended and cancelled
	// heads that failed: the restrictions gone from the set are not
	// judged until one answers.
	CounterHeadsFailed = "cis_restriction_heads_failed"
	// CounterHeadsTruncated counts the lookups whose list was full
	// (MaxRestrictionHeads) without the restriction asked for: not
	// judged, as a head beyond the list may have ended it.
	CounterHeadsTruncated = "cis_restriction_heads_truncated"
)

// MaxRestrictionHeads is the limit of one heads list (the CISP's
// maximum).
const MaxRestrictionHeads = 500

// MaxInconsistenciesListed bounds the gone restrictions /readyz names
// (E-10); the rest are counted there.
const MaxInconsistenciesListed = 20

// restrictionEnds is what the CISP answered for one restrictions
// version: the feature ids of its ended and cancelled heads.
type restrictionEnds struct {
	version int64
	ids     map[string]bool
	full    bool
}

// inconsistency is the gone restrictions of one restrictions version.
type inconsistency struct {
	version int64
	ids     []string
	more    int
}

// RestrictionLift is the Evaluator's answer with an absent restriction
// resolved from the CISP's heads: LiftEnded when the CISP holds a head
// for it ended or cancelled (the normal way a restriction leaves the
// current set), LiftGone when it holds none (a CIS inconsistency:
// counted, logged and named on /readyz until the next restrictions
// version), LiftUnjudged when the CISP cannot be asked or does not
// answer, or its list is full without it. The heads are read once per
// restrictions version.
func (c *Cache) RestrictionLift(ctx context.Context, id string) Lift {
	l := c.cfg.Evaluator.RestrictionLift(id)
	if l != LiftAbsent {
		return l
	}
	v := c.cfg.Evaluator.Snapshot().Version(Restrictions)
	if v == nil || c.cfg.Client == nil {
		return LiftUnjudged
	}
	ends, err := c.restrictionEnds(ctx, v.Number)
	if err != nil {
		c.cfg.Counters.Inc(CounterHeadsFailed)
		c.cfg.Logger.Warn("restriction heads not read: a restriction gone from the set is not judged",
			slog.String("restriction_id", id), slog.Int64("version", v.Number), obs.Err(err))
		return LiftUnjudged
	}
	switch {
	case ends.ids[id]:
		return LiftEnded
	case ends.full:
		c.cfg.Counters.Inc(CounterHeadsTruncated)
		return LiftUnjudged
	}
	c.markGone(ctx, id, v.Number)
	return LiftGone
}

// restrictionEnds reads the ended and cancelled heads for version once.
func (c *Cache) restrictionEnds(ctx context.Context, version int64) (*restrictionEnds, error) {
	c.mu.Lock()
	if e := c.ends; e != nil && e.version == version {
		c.mu.Unlock()
		return e, nil
	}
	c.mu.Unlock()
	e := &restrictionEnds{version: version, ids: map[string]bool{}}
	for _, state := range []string{"ended", "cancelled"} {
		list, err := c.cfg.Client.RestrictionHeads(ctx, state, MaxRestrictionHeads)
		if err != nil {
			return nil, err
		}
		for i := range list.Restrictions {
			if h := &list.Restrictions[i]; string(h.State) == state {
				e.ids[h.FeatureId] = true
			}
		}
		if len(list.Restrictions) >= MaxRestrictionHeads {
			e.full = true
		}
	}
	c.mu.Lock()
	c.ends = e
	c.mu.Unlock()
	return e, nil
}

// markGone records id gone from version: counted and logged once per
// version, named on /readyz (at most MaxInconsistenciesListed).
func (c *Cache) markGone(ctx context.Context, id string, version int64) {
	c.mu.Lock()
	in := c.inconsistent
	if in == nil || in.version != version {
		in = &inconsistency{version: version}
		c.inconsistent = in
	}
	if slices.Contains(in.ids, id) {
		c.mu.Unlock()
		return
	}
	if len(in.ids) < MaxInconsistenciesListed {
		in.ids = append(in.ids, id)
	} else {
		in.more++
	}
	c.mu.Unlock()
	c.cfg.Counters.Inc(CounterInconsistency)
	c.cfg.Logger.LogAttrs(ctx, slog.LevelError, "cis_inconsistency: a restriction left the current set while the CISP holds it neither ended nor cancelled; its alerts stay raised",
		slog.String("alarm", CounterInconsistency), slog.String("restriction_id", id), slog.Int64("version", version))
}

// inconsistencyProblem is the /readyz line of the gone restrictions of
// the restrictions version held ("" when none). The caller holds c.mu.
func (c *Cache) inconsistencyProblem(held int64) string {
	in := c.inconsistent
	if in == nil || in.version != held || len(in.ids) == 0 {
		return ""
	}
	more := ""
	if in.more > 0 {
		more = fmt.Sprintf(" and %d more", in.more)
	}
	return fmt.Sprintf("cis_inconsistency: restrictions version %d no longer holds %s%s while the CISP holds them neither ended nor cancelled; their alerts stay raised",
		in.version, strings.Join(in.ids, ", "), more)
}
