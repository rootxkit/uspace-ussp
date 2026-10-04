package coordination

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// Airspaces is cis_current's U-space airspaces as a check reads them: by
// identifier, in_controlled_airspace as published (nil: the requirement
// block does not say), and the dataset version.
type Airspaces struct {
	Version    string
	Controlled map[string]*bool
}

// AirspaceSource gives the current U-space airspaces; false when no
// version is installed (the check waits: nothing is judged on nothing).
type AirspaceSource func() (Airspaces, bool)

// Judge is the check of an intent's U-space airspaces against as: an
// airspace in controlled airspace makes the intent controlled, and so
// does one whose block does not say or that as no longer holds (listed
// in UnstatedIDs: the ANSP is told rather than not). No airspace (an
// intent outside U-space airspace) is not controlled.
func Judge(ids []string, as Airspaces) Check {
	c := Check{CISVersion: as.Version}
	for _, id := range ids {
		v, held := as.Controlled[id]
		switch {
		case !held || v == nil:
			c.AirspaceIDs = append(c.AirspaceIDs, id)
			c.UnstatedIDs = append(c.UnstatedIDs, id)
		case *v:
			c.AirspaceIDs = append(c.AirspaceIDs, id)
		}
	}
	c.Controlled = len(c.AirspaceIDs) > 0
	return c
}

// remarksOf names the airspaces a notice is sent for without the CIS
// saying they are controlled.
func remarksOf(c Check) string {
	if len(c.UnstatedIDs) == 0 {
		return ""
	}
	return "in_controlled_airspace not stated by CIS version " + c.CISVersion + " for U-space airspace " + strings.Join(c.UnstatedIDs, ", ") + "; notified as controlled"
}

// Counters of the Notifier.
const (
	CounterQueued         = "coordination_notices_queued"
	CounterBuildFailed    = "coordination_notices_not_built"
	CounterChecked        = "coordination_intents_checked"
	CounterControlled     = "coordination_intents_controlled"
	CounterCISUnavailable = "coordination_check_cis_unavailable"
	CounterSweepFailed    = "coordination_sweep_failed"
)

// Defaults of the Notifier.
const (
	DefaultSweepEvery = time.Second
	DefaultBatch      = 100
	// DefaultLookback is how far back a conformance state or an end is
	// still notified: the CONF stream's default age, so a state api
	// records late (after an outage) is still told, with its time.
	DefaultLookback = 48 * time.Hour
)

// Notifier decides the notices and queues them (see the package
// documentation). Sweep is one pass; Run sweeps every Every.
type Notifier struct {
	Store     Store
	Airspaces AirspaceSource
	// SystemID is this USSP's code (USSP_SYSTEM_ID), the notices'
	// ussp_id and the prefix of their notice_ref.
	SystemID string
	Lookback time.Duration
	Batch    int
	Counters *core.Counters
	Logger   *slog.Logger
	Now      func() time.Time
}

func (n *Notifier) logger() *slog.Logger {
	if n.Logger == nil {
		return obs.Discard()
	}
	return n.Logger
}

func (n *Notifier) count(name string, d uint64) {
	if n.Counters != nil && d > 0 {
		n.Counters.Add(name, d)
	}
}

func (n *Notifier) now() time.Time {
	if n.Now != nil {
		return n.Now()
	}
	return time.Now()
}

func (n *Notifier) batch() int {
	if n.Batch <= 0 {
		return DefaultBatch
	}
	return n.Batch
}

func (n *Notifier) lookback() time.Duration {
	if n.Lookback <= 0 {
		return DefaultLookback
	}
	return n.Lookback
}

// Run sweeps until ctx ends.
func (n *Notifier) Run(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = DefaultSweepEvery
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := n.Sweep(ctx); err != nil && ctx.Err() == nil {
			n.count(CounterSweepFailed, 1)
			n.logger().LogAttrs(ctx, slog.LevelWarn, "coordination sweep failed; the next one retries", obs.Err(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Sweep queues every notice due now: the deviations, the intents to
// check and the ends. The error is the first failure; the rest still
// run.
func (n *Notifier) Sweep(ctx context.Context) error {
	return errors.Join(n.deviations(ctx), n.checks(ctx), n.ends(ctx))
}

// notice builds one notice; a notice that cannot be built is queued
// failed with the reason (the console shows it), never dropped.
func (n *Notifier) notice(ctx context.Context, k Kind, in Intent, dev *Deviation, remarks string) Notice {
	var stateID int64
	var flightID string
	if dev != nil {
		stateID, flightID = dev.StateID, dev.FlightID
	}
	out := Notice{Ref: Ref(n.SystemID, k, in.ID, stateID), Kind: k, IntentID: in.ID, FlightID: flightID, StateID: stateID}
	_, body, err := Build(k, out.Ref, n.SystemID, in, dev, remarks, n.now())
	if err != nil {
		out.FailReason = err.Error()
		n.count(CounterBuildFailed, 1)
		n.logger().LogAttrs(ctx, slog.LevelError, "Annex V notice not built; queued failed for the console",
			obs.IntentID(in.ID), slog.String("kind", string(k)), obs.Err(err))
		return out
	}
	out.Body = body
	return out
}

func (n *Notifier) enqueue(ctx context.Context, intentID string, c *Check, ns []Notice) error {
	q, err := n.Store.Enqueue(ctx, intentID, c, ns)
	if err != nil {
		return err
	}
	n.count(CounterQueued, uint64(q))
	if q > 0 {
		kinds := make([]string, 0, len(ns))
		for _, x := range ns {
			kinds = append(kinds, string(x.Kind))
		}
		n.logger().LogAttrs(ctx, slog.LevelInfo, "Annex V notices queued", obs.IntentID(intentID),
			slog.Int("queued", q), slog.String("kinds", strings.Join(kinds, ",")))
	}
	return nil
}

func (n *Notifier) deviations(ctx context.Context) error {
	trs, err := n.Store.Transitions(ctx, n.lookback(), n.batch())
	if err != nil {
		return err
	}
	var errs []error
	for i := range trs {
		tr := &trs[i]
		k := KindOfState(tr.State)
		if k == "" {
			continue
		}
		if err := n.enqueue(ctx, tr.Intent.ID, nil, []Notice{n.notice(ctx, k, tr.Intent, &tr.Deviation, "")}); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (n *Notifier) checks(ctx context.Context) error {
	cs, err := n.Store.Candidates(ctx, n.lookback(), n.batch())
	if err != nil || len(cs) == 0 {
		return err
	}
	as, ok := n.Airspaces()
	if !ok {
		n.count(CounterCISUnavailable, uint64(len(cs)))
		n.logger().LogAttrs(ctx, slog.LevelWarn, "no U-space airspace version installed: activated intents wait for their coordination check",
			slog.Int("intents", len(cs)))
		return nil
	}
	var errs []error
	for i := range cs {
		c := &cs[i]
		chk := Judge(slices.Clone(c.AirspaceIDs), as)
		n.count(CounterChecked, 1)
		var ns []Notice
		if chk.Controlled {
			n.count(CounterControlled, 1)
			r := remarksOf(chk)
			ns = append(ns, n.notice(ctx, KindIntentNotice, c.Intent, nil, r))
			if c.Ended {
				ns = append(ns, n.notice(ctx, KindEnded, c.Intent, nil, r))
			}
		}
		if err := n.enqueue(ctx, c.ID, &chk, ns); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (n *Notifier) ends(ctx context.Context) error {
	es, err := n.Store.Ended(ctx, n.batch())
	if err != nil {
		return err
	}
	var errs []error
	for i := range es {
		in := &es[i]
		if err := n.enqueue(ctx, in.ID, nil, []Notice{n.notice(ctx, KindEnded, *in, nil, "")}); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
