package monitor

import (
	"context"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/geodesy"
	"github.com/rootxkit/uspace-core/geoid"
	"github.com/rootxkit/uspace-core/identify"

	"github.com/rootxkit/uspace-ussp/internal/app/proc"
	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/cis"
	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/manned"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/peers"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/telemetry"
	"github.com/rootxkit/uspace-ussp/internal/traffic"
)

// Readiness dependencies of the manned and peer inputs (brief WP-14).
const (
	DepANSPFeed   = manned.SourceANSPFeed
	DepAdsbRx     = manned.SourceAdsbRx
	DepNetworkRID = peers.SourceNetworkRID
)

// Bounds of the own-flight echo guard (E-10).
const (
	// ownSeenFor is how long a flight seen on trk.v1 counts as one of
	// ours for the echo guard: F3411's 60 s window, the longest a peer
	// serves it back.
	ownSeenFor = 60 * time.Second
	// maxOwnSeen bounds the flights remembered; past it the oldest go.
	maxOwnSeen = 100_000
	// areasEvery is how often the areas of interest are rebuilt from
	// cis_current.
	areasEvery = 5 * time.Second
)

// ownFlights is the echo guard's view of this USSP's own flights (PLAN
// §15 Q23): the flights of the active intents (intent_active) with
// their declared UA registration, and the flights seen on trk.v1 in the
// last minute with their last sample. It implements manned.Own and
// peers.Own.
type ownFlights struct {
	intents interface {
		Snapshot() (map[string]intentBody, float64, bool)
	}
	// policy gives the co-location bounds (echo_colocation_m and
	// echo_colocation_s); the defaults without it.
	policy func() policy.Values
	now    func() time.Time

	mu   sync.Mutex
	seen map[string]ownSeen
}

// ownSeen is one own flight as trk.v1 last showed it: when the monitor
// took it (the 60 s window of OwnFlight) and where the sample placed
// the flight with its ingest times, which the co-location of EchoOf
// judges.
type ownSeen struct {
	at    time.Time
	pos   core.LatLon
	times core.Times
}

func (o *ownFlights) clock() time.Time {
	if o.now != nil {
		return o.now()
	}
	return time.Now()
}

func (o *ownFlights) values() policy.Values {
	if o.policy == nil {
		return policy.Defaults()
	}
	return o.policy()
}

// Seen records one sample of one of this USSP's flights on trk.v1.
func (o *ownFlights) Seen(in traffic.Input) {
	now := o.clock()
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.seen == nil {
		o.seen = map[string]ownSeen{}
	}
	if _, ok := o.seen[in.FlightID]; !ok && len(o.seen) >= maxOwnSeen {
		for id, s := range o.seen {
			if now.Sub(s.at) > ownSeenFor {
				delete(o.seen, id)
			}
		}
		if len(o.seen) >= maxOwnSeen {
			return
		}
	}
	o.seen[in.FlightID] = ownSeen{at: now, pos: in.Position, times: in.Times}
}

// active are the intents that have a flight now.
func (o *ownFlights) active() []intentBody {
	if o.intents == nil {
		return nil
	}
	m, _, _ := o.intents.Snapshot()
	out := make([]intentBody, 0, len(m))
	for k := range m {
		if f := m[k].FlightID; f != nil && *f != "" {
			out = append(out, m[k])
		}
	}
	return out
}

// OwnFlight implements peers.Own: a RID flight id that is the flight id
// of an active intent or of a flight seen on trk.v1 in the last minute.
func (o *ownFlights) OwnFlight(id string) bool {
	now := o.clock()
	o.mu.Lock()
	s, ok := o.seen[id]
	o.mu.Unlock()
	if ok && now.Sub(s.at) <= ownSeenFor {
		return true
	}
	act := o.active()
	for i := range act {
		if *act[i].FlightID == id {
			return true
		}
	}
	return false
}

// EchoOf implements manned.Own: the flight of an active intent whose
// declared UA registration (Annex IV item 10) is the record's
// registration or callsign, and whose live trk.v1 track places it
// within echo_colocation_m of the record (PLAN §15 Q23, Q25). The
// co-location is uspace-core's spoofing guard (identify.JudgeFleet, the
// serial_conflict rule of spec 04 §3.2): the flight's last sample is
// live when received at most echo_colocation_s ago, captured at most
// that long before its receipt, and not backlog; within the distance
// the record is withheld as the echo. The mark alone never hides an
// aircraft: a record with the mark but away from the flight, with the
// flight's track quiet or not seen here, or with a guard that cannot
// judge, is no echo and is shown as a second aircraft (a false alert,
// never a missed one). No intent declares a 24-bit address, so a record
// that carries neither mark is no echo.
func (o *ownFlights) EchoOf(_ string, callsign, registration *string, at core.LatLon) (string, bool) {
	var keys []string
	for _, s := range []*string{registration, callsign} {
		if s != nil {
			if k := manned.NormRegistration(*s); k != "" {
				keys = append(keys, k)
			}
		}
	}
	if len(keys) == 0 {
		return "", false
	}
	act := o.active()
	pol := o.values()
	now := o.clock()
	for i := range act {
		b := &act[i]
		if b.UARegistration == nil || !slices.Contains(keys, manned.NormRegistration(*b.UARegistration)) {
			continue
		}
		o.mu.Lock()
		s, ok := o.seen[*b.FlightID]
		o.mu.Unlock()
		if !ok {
			continue
		}
		pos := s.pos
		row := identify.AuthRow{HeardAtS: s.times.RxTS.Sub(now).Seconds(), Pos: &pos, Backlog: s.times.Backlog,
			BehindS: s.times.RxTS.Sub(s.times.CapturedAt).Seconds()}
		r := identify.JudgeFleet(identify.FleetInput{SerialIsOurs: true, Rows: []identify.AuthRow{row}, Broadcast: at,
			LiveForS: pol.EchoColocationS, SpoofDistanceM: pol.EchoColocationM})
		if r.Verdict == identify.VerdictWithhold && r.Problem == nil && r.ApartM != nil {
			return *b.FlightID, true
		}
	}
	return "", false
}

// ParseBBox reads USSP_TRAFFIC_INPUT_BBOX: min_lng,min_lat,max_lng,max_lat
// in WGS84 degrees; ok false for an empty value.
func ParseBBox(s string) (b geodesy.BBox, ok bool, err error) {
	if strings.TrimSpace(s) == "" {
		return geodesy.BBox{}, false, nil
	}
	parts := strings.Split(s, ",")
	if len(parts) != 4 {
		return geodesy.BBox{}, false, core.Fieldf("USSP_TRAFFIC_INPUT_BBOX", "must be min_lng,min_lat,max_lng,max_lat")
	}
	var v [4]float64
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil || !core.IsFinite(f) {
			return geodesy.BBox{}, false, core.Fieldf("USSP_TRAFFIC_INPUT_BBOX", "value %d is not a number", i+1)
		}
		v[i] = f
	}
	b = geodesy.BBox{MinLon: v[0], MinLat: v[1], MaxLon: v[2], MaxLat: v[3]}
	if b.MinLon < -180 || b.MaxLon > 180 || b.MinLat < -90 || b.MaxLat > 90 || b.MinLon > b.MaxLon || b.MinLat > b.MaxLat {
		return geodesy.BBox{}, false, core.Fieldf("USSP_TRAFFIC_INPUT_BBOX", "not a box within WGS84")
	}
	return b, true, nil
}

// cisAreas are the U-space airspaces of cis_current (the uspace_airspace
// dataset's USPACE features), each as its box, rebuilt at most every
// areasEvery. ok is false while cis_current was never read.
type cisAreas struct {
	m interface {
		Snapshot() (map[string]telemetry.CISValue, float64, bool)
	}

	mu    sync.Mutex
	at    time.Time
	areas []peers.Area
	ok    bool
}

func (c *cisAreas) get() ([]peers.Area, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at.IsZero() && time.Since(c.at) < areasEvery {
		return c.areas, c.ok
	}
	c.at = time.Now()
	vals, _, loaded := c.m.Snapshot()
	if !loaded {
		c.areas, c.ok = nil, false
		return nil, false
	}
	boxes := map[string]geodesy.BBox{}
	for _, v := range vals {
		if v.Cell == nil {
			continue
		}
		for i := range v.Cell.Zones {
			z := &v.Cell.Zones[i]
			if z.Dataset != string(cis.USpaceAirspace) || z.Type != string(core.ZoneUSpace) {
				continue
			}
			if _, done := boxes[z.Identifier]; done {
				continue
			}
			zs, err := cis.FeatureZones(z.Feature)
			if err != nil || len(zs) == 0 {
				continue
			}
			b := zs[0].BBox
			for _, x := range zs[1:] {
				b = union(b, x.BBox)
			}
			boxes[z.Identifier] = b
		}
	}
	out := make([]peers.Area, 0, len(boxes))
	for _, id := range slices.Sorted(maps.Keys(boxes)) {
		out = append(out, peers.Area{ID: id, Box: boxes[id]})
	}
	c.areas, c.ok = out, true
	return out, true
}

// slugRe is the schema's source_instance of a manned track.
var slugRe = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

func union(a, b geodesy.BBox) geodesy.BBox {
	return geodesy.BBox{MinLat: min(a.MinLat, b.MinLat), MinLon: min(a.MinLon, b.MinLon),
		MaxLat: max(a.MaxLat, b.MaxLat), MaxLon: max(a.MaxLon, b.MaxLon)}
}

// inputAreas are the areas of the peer Display Provider (each U-space
// airspace padded by peer_subscription_margin_m, as WP-13's
// subscriptions, or the configured box as it is) and the box of the
// ANSP's stream (their union padded by manned_margin_m, or the
// configured box).
type inputAreas struct {
	configured *geodesy.BBox
	cis        *cisAreas
	policy     func() policy.Values
}

func (a *inputAreas) peerAreas() ([]peers.Area, bool) {
	if a.configured != nil {
		return []peers.Area{{ID: "configured", Box: *a.configured}}, true
	}
	if a.cis == nil {
		return nil, false
	}
	as, ok := a.cis.get()
	if !ok {
		return nil, false
	}
	m := a.policy().PeerSubscriptionMarginM
	out := make([]peers.Area, len(as))
	for i, x := range as {
		out[i] = peers.Area{ID: x.ID, Box: x.Box.PadM(m)}
	}
	return out, true
}

func (a *inputAreas) mannedBBox() (geodesy.BBox, bool) {
	if a.configured != nil {
		return *a.configured, true
	}
	if a.cis == nil {
		return geodesy.BBox{}, false
	}
	as, ok := a.cis.get()
	if !ok || len(as) == 0 {
		return geodesy.BBox{}, false
	}
	b := as[0].Box
	for _, x := range as[1:] {
		b = union(b, x.Box)
	}
	return b.PadM(a.policy().MannedMarginM), true
}

// inputs are what startInputs needs of the process beside its runtime.
type inputs struct {
	current func() policy.Record
	gate    manned.Gate
	own     *ownFlights
	und     geoid.Undulator
	cis     *bus.Mirror[telemetry.CISValue]
}

// startInputs starts the manned and peer inputs of brief WP-14 inside the
// monitor process, each a separate Run behind its own source switch and
// /readyz entry (WP-20 may move each to a container of its own, B-16):
// the ANSP's stream (USSP_ANSP_STREAM_URL; mTLS per USSP_MTLS_MODE,
// required without its files refuses the start, naming them), the
// e-conspicuity receiver (USSP_ADSB_SOURCE; a value that does not parse
// refuses the start) and the peer Display Provider (USSP_DSS_BASE_URL
// and USSP_USS_BASE_URL). An input not configured, or without an
// outgoing token client where it needs one, says so on /readyz.
func startInputs(ctx context.Context, rt *proc.Runtime, in inputs) error {
	cfg := rt.Config
	pol := func() policy.Values { return in.current().Values }
	box, hasBox, err := ParseBBox(cfg.TrafficInputBBox)
	if err != nil {
		return err
	}
	areas := &inputAreas{cis: &cisAreas{m: in.cis}, policy: pol}
	if hasBox {
		areas.configured = &box
	}
	tokens, err := proc.OutgoingTokens(cfg)
	if err != nil {
		return err
	}
	counters := &core.Counters{}
	proc.Publish(rt, "manned_peers", counters)
	sink := bus.NewPublisher(rt.Bus, counters)

	// The ANSP's manned-traffic stream (02 F4).
	switch cfg.ANSPStreamURL {
	case "":
		rt.Health.Register(DepANSPFeed, false, func(context.Context) (obs.State, string) {
			return obs.StateDown, "USSP_ANSP_STREAM_URL is not set: no manned traffic is read from the ANSP (unavailable, shown as such)"
		})
	default:
		hc, err := httpx.MTLSClient(httpx.MTLSConfig{Mode: cfg.MTLSMode, CertFile: cfg.MTLSCertFile, KeyFile: cfg.MTLSKeyFile, CAFile: cfg.MTLSCAFile}, 0)
		if err != nil {
			return err
		}
		logger := rt.Logger.With("component", DepANSPFeed)
		off := cfg.MTLSMode == httpx.MTLSOff
		if off {
			logger.Error("USSP_MTLS_MODE=off: the ANSP's manned-traffic stream is read without a client certificate (the lab and staging only)")
		}
		if tokens == nil {
			rt.Health.Register(DepANSPFeed, false, func(context.Context) (obs.State, string) {
				return obs.StateDown, "USSP_ANSP_STREAM_URL is set but no outgoing token client (USSP_TOKEN_ISSUERS, USSP_TOKEN_CLIENT_SECRET_FILE): the stream is not read"
			})
			break
		}
		fc := &core.Counters{}
		proc.Publish(rt, DepANSPFeed, fc)
		f := &manned.ANSPStream{URL: cfg.ANSPStreamURL, Tokens: tokens, HTTP: hc, Sink: sink, Own: in.own, Gate: in.gate, Policy: pol,
			BBox: areas.mannedBBox, Counters: fc, Logger: logger, MTLSOff: off, MTLSOffEvery: time.Duration(cfg.StatusIntervalS) * time.Second}
		rt.Health.Register(DepANSPFeed, false, f.Probe)
		rt.Go(ctx, f.Run)
	}

	// This USSP's own e-conspicuity receiver.
	switch cfg.ADSBSource {
	case "":
		rt.Health.Register(DepAdsbRx, false, func(context.Context) (obs.State, string) {
			return obs.StateDown, "USSP_ADSB_SOURCE is not set: no e-conspicuity receiver (unavailable, shown as such)"
		})
	default:
		if _, _, err := manned.ParseFeed(cfg.ADSBSource); err != nil {
			return err
		}
		if !slugRe.MatchString(cfg.ADSBReceiverID) || len(cfg.ADSBReceiverID) > 64 {
			return core.Fieldf("USSP_ADSB_RECEIVER_ID", "must be a slug of lower-case letters, digits and dashes (the source_instance of track/manned/v1)")
		}
		ec := &core.Counters{}
		proc.Publish(rt, DepAdsbRx, ec)
		e := &manned.Econspicuity{Source: cfg.ADSBSource, ReceiverID: cfg.ADSBReceiverID, HTTP: &http.Client{Timeout: 3 * time.Second},
			Sink: sink, Own: in.own, Gate: in.gate, Policy: pol, Counters: ec, Logger: rt.Logger.With("component", DepAdsbRx)}
		rt.Health.Register(DepAdsbRx, false, e.Probe)
		rt.Go(ctx, e.Run)
	}

	// The peer Display Provider (F3411).
	switch {
	case cfg.DSSBaseURL == "" || cfg.USSBaseURL == "":
		rt.Health.Register(DepNetworkRID, false, func(context.Context) (obs.State, string) {
			return obs.StateDown, "USSP_DSS_BASE_URL or USSP_USS_BASE_URL is not set: no peer is discovered, other USSPs' flights are not shown"
		})
	case tokens == nil:
		rt.Health.Register(DepNetworkRID, false, func(context.Context) (obs.State, string) {
			return obs.StateDown, "no outgoing token client (USSP_TOKEN_ISSUERS, USSP_TOKEN_CLIENT_SECRET_FILE): no peer is discovered"
		})
	default:
		js := rt.Bus.JetStream()
		pc := &core.Counters{}
		proc.Publish(rt, DepNetworkRID, pc)
		notes := &bus.Mirror[peers.ISANotification]{JS: js, Bucket: bus.BucketISANotifications, Decode: peers.DecodeNotification,
			Counters: pc, Logger: rt.Logger}
		d := &peers.DP{DSSBaseURL: cfg.DSSBaseURL, USSBaseURL: cfg.USSBaseURL, Tokens: tokens, HTTP: &http.Client{Timeout: 5 * time.Second},
			Subscriptions: peers.KVSubscriptions{KV: bus.KVStore{JS: js, Bucket: bus.BucketRIDSubscriptions}},
			Areas:         areas.peerAreas, Notifications: func() (map[string]peers.ISANotification, bool) {
				m, _, ok := notes.Snapshot()
				return m, ok
			},
			Sink: sink, Gate: in.gate, Own: in.own, Geoid: in.und, Policy: pol, Counters: pc, Logger: rt.Logger.With("component", DepNetworkRID)}
		rt.Health.Register(DepNetworkRID, false, d.Probe)
		rt.Go(ctx, notes.Run)
		rt.Go(ctx, d.Run)
	}
	return nil
}
