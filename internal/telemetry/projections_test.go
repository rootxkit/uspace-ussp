package telemetry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/zones"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/cell"
	"github.com/rootxkit/uspace-ussp/internal/cis"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/registry"
)

// The KV readers read the keys their writers write: client_bindings
// under the client's key token, intent_active under the intent id,
// registry_validity under registry.KVKey; a mirror never read is "not
// loaded", never "nothing there" (SC-22).
func TestKVReaders(t *testing.T) {
	bm := &bus.Mirror[[]string]{}
	b := KVBindings{M: bm}
	if _, _, loaded := b.Folds(clientA); loaded {
		t.Fatal("loaded before a read")
	}
	bm.Seed(map[string][]string{bus.KeyToken(clientA): {"TEST-SN-A"}})
	if folds, _, loaded := b.Folds(clientA); !loaded || len(folds) != 1 {
		t.Fatalf("%v %v", folds, loaded)
	}
	im := &bus.Mirror[IntentFacts]{}
	i := KVIntents{M: im}
	if _, _, loaded := i.Intent(intent1); loaded {
		t.Fatal("loaded before a read")
	}
	im.Seed(map[string]IntentFacts{intent1: {IntentID: intent1, LocalState: "activated"}})
	if f, found, loaded := i.Intent(intent1); !found || !loaded || f.LocalState != "activated" {
		t.Fatalf("%+v %v %v", f, found, loaded)
	}
	if _, found, loaded := i.Intent("not a key!"); found || !loaded {
		t.Fatal("an invalid key found")
	}
	rm := &bus.Mirror[registry.Entry]{}
	k := registry.Key{Entity: registry.EntityUAS, Key: snA}
	rm.Seed(map[string]registry.Entry{registry.KVKey(k): {Key: k, Status: registry.StatusValid}})
	if e, found, loaded := (KVRegistry{M: rm}).Entry(k); !found || !loaded || e.Status != registry.StatusValid {
		t.Fatalf("%+v", e)
	}
}

// intent_active holds intent/state/v1: the facts the ingest needs read
// from the schema's own example (E-03).
func TestIntentFactsReadTheStateSchema(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "schemas", "intent", "state", "v1", "examples", "accepted.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f IntentFacts
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if f.IntentID == "" || f.LocalState != "accepted" || f.UASSerial == "" || f.OperatorReg == "" || f.TimeEnd.IsZero() {
		t.Fatalf("%+v", f)
	}
	if flying(f.LocalState) || !flying("activated") || !flying("nonconforming") {
		t.Fatal("flying states")
	}
}

// The policy follower reads a stored version onto the defaults (a value
// added later reads its default) and refuses one that does not validate.
func TestDecodePolicy(t *testing.T) {
	r, err := DecodePolicy([]byte(`{"policy_version":4,"values":{"telemetry_lost_s":7}}`))
	if err != nil || r.Version != 4 || r.Values.TelemetryLostS != 7 || r.Values.TelemetryRateHz != policy.Defaults().TelemetryRateHz {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := DecodePolicy([]byte(`{"values":{"telemetry_rate_hz":0}}`)); err == nil {
		t.Fatal("an invalid policy taken")
	}
	if _, err := DecodePolicy([]byte(`nope`)); err == nil {
		t.Fatal("not JSON taken")
	}
}

// feature is a USPACE feature over the fixture point, AMSL limits.
func feature(id string, lower, upper float64, ref string) json.RawMessage {
	poly := `[[[44.80,41.70],[44.85,41.70],[44.85,41.73],[44.80,41.73],[44.80,41.70]]]`
	return json.RawMessage(`{"type":"Feature","geometry":{"type":"Polygon","coordinates":` + poly + `,"layer":{"uom":"m","lower":` +
		itoa(int64(lower)) + `,"lowerReference":"` + ref + `","upper":` + itoa(int64(upper)) + `,"upperReference":"` + ref + `"}},` +
		`"properties":{"identifier":"` + id + `","country":"GEO","name":[{"text":"Test","lang":"en-GB"}],"type":"USPACE","variant":"COMMON",` +
		`"reason":["AIR_TRAFFIC"],"zoneAuthority":[{"name":[{"text":"Test","lang":"en-GB"}],"purpose":"AUTHORIZATION"}]}}`)
}

func cisMirror(t *testing.T, zs ...cis.ApplicableZone) *bus.Mirror[CISValue] {
	t.Helper()
	c5, _, err := cell.Key(tbilisi.LatLon())
	if err != nil {
		t.Fatal(err)
	}
	m := &bus.Mirror[CISValue]{}
	m.Seed(map[string]CISValue{
		cis.KeyBasis:     {Basis: &cis.BasisValue{Basis: cis.Basis{CISVersion: "v1"}}},
		cell.KVToken(c5): {Cell: &cis.CellEntry{Cell: c5, Zones: zs}},
	})
	return m
}

func amsl(v float64) zones.Aircraft { return zones.Aircraft{AltAMSLM: &v, AltSource: core.AltGeodetic} }

// E-01 pairs of the airspace judge: inside an applying U-space airspace
// (horizontally and between its AMSL limits) is inside; above it, or
// outside it horizontally, or an airspace that does not apply, is
// outside; nothing loaded or a feature that does not build is not
// judged, said so.
func TestCISAirspace(t *testing.T) {
	usp := cis.ApplicableZone{Identifier: "USP-1", Type: "USPACE", Applies: true, CISApplicability: "applies", Version: "u@1",
		Dataset: string(cis.USpaceAirspace), Feature: feature("USP-1", 0, 500, "AMSL")}
	j := NewCISAirspace(cisMirror(t, usp))
	pt := tbilisi.LatLon()
	if v := j.At(pt, amsl(300), zones.Env{}); !v.Judged || !v.Inside || v.AirspaceID != "USP-1" {
		t.Fatalf("inside: %+v", v)
	}
	if v := j.At(pt, amsl(800), zones.Env{}); !v.Judged || v.Inside {
		t.Fatalf("above: %+v", v)
	}
	if v := j.At(core.LatLon{LatDeg: 41.80, LonDeg: 44.8271}, amsl(300), zones.Env{}); !v.Judged || v.Inside {
		t.Fatalf("north: %+v", v)
	}
	// No altitude: may be inside (fail-safe), with the reason.
	if v := j.At(pt, zones.Aircraft{AltSource: core.AltNone}, zones.Env{}); !v.Inside || !strings.HasPrefix(v.Reason, "vertical_not_judged") {
		t.Fatalf("no altitude: %+v", v)
	}
	off := usp
	off.Applies, off.CISApplicability = false, "not_applicable"
	if v := NewCISAirspace(cisMirror(t, off)).At(pt, amsl(300), zones.Env{}); v.Inside {
		t.Fatalf("not applicable: %+v", v)
	}
	zone := usp
	zone.Dataset = "zones"
	if v := NewCISAirspace(cisMirror(t, zone)).At(pt, amsl(300), zones.Env{}); v.Inside {
		t.Fatalf("a zone of another dataset: %+v", v)
	}
	bad := usp
	bad.Feature = json.RawMessage(`{}`)
	if v := NewCISAirspace(cisMirror(t, bad)).At(pt, amsl(300), zones.Env{}); v.Judged || v.Reason != ReasonCISUnreadable {
		t.Fatalf("unreadable: %+v", v)
	}
	if v := NewCISAirspace(&bus.Mirror[CISValue]{}).At(pt, amsl(300), zones.Env{}); v.Judged || v.Reason != ReasonCISUnavailable {
		t.Fatalf("never read: %+v", v)
	}
	empty := &bus.Mirror[CISValue]{}
	empty.Seed(map[string]CISValue{})
	if v := NewCISAirspace(empty).At(pt, amsl(300), zones.Env{}); v.Judged || v.Reason != ReasonCISNotLoaded {
		t.Fatalf("no basis: %+v", v)
	}
	// The zones are built once per version.
	j.At(pt, amsl(300), zones.Env{})
	if len(j.cache) != 1 {
		t.Fatalf("cache %d", len(j.cache))
	}
}

func TestDecodeCIS(t *testing.T) {
	if v, err := DecodeCIS(cis.KeyBasis, []byte(`{"cis_version":"v1","cis_age_s":3}`)); err != nil || v.Basis == nil || v.Basis.CISVersion != "v1" {
		t.Fatalf("%+v %v", v, err)
	}
	if v, err := DecodeCIS("c5.1.1", []byte(`{"cell":"c5:1:1","zones":[]}`)); err != nil || v.Cell == nil {
		t.Fatalf("%+v %v", v, err)
	}
	for _, k := range []string{cis.KeyBasis, "c5.1.1"} {
		if _, err := DecodeCIS(k, []byte(`[`)); err == nil {
			t.Errorf("%s: garbage taken", k)
		}
	}
}

// The acknowledgement covers this connection's samples only: a seq
// still on its way holds it back; a refusal settles; one not handed
// stays open until its copy lands.
func TestSessionAcknowledgement(t *testing.T) {
	r := newRig(t, rigOpts{})
	s := r.in.NewSession(clientA, t0)
	if st := s.Status(t0); len(st.AckedSeq) != 0 {
		t.Fatal(st.AckedSeq)
	}
	s.pending(snA, 1)
	s.outcome(Result{Serial: snA, Seq: 1, Reason: OutcomeAccepted})
	s.pending(snA, 2)
	s.outcome(Result{Serial: snA, Seq: 2, Reason: OutcomeAccepted})
	s.outcome(Result{Serial: snA, Seq: 3, Reason: RefusedUnbound})
	// Only 3 is settled, below the open 1 and 2: no sample of this
	// connection up to 0, so 0 vouches for nothing it sent.
	if st := s.Status(t0); st.AckedSeq[snA] != 0 {
		t.Fatalf("nothing landed yet, acked %v", st.AckedSeq)
	}
	s.handed(snA, 2, true)
	if st := s.Status(t0); st.AckedSeq[snA] != 0 {
		t.Fatalf("1 still open: acked %v", st.AckedSeq)
	}
	s.handed(snA, 1, false) // not handed: stays open
	if st := s.Status(t0); st.AckedSeq[snA] != 0 {
		t.Fatalf("acked %v", st.AckedSeq)
	}
	s.handed(snA, 1, true) // its copy landed
	st := s.Status(t0.Add(time.Second))
	if st.AckedSeq[snA] != 3 || st.Accepted != 2 || st.Refused != 1 || st.Rate != 2 {
		t.Fatalf("status %+v", st)
	}
	s.outcome(Result{Serial: snA, Seq: 4, Reason: DroppedQueueFull})
	if st := s.Status(t0.Add(2 * time.Second)); st.AckedSeq[snA] != 3 || st.Dropped != 1 {
		t.Fatalf("a dropped sample acknowledged: %+v", st)
	}
	// E-10: the open seqs are bounded.
	for i := range maxOpenSeqs + 1 {
		s.pending(snB, int64(i))
	}
	if s.Status(t0).Overflow != 1 {
		t.Fatal("bound not counted")
	}
}

// The client's status: live while it sends, stale after telemetry_lost_s,
// disabled by whom when switched off; lagging while its newest sample
// is old (B-03).
func TestClientStatus(t *testing.T) {
	r := newRig(t, rigOpts{})
	st := &Status{Ingest: r.in, Pub: r.pub, Sources: r.gate, Now: r.clk.now, Counters: r.counters}
	if b := st.Body(clientB); b.State != StateUnknown || b.AgeS != nil || b.Counters["accepted"] != 0 {
		t.Fatalf("never heard: %+v", b)
	}
	one(t, r.take(clientA, nil, frame(snA, 1, t0)), OutcomeAccepted)
	one(t, r.take(clientA, nil, frame("TEST-SN-NONE", 1, t0)), RefusedUnbound)
	b := st.Body(clientA)
	if b.State != StateLive || b.Counters["accepted"] != 1 || b.Counters["refused"] != 1 || b.Lagging {
		t.Fatalf("live: %+v", b)
	}
	r.clk.add(10 * time.Second)
	if b := st.Body(clientA); b.State != StateStale || *b.AgeS != 10 {
		t.Fatalf("stale: %+v", b)
	}
	old := frame(snA, 2, r.clk.now().Add(-time.Minute))
	old.Backlog = true
	one(t, r.take(clientA, nil, old), OutcomeAccepted)
	if b := st.Body(clientA); !b.Lagging || b.LagS == nil || *b.LagS < 59 {
		t.Fatalf("lagging: %+v", b)
	}
	r.gate.set(clientA, true)
	if b := st.Body(clientA); b.State != StateDisabled || b.DisabledBy == nil || *b.DisabledBy != "instance" {
		t.Fatalf("disabled: %+v", b)
	}
}
