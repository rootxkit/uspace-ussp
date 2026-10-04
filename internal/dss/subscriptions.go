package dss

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3548"

	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/store"
)

// Counter names of the subscriptions.
const (
	CounterSubscriptionPut     = "dss_subscription_put"
	CounterSubscriptionFailed  = "dss_subscription_failed"
	CounterSubscriptionDeleted = "dss_subscription_deleted"
	CounterAreasUnknown        = "dss_subscription_areas_unknown"
)

// Bounds of the subscriptions.
const (
	// MaxAreas bounds the areas of interest subscribed to.
	MaxAreas = 100
	// DefaultSubscriptionsEvery is the period of the check.
	DefaultSubscriptionsEvery = time.Minute
	// renewAt is the share of a subscription's window after which it is
	// renewed (brief WP-13: at 80 %).
	renewAt = 0.8
	// subscriptionSlack keeps a window inside
	// DSSMaxSubscriptionDurationHours whatever the clocks.
	subscriptionSlack = time.Minute
)

// Subscriptions keeps one DSS subscription per area of interest (the
// U-space airspaces of cis_current, each box widened by
// policy.PeerSubscriptionMarginM), telling us of the operational intents
// and the constraints there, for at most
// DSSMaxSubscriptionDurationHours, renewed at 80 % of the window. An area
// no longer published loses its subscription; while the areas are
// unknown (no CIS yet) nothing is changed.
type Subscriptions struct {
	Client     *Client
	Store      Store
	USSBaseURL string
	// Areas are the areas of interest now; ok false while they are not
	// known.
	Areas    func() (areas []Area, ok bool)
	Policy   func() policy.Values
	Every    time.Duration
	Counters *core.Counters
	Logger   *slog.Logger
}

func (s *Subscriptions) count(name string) {
	if s.Counters != nil {
		s.Counters.Inc(name)
	}
}

func (s *Subscriptions) logger() *slog.Logger {
	if s.Logger == nil {
		return obs.Discard()
	}
	return s.Logger
}

// SubscriptionID is the stable id of our subscription of an area: a
// version 4 UUID shaped hash of our base URL and the area id, so a restart
// and a second replica name the same subscription.
func SubscriptionID(ussBaseURL, areaID string) string {
	sum := sha256.Sum256([]byte(ussBaseURL + "|" + areaID))
	b := sum[:16]
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// Once puts, renews and deletes the subscriptions as the areas now ask.
func (s *Subscriptions) Once(ctx context.Context) error {
	areas, ok := s.Areas()
	if !ok {
		s.count(CounterAreasUnknown)
		return nil
	}
	if len(areas) > MaxAreas {
		return core.Fieldf("areas", "%d areas of interest; at most %d", len(areas), MaxAreas)
	}
	pol := policy.Defaults()
	if s.Policy != nil {
		pol = s.Policy()
	}
	existing, err := s.Store.Subscriptions(ctx)
	if err != nil {
		return err
	}
	byID := map[string]SubscriptionRecord{}
	for i := range existing {
		byID[existing[i].ID] = existing[i]
	}
	now, err := s.Store.Now(ctx)
	if err != nil {
		return err
	}
	var errs []error
	wanted := map[string]bool{}
	for _, a := range areas {
		a.Box = a.Box.PadM(pol.PeerSubscriptionMarginM)
		id := SubscriptionID(s.USSBaseURL, a.ID)
		wanted[id] = true
		cur, have := byID[id]
		if have && cur.Area == a && !due(cur, now) {
			continue
		}
		if err := s.Store.Lock(ctx, store.LockClassSub, id, func() error { return s.put(ctx, id, a, cur, have, now) }); err != nil {
			s.count(CounterSubscriptionFailed)
			s.logger().LogAttrs(ctx, slog.LevelWarn, "DSS subscription not put; tried again", slog.String("subscription_id", id),
				slog.String("area", a.ID), obs.Err(err))
			errs = append(errs, err)
		}
	}
	for i := range existing {
		e := &existing[i]
		if wanted[e.ID] {
			continue
		}
		if err := s.Client.DeleteSubscription(ctx, e.ID, e.Version); err != nil && !errors.Is(err, ErrNotFound) {
			errs = append(errs, err)
			continue
		}
		if err := s.Store.DeleteSubscription(ctx, e.ID); err != nil {
			errs = append(errs, err)
			continue
		}
		s.count(CounterSubscriptionDeleted)
	}
	return errors.Join(errs...)
}

// due reports whether 80 % of the subscription's window has passed.
func due(r SubscriptionRecord, now time.Time) bool {
	window := time.Duration(f3548.DSSMaxSubscriptionDurationHours) * time.Hour
	return r.TimeEnd.Sub(now) <= time.Duration(float64(window)*(1-renewAt))
}

func (s *Subscriptions) put(ctx context.Context, id string, a Area, cur SubscriptionRecord, have bool, now time.Time) error {
	end := now.Add(time.Duration(f3548.DSSMaxSubscriptionDurationHours)*time.Hour - subscriptionSlack)
	yes := true
	p := f3548.PutSubscriptionParameters{
		Extents: f3548.Volume4D{Volume: boxVolume(a.Box, nil, nil),
			TimeStart: &f3548.Time{Format: f3548.RFC3339, Value: now.UTC()}, TimeEnd: &f3548.Time{Format: f3548.RFC3339, Value: end.UTC()}},
		NotifyForOperationalIntents: &yes, NotifyForConstraints: &yes, UssBaseUrl: s.USSBaseURL,
	}
	version := ""
	if have {
		version = cur.Version
	}
	res, err := s.Client.PutSubscription(ctx, id, version, p)
	if err != nil && version == "" {
		// A create the DSS refused because the subscription exists (a
		// record lost, a second replica): its version is read and it is
		// updated.
		if sub, gerr := s.Client.GetSubscription(ctx, id); gerr == nil && sub.Version != "" {
			res, err = s.Client.PutSubscription(ctx, id, sub.Version, p)
		}
	} else if errors.Is(err, ErrNotFound) {
		res, err = s.Client.PutSubscription(ctx, id, "", p)
	}
	if err != nil {
		return err
	}
	if err := s.Store.UpsertSubscription(ctx, SubscriptionRecord{ID: id, Area: a, Version: res.Subscription.Version,
		NotificationIndex: res.Subscription.NotificationIndex, TimeEnd: end, USSBaseURL: s.USSBaseURL}); err != nil {
		return err
	}
	s.count(CounterSubscriptionPut)
	s.logger().LogAttrs(ctx, slog.LevelInfo, "DSS subscription put", slog.String("subscription_id", id), slog.String("area", a.ID),
		slog.Time("time_end", end))
	return nil
}

func (s *Subscriptions) every() time.Duration {
	if s.Every <= 0 {
		return DefaultSubscriptionsEvery
	}
	return s.Every
}

// Run checks the subscriptions every Every until ctx ends.
func (s *Subscriptions) Run(ctx context.Context) {
	t := time.NewTicker(s.every())
	defer t.Stop()
	for {
		if err := s.Once(ctx); err != nil && ctx.Err() == nil {
			s.logger().LogAttrs(ctx, slog.LevelWarn, "DSS subscriptions not all in place; tried again", obs.Err(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Counter names of the purge.
const (
	CounterPurgedPeers       = "dss_purged_peer_intents"
	CounterPurgedConstraints = "dss_purged_constraints"
	CounterPurgedExchanges   = "dss_purged_exchanges"
	CounterPurgeFailed       = "dss_purge_failed"
)

// DefaultPurgeEvery is the period of the purge (brief WP-13: 10 min).
const DefaultPurgeEvery = 10 * time.Minute

// Purger deletes the peers' intents and the constraints fetched more than
// ExternalDataMaxRetentionTimeHours ago that no decision record names
// (a decision copies what it relied on into its conflicts), and the
// exchange log beyond policy.DSSExchangeRetentionDays and MaxExchangeRows.
type Purger struct {
	Store    Store
	Policy   func() policy.Values
	Every    time.Duration
	Counters *core.Counters
	Logger   *slog.Logger
}

// Once purges once, on the database clock.
func (p *Purger) Once(ctx context.Context) (PurgeCounts, error) {
	pol := policy.Defaults()
	if p.Policy != nil {
		pol = p.Policy()
	}
	now, err := p.Store.Now(ctx)
	if err != nil {
		return PurgeCounts{}, err
	}
	peers := now.Add(-time.Duration(f3548.ExternalDataMaxRetentionTimeHours) * time.Hour)
	logs := now.Add(-time.Duration(pol.DSSExchangeRetentionDays) * 24 * time.Hour)
	n, err := p.Store.Purge(ctx, peers, logs, MaxExchangeRows)
	if err != nil {
		if p.Counters != nil {
			p.Counters.Inc(CounterPurgeFailed)
		}
		return n, err
	}
	if p.Counters != nil {
		p.Counters.Add(CounterPurgedPeers, uint64(n.PeerIntents))
		p.Counters.Add(CounterPurgedConstraints, uint64(n.Constraints))
		p.Counters.Add(CounterPurgedExchanges, uint64(n.Exchanges))
	}
	return n, nil
}

// Run purges every Every until ctx ends.
func (p *Purger) Run(ctx context.Context) {
	every := p.Every
	if every <= 0 {
		every = DefaultPurgeEvery
	}
	logger := p.Logger
	if logger == nil {
		logger = obs.Discard()
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if n, err := p.Once(ctx); err != nil && ctx.Err() == nil {
			logger.LogAttrs(ctx, slog.LevelWarn, "peer data not purged; tried again", obs.Err(err))
		} else if n.PeerIntents+n.Constraints > 0 {
			logger.LogAttrs(ctx, slog.LevelInfo, "peer data past 24 h purged", slog.Int64("peer_intents", n.PeerIntents),
				slog.Int64("constraints", n.Constraints))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
