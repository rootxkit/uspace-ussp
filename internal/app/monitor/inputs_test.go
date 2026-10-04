package monitor

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/geodesy"

	"github.com/rootxkit/uspace-ussp/internal/cis"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/telemetry"
)

type intentsSnap map[string]intentBody

func (s intentsSnap) Snapshot() (map[string]intentBody, float64, bool) { return s, 0, true }

func sp(s string) *string { return &s }

// The echo guard's view of our own flights (Q23): a peer record of one
// of our flights (an active intent's flight or one seen on trk.v1 in
// the last minute) is ours; a manned record whose callsign or
// registration is an active flight's declared UA registration is its
// echo. Every twin is not (E-01): another id, a flight seen over a
// minute ago, another registration, an intent without a flight, a
// record that carries neither.
func TestOwnFlightsEchoGuard(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	o := &ownFlights{now: func() time.Time { return now }, intents: intentsSnap{
		"i1": {IntentID: "i1", FlightID: sp("f-intent"), UARegistration: sp("4L-UAV01")},
		"i2": {IntentID: "i2", UARegistration: sp("4L-NOFLT")},
	}}
	o.Seen("f-trk")
	if !o.OwnFlight("f-intent") || !o.OwnFlight("f-trk") || o.OwnFlight("someone-else") {
		t.Fatal("OwnFlight")
	}
	now = now.Add(61 * time.Second)
	if o.OwnFlight("f-trk") {
		t.Fatal("a flight seen over a minute ago is still ours")
	}
	if fid, ok := o.EchoOf("4ca123", sp("4luav01"), nil); !ok || fid != "f-intent" {
		t.Fatalf("callsign echo %q %v", fid, ok)
	}
	if fid, ok := o.EchoOf("4ca123", sp("OTHER"), sp("4L UAV01")); !ok || fid != "f-intent" {
		t.Fatalf("registration echo %q %v", fid, ok)
	}
	for _, c := range [][2]*string{{sp("OTHER"), nil}, {sp("4L-NOFLT"), nil}, {nil, nil}, {sp(" "), nil}} {
		if _, ok := o.EchoOf("4ca123", c[0], c[1]); ok {
			t.Fatalf("%v taken for an echo", c)
		}
	}
	if (&ownFlights{}).OwnFlight("x") {
		t.Fatal("no intents: nothing is ours")
	}
}

// E-10: past maxOwnSeen a new flight is not remembered, unless the old
// ones can be forgotten.
func TestOwnFlightsBound(t *testing.T) {
	now := time.Now()
	o := &ownFlights{now: func() time.Time { return now }}
	o.seen = map[string]time.Time{}
	for i := 0; i < maxOwnSeen; i++ {
		o.seen["f"+strconv.Itoa(i)] = now
	}
	o.Seen("new")
	if _, ok := o.seen["new"]; ok {
		t.Fatal("remembered past the bound")
	}
	now = now.Add(2 * ownSeenFor)
	o.Seen("new")
	if _, ok := o.seen["new"]; !ok || len(o.seen) != 1 {
		t.Fatalf("the old ones were not forgotten: %d", len(o.seen))
	}
}

func TestParseBBox(t *testing.T) {
	b, ok, err := ParseBBox("44.7,41.6,45.0,41.9")
	if err != nil || !ok || b != (geodesy.BBox{MinLon: 44.7, MinLat: 41.6, MaxLon: 45, MaxLat: 41.9}) {
		t.Fatalf("%+v %v %v", b, ok, err)
	}
	if _, ok, err := ParseBBox(" "); ok || err != nil {
		t.Fatal("empty")
	}
	for _, bad := range []string{"1,2,3", "a,1,2,3", "45,41,44,42", "1,95,2,96"} {
		if _, _, err := ParseBBox(bad); err == nil || !strings.Contains(err.Error(), "USSP_TRAFFIC_INPUT_BBOX") {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

type cisSnap struct {
	vals   map[string]telemetry.CISValue
	loaded bool
}

func (c cisSnap) Snapshot() (map[string]telemetry.CISValue, float64, bool) {
	return c.vals, 0, c.loaded
}

func uspaceFeature(id string, minLon, minLat, maxLon, maxLat float64) json.RawMessage {
	poly := []any{[]any{[]any{minLon, minLat}, []any{maxLon, minLat}, []any{maxLon, maxLat}, []any{minLon, maxLat}, []any{minLon, minLat}}}
	b, _ := json.Marshal(map[string]any{"type": "Feature",
		"geometry": map[string]any{"type": "Polygon", "coordinates": poly, "layer": map[string]any{"uom": "m", "lower": 0, "lowerReference": "AMSL", "upper": 500, "upperReference": "AMSL"}},
		"properties": map[string]any{"identifier": id, "country": "GEO", "name": []any{map[string]any{"text": id, "lang": "en-GB"}},
			"type": "USPACE", "variant": "COMMON", "reason": []string{"AIR_TRAFFIC"},
			"message":       []any{map[string]any{"text": "m", "lang": "en-GB"}},
			"zoneAuthority": []any{map[string]any{"name": []any{map[string]any{"text": "a", "lang": "en-GB"}}, "purpose": "AUTHORIZATION"}},
			"dataSource":    map[string]any{"updateDateTime": "2026-10-01T08:00:00Z"}}})
	return b
}

// The areas of interest from cis_current: every U-space airspace once,
// its box, whatever cells list it; other datasets and types ignored;
// the peer areas padded by peer_subscription_margin_m, the ANSP's box
// their union padded by manned_margin_m; a configured box as it is;
// cis_current not read: not known (SC-22, nothing assumed).
func TestInputAreas(t *testing.T) {
	a := cis.ApplicableZone{Identifier: "TSA-1", Type: "USPACE", Dataset: string(cis.USpaceAirspace), Feature: uspaceFeature("TSA-1", 44.7, 41.6, 44.8, 41.7)}
	b := cis.ApplicableZone{Identifier: "TSA-2", Type: "USPACE", Dataset: string(cis.USpaceAirspace), Feature: uspaceFeature("TSA-2", 44.9, 41.8, 45.0, 41.9)}
	z := cis.ApplicableZone{Identifier: "Z-1", Type: "PROHIBITED", Dataset: string(cis.Zones), Feature: uspaceFeature("Z-1", 40, 40, 41, 41)}
	bad := cis.ApplicableZone{Identifier: "TSA-BAD", Type: "USPACE", Dataset: string(cis.USpaceAirspace), Feature: json.RawMessage(`{}`)}
	snap := cisSnap{loaded: true, vals: map[string]telemetry.CISValue{
		"c5:1:1": {Cell: &cis.CellEntry{Zones: []cis.ApplicableZone{a, z, bad}}},
		"c5:1:2": {Cell: &cis.CellEntry{Zones: []cis.ApplicableZone{a, b}}},
		"basis":  {Basis: &cis.BasisValue{}},
	}}
	pol := policy.Defaults()
	in := &inputAreas{cis: &cisAreas{m: snap}, policy: func() policy.Values { return pol }}
	as, ok := in.peerAreas()
	if !ok || len(as) != 2 || as[0].ID != "TSA-1" || as[1].ID != "TSA-2" {
		t.Fatalf("areas %+v %v", as, ok)
	}
	want := geodesy.BBox{MinLat: 41.6, MinLon: 44.7, MaxLat: 41.7, MaxLon: 44.8}.PadM(pol.PeerSubscriptionMarginM)
	if as[0].Box != want {
		t.Fatalf("padded %+v, want %+v", as[0].Box, want)
	}
	mb, ok := in.mannedBBox()
	if wantM := (geodesy.BBox{MinLat: 41.6, MinLon: 44.7, MaxLat: 41.9, MaxLon: 45.0}).PadM(pol.MannedMarginM); !ok || mb != wantM {
		t.Fatalf("manned box %+v, want %+v", mb, wantM)
	}
	unread := &inputAreas{cis: &cisAreas{m: cisSnap{}}, policy: func() policy.Values { return pol }}
	if _, ok := unread.peerAreas(); ok {
		t.Fatal("areas known without cis_current")
	}
	if _, ok := unread.mannedBBox(); ok {
		t.Fatal("a manned box without cis_current")
	}
	conf := geodesy.BBox{MinLat: 1, MinLon: 2, MaxLat: 3, MaxLon: 4}
	c := &inputAreas{configured: &conf, policy: func() policy.Values { return pol }}
	if as, ok := c.peerAreas(); !ok || as[0].Box != conf {
		t.Fatal("configured peer area")
	}
	if b, ok := c.mannedBBox(); !ok || b != conf {
		t.Fatal("configured manned box")
	}
	if _, ok := (&inputAreas{policy: func() policy.Values { return pol }}).peerAreas(); ok {
		t.Fatal("no source, known")
	}
}
