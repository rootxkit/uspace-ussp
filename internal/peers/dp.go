package peers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3411"
	"github.com/rootxkit/uspace-core/geodesy"
	"github.com/rootxkit/uspace-core/geoid"
	coresources "github.com/rootxkit/uspace-core/sources"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/sources"
	stdf3411 "github.com/rootxkit/uspace-ussp/internal/stdapi/f3411"
)

// SourceNetworkRID is the adapter type of the peer flights (04 §2, the
// source-control type); its instances are the peers' base URLs.
const SourceNetworkRID = "network_rid"

// Producer is the envelope producer of this package.
const Producer = "ussp/peers"

// Periods and bounds of the Display Provider (E-10).
const (
	// DefaultEvery is a view's poll period (F3411: 1 Hz).
	DefaultEvery = time.Second
	// DefaultSlowEvery is a slow peer's poll period (0.5 Hz).
	DefaultSlowEvery = 2 * time.Second
	// DefaultSearchEvery is the period of the ISA search of every view,
	// beside the subscription's notifications.
	DefaultSearchEvery = time.Minute
	// DefaultReconcileEvery is how often the pollers are matched to the
	// ISAs and the switches: a switch takes effect within it.
	DefaultReconcileEvery = 250 * time.Millisecond
	// DefaultStatusEvery is the period of src.v1 (2 s).
	DefaultStatusEvery = 2 * time.Second
	// PollDeadline bounds one poll (F3411's 99th percentile data
	// response, 3 s).
	PollDeadline = f3411.NetSpDataResponseTime99thPercentileSeconds * time.Second
	// SlowAfter is F3411's 95th percentile data response (1 s): a peer
	// slower than it more than once in its last SlowWindow answers is
	// slow.
	SlowAfter  = f3411.NetSpDataResponseTime95thPercentileSeconds * time.Second
	SlowWindow = 20
	// MaxAreas bounds the areas of interest.
	MaxAreas = 100
	// MaxISAs bounds the peers' ISAs held; MaxPollers the pollers run.
	MaxISAs    = 10_000
	MaxPollers = 2000
	// MaxBaseURLBytes bounds a peer's base URL (a source instance).
	MaxBaseURLBytes = 128
	// dssTimeout bounds one DSS call.
	dssTimeout = 5 * time.Second
	// subscriptionSlack keeps a subscription inside the DSS's 24 h.
	subscriptionSlack = time.Minute
	// renewAt is the share of a subscription's window after which it is
	// renewed (as WP-13's: 80 %).
	renewAt = 0.8
)

// Counters of the Display Provider.
const (
	CounterPolls            = "peer_polls"
	CounterPollFailed       = "peer_poll_failed"
	CounterPollRefused      = "peer_poll_refused"
	CounterOverMax          = "peer_flights_over_max"
	CounterNoISAs           = "peer_poll_no_isas_present"
	CounterFlights          = "peer_flights"
	CounterFlightNoState    = "peer_flight_no_current_state"
	CounterFlightRefused    = "peer_flight_refused"
	CounterFlightTooOld     = "peer_flight_too_old"
	CounterFlightAtReceipt  = "peer_flight_placed_at_receipt"
	CounterEchoOwnFlight    = "peer_echo_own_flight"
	CounterPublished        = "peer_published"
	CounterPublishFailed    = "peer_publish_failed"
	CounterSearches         = "peer_isa_searches"
	CounterSearchFailed     = "peer_isa_search_failed"
	CounterSubscriptionPut  = "peer_subscription_put"
	CounterSubscriptionFail = "peer_subscription_failed"
	CounterSubscriptionDel  = "peer_subscription_deleted"
	CounterSubscriptionKept = "peer_subscription_restored"
	CounterSubStoreFailed   = "peer_subscription_store_failed"
	CounterISAOwn           = "peer_isa_own_skipped"
	CounterISARefused       = "peer_isa_refused"
	CounterISAOverBound     = "peer_isa_over_bound"
	CounterPollersOverBound = "peer_pollers_over_bound"
	CounterAreaRefused      = "peer_area_refused"
	CounterPollerStarted    = "peer_poller_started"
	CounterPollerStopped    = "peer_poller_stopped"
	CounterStatusFailed     = "peer_status_publish_failed"
)

// TokenSource hands out an ecosystem token whose audience is the host of
// baseURL (auth.Outgoing; M18).
type TokenSource interface {
	Token(ctx context.Context, baseURL string, scopes ...string) (string, error)
}

// Sink is the bus (bus.Publisher).
type Sink interface {
	Publish(ctx context.Context, subject string, m bus.Enveloped) error
}

// Gate is the source switches (internal/sources.Follower).
type Gate interface {
	Query(sourceType string, instanceID *string) coresources.Decision
}

// Own is this USSP's own flights as the echo guard reads them (PLAN §15
// Q23): OwnFlight reports whether a RID flight id is the flight id of
// one of this USSP's flights (its own Service Provider serves its
// flights under their flight ids).
type Own interface {
	OwnFlight(ridFlightID string) bool
}

// Resolver is the registry's ResolveBroadcast (WP-5's
// registry.Lookup).
type Resolver interface {
	ResolveBroadcast(sn, operatorReg *string) core.Identification
}

// Area is one area of interest: an id and a box.
type Area struct {
	ID  string
	Box geodesy.BBox
}

// ISANotification is the part of an ISA notification rid-sp keeps in
// rid_isa_notifications (its ISANotification) that discovery reads.
type ISANotification struct {
	ISAID       string                           `json:"isa_id"`
	ReceivedAt  time.Time                        `json:"received_at"`
	Deleted     bool                             `json:"deleted"`
	ServiceArea *f3411.IdentificationServiceArea `json:"service_area,omitempty"`
	Extents     *f3411.Volume4D                  `json:"extents,omitempty"`
}

// DecodeNotification reads one rid_isa_notifications value.
func DecodeNotification(_ string, data []byte) (ISANotification, error) {
	var n ISANotification
	if err := json.Unmarshal(data, &n); err != nil {
		return ISANotification{}, core.Fieldf("notification", "not an ISA notification")
	}
	return n, nil
}

// isa is one peer ISA as discovery holds it: the peer, its end, and the
// views it touches.
type isa struct {
	base  string
	end   time.Time
	views map[string]geodesy.BBox
}

// pollKey is one poller: a peer and a view.
type pollKey struct{ base, view string }

type poller struct {
	key    pollKey
	box    geodesy.BBox
	cancel context.CancelFunc
	done   chan struct{}
}

// peerState is what is known of one peer's answers: down since its
// first failure after its last answer. idle is when the peer was first
// seen unpolled with no answer and no failure to date it by.
type peerState struct {
	ok, failed     time.Time
	idle           time.Time
	down           bool
	since          time.Time
	lastErr        string
	latencies      []time.Duration
	polls, refused uint64
	published      uint64
	echoes         uint64
}

// forgotten reports, for a peer no longer polled, whether
// peer_unavailable_s has passed since its last answer or failure (since
// it was first seen idle when it has neither).
func (p *peerState) forgotten(now time.Time, unavailable time.Duration) bool {
	last := p.ok
	if p.failed.After(last) {
		last = p.failed
	}
	if last.IsZero() {
		if p.idle.IsZero() {
			p.idle = now
		}
		last = p.idle
	}
	return now.Sub(last) > unavailable
}

func (p *peerState) slow() bool {
	n := 0
	for _, l := range p.latencies {
		if l > SlowAfter {
			n++
		}
	}
	return n > 1
}

// subState is one of our DSS subscriptions.
type subState struct {
	area    Area
	version string
	end     time.Time
}

// DP is the Display Provider; see the package documentation.
type DP struct {
	DSSBaseURL string
	USSBaseURL string
	Tokens     TokenSource
	HTTP       *http.Client
	// Areas are the areas of interest now; false while they are not
	// known (no CIS yet and no configured box).
	Areas func() ([]Area, bool)
	// Notifications is rid_isa_notifications now; false while unread.
	Notifications func() (map[string]ISANotification, bool)
	// Subscriptions keeps our DSS subscriptions across a restart
	// (rid_dp_subscriptions), so that one whose area was dropped meanwhile
	// is deleted; nil keeps them in memory only.
	Subscriptions SubscriptionStore
	Sink          Sink
	Gate          Gate
	Own           Own
	Resolver      Resolver
	Geoid         geoid.Undulator
	Policy        func() policy.Values
	Counters      *core.Counters
	Logger        *slog.Logger
	Now           func() time.Time
	// Every (1 s), SlowEvery (2 s), SearchEvery (60 s), ReconcileEvery
	// (250 ms) and StatusEvery (2 s) are what a test shortens.
	Every          time.Duration
	SlowEvery      time.Duration
	SearchEvery    time.Duration
	ReconcileEvery time.Duration
	StatusEvery    time.Duration

	mu       sync.Mutex
	once     sync.Once
	started  time.Time
	searched map[string]*isa
	isas     map[string]*isa
	pollers  map[pollKey]*poller
	peers    map[string]*peerState
	subs     map[string]*subState
	restored bool
	// restoreErr says why the saved subscriptions are not read yet,
	// storeErr why the last write of one failed.
	restoreErr string
	storeErr   string
	dssOK      time.Time
	dssFailed  time.Time
	dssDown    bool
	dssErr     string
	areasErr   string
	offSince   time.Time
	agg        string
	aggSince   time.Time
	tiles      map[string][]geodesy.BBox
	dss        *stdf3411.StdClient
	dssCErr    error
	clients    map[string]*stdf3411.StdClient
	wake       chan struct{}
}

func (d *DP) init() {
	d.once.Do(func() {
		if d.Counters == nil {
			d.Counters = &core.Counters{}
		}
		d.started = d.now()
		d.searched, d.isas = map[string]*isa{}, map[string]*isa{}
		d.pollers, d.peers, d.subs = map[pollKey]*poller{}, map[string]*peerState{}, map[string]*subState{}
		d.tiles, d.clients = map[string][]geodesy.BBox{}, map[string]*stdf3411.StdClient{}
		d.wake = make(chan struct{}, 1)
	})
}

func (d *DP) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *DP) logger() *slog.Logger {
	if d.Logger == nil {
		return obs.Discard()
	}
	return d.Logger
}

func (d *DP) policy() policy.Values {
	if d.Policy == nil {
		return policy.Defaults()
	}
	return d.Policy()
}

func every(v, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	return v
}

func (d *DP) decision(instance *string) coresources.Decision {
	if d.Gate == nil {
		return coresources.Decision{Enabled: true}
	}
	return d.Gate.Query(SourceNetworkRID, instance)
}

func clip(s string) string {
	if len(s) > 200 {
		s = s[:200]
		for len(s) > 0 && !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return s
}

func clipErr(err error) string {
	if err == nil {
		return ""
	}
	return clip(err.Error())
}

// normBase is a base URL as compared: no trailing slash.
func normBase(s string) string { return strings.TrimRight(strings.TrimSpace(s), "/") }

// IsOwn reports whether base is this USSP's own base URL.
func (d *DP) IsOwn(base string) bool {
	return d.USSBaseURL != "" && strings.EqualFold(normBase(base), normBase(d.USSBaseURL))
}

// checkBase refuses a base URL that is not an absolute http(s) URL or
// is longer than MaxBaseURLBytes.
func checkBase(base string) error {
	u, err := url.Parse(base)
	switch {
	case err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https"):
		return core.Fieldf("uss_base_url", "not an absolute http(s) URL")
	case len(base) > MaxBaseURLBytes:
		return core.Fieldf("uss_base_url", "longer than %d bytes", MaxBaseURLBytes)
	}
	return nil
}

// Run discovers, polls and reports until ctx ends: the search and
// subscription loop, the reconciliation of the pollers, the status.
func (d *DP) Run(ctx context.Context) {
	d.init()
	var wg sync.WaitGroup
	wg.Go(func() { d.discoverLoop(ctx) })
	wg.Go(func() { d.statusLoop(ctx) })
	t := time.NewTicker(every(d.ReconcileEvery, DefaultReconcileEvery))
	defer t.Stop()
	for {
		d.Reconcile(ctx)
		select {
		case <-ctx.Done():
			d.stopAll()
			wg.Wait()
			return
		case <-t.C:
		case <-d.wake:
		}
	}
}

func (d *DP) poke() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// areas are the areas now, each with its tiles; ok false while unknown.
func (d *DP) areas() ([]Area, map[string][]geodesy.BBox, bool) {
	if d.Areas == nil {
		return nil, nil, false
	}
	as, ok := d.Areas()
	if !ok {
		return nil, nil, false
	}
	if len(as) > MaxAreas {
		d.Counters.Add(CounterAreaRefused, uint64(len(as)-MaxAreas))
		as = as[:MaxAreas]
	}
	out := make([]Area, 0, len(as))
	tiles := map[string][]geodesy.BBox{}
	d.mu.Lock()
	defer d.mu.Unlock()
	var bad []string
	for _, a := range as {
		key := a.ID + "|" + AreaString(a.Box)
		ts, have := d.tiles[key]
		if !have {
			var ok bool
			if ts, ok = Tile(a.Box, MaxViewDiagonalM); !ok {
				d.Counters.Inc(CounterAreaRefused)
				bad = append(bad, a.ID)
				continue
			}
			if len(d.tiles) > 4*MaxAreas {
				d.tiles = map[string][]geodesy.BBox{}
			}
			d.tiles[key] = ts
		}
		tiles[a.ID] = ts
		out = append(out, a)
	}
	d.areasErr = ""
	if len(bad) > 0 {
		d.areasErr = "areas not tiled (more than " + fmt.Sprint(MaxTiles) + " views, or not a box): " + strings.Join(bad, ", ")
	}
	return out, tiles, true
}

func (d *DP) discoverLoop(ctx context.Context) {
	t := time.NewTicker(every(d.SearchEvery, DefaultSearchEvery))
	defer t.Stop()
	for {
		if d.decision(nil).Enabled {
			d.Discover(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (d *DP) dssClient() (*stdf3411.StdClient, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dss == nil && d.dssCErr == nil {
		d.dss, d.dssCErr = stdf3411.NewClient(strings.TrimRight(d.DSSBaseURL, "/")+"/rid/v2", stdf3411.WithHTTPClient(d.httpClient()))
	}
	return d.dss, d.dssCErr
}

func (d *DP) httpClient() *http.Client {
	if d.HTTP != nil {
		return d.HTTP
	}
	return &http.Client{Timeout: dssTimeout}
}

// bearer adds a token of scope rid.display_provider for target.
func (d *DP) bearer(target string) stdf3411.RequestEditorFn {
	return func(ctx context.Context, req *http.Request) error {
		tok, err := d.Tokens.Token(ctx, target, string(f3411.ScopeDisplayProvider))
		if err != nil {
			return fmt.Errorf("token for %s: %w", target, err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		return nil
	}
}

func (d *DP) dssResult(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	if err == nil {
		d.dssOK, d.dssErr, d.dssDown = now, "", false
		return
	}
	if !d.dssDown {
		d.dssFailed, d.dssDown = now, true
	}
	d.dssErr = clipErr(err)
}

// read reads a bounded answer body.
func read(res *http.Response) ([]byte, error) {
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(res.Body, f3411.MaxMessageBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > f3411.MaxMessageBytes {
		return nil, core.Fieldf("answer", "over %d bytes", f3411.MaxMessageBytes)
	}
	return b, nil
}

// Discover puts our subscriptions and searches every view of every
// area once: the ISAs found replace the ones the last search found.
func (d *DP) Discover(ctx context.Context) {
	d.init()
	areas, tiles, ok := d.areas()
	if !ok {
		return
	}
	c, err := d.dssClient()
	if err != nil {
		d.dssResult(err)
		return
	}
	now := d.now()
	d.subscriptions(ctx, c, areas, now)
	found := map[string]*isa{}
	var errs []error
	for _, a := range areas {
		for _, tile := range tiles[a.ID] {
			isas, err := d.search(ctx, c, tile, now)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			for i := range isas {
				d.addISA(found, &isas[i], tile)
			}
		}
	}
	if len(errs) > 0 {
		// A search that failed keeps what the last one found.
		d.dssResult(errors.Join(errs...))
		return
	}
	d.dssResult(nil)
	d.mu.Lock()
	d.searched = found
	d.mu.Unlock()
	d.poke()
}

// addISA adds tile to the views of the ISA s in m, unless s is ours,
// ended, or not usable.
func (d *DP) addISA(m map[string]*isa, s *f3411.IdentificationServiceArea, tile geodesy.BBox) {
	base := normBase(s.UssBaseUrl)
	switch {
	case d.IsOwn(base):
		d.Counters.Inc(CounterISAOwn)
		return
	case checkBase(base) != nil || s.Id == "":
		d.Counters.Inc(CounterISARefused)
		return
	case !s.TimeEnd.Value.IsZero() && s.TimeEnd.Value.Before(d.now()):
		return
	}
	e := m[s.Id]
	if e == nil {
		if len(m) >= MaxISAs {
			d.Counters.Inc(CounterISAOverBound)
			return
		}
		e = &isa{base: base, end: s.TimeEnd.Value, views: map[string]geodesy.BBox{}}
		m[s.Id] = e
	}
	e.views[ViewString(tile)] = tile
}

func (d *DP) search(ctx context.Context, c *stdf3411.StdClient, tile geodesy.BBox, now time.Time) ([]f3411.IdentificationServiceArea, error) {
	d.Counters.Inc(CounterSearches)
	cctx, cancel := context.WithTimeout(ctx, dssTimeout)
	defer cancel()
	res, err := c.SearchIdentificationServiceAreas(cctx, &f3411.SearchIdentificationServiceAreasParams{
		Area: AreaString(tile), EarliestTime: now.UTC(), LatestTime: now.Add(time.Hour).UTC()}, d.bearer(d.DSSBaseURL))
	if err != nil {
		d.Counters.Inc(CounterSearchFailed)
		return nil, err
	}
	body, err := read(res)
	if err != nil || res.StatusCode != http.StatusOK {
		d.Counters.Inc(CounterSearchFailed)
		return nil, fmt.Errorf("ISA search answered %d: %s", res.StatusCode, clip(string(body)))
	}
	var ans f3411.SearchIdentificationServiceAreasResponse
	if err := json.Unmarshal(body, &ans); err != nil {
		d.Counters.Inc(CounterSearchFailed)
		return nil, core.Fieldf("answer", "not a SearchIdentificationServiceAreasResponse")
	}
	if ans.ServiceAreas == nil {
		return nil, nil
	}
	return *ans.ServiceAreas, nil
}

// SubscriptionID is the stable id of our RID subscription of an area: a
// version 4 UUID shaped hash of our base URL and the area, so a restart
// names the same subscription.
func SubscriptionID(ussBaseURL, areaID string) string {
	sum := sha256.Sum256([]byte("rid|" + normBase(ussBaseURL) + "|" + areaID))
	b := sum[:16]
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// restore reads back, once, the subscriptions an earlier run saved
// (Subscriptions), so that those of areas no longer wanted are deleted
// below like any other; one already known here keeps what this run
// knows. A store that cannot be read is tried again at the next
// discovery, and said so meanwhile.
func (d *DP) restore(ctx context.Context) {
	d.mu.Lock()
	done := d.restored || d.Subscriptions == nil
	d.mu.Unlock()
	if done {
		return
	}
	saved, err := d.Subscriptions.Load(ctx)
	if err != nil {
		what := "the subscriptions of an earlier run are not known (one of an area dropped meanwhile is not deleted)"
		d.Counters.Inc(CounterSubStoreFailed)
		d.mu.Lock()
		d.restoreErr = what + ": " + clipErr(err)
		d.mu.Unlock()
		d.logger().LogAttrs(ctx, slog.LevelWarn, "DSS RID subscription store: "+what, obs.Err(err))
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for id, s := range saved {
		if !d.IsOwn(s.USSBaseURL) {
			continue // another base URL's (a shared bus): not ours to delete
		}
		if _, have := d.subs[id]; !have {
			d.subs[id] = s.state()
			d.Counters.Inc(CounterSubscriptionKept)
		}
	}
	d.restored, d.restoreErr = true, ""
}

// storeFailed counts and keeps a failure of the subscription store.
func (d *DP) storeFailed(ctx context.Context, what string, err error) {
	d.Counters.Inc(CounterSubStoreFailed)
	d.mu.Lock()
	d.storeErr = what + ": " + clipErr(err)
	d.mu.Unlock()
	d.logger().LogAttrs(ctx, slog.LevelWarn, "DSS RID subscription store: "+what, obs.Err(err))
}

// saved records a store write that succeeded.
func (d *DP) saved() {
	d.mu.Lock()
	d.storeErr = ""
	d.mu.Unlock()
}

// subscriptions puts one subscription per area (renewed at 80 % of the
// DSS's 24 h) and deletes those of areas no longer wanted, those of an
// earlier run included (restore). Each answer's ISAs are added to the
// discovery at once.
func (d *DP) subscriptions(ctx context.Context, c *stdf3411.StdClient, areas []Area, now time.Time) {
	if d.USSBaseURL == "" {
		return
	}
	d.restore(ctx)
	window := time.Duration(f3411.NetDSSMaxSubscriptionDurationHours)*time.Hour - subscriptionSlack
	want := map[string]bool{}
	for _, a := range areas {
		id := SubscriptionID(d.USSBaseURL, a.ID)
		want[id] = true
		d.mu.Lock()
		cur := d.subs[id]
		d.mu.Unlock()
		if cur != nil && cur.area == a && cur.end.Sub(now) > time.Duration(float64(window)*(1-renewAt)) {
			continue
		}
		version := ""
		if cur != nil {
			version = cur.version
		}
		st, err := d.putSubscription(ctx, c, id, version, a, now, now.Add(window))
		if err != nil {
			d.Counters.Inc(CounterSubscriptionFail)
			d.logger().LogAttrs(ctx, slog.LevelWarn, "DSS RID subscription not put; tried again", slog.String("subscription_id", id),
				slog.String("area", a.ID), obs.Err(err))
			continue
		}
		d.Counters.Inc(CounterSubscriptionPut)
		d.mu.Lock()
		d.subs[id] = st
		d.mu.Unlock()
		if d.Subscriptions != nil {
			if err := d.Subscriptions.Put(ctx, id, savedOf(d.USSBaseURL, st)); err != nil {
				d.storeFailed(ctx, "a subscription is not saved (a restart would not delete it once its area is dropped)", err)
			} else {
				d.saved()
			}
		}
	}
	d.mu.Lock()
	var gone []string
	for id := range d.subs {
		if !want[id] {
			gone = append(gone, id)
		}
	}
	d.mu.Unlock()
	for _, id := range gone {
		d.mu.Lock()
		st := d.subs[id]
		d.mu.Unlock()
		if !d.deleteSubscription(ctx, c, id, st.version) {
			continue
		}
		d.Counters.Inc(CounterSubscriptionDel)
		d.mu.Lock()
		delete(d.subs, id)
		d.mu.Unlock()
		if d.Subscriptions != nil {
			if err := d.Subscriptions.Delete(ctx, id); err != nil {
				d.storeFailed(ctx, "a deleted subscription is not forgotten", err)
			} else {
				d.saved()
			}
		}
	}
}

// deleteSubscription deletes one subscription at the DSS: true once it
// is gone (deleted, or not there). A version the DSS no longer holds (a
// saved one another run renewed) is read again and the delete retried
// once.
func (d *DP) deleteSubscription(ctx context.Context, c *stdf3411.StdClient, id, version string) bool {
	auth := d.bearer(d.DSSBaseURL)
	del := func(version string) int {
		cctx, cancel := context.WithTimeout(ctx, dssTimeout)
		defer cancel()
		res, err := c.DeleteSubscription(cctx, id, version, auth)
		if err != nil {
			return 0
		}
		_, _ = read(res)
		return res.StatusCode
	}
	code := del(version)
	if code == http.StatusConflict {
		cctx, cancel := context.WithTimeout(ctx, dssTimeout)
		g, err := c.GetSubscription(cctx, id, auth)
		cancel()
		if err != nil {
			return false
		}
		body, _ := read(g)
		var ans f3411.GetSubscriptionResponse
		switch {
		case g.StatusCode == http.StatusNotFound:
			return true
		case g.StatusCode != http.StatusOK || json.Unmarshal(body, &ans) != nil || ans.Subscription.Version == "":
			return false
		}
		code = del(ans.Subscription.Version)
	}
	return code == http.StatusOK || code == http.StatusNotFound
}

// putSubscription creates (version "") or updates the subscription; a
// create the DSS refuses because it exists (a restart) reads its
// version and updates it.
func (d *DP) putSubscription(ctx context.Context, c *stdf3411.StdClient, id, version string, a Area, start, end time.Time) (*subState, error) {
	ext := f3411.Volume4D{Volume: boxVolume(a.Box), TimeStart: &f3411.Time{Format: f3411.RFC3339, Value: start.UTC()},
		TimeEnd: &f3411.Time{Format: f3411.RFC3339, Value: end.UTC()}}
	auth := d.bearer(d.DSSBaseURL)
	put := func(version string) (*http.Response, error) {
		cctx, cancel := context.WithTimeout(ctx, dssTimeout)
		defer cancel()
		var res *http.Response
		var err error
		if version == "" {
			res, err = c.CreateSubscription(cctx, id, f3411.CreateSubscriptionParameters{Extents: ext, UssBaseUrl: d.USSBaseURL}, auth)
		} else {
			res, err = c.UpdateSubscription(cctx, id, version, f3411.UpdateSubscriptionParameters{Extents: ext, UssBaseUrl: d.USSBaseURL}, auth)
		}
		if err != nil {
			return nil, err
		}
		body, rerr := read(res)
		if rerr != nil {
			return nil, rerr
		}
		res.Body = io.NopCloser(strings.NewReader(string(body)))
		return res, nil
	}
	res, err := put(version)
	if err == nil && (res.StatusCode == http.StatusConflict || res.StatusCode == http.StatusNotFound) {
		// Exists already (a create after a restart) or no longer (an
		// update of one the DSS dropped): read it, or create it anew.
		v := ""
		cctx, cancel := context.WithTimeout(ctx, dssTimeout)
		g, gerr := c.GetSubscription(cctx, id, auth)
		cancel()
		if gerr == nil {
			body, _ := read(g)
			var ans f3411.GetSubscriptionResponse
			if g.StatusCode == http.StatusOK && json.Unmarshal(body, &ans) == nil {
				v = ans.Subscription.Version
			}
		}
		res, err = put(v)
	}
	if err != nil {
		d.dssResult(err)
		return nil, err
	}
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		if res.StatusCode >= 500 || res.StatusCode == http.StatusTooManyRequests {
			d.dssResult(fmt.Errorf("subscription answered %d", res.StatusCode))
		}
		return nil, fmt.Errorf("subscription answered %d: %s", res.StatusCode, clip(string(body)))
	}
	var ans f3411.PutSubscriptionResponse
	if err := json.Unmarshal(body, &ans); err != nil || ans.Subscription.Version == "" {
		return nil, core.Fieldf("answer", "not a PutSubscriptionResponse")
	}
	if ans.ServiceAreas != nil && len(*ans.ServiceAreas) > 0 {
		// The ISAs in the area now: searched view by view at once.
		d.poke()
	}
	return &subState{area: a, version: ans.Subscription.Version, end: end}, nil
}

// notified is the ISAs the notifications describe: each one's views are
// the tiles of the areas its extents touch; a deleted ISA is listed with
// no views, so it is removed whatever a search last found.
func (d *DP) notified(tiles map[string][]geodesy.BBox) map[string]*isa {
	out := map[string]*isa{}
	if d.Notifications == nil {
		return out
	}
	ns, ok := d.Notifications()
	if !ok {
		return out
	}
	now := d.now()
	for _, id := range slices.Sorted(maps.Keys(ns)) {
		n := ns[id]
		if n.Deleted || n.ServiceArea == nil {
			out[id] = &isa{}
			continue
		}
		base := normBase(n.ServiceArea.UssBaseUrl)
		if d.IsOwn(base) || checkBase(base) != nil || (!n.ServiceArea.TimeEnd.Value.IsZero() && n.ServiceArea.TimeEnd.Value.Before(now)) {
			continue
		}
		e := &isa{base: base, end: n.ServiceArea.TimeEnd.Value, views: map[string]geodesy.BBox{}}
		var box *geodesy.BBox
		if n.Extents != nil {
			if b, _, _, err := f3411.Volume4DToZonesEnvelope(*n.Extents); err == nil {
				box = &b
			}
		}
		for _, ts := range tiles {
			for _, t := range ts {
				if box == nil || Overlaps(*box, t) {
					e.views[ViewString(t)] = t
				}
			}
		}
		if len(out) < MaxISAs {
			out[id] = e
		}
	}
	return out
}

// Reconcile matches the pollers to the ISAs and the switches: every
// view of every peer ISA in force is polled, unless network_rid or the
// peer is switched off; a poller no longer wanted is stopped at once.
func (d *DP) Reconcile(ctx context.Context) {
	d.init()
	_, tiles, _ := d.areas()
	notes := d.notified(tiles)
	now := d.now()
	d.mu.Lock()
	merged := map[string]*isa{}
	for id, e := range d.searched {
		merged[id] = e
	}
	for id, e := range notes {
		if e.base == "" {
			delete(merged, id)
			continue
		}
		merged[id] = e
	}
	d.isas = merged
	d.mu.Unlock()
	want := map[pollKey]geodesy.BBox{}
	if d.decision(nil).Enabled {
		for _, e := range merged {
			if !e.end.IsZero() && e.end.Before(now) {
				continue
			}
			base := e.base
			if !d.decision(&base).Enabled {
				continue
			}
			for v, b := range e.views {
				want[pollKey{base, v}] = b
			}
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, p := range d.pollers {
		if _, ok := want[k]; !ok {
			p.cancel()
			delete(d.pollers, k)
			d.Counters.Inc(CounterPollerStopped)
		}
	}
	for _, k := range slices.SortedFunc(maps.Keys(want), func(a, b pollKey) int {
		return strings.Compare(a.base+" "+a.view, b.base+" "+b.view)
	}) {
		if _, ok := d.pollers[k]; ok {
			continue
		}
		if len(d.pollers) >= MaxPollers {
			d.Counters.Inc(CounterPollersOverBound)
			break
		}
		pctx, cancel := context.WithCancel(ctx)
		p := &poller{key: k, box: want[k], cancel: cancel, done: make(chan struct{})}
		d.pollers[k] = p
		if d.peers[k.base] == nil {
			d.peers[k.base] = &peerState{}
		}
		d.Counters.Inc(CounterPollerStarted)
		go d.poll(pctx, p)
	}
}

func (d *DP) stopAll() {
	d.mu.Lock()
	ps := slices.Collect(maps.Values(d.pollers))
	d.pollers = map[pollKey]*poller{}
	d.mu.Unlock()
	for _, p := range ps {
		p.cancel()
		<-p.done
	}
}

// Pollers is the number of pollers running.
func (d *DP) Pollers() int {
	d.init()
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.pollers)
}

func (d *DP) peerClient(base string) (*stdf3411.StdClient, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if c := d.clients[base]; c != nil {
		return c, nil
	}
	c, err := stdf3411.NewClient(base, stdf3411.WithHTTPClient(d.httpClient()))
	if err != nil {
		return nil, err
	}
	if len(d.clients) > MaxPollers {
		d.clients = map[string]*stdf3411.StdClient{}
	}
	d.clients[base] = c
	return c, nil
}

// poll is one poller's loop: one request in flight, every Every (every
// SlowEvery while the peer is slow) until it is stopped.
func (d *DP) poll(ctx context.Context, p *poller) {
	defer close(p.done)
	for {
		d.PollOnce(ctx, p.key.base, p.box)
		d.mu.Lock()
		slow := d.peers[p.key.base] != nil && d.peers[p.key.base].slow()
		d.mu.Unlock()
		wait := every(d.Every, DefaultEvery)
		if slow {
			wait = every(d.SlowEvery, DefaultSlowEvery)
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// PollOnce polls one view of one peer: GET /uss/flights?view= within
// PollDeadline; the flights of a usable answer are published, the peer's
// answer recorded either way.
func (d *DP) PollOnce(ctx context.Context, base string, view geodesy.BBox) {
	d.init()
	d.Counters.Inc(CounterPolls)
	start := d.now()
	err := d.pollOnce(ctx, base, view)
	if ctx.Err() != nil {
		return
	}
	took := d.now().Sub(start)
	d.mu.Lock()
	defer d.mu.Unlock()
	ps := d.peers[base]
	if ps == nil {
		ps = &peerState{}
		d.peers[base] = ps
	}
	ps.polls++
	now := d.now()
	if err != nil {
		if errors.Is(err, errRefused) {
			ps.refused++
		}
		d.Counters.Inc(CounterPollFailed)
		if !ps.down {
			ps.since, ps.down = now, true
		}
		ps.failed, ps.lastErr = now, clipErr(err)
		return
	}
	ps.ok, ps.lastErr, ps.down = now, "", false
	ps.latencies = append(ps.latencies, took)
	if len(ps.latencies) > SlowWindow {
		ps.latencies = ps.latencies[len(ps.latencies)-SlowWindow:]
	}
}

var errRefused = errors.New("answer refused")

func (d *DP) pollOnce(ctx context.Context, base string, view geodesy.BBox) error {
	c, err := d.peerClient(base)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, PollDeadline)
	defer cancel()
	res, err := c.SearchFlights(cctx, &f3411.SearchFlightsParams{View: ViewString(view)}, d.bearer(base))
	if err != nil {
		return err
	}
	body, err := read(res)
	rx := d.now()
	if err != nil {
		d.Counters.Inc(CounterPollRefused)
		return fmt.Errorf("%w: %w", errRefused, err)
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("GET /uss/flights answered %d", res.StatusCode)
	}
	ans, err := f3411.UnmarshalGetFlightsResponse(body)
	if err != nil {
		d.Counters.Inc(CounterPollRefused)
		return fmt.Errorf("%w: %w", errRefused, err)
	}
	if ans.Flights != nil && len(*ans.Flights) > d.policy().PeerFlightsMax {
		d.Counters.Inc(CounterOverMax)
		d.logger().LogAttrs(ctx, slog.LevelWarn, "peer answer over peer_flights_max_count refused whole", slog.String("peer", base),
			slog.Int("flights", len(*ans.Flights)), slog.Int("max", d.policy().PeerFlightsMax))
		return fmt.Errorf("%w: %d flights, more than peer_flights_max_count %d", errRefused, len(*ans.Flights), d.policy().PeerFlightsMax)
	}
	if ans.NoIsasPresent != nil && *ans.NoIsasPresent {
		d.Counters.Inc(CounterNoISAs)
	}
	if ans.Flights == nil {
		return nil
	}
	respTS := ans.Timestamp.Value
	for i := range *ans.Flights {
		d.publishFlight(ctx, base, &(*ans.Flights)[i], respTS, rx)
	}
	return nil
}

// publishFlight publishes one flight of a peer's answer received at rx,
// unless it is an echo of one of ours.
func (d *DP) publishFlight(ctx context.Context, base string, f *f3411.RIDFlight, respTS, rx time.Time) {
	d.Counters.Inc(CounterFlights)
	if d.Own != nil && d.Own.OwnFlight(f.Id) {
		d.Counters.Inc(CounterEchoOwnFlight)
		d.mu.Lock()
		if ps := d.peers[base]; ps != nil {
			ps.echoes++
		}
		d.mu.Unlock()
		return
	}
	m, subject, err := FlightTrack(base, f, respTS, rx, Conv{Resolver: d.Resolver, Geoid: d.Geoid, Policy: d.policy()})
	switch {
	case errors.Is(err, ErrNoState):
		d.Counters.Inc(CounterFlightNoState)
		return
	case errors.Is(err, ErrTooOld):
		d.Counters.Inc(CounterFlightTooOld)
		return
	case err != nil:
		d.Counters.Inc(CounterFlightRefused)
		return
	}
	if m.TimeSource == core.TimeReceiver {
		d.Counters.Inc(CounterFlightAtReceipt)
	}
	if err := d.Sink.Publish(ctx, subject, m); err != nil {
		d.Counters.Inc(CounterPublishFailed)
		return
	}
	d.Counters.Inc(CounterPublished)
	d.mu.Lock()
	if ps := d.peers[base]; ps != nil {
		ps.published++
	}
	d.mu.Unlock()
}

// Statuses are the src.v1 bodies now: network_rid as a whole and each
// peer polled, down since its first failure while it does not answer,
// disabled while switched off.
func (d *DP) Statuses() []sources.StatusBody {
	d.init()
	now := d.now()
	dec := d.decision(nil)
	unavailable := time.Duration(d.policy().PeerUnavailableS * float64(time.Second))
	d.mu.Lock()
	defer d.mu.Unlock()
	agg := sources.StatusBody{Source: SourceNetworkRID, Counters: map[string]uint64{}}
	var since time.Time
	down := 0
	out := make([]sources.StatusBody, 0, len(d.peers))
	for _, base := range slices.Sorted(maps.Keys(d.peers)) {
		ps := d.peers[base]
		inst := base
		b := sources.StatusBody{Source: SourceNetworkRID, SourceInstance: &inst,
			Counters: map[string]uint64{"accepted": ps.published, "refused": ps.refused, "polls": ps.polls, "echo_own_flight": ps.echoes}}
		if !ps.ok.IsZero() {
			v := max(0, now.Sub(ps.ok).Seconds())
			b.AgeS = &v
		}
		polled := false
		for k := range d.pollers {
			if k.base == base {
				polled = true
				break
			}
		}
		disabled := b.Disabled(d.decision(&inst))
		if !polled && !disabled && ps.forgotten(now, unavailable) {
			// No longer polled (its ISAs ended) for peer_unavailable_s
			// since its last answer or failure: forgotten, answering or
			// not, so a peer nobody polls never keeps network_rid stale.
			delete(d.peers, base)
			continue
		}
		if polled {
			ps.idle = time.Time{}
		}
		switch {
		case disabled:
			b.Since, b.Detail = bus.Stamp{Time: now.UTC()}, "network_rid is switched off for this peer: not polled"
		case ps.down:
			b.State, b.Since = sources.StateDown, bus.Stamp{Time: ps.since.UTC()}
			b.Detail = "the peer does not answer: " + ps.lastErr
			down++
		case ps.ok.IsZero():
			b.State, b.Since, b.Detail = sources.StateUnknown, bus.Stamp{Time: now.UTC()}, "not answered yet"
		default:
			b.State, b.Since, b.Detail = sources.StateLive, bus.Stamp{Time: ps.ok.UTC()}, "answering"
			if ps.slow() {
				b.Detail = fmt.Sprintf("slow (over %s more than once in its last %d answers): polled at 0.5 Hz", SlowAfter, SlowWindow)
			}
		}
		agg.Counters["accepted"] += ps.published
		agg.Counters["refused"] += ps.refused
		out = append(out, b)
	}
	switch {
	case agg.Disabled(dec):
		if d.offSince.IsZero() {
			d.offSince = now
		}
		since, agg.Detail = d.offSince, "network_rid is switched off: no peer is polled, peer flights are not shown"
	case d.dssOK.IsZero() && d.dssFailed.IsZero():
		agg.State, since, agg.Detail = sources.StateUnknown, d.started, "peer discovery has not run yet (no area of interest known)"
	case d.dssDown:
		agg.State, since = sources.StateStale, d.dssFailed
		agg.Detail = "the DSS cannot be searched: the peers known before are polled, new ones are not found (" + d.dssErr + ")"
	case down > 0:
		agg.State, agg.Detail = sources.StateStale, fmt.Sprintf("%d of %d peers do not answer", down, len(out))
	default:
		agg.State, agg.Detail = sources.StateLive, fmt.Sprintf("%d peers polled through %d views", len(out), len(d.pollers))
	}
	if d.areasErr != "" {
		agg.Detail += "; " + d.areasErr
	}
	for _, e := range []string{d.restoreErr, d.storeErr} {
		if e != "" {
			agg.Detail += "; " + e
		}
	}
	if dec.Enabled {
		d.offSince = time.Time{}
	}
	if agg.State != d.agg {
		d.agg, d.aggSince = agg.State, now
	}
	if since.IsZero() {
		since = d.aggSince
	}
	agg.Since = bus.Stamp{Time: since.UTC()}
	return append([]sources.StatusBody{agg}, out...)
}

func (d *DP) statusLoop(ctx context.Context) {
	t := time.NewTicker(every(d.StatusEvery, DefaultStatusEvery))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if d.Sink == nil {
			continue
		}
		now := d.now()
		for _, b := range d.Statuses() {
			if err := sources.PublishStatus(ctx, d.Sink, Producer, now, b); err != nil {
				d.Counters.Inc(CounterStatusFailed)
			}
		}
	}
}

// Probe is the readiness of the Display Provider (network_rid on
// /readyz): the state of network_rid as a whole with what it says.
func (d *DP) Probe(context.Context) (obs.State, string) {
	all := d.Statuses()
	a := all[0]
	since := a.Since.UTC().Format(time.RFC3339)
	switch a.State {
	case sources.StateLive:
		return obs.StateUp, a.Detail
	case sources.StateUnknown:
		return obs.StateUnknown, a.Detail
	}
	return obs.StateDegraded, a.State + " since " + since + ": " + a.Detail
}

// ErrDetailsViewTooLarge refuses a details request for a view over
// NetDetailsMaxDisplayAreaDiagonalKm.
var ErrDetailsViewTooLarge = errors.New("details are given for a view of at most 2 km")

// Details fetches the details of one peer flight for a view the console
// shows (GET {base}/uss/flights/{id}/details), only when that view's
// diagonal is at most 2 km (02 F7); never on the 1 Hz path. The answer
// is read bounded and returned as the peer gave it.
func (d *DP) Details(ctx context.Context, base, id string, view geodesy.BBox) (json.RawMessage, error) {
	d.init()
	if DiagonalM(view) > MaxDetailsDiagonalM {
		return nil, ErrDetailsViewTooLarge
	}
	// The peer's instance is its normalised base, as the pollers and the
	// switches key it.
	base = normBase(base)
	if !d.decision(&base).Enabled {
		return nil, errors.New("network_rid is switched off for this peer")
	}
	c, err := d.peerClient(base)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, f3411.NetDpDetailsResponse99thPercentileSeconds*time.Second)
	defer cancel()
	res, err := c.GetFlightDetails(cctx, id, d.bearer(base))
	if err != nil {
		return nil, err
	}
	body, err := read(res)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("details answered %d", res.StatusCode)
	}
	var probe f3411.GetFlightDetailsResponse
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, core.Fieldf("answer", "not a GetFlightDetailsResponse")
	}
	return body, nil
}
