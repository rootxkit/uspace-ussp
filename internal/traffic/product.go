package traffic

import (
	"encoding/json"
	"hash/maphash"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/geodesy"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/policy"
)

// Schemas of traffic-ws.
const (
	// SchemaProduct is traffic/product/v1 (schemas/traffic/product/v1).
	SchemaProduct = "traffic/product/v1"
	// ProducerWS is the envelope producer of traffic-ws.
	ProducerWS = "ussp/traffic-ws"
)

// Counters of the picture.
const (
	CounterPictureTaken     = "picture_samples"
	CounterPictureOlder     = "picture_older_than_held"
	CounterPictureOverBound = "picture_over_bound"
	CounterPictureDropped   = "picture_dropped_after_silence"
)

// DefaultMaxPictureTracks bounds the picture (E-10): past it a new track
// is refused and counted; a held track is never evicted for room.
const DefaultMaxPictureTracks = 100_000

// SourceGate is the source-control follower (internal/sources).
type SourceGate interface {
	Enabled(sourceType, instance string) bool
}

// held is one track of the picture: its last sample and the message it
// came in (the snapshot carries the message itself).
type held struct {
	in  Input
	raw json.RawMessage
}

// Picture is the latest sample of every track traffic-ws hears (trk.v1,
// peer.v1, man.v1), never overwritten by an older one (T-06), aged at
// read time (Tracks). Safe for concurrent use; a read copies out only
// what its filter selects.
type Picture struct {
	Counters  *core.Counters
	MaxTracks int

	mu     sync.RWMutex
	tracks map[string]*held
	once   sync.Once
}

func (p *Picture) counters() *core.Counters {
	p.once.Do(func() {
		if p.Counters == nil {
			p.Counters = &core.Counters{}
		}
	})
	return p.Counters
}

// Put holds in as its track's latest sample unless it is older than the
// one held; raw is the message it came in.
func (p *Picture) Put(in Input, raw []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tracks == nil {
		p.tracks = map[string]*held{}
	}
	cur, ok := p.tracks[in.ID]
	switch {
	case ok && in.Times.CapturedAt.Before(cur.in.Times.CapturedAt):
		p.counters().Inc(CounterPictureOlder)
		return
	case !ok && len(p.tracks) >= p.max():
		p.counters().Inc(CounterPictureOverBound)
		return
	}
	in.Identification = identStatusOnly(in.Identification)
	p.tracks[in.ID] = &held{in: in, raw: json.RawMessage(slices.Clone(raw))}
	p.counters().Inc(CounterPictureTaken)
}

func (p *Picture) max() int {
	if p.MaxTracks > 0 {
		return p.MaxTracks
	}
	return DefaultMaxPictureTracks
}

// identStatusOnly keeps the status of an identification and nothing
// else (no serial, no operator number reaches a product, rule 8).
func identStatusOnly(id *core.Identification) *core.Identification {
	if id == nil {
		return nil
	}
	return &core.Identification{Status: id.Status}
}

// Sweep drops the tracks last captured before cut (after
// traffic_drop_after_s, having been shown stale all that time).
func (p *Picture) Sweep(cut time.Time) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for id, h := range p.tracks {
		if h.in.Times.CapturedAt.Before(cut) {
			delete(p.tracks, id)
			n++
		}
	}
	p.counters().Add(CounterPictureDropped, uint64(n))
	return n
}

// Len is the number of tracks held.
func (p *Picture) Len() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.tracks)
}

// Area is what a subscription covers: the boxes (an intent's volumes
// padded by the traffic radius, or a staff bbox) and the flight that is
// the subscriber's own.
type Area struct {
	Boxes    []geodesy.BBox
	OwnTrack string // the monitor id of the subscriber's own flight, "" for none
}

// Contains reports whether p is in the area.
func (a Area) Contains(p core.LatLon) bool {
	for _, b := range a.Boxes {
		if b.Contains(p) {
			return true
		}
	}
	return false
}

// Selected is one track of a read with what the product shows of it.
type Selected struct {
	Track ProductTrack
	Raw   json.RawMessage
	// Manned is true for a track/manned/v1 message.
	Manned bool
	// ID is the monitor id (namespace:track id).
	ID string
}

// Tracks are the held tracks in a (and the subscriber's own flight
// wherever it is), as shown at now, sorted by track id: live, stale
// (captured more than traffic_stale_after_s ago, or the source said so)
// or source_disabled (its source is switched off now, or the source said
// so), each with its age. The lock is held only to collect the held
// tracks, which are replaced and never changed; the product is built
// after it.
func (p *Picture) Tracks(a Area, now time.Time, v policy.Values, gate SourceGate) []Selected {
	type pick struct {
		id string
		h  *held
	}
	p.mu.RLock()
	picks := make([]pick, 0, 64)
	for id, h := range p.tracks {
		if id == a.OwnTrack || a.Contains(h.in.Position) {
			picks = append(picks, pick{id, h})
		}
	}
	p.mu.RUnlock()
	out := make([]Selected, 0, len(picks))
	for _, pk := range picks {
		in := &pk.h.in
		t := productTrack(in)
		t.AgeS = max(now.Sub(in.Times.CapturedAt).Seconds(), 0)
		switch {
		case in.State == StateSourceDisabled || (gate != nil && !gate.Enabled(in.Source, in.Instance)):
			t.State = StateSourceDisabled
		case in.State == StateStale || t.AgeS > v.TrafficStaleAfterS:
			t.State = StateStale
		default:
			t.State = StateLive
		}
		t.Own = pk.id == a.OwnTrack
		out = append(out, Selected{ID: pk.id, Raw: pk.h.raw, Manned: strings.HasPrefix(pk.id, NSManned+":"), Track: t})
	}
	slices.SortFunc(out, func(x, y Selected) int { return strings.Compare(x.ID, y.ID) })
	return out
}

// Position is a WGS84 position as the product writes it.
type Position struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

// IdentStatus is the identification of a product track: the status
// only.
type IdentStatus struct {
	Status core.IdentStatus `json:"status"`
}

// ProductTrack is one track of traffic/product/v1.
type ProductTrack struct {
	TrackID        string         `json:"track_id"`
	Trust          core.Trust     `json:"trust"`
	Source         string         `json:"source"`
	State          string         `json:"state"`
	AgeS           float64        `json:"age_s"`
	Position       Position       `json:"position"`
	AltAMSLM       *float64       `json:"alt_amsl_m"`
	AltSource      core.AltSource `json:"alt_source"`
	SpeedMS        *float64       `json:"speed_ms"`
	TrackDeg       *float64       `json:"track_deg"`
	VSpeedMS       *float64       `json:"vspeed_ms"`
	Emergency      *bool          `json:"emergency"`
	Identification *IdentStatus   `json:"identification"`
	TimeOfReport   bus.Stamp      `json:"time_of_report"`
	Callsign       *string        `json:"callsign,omitempty"`
	Own            bool           `json:"own,omitempty"`
}

func productTrack(in *Input) ProductTrack {
	t := ProductTrack{
		TrackID: in.TrackID, Trust: in.Trust, Source: in.Source, Position: Position{Lat: in.Position.LatDeg, Lng: in.Position.LonDeg},
		AltAMSLM: in.AltAMSLM, AltSource: in.AltSource, SpeedMS: in.SpeedMS, TrackDeg: in.TrackDeg, VSpeedMS: in.VSpeedMS,
		Emergency: in.Emergency, TimeOfReport: bus.Stamp{Time: in.Times.CapturedAt}, Callsign: in.Callsign,
	}
	if in.Identification != nil {
		t.Identification = &IdentStatus{Status: in.Identification.Status}
	}
	if t.AltSource == "" {
		t.AltSource = core.AltNone
	}
	return t
}

// Degraded is one input that is missing or stale.
type Degraded struct {
	Input  string     `json:"input"`
	Since  *bus.Stamp `json:"since"`
	Reason string     `json:"reason"`
}

// For says what a product is for: an intent or a bbox.
type For struct {
	IntentID *string     `json:"intent_id,omitempty"`
	BBox     *[4]float64 `json:"bbox,omitempty"`
}

// ProductBody is traffic/product/v1.
type ProductBody struct {
	At            bus.Stamp         `json:"at"`
	For           For               `json:"for"`
	Tracks        []ProductTrack    `json:"tracks"`
	Alerts        []json.RawMessage `json:"alerts"`
	Degraded      []Degraded        `json:"degraded"`
	CISVersion    *string           `json:"cis_version"`
	PolicyVersion int64             `json:"policy_version"`
	DroppedFrames uint64            `json:"dropped_frames"`
	Throttled     bool              `json:"throttled,omitempty"`
	// ClientID is set on the record sample (traffic.product.v1) only:
	// whose product it was.
	ClientID string `json:"client_id,omitempty"`
}

// Product is one traffic/product/v1 message.
type Product struct {
	bus.Envelope
	Body ProductBody `json:"body"`
}

// Throttle selects the tracks a product of tick carries: every track
// when there are at most limit; above it each track every other second
// (by a hash of its id, so a track keeps its parity), and held back is
// how many were left out (05 §5: dropped_frames).
func Throttle(sel []Selected, limit int, tick uint64) (kept []Selected, heldBack int) {
	if limit <= 0 || len(sel) <= limit {
		return sel, 0
	}
	kept = make([]Selected, 0, len(sel)/2+1)
	for i := range sel {
		if maphash.String(throttleSeed, sel[i].ID)%2 == tick%2 {
			kept = append(kept, sel[i])
		} else {
			heldBack++
		}
	}
	return kept, heldBack
}

// throttleSeed keeps each track's parity for the life of the process
// (a hash whose low bit depends on every byte; FNV's does not).
var throttleSeed = maphash.MakeSeed()

// Tracked is the product's track list of sel.
func Tracked(sel []Selected) []ProductTrack {
	out := make([]ProductTrack, len(sel))
	for i := range sel {
		out[i] = sel[i].Track
	}
	return out
}

// DegradedSorted is ds by input, for a stable product.
func DegradedSorted(ds map[string]Degraded) []Degraded {
	out := make([]Degraded, 0, len(ds))
	for _, k := range slices.Sorted(maps.Keys(ds)) {
		out = append(out, ds[k])
	}
	return out
}
