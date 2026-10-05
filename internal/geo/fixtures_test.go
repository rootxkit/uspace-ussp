package geo

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/alerting"
	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/cis"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/telemetry"
)

// box is minLon, minLat, maxLon, maxLat.
type box [4]float64

// zoneBox is a small box east of Tbilisi; the flights fly through it
// along its centre latitude.
var zoneBox = box{44.80, 41.70, 44.85, 41.73}

const centreLat = 41.715

type feat struct {
	id, typ            string
	lower, upper       *float64
	lowerRef, upperRef string
	rect               box
	applicability      []any
	extended           map[string]any
}

func f64(v float64) *float64 { return &v }

func (f feat) json() json.RawMessage {
	layer := map[string]any{"uom": "m"}
	if f.lower != nil {
		layer["lower"], layer["lowerReference"] = *f.lower, f.lowerRef
	}
	if f.upper != nil {
		layer["upper"], layer["upperReference"] = *f.upper, f.upperRef
	}
	r := f.rect
	poly := []any{[]any{
		[]any{r[0], r[1]}, []any{r[2], r[1]}, []any{r[2], r[3]}, []any{r[0], r[3]}, []any{r[0], r[1]},
	}}
	reason := []string{"SENSITIVE"}
	if f.typ == "USPACE" {
		reason = []string{"AIR_TRAFFIC"}
	}
	props := map[string]any{
		"identifier": f.id, "country": "GEO", "name": []any{map[string]any{"text": "Test zone " + f.id, "lang": "en-GB"}},
		"type": f.typ, "variant": "COMMON", "reason": reason,
		"message":       []any{map[string]any{"text": "Test message " + f.id, "lang": "en-GB"}},
		"zoneAuthority": []any{map[string]any{"name": []any{map[string]any{"text": "Test authority", "lang": "en-GB"}}, "purpose": "AUTHORIZATION"}},
		"dataSource":    map[string]any{"updateDateTime": "2026-10-01T08:00:00Z"},
	}
	if f.applicability != nil {
		props["limitedApplicability"] = f.applicability
	}
	if f.extended != nil {
		props["extendedProperties"] = f.extended
	}
	b, err := json.Marshal(map[string]any{"type": "Feature", "geometry": map[string]any{"type": "Polygon", "coordinates": poly, "layer": layer}, "properties": props})
	if err != nil {
		panic(err)
	}
	return b
}

// amslZone is a zone of type typ from 0 to 500 m AMSL over zoneBox.
func amslZone(id, typ string) feat {
	return feat{id: id, typ: typ, lower: f64(0), lowerRef: "AMSL", upper: f64(500), upperRef: "AMSL", rect: zoneBox}
}

// aglZone is a zone of type typ from 0 to 120 m AGL over zoneBox (its
// upper limit needs the ground).
func aglZone(id, typ string) feat {
	return feat{id: id, typ: typ, lower: f64(0), lowerRef: "AGL", upper: f64(120), upperRef: "AGL", rect: zoneBox}
}

// projection is cis_current with fs listed under one cell (dataset d)
// and its basis.
func projection(version string, at time.Time, fs map[string][]feat) map[string]telemetry.CISValue {
	ce := &cis.CellEntry{Cell: "c5:test", CISVersion: version, At: at}
	for d, list := range fs {
		for i := range list {
			f := &list[i]
			ce.Zones = append(ce.Zones, cis.ApplicableZone{
				Identifier: f.id, Type: f.typ, Applies: true, CISApplicability: "applies", Version: d + ":1", At: at,
				CISVersion: version, Dataset: d, Feature: f.json(),
			})
		}
	}
	return map[string]telemetry.CISValue{
		"c5test":     {Cell: ce},
		cis.KeyBasis: {Basis: &cis.BasisValue{Basis: cis.Basis{CISVersion: version}, At: at, Cells: 1}},
	}
}

func setOf(t *testing.T, version string, at time.Time, fs map[string][]feat) *ZoneSet {
	t.Helper()
	s := BuildZoneSet(projection(version, at, fs), nil, at)
	if !s.Loaded || len(s.Unbuildable) > 0 {
		t.Fatalf("set %+v", s)
	}
	return s
}

// rig drives one tracker with one flight on a placed clock that steps
// one second per sample.
type rig struct {
	t      *testing.T
	tr     *Tracker
	pol    policy.Record
	clk    time.Time
	flight string
	ref    Ref
}

func newRig(t *testing.T, s *ZoneSet) *rig {
	g := &rig{t: t, tr: NewTracker(nil), pol: policy.Record{Version: 7, Values: policy.Defaults()},
		clk: time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC), flight: "0b1d6a52-9b62-4b5e-9f0d-3f1c2a4b5c6d"}
	g.ref = Ref{FlightID: g.flight, IntentID: "1c2d3e4f-5a6b-4c7d-8e9f-0a1b2c3d4e5f", AuthorisationNumber: "DEV01-GEO-TEST-01-X", Cell5: "c5:4180:4470"}
	if evs := g.tr.Configure(s, g.pol, g.clk); len(evs) != 0 {
		t.Fatalf("first configure: %v", evs)
	}
	return g
}

func (g *rig) wallS() float64 { return float64(g.clk.UnixNano()) / 1e9 }

// sample is one flying sample at lon, altAMSLM, one second after the
// previous one.
func (g *rig) sample(lon, altAMSLM float64) []Event {
	g.clk = g.clk.Add(time.Second)
	return g.tr.Observe(g.track(lon, altAMSLM, true), g.ref, g.wallS(), g.clk)
}

func (g *rig) track(lon, altAMSLM float64, flying bool) alerting.Track {
	alt := altAMSLM
	s := g.wallS()
	return alerting.Track{
		ID: "trk:" + g.flight, Pos: core.LatLon{LatDeg: centreLat, LonDeg: lon}, AltAMSLM: &alt, AltSource: core.AltGeodetic,
		Flying: &flying, CapturedAtS: s, RxAtS: s, Source: "operator_ws", Station: "client-1",
	}
}

func states(evs []Event) string {
	out := ""
	for i := range evs {
		out += fmt.Sprintf("%s:%s:%s;", evs[i].State, evs[i].Alert.Kind, evs[i].ClearReason)
	}
	return out
}
