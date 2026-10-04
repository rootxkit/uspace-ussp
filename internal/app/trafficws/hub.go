package trafficws

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3548"
	"github.com/rootxkit/uspace-core/geodesy"
	"github.com/rootxkit/uspace-core/geoid"
	"github.com/rootxkit/uspace-core/serial"
	coresources "github.com/rootxkit/uspace-core/sources"

	"github.com/rootxkit/uspace-ussp/internal/alerts"
	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/cis"
	"github.com/rootxkit/uspace-ussp/internal/intent"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/telemetry"
	"github.com/rootxkit/uspace-ussp/internal/traffic"
)

// Counters of the hub.
const (
	CounterTrackUnreadable     = "traffic_ws_track_unreadable"
	CounterAlertUnreadable     = "traffic_ws_alert_unreadable"
	CounterSourceUnreadable    = "traffic_ws_source_status_unreadable"
	CounterDeliveryPublished   = "traffic_ws_delivery_published"
	CounterDeliveryFailed      = "traffic_ws_delivery_failed"
	CounterRecordPublished     = "traffic_ws_record_published"
	CounterRecordFailed        = "traffic_ws_record_failed"
	CounterSourcesOverBound    = "traffic_ws_sources_over_bound"
	CounterDeliveriesForgotten = "traffic_ws_deliveries_forgotten"
)

// Bounds and periods of the hub (E-10).
const (
	// MannedMissingAfter is the silence of the manned inputs (man.v1 and
	// the ANSP feed's status) after which the product says manned is
	// unavailable.
	MannedMissingAfter = 10 * time.Second
	// MaxSources bounds the source statuses held.
	MaxSources = 1024
	// MaxDeliveries bounds the deliveries remembered as recorded.
	MaxDeliveries = 100_000
	// SourceStatusStaleAfter drops a source status not refreshed for this
	// long from the status frames (sources publish every 2 s).
	SourceStatusStaleAfter = 30 * time.Second
)

// IntentSource is the intent_active projection (bus.Mirror).
type IntentSource interface {
	Get(id string) (v intent.StateBody, found bool, ageS float64, loaded bool)
}

// BindingSource is the client_bindings projection (bus.Mirror): the
// serial fold keys bound to a client.
type BindingSource interface {
	Get(key string) (v []string, found bool, ageS float64, loaded bool)
}

// Gate is the source switches (internal/sources.Follower).
type Gate interface {
	Query(sourceType string, instanceID *string) coresources.Decision
}

// Publisher is the bus (bus.Publisher).
type Publisher interface {
	Publish(ctx context.Context, subject string, m bus.Enveloped) error
}

// srcStatus is the last source/status/v1 of one source: its state,
// since when (the producer's time) and what it says, received at.
type srcStatus struct {
	source, instance, state, detail string
	since                           time.Time
	body                            json.RawMessage
	at                              time.Time
}

// Hub is traffic-ws's picture of the inputs: the tracks, the alerts, the
// source statuses and the projections, shared by every connection.
type Hub struct {
	Picture  *traffic.Picture
	Book     *traffic.Book
	Policy   func() policy.Record
	Gate     Gate
	Intents  IntentSource
	Bindings BindingSource
	// CIS is the cis_current basis; false when it was never read.
	CIS func() (cis.BasisValue, bool)
	// GateLoaded reports whether the source switches were read.
	GateLoaded func() bool
	// PolicyLoaded reports whether the policy was read.
	PolicyLoaded func() bool
	Pub          Publisher
	// Geoid is the geoid grid (USSP_GEOID_FILE): a manned track's
	// geometric altitude is shown as AMSL through it; nil without one.
	Geoid    geoid.Undulator
	Counters *core.Counters
	Logger   *slog.Logger
	Now      func() time.Time

	mu          sync.Mutex
	start       time.Time
	mannedSeen  time.Time
	srcs        map[string]srcStatus
	alertFeed   bool
	alertSince  time.Time
	alertReason string
	delivered   map[string]bool
	once        sync.Once
}

func (h *Hub) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *Hub) logger() *slog.Logger {
	if h.Logger == nil {
		return obs.Discard()
	}
	return h.Logger
}

func (h *Hub) counters() *core.Counters {
	h.once.Do(func() {
		if h.Counters == nil {
			h.Counters = &core.Counters{}
		}
	})
	return h.Counters
}

func (h *Hub) policy() policy.Record {
	if h.Policy == nil {
		return policy.Record{Values: policy.Defaults()}
	}
	return h.Policy()
}

// Started marks the hub's start (the "since" of an input never seen).
func (h *Hub) Started() {
	h.mu.Lock()
	h.start = h.now()
	h.mu.Unlock()
}

// Enabled implements traffic.SourceGate.
func (h *Hub) Enabled(sourceType, instance string) bool {
	if h.Gate == nil {
		return true
	}
	var inst *string
	if instance != "" {
		inst = &instance
	}
	return h.Gate.Query(sourceType, inst).Enabled
}

// TakeTrack takes one trk.v1 or peer.v1 message.
func (h *Hub) TakeTrack(ns string, data []byte) {
	tr, err := traffic.DecodeTrack(data)
	if err != nil {
		h.counters().Inc(CounterTrackUnreadable)
		return
	}
	h.Picture.Put(traffic.TrackInputOf(ns, tr), data)
}

// TakeManned takes one man.v1 message.
func (h *Hub) TakeManned(data []byte) {
	m, err := traffic.DecodeManned(data)
	if err != nil {
		h.counters().Inc(CounterTrackUnreadable)
		return
	}
	if m.Body.Source == traffic.SourceANSPFeed && m.Body.State == traffic.StateLive {
		h.mu.Lock()
		h.mannedSeen = h.now()
		h.mu.Unlock()
	}
	h.Picture.Put(traffic.MannedInputOf(m, h.Geoid), data)
}

// TakeAlert takes one alrt.v1 message (alert/v1; a delivery record is
// not an alert and is ignored here).
func (h *Hub) TakeAlert(subject string, data []byte) {
	if alerts.SchemaOf(data) != alerts.SchemaAlert {
		return
	}
	m, err := alerts.Decode(data)
	if err != nil {
		h.counters().Inc(CounterAlertUnreadable)
		return
	}
	b := m.Body
	body, err := json.Marshal(b)
	if err != nil {
		h.counters().Inc(CounterAlertUnreadable)
		return
	}
	e := traffic.Entry{AlertID: b.AlertID, Kind: b.Kind, Severity: b.Severity, State: b.State, FlightID: b.FlightID,
		Acked: b.AckedAt != nil, Escalated: b.EscalatedAt != nil, UpdatedAt: b.UpdatedAt, Seen: h.now(), Message: json.RawMessage(data), Body: body}
	if b.IntentID != nil {
		e.IntentID = *b.IntentID
	}
	if s, err := bus.Parse(subject); err == nil {
		e.Cell5 = s.Cell5
	}
	var d struct {
		LoSStartS float64 `json:"los_start_s"`
	}
	if json.Unmarshal(b.Detail, &d) == nil {
		e.LoSStartS = d.LoSStartS
	}
	h.Book.Put(e)
}

// AlertFeed records whether the alrt.v1 feed is open.
func (h *Hub) AlertFeed(open bool, reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if open != h.alertFeed || h.alertSince.IsZero() {
		h.alertSince = h.now()
	}
	h.alertFeed, h.alertReason = open, reason
}

// TakeSourceStatus takes one src.v1 message (source/status/v1).
func (h *Hub) TakeSourceStatus(data []byte) {
	var m struct {
		Schema string `json:"schema"`
		Body   struct {
			Source         string    `json:"source"`
			SourceInstance *string   `json:"source_instance"`
			State          string    `json:"state"`
			Since          time.Time `json:"since"`
			Detail         string    `json:"detail"`
		} `json:"body"`
	}
	if len(data) > bus.TrackMsgBytes || json.Unmarshal(data, &m) != nil || m.Schema != telemetry.SchemaSourceStatus || m.Body.Source == "" {
		h.counters().Inc(CounterSourceUnreadable)
		return
	}
	var raw struct {
		Body json.RawMessage `json:"body"`
	}
	_ = json.Unmarshal(data, &raw)
	inst := ""
	if m.Body.SourceInstance != nil {
		inst = *m.Body.SourceInstance
	}
	key := m.Body.Source + "/" + inst
	now := h.now()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.srcs == nil {
		h.srcs = map[string]srcStatus{}
	}
	if _, ok := h.srcs[key]; !ok && len(h.srcs) >= MaxSources {
		h.counters().Inc(CounterSourcesOverBound)
		return
	}
	h.srcs[key] = srcStatus{source: m.Body.Source, instance: inst, state: m.Body.State, detail: clipDetail(m.Body.Detail),
		since: m.Body.Since, body: raw.Body, at: now}
	if m.Body.Source == traffic.SourceANSPFeed && m.Body.State == "live" {
		h.mannedSeen = now
	}
}

// clipDetail bounds a source's detail as a degraded reason quotes it
// (the product's reason is at most 512 bytes).
func clipDetail(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	if len(s) > 300 {
		s = s[:300]
		for len(s) > 0 && !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return s
}

// srcFreshLocked is the status of source/instance received within
// MannedMissingAfter; false when there is none that recent (h.mu held).
func (h *Hub) srcFreshLocked(source, instance string, now time.Time) (srcStatus, bool) {
	st, ok := h.srcs[source+"/"+instance]
	if !ok || now.Sub(st.at) > MannedMissingAfter {
		return srcStatus{}, false
	}
	return st, true
}

// PeerUnavailable implements traffic.PeerState: the peer whose base URL
// is instance says, through its network_rid status, that it does not
// answer.
func (h *Hub) PeerUnavailable(instance string) bool {
	now := h.now()
	h.mu.Lock()
	defer h.mu.Unlock()
	st, ok := h.srcFreshLocked(traffic.SourceNetworkRID, instance, now)
	return ok && st.state == "down"
}

// Sources are the source statuses a subscriber sees: the manned and
// peer inputs, and for an operator its own client's.
func (h *Hub) Sources(s *Sub) []json.RawMessage {
	now := h.now()
	h.mu.Lock()
	defer h.mu.Unlock()
	keys := slices.Sorted(maps.Keys(h.srcs))
	out := make([]json.RawMessage, 0, len(keys))
	for _, k := range keys {
		st := h.srcs[k]
		if now.Sub(st.at) > SourceStatusStaleAfter {
			continue
		}
		switch {
		case st.source == traffic.SourceANSPFeed || st.source == traffic.SourceAdsbRx || st.source == "network_rid":
		case s.Staff:
		case st.source == telemetry.SourceOperatorWS && st.instance == s.ClientID:
		default:
			continue
		}
		out = append(out, st.body)
	}
	return out
}

// Sub is one subscription: an operator's intent or a staff bbox.
type Sub struct {
	ClientID string
	Staff    bool
	IntentID string
	BBox     *[4]float64
	// Layers are the console/subscribe/v1 layers (nil: all).
	Layers map[string]bool
}

// ParseBBox reads west,south,east,north in WGS84 degrees.
func ParseBBox(s string) (*[4]float64, error) {
	parts := strings.Split(s, ",")
	if len(parts) != 4 {
		return nil, core.Fieldf("bbox", "four numbers west,south,east,north")
	}
	var b [4]float64
	for i, p := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil || !core.IsFinite(v) {
			return nil, core.Fieldf("bbox", "not a number: %q", p)
		}
		b[i] = v
	}
	return checkBBox(b)
}

func checkBBox(b [4]float64) (*[4]float64, error) {
	for i, v := range b {
		if !core.IsFinite(v) {
			return nil, core.Fieldf("bbox", "not finite")
		}
		if i%2 == 0 && (v < -180 || v > 180) || i%2 == 1 && (v < -90 || v > 90) {
			return nil, core.Fieldf("bbox", "out of range")
		}
	}
	if b[1] > b[3] {
		return nil, core.Fieldf("bbox", "south above north")
	}
	return &b, nil
}

// boxesOf is a GeoJSON bbox as WGS84 boxes (two across the antimeridian).
func boxesOf(b [4]float64) []geodesy.BBox {
	if b[0] <= b[2] {
		return []geodesy.BBox{{MinLat: b[1], MaxLat: b[3], MinLon: b[0], MaxLon: b[2]}}
	}
	return []geodesy.BBox{{MinLat: b[1], MaxLat: b[3], MinLon: b[0], MaxLon: 180}, {MinLat: b[1], MaxLat: b[3], MinLon: -180, MaxLon: b[2]}}
}

// RefusalError is a refused subscription, answered before the upgrade.
type RefusalError struct {
	status       int
	slug, detail string
}

func (r *RefusalError) Error() string { return r.slug + ": " + r.detail }

// Owns reports whether clientID may subscribe to the intent: the
// intent's aircraft is bound to the client (client_bindings), as the
// intent's creation required. An unknown intent and another client's
// are the same answer (404, never 403); an unread projection is 503.
func (h *Hub) Owns(clientID, intentID string) (intent.StateBody, *RefusalError) {
	if h.Intents == nil || h.Bindings == nil || !bus.ValidKey(intentID) {
		return intent.StateBody{}, &RefusalError{404, "intent_not_found", "no intent of this client has this id"}
	}
	b, found, _, loaded := h.Intents.Get(intentID)
	if !loaded {
		return intent.StateBody{}, &RefusalError{503, "intent_active_unavailable", "the active intents are not read yet; retry"}
	}
	folds, _, _, bLoaded := h.Bindings.Get(bus.KeyToken(clientID))
	if !bLoaded {
		return intent.StateBody{}, &RefusalError{503, "client_bindings_unavailable", "the client bindings are not read yet; retry"}
	}
	if !found || !slices.Contains(folds, serial.FoldKey(b.UASSerial)) {
		return intent.StateBody{}, &RefusalError{404, "intent_not_found", "no active intent of this client has this id"}
	}
	return b, nil
}

// Area is what a subscription covers now: an intent's volumes padded by
// traffic_radius_m (and its flight wherever it is), or the staff bbox;
// reason says why it covers nothing.
func (h *Hub) Area(s *Sub) (traffic.Area, string, string) {
	v := h.policy().Values
	if s.Staff {
		if s.BBox == nil {
			return traffic.Area{}, "", "no bbox subscribed yet"
		}
		return traffic.Area{Boxes: boxesOf(*s.BBox)}, "", ""
	}
	b, ref := h.Owns(s.ClientID, s.IntentID)
	if ref != nil {
		return traffic.Area{}, "", ref.detail
	}
	a := traffic.Area{}
	for i := range b.Volumes {
		box, _, _, err := f3548.Volume4DToZonesEnvelope(b.Volumes[i])
		if err != nil {
			continue
		}
		a.Boxes = append(a.Boxes, box.PadM(v.TrafficRadiusM))
	}
	flight := ""
	if b.FlightID != nil {
		flight = *b.FlightID
		a.OwnTrack = traffic.NSTrack + ":" + flight
	}
	return a, flight, ""
}

// AlertsFor are the active alerts of a subscription: an operator's
// intent's flight, or the flights in a staff bbox.
func (h *Hub) AlertsFor(s *Sub, a traffic.Area, flightID string, kind string) []traffic.Entry {
	in := map[string]bool{}
	if s.Staff {
		ts := h.Picture.Tracks(a, h.now(), h.policy().Values, nil)
		for i := range ts {
			in[ts[i].Track.TrackID] = true
		}
	}
	return h.Book.Active(func(e *traffic.Entry) bool {
		if kind != "" && e.Kind != kind {
			return false
		}
		if s.Staff {
			return in[e.FlightID]
		}
		return (flightID != "" && e.FlightID == flightID) || (e.IntentID != "" && e.IntentID == s.IntentID)
	})
}

// Degraded are the inputs missing or stale now (SC-22: an empty product
// never looks like an empty sky), by input.
func (h *Hub) Degraded(now time.Time) map[string]traffic.Degraded {
	v := h.policy().Values
	out := map[string]traffic.Degraded{}
	stamp := func(t time.Time) *bus.Stamp {
		if t.IsZero() {
			return nil
		}
		return &bus.Stamp{Time: t.UTC()}
	}
	h.mu.Lock()
	manned, start, feed, feedSince, feedReason := h.mannedSeen, h.start, h.alertFeed, h.alertSince, h.alertReason
	ansp, anspOK := h.srcFreshLocked(traffic.SourceANSPFeed, "", now)
	econ, econOK := h.freshestLocked(traffic.SourceAdsbRx, now)
	peers, peersOK := h.srcFreshLocked(traffic.SourceNetworkRID, "", now)
	peersDown, peersDownSince := h.downLocked(traffic.SourceNetworkRID, now)
	h.mu.Unlock()
	switch {
	case !h.Enabled(traffic.SourceANSPFeed, ""):
		out["manned"] = traffic.Degraded{Input: "manned", Since: stamp(manned), Reason: "the ANSP feed is switched off: manned traffic is not shown"}
	case anspOK && ansp.state == "down":
		out["manned"] = traffic.Degraded{Input: "manned", Since: stamp(ansp.since), Reason: reasonOf("manned traffic unavailable since "+ts(ansp.since), ansp.detail)}
	case anspOK && ansp.state == "stale":
		out["manned"] = traffic.Degraded{Input: "manned", Since: stamp(ansp.since),
			Reason: reasonOf("manned: stale since "+ts(ansp.since)+" (the ANSP's time)", ansp.detail)}
	case anspOK && ansp.state == "disabled":
		out["manned"] = traffic.Degraded{Input: "manned", Since: stamp(ansp.since), Reason: reasonOf("the ANSP feed is switched off since "+ts(ansp.since), ansp.detail)}
	case anspOK && ansp.state == "live":
	case manned.IsZero():
		out["manned"] = traffic.Degraded{Input: "manned", Since: stamp(start),
			Reason: "manned traffic unavailable: no ANSP feed track or status received since traffic-ws started"}
	case now.Sub(manned) > MannedMissingAfter:
		out["manned"] = traffic.Degraded{Input: "manned", Since: stamp(manned),
			Reason: fmt.Sprintf("manned traffic unavailable: the ANSP feed sent nothing for %.0f s", now.Sub(manned).Seconds())}
	}
	// e-conspicuity: a required input outside ATC service (02 F4); no
	// receiver is shown as such, never as an empty sky.
	switch {
	case !h.Enabled(traffic.SourceAdsbRx, ""):
		out["econspicuity"] = traffic.Degraded{Input: "econspicuity", Since: nil, Reason: "the e-conspicuity receiver is switched off: broadcast traffic is not shown"}
	case !econOK:
		out["econspicuity"] = traffic.Degraded{Input: "econspicuity", Since: stamp(start),
			Reason: "no e-conspicuity receiver status received: no receiver is configured (USSP_ADSB_SOURCE) or the monitor is not running"}
	case econ.state == "down":
		out["econspicuity"] = traffic.Degraded{Input: "econspicuity", Since: stamp(econ.since),
			Reason: reasonOf("e-conspicuity unavailable since "+ts(econ.since), econ.detail)}
	case econ.state == "stale" || econ.state == "disabled":
		out["econspicuity"] = traffic.Degraded{Input: "econspicuity", Since: stamp(econ.since),
			Reason: reasonOf("e-conspicuity "+econ.state+" since "+ts(econ.since), econ.detail)}
	}
	// Peer traffic through F3411 (internal/peers).
	switch {
	case !h.Enabled(traffic.SourceNetworkRID, ""):
		out["peers"] = traffic.Degraded{Input: "peers", Since: nil, Reason: "network_rid is switched off: other USSPs' flights are not shown"}
	case !peersOK:
		out["peers"] = traffic.Degraded{Input: "peers", Since: stamp(start),
			Reason: "peer traffic unavailable: no network_rid status received (the Display Provider is not running): other USSPs' flights are not shown"}
	case peers.state != "live":
		out["peers"] = traffic.Degraded{Input: "peers", Since: stamp(peers.since), Reason: reasonOf("peer traffic "+peers.state+" since "+ts(peers.since), peers.detail)}
	case peersDown > 0:
		out["peers"] = traffic.Degraded{Input: "peers", Since: stamp(peersDownSince),
			Reason: fmt.Sprintf("%d peer USSPs do not answer since %s: their flights are shown peer_unavailable, then age out", peersDown, ts(peersDownSince))}
	}
	if h.CIS != nil {
		basis, ok := h.CIS()
		switch {
		case !ok:
			out["cis"] = traffic.Degraded{Input: "cis", Since: nil, Reason: "cis_current not read: the CIS version is unknown"}
		default:
			age := basis.CISAgeS + max(0, now.Sub(basis.At).Seconds())
			if basis.Stale || age > v.CISStaleS {
				out["cis"] = traffic.Degraded{Input: "cis", Since: stamp(basis.At),
					Reason: fmt.Sprintf("CIS version %s is %.0f s old (cis_stale_s %.0f)", basis.CISVersion, age, v.CISStaleS)}
			}
		}
	}
	if !feed {
		out["alerts"] = traffic.Degraded{Input: "alerts", Since: stamp(feedSince), Reason: "the alert feed is not open: " + feedReason}
	} else if since := h.Book.Unrefreshed(now, traffic.DefaultUnrefreshedAge, monitored); !since.IsZero() {
		out["monitor"] = traffic.Degraded{Input: "monitor", Since: stamp(since),
			Reason: "active alerts not republished by the monitor: shown with their last numbers"}
	}
	if h.GateLoaded != nil && !h.GateLoaded() {
		out["source_control"] = traffic.Degraded{Input: "source_control", Since: nil, Reason: "the source switches are not read: every source is treated as enabled (B-09)"}
	}
	if h.PolicyLoaded != nil && !h.PolicyLoaded() {
		out["policy"] = traffic.Degraded{Input: "policy", Since: nil, Reason: "the policy is not read: its defaults apply"}
	}
	return out
}

// ts is t as a degraded reason writes it.
func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// reasonOf is a degraded reason: what, and the source's own detail.
func reasonOf(what, detail string) string {
	if detail == "" {
		return what
	}
	return clipDetail(what + ": " + detail)
}

// freshestLocked is the most recently received fresh status of any
// instance of source (h.mu held).
func (h *Hub) freshestLocked(source string, now time.Time) (srcStatus, bool) {
	var best srcStatus
	found := false
	for k := range h.srcs {
		st := h.srcs[k]
		if st.source != source || now.Sub(st.at) > MannedMissingAfter {
			continue
		}
		if !found || st.at.After(best.at) {
			best, found = st, true
		}
	}
	return best, found
}

// downLocked counts the fresh instance statuses of source that are down
// and the earliest since (h.mu held).
func (h *Hub) downLocked(source string, now time.Time) (int, time.Time) {
	n := 0
	var since time.Time
	for k := range h.srcs {
		st := h.srcs[k]
		if st.source != source || st.instance == "" || st.state != "down" || now.Sub(st.at) > MannedMissingAfter {
			continue
		}
		n++
		if since.IsZero() || st.since.Before(since) {
			since = st.since
		}
	}
	return n, since
}

// monitored selects the alerts the monitor republishes every second: a
// restriction_activated notice is api's, published once by the intent's
// projection and republished from api's record (alerts.Service), so an
// open one never says the monitor is late.
func monitored(e *traffic.Entry) bool { return e.Kind != alerts.KindRestrictionActivated }

// Delivered records, once per alert and client, that an alert was sent
// (alert/delivery/v1 on alrt.v1.delivery, recorded by api).
func (h *Hub) Delivered(ctx context.Context, e *traffic.Entry, clientID string) {
	key := e.AlertID + "|" + clientID
	h.mu.Lock()
	if h.delivered == nil || len(h.delivered) >= MaxDeliveries {
		if len(h.delivered) > 0 {
			h.counters().Add(CounterDeliveriesForgotten, uint64(len(h.delivered)))
		}
		h.delivered = map[string]bool{}
	}
	done := h.delivered[key]
	h.delivered[key] = true
	h.mu.Unlock()
	if done || h.Pub == nil || e.Cell5 == "" {
		return
	}
	alertID := e.AlertID
	subject, err := bus.Alrt(alerts.KindDelivery, e.Cell5, alertID)
	if err != nil {
		h.counters().Inc(CounterDeliveryFailed)
		return
	}
	now := h.now()
	d := &alerts.Delivery{Envelope: bus.SystemEnvelope(alerts.SchemaDelivery, traffic.ProducerWS, now),
		Body: alerts.DeliveryBody{AlertID: alertID, ClientID: clientID, SentAt: now.UTC()}}
	// Off the connection's loop: a slow bus never delays a frame (C-13).
	go func() {
		pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		if err := h.Pub.Publish(pctx, subject, d); err != nil {
			h.counters().Inc(CounterDeliveryFailed)
			h.logger().LogAttrs(ctx, slog.LevelWarn, "alert delivery not recorded", slog.String("alert_id", alertID), obs.Err(err))
			return
		}
		h.counters().Inc(CounterDeliveryPublished)
	}()
}
