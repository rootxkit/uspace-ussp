package telemetry

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3411"
	"github.com/rootxkit/uspace-core/serial"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/registry"
)

func one(t *testing.T, rs []Result, want string) Result {
	t.Helper()
	if len(rs) != 1 || rs[0].Reason != want {
		t.Fatalf("outcomes %v (%+v), want [%s]", reasons(rs), rs, want)
	}
	return rs[0]
}

// E-01 pair (06 T3): a sample for a serial bound to another client, or
// to none, is refused_unbound and publishes nothing; the bound one is
// accepted and published as an authenticated track of this client.
func TestUnboundRefusedBoundAccepted(t *testing.T) {
	r := newRig(t, rigOpts{geoid: true})
	r.bind.bind(clientB, "TEST-SN-OTHER")
	one(t, r.take(clientA, nil, frame("TEST-SN-OTHER", 1, t0)), RefusedUnbound)
	one(t, r.take(clientA, nil, frame("TEST-SN-NOBODY", 1, t0)), RefusedUnbound)
	if n := len(r.pub.tracks(t)); n != 0 {
		t.Fatalf("%d tracks published for unbound serials", n)
	}
	one(t, r.take(clientA, nil, frame("test-sn-a", 1, t0)), OutcomeAccepted) // G-05: the fold key binds
	trs := r.pub.tracks(t)
	if len(trs) != 1 {
		t.Fatalf("%d tracks", len(trs))
	}
	b := trs[0].Body
	if b.Trust != core.TrustAuthenticated || b.Source != SourceOperatorWS || b.SourceInstance != clientA || b.FlightID == nil ||
		*b.FlightID != b.TrackID {
		t.Fatalf("track body %+v", b)
	}
	if r.counters.Get(RefusedUnbound) != 2 || r.counters.Get(OutcomeAccepted) != 1 {
		t.Fatalf("counters %v", r.counters.Snapshot())
	}
}

// A projection never read refuses (nothing can be checked, nothing
// passes by default); once read, the same sample is accepted.
func TestBindingsNotReadRefusedReadAccepted(t *testing.T) {
	r := newRig(t, rigOpts{})
	r.bind.loaded = false
	one(t, r.take(clientA, nil, frame(snA, 1, t0)), RefusedBindingsUnavailable)
	r.bind.loaded = true
	one(t, r.take(clientA, nil, frame(snA, 2, t0)), OutcomeAccepted)
}

// E-01 pair (05 §5): live samples above telemetry_rate_hz per aircraft
// are dropped and counted; at the rate they are accepted. Another
// aircraft of the same client has its own budget.
func TestOverRateDroppedInRateAccepted(t *testing.T) {
	r := newRig(t, rigOpts{})
	rs := r.take(clientA, nil, frame(snA, 1, t0), frame(snA, 2, t0.Add(100*time.Millisecond)), frame(snA, 3, t0.Add(200*time.Millisecond)))
	if got := reasons(rs); !slices.Equal(got, []string{OutcomeAccepted, OutcomeAccepted, DroppedRate}) {
		t.Fatalf("burst: %v", got)
	}
	one(t, r.take(clientA, nil, frame(snB, 1, t0)), OutcomeAccepted)
	for i := range 4 {
		r.clk.add(time.Second)
		one(t, r.take(clientA, nil, frame(snA, int64(10+i), t0.Add(time.Duration(i+1)*time.Second))), OutcomeAccepted)
	}
	if r.counters.Get(DroppedRate) != 1 {
		t.Fatalf("dropped_rate %d", r.counters.Get(DroppedRate))
	}
}

// E-01 pair (B-10): a client whose operator_ws source is switched off
// is refused and nothing is stored; switched on, accepted.
func TestDisabledSourceRefusedEnabledAccepted(t *testing.T) {
	r := newRig(t, rigOpts{})
	r.gate.set(clientA, true)
	res := one(t, r.take(clientA, nil, frame(snA, 1, t0)), RefusedSourceDisabled)
	if !strings.Contains(res.Detail, "instance") {
		t.Fatalf("detail %q", res.Detail)
	}
	if len(r.pub.tracks(t)) != 0 {
		t.Fatal("a disabled source's sample was published")
	}
	r.gate.set(clientA, false)
	one(t, r.take(clientA, nil, frame(snA, 2, t0)), OutcomeAccepted)
}

// E-01 pair (B-14): a second session for an aircraft replaces the
// first; the first one's next sample is refused_replaced, and its
// teardown leaves the new session's aircraft alone.
func TestSecondSessionReplacesTheFirst(t *testing.T) {
	r := newRig(t, rigOpts{})
	old := r.in.NewSession(clientA, t0)
	young := r.in.NewSession(clientA, t0)
	one(t, r.take(clientA, old, frame(snA, 1, t0)), OutcomeAccepted)
	r.clk.add(time.Second)
	one(t, r.take(clientA, young, frame(snA, 2, t0.Add(time.Second))), OutcomeAccepted)
	if r.counters.Get(CounterSessionReplaced) != 1 {
		t.Fatalf("session_replaced %d", r.counters.Get(CounterSessionReplaced))
	}
	r.clk.add(time.Second)
	one(t, r.take(clientA, old, frame(snA, 3, t0.Add(2*time.Second))), RefusedReplaced)
	r.in.Release(old) // the old teardown
	r.clk.add(time.Second)
	one(t, r.take(clientA, young, frame(snA, 4, t0.Add(3*time.Second))), OutcomeAccepted)
	r.clk.add(time.Second)
	one(t, r.take(clientA, old, frame(snA, 5, t0.Add(4*time.Second))), RefusedReplaced)
	// Once the young session ends too, nobody owns the aircraft.
	r.in.Release(young)
	r.clk.add(time.Second)
	one(t, r.take(clientA, old, frame(snA, 6, t0.Add(5*time.Second))), OutcomeAccepted)
	// A batch never owns an aircraft and is never refused for a session.
	r.clk.add(time.Second)
	one(t, r.take(clientA, nil, frame(snA, 7, t0.Add(6*time.Second))), OutcomeAccepted)
}

// E-01 pair (06 T11): a sample that carries a trust class or a source is
// refused at the decoder; a normal sample is accepted and published as
// authenticated.
func TestSimulatedRefusedAuthenticatedAccepted(t *testing.T) {
	for name, msg := range map[string]string{
		"trust simulated": `{"schema":"telemetry/v1","body":` + sampleJSON(`"trust":"simulated",`) + `}`,
		"source sitl":     `{"schema":"telemetry/v1","body":` + sampleJSON(`"source":"sitl",`) + `}`,
	} {
		if _, err := DecodeMessage([]byte(msg)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	f, err := DecodeMessage([]byte(`{"schema":"telemetry/v1","msg_id":"ignored","body":` + sampleJSON("") + `}`))
	if err != nil {
		t.Fatal(err)
	}
	r := newRig(t, rigOpts{})
	one(t, r.take(clientA, nil, f), OutcomeAccepted)
	if tr := r.pub.tracks(t); len(tr) != 1 || tr[0].Body.Trust != core.TrustAuthenticated {
		t.Fatalf("tracks %+v", tr)
	}
}

func sampleJSON(extra string) string {
	return `{` + extra + `"ts":"2026-10-02T09:00:00.000Z","serial":"TEST-SN-A","seq":1,"position":{"lat":41.7151,"lng":44.8271},` +
		`"alt_wgs84_m":650,"height_m":80,"height_ref":"TakeoffLocation","speed_ms":8,"track_deg":90,"vspeed_ms":0,` +
		`"status":"Airborne","emergency":false,"accuracy_h":"HA3m","accuracy_v":"VA10m","timestamp_accuracy_s":0.1}`
}

// E-01 pairs (Art. 9, 10): inside U-space airspace a sample without an
// intent is refused_no_authorisation, outside it is accepted as a
// session flight; an intent not yet activated is refused_intent_state
// inside and accepted without the intent outside; an activated intent of
// this aircraft is accepted inside, bound to the intent.
func TestUSpaceAirspaceNeedsAnActivatedIntent(t *testing.T) {
	r := newRig(t, rigOpts{})
	inside := AirspaceVerdict{Judged: true, Inside: true, AirspaceID: "USP-TBS"}
	r.air.set(inside)
	res := one(t, r.take(clientA, nil, frame(snA, 1, t0)), RefusedNoAuthorisation)
	if !strings.Contains(res.Detail, "USP-TBS") {
		t.Fatalf("detail %q", res.Detail)
	}
	r.air.set(AirspaceVerdict{Judged: true})
	one(t, r.take(clientA, nil, frame(snA, 2, t0)), OutcomeAccepted)

	r.intents.set(IntentFacts{IntentID: intent1, LocalState: "accepted", UASSerial: snA, OperatorReg: "GEO-TEST-0001"})
	f := frame(snA, 3, t0.Add(time.Second))
	f.IntentID = str(intent1)
	r.clk.add(time.Second)
	r.air.set(inside)
	res = one(t, r.take(clientA, nil, f), RefusedIntentState)
	if !strings.Contains(res.Detail, "intent_accepted") {
		t.Fatalf("detail %q", res.Detail)
	}
	r.air.set(AirspaceVerdict{Judged: true})
	one(t, r.take(clientA, nil, f), OutcomeAccepted)
	if tr := r.pub.tracks(t); tr[len(tr)-1].Body.IntentID != nil {
		t.Fatal("a refused intent was put on the track")
	}

	r.intents.set(IntentFacts{IntentID: intent1, LocalState: "activated", UASSerial: snA, OperatorReg: "GEO-TEST-0001", AuthorisationNumber: str("GE-1")})
	r.air.set(inside)
	f = frame(snA, 4, t0.Add(2*time.Second))
	f.IntentID = str(intent1)
	r.clk.add(time.Second)
	one(t, r.take(clientA, nil, f), OutcomeAccepted)
	tr := r.pub.tracks(t)
	last := tr[len(tr)-1].Body
	if last.IntentID == nil || *last.IntentID != intent1 {
		t.Fatalf("intent %v", last.IntentID)
	}
	// Another aircraft cannot fly this intent.
	g := frame(snB, 1, t0.Add(2*time.Second))
	g.IntentID = str(intent1)
	res = one(t, r.take(clientA, nil, g), RefusedIntentState)
	if !strings.Contains(res.Detail, "intent_not_this_aircraft") {
		t.Fatalf("detail %q", res.Detail)
	}
	if r.counters.Get(CounterIntentRefused) != 3 {
		t.Fatalf("intent_refused %d", r.counters.Get(CounterIntentRefused))
	}
}

// An airspace that could not be judged lets the sample through, counted
// and visible (never a silent pass), and never refuses it.
func TestAirspaceNotJudgedIsCounted(t *testing.T) {
	r := newRig(t, rigOpts{})
	r.air.set(AirspaceVerdict{Reason: ReasonCISNotLoaded})
	one(t, r.take(clientA, nil, frame(snA, 1, t0)), OutcomeAccepted)
	if r.counters.Get(CounterAirspaceNotJudged) != 1 {
		t.Fatal("not counted")
	}
	r.in.cfg.Airspace = nil
	r.clk.add(time.Second)
	one(t, r.take(clientA, nil, frame(snA, 2, t0.Add(time.Second))), OutcomeAccepted)
	if r.counters.Get(CounterAirspaceNotJudged) != 2 {
		t.Fatal("no judge not counted")
	}
}

// An intent the projection does not hold, or a projection never read,
// is refused (the sample goes on as a session flight outside U-space).
func TestIntentUnknownOrUnread(t *testing.T) {
	r := newRig(t, rigOpts{})
	f := frame(snA, 1, t0)
	f.IntentID = str(intent1)
	one(t, r.take(clientA, nil, f), OutcomeAccepted)
	r.intents.loaded = false
	f.Seq = 2
	f.TS = f.TS.Add(time.Second)
	r.clk.add(time.Second)
	r.air.set(AirspaceVerdict{Judged: true, Inside: true, AirspaceID: "A"})
	res := one(t, r.take(clientA, nil, f), RefusedIntentState)
	if !strings.Contains(res.Detail, "intents_unavailable") {
		t.Fatalf("detail %q", res.Detail)
	}
	r.in.cfg.Intents = nil
	f.Seq = 3
	res = one(t, r.take(clientA, nil, f), RefusedIntentState)
	if !strings.Contains(res.Detail, "intents_unavailable") {
		t.Fatalf("detail %q", res.Detail)
	}
}

// B-05 pair: a (serial, seq) taken before is acknowledged and publishes
// nothing twice; a new seq is published.
func TestDuplicateAcknowledgedOnceNewPublished(t *testing.T) {
	r := newRig(t, rigOpts{})
	one(t, r.take(clientA, nil, frame(snA, 1, t0)), OutcomeAccepted)
	r.clk.add(time.Second)
	one(t, r.take(clientA, nil, frame(snA, 1, t0)), OutcomeDuplicate)
	if n := len(r.pub.tracks(t)); n != 1 {
		t.Fatalf("%d tracks", n)
	}
	one(t, r.take(clientA, nil, frame(snA, 2, t0.Add(time.Second))), OutcomeAccepted)
	// Beyond the window the seq is new again.
	r.clk.add(11 * time.Minute)
	f := frame(snA, 1, t0.Add(11*time.Minute))
	one(t, r.take(clientA, nil, f), OutcomeAccepted)
}

// T-03, T-04: a live sample older than the newest live one is
// rejected_out_of_order; backlog has its own order, so a drained history
// is taken while live samples flow.
func TestOutOfOrderRejectedBacklogHasItsOwnOrder(t *testing.T) {
	r := newRig(t, rigOpts{})
	one(t, r.take(clientA, nil, frame(snA, 5, t0)), OutcomeAccepted)
	r.clk.add(time.Second)
	one(t, r.take(clientA, nil, frame(snA, 4, t0.Add(-time.Second))), RejectedOutOfOrder)
	r.clk.add(time.Second)
	old := frame(snA, 3, t0.Add(-30*time.Second))
	old.Backlog = true
	res := one(t, r.take(clientA, nil, old), OutcomeAccepted)
	if !res.Backlog {
		t.Fatal("a backlog sample not marked backlog")
	}
	older := frame(snA, 2, t0.Add(-40*time.Second))
	older.Backlog = true
	r.clk.add(time.Second)
	one(t, r.take(clientA, nil, older), RejectedOutOfOrder)
	tr := r.pub.tracks(t)
	if !tr[len(tr)-1].Backlog {
		t.Fatal("track not backlog")
	}
}

// T-13: a sample far in the future is clamped to its receipt and does
// not pin the order: the next real sample is taken.
func TestFutureSampleClampedNotPinning(t *testing.T) {
	r := newRig(t, rigOpts{})
	res := one(t, r.take(clientA, nil, frame(snA, 1, t0.Add(time.Hour))), OutcomeAccepted)
	_ = res
	tr := r.pub.tracks(t)
	if !tr[0].CapturedAt.Equal(t0) || tr[0].TimeSource != core.TimeReceiver {
		t.Fatalf("captured %v source %s", tr[0].CapturedAt, tr[0].TimeSource)
	}
	if r.counters.Get(CounterTSAheadClamped) != 1 {
		t.Fatal("not counted")
	}
	r.clk.add(time.Second)
	one(t, r.take(clientA, nil, frame(snA, 2, t0.Add(time.Second))), OutcomeAccepted)
}

// A sample older than ingest_backlog_max_s is not taken (counted), a
// recent one is.
func TestTooOldRejected(t *testing.T) {
	r := newRig(t, rigOpts{})
	old := frame(snA, 1, t0.Add(-time.Hour))
	old.Backlog = true
	one(t, r.take(clientA, nil, old), RejectedTooOld)
	recent := frame(snA, 2, t0.Add(-time.Minute))
	recent.Backlog = true
	one(t, r.take(clientA, nil, recent), OutcomeAccepted)
}

// E-02 (R-07, SC-22): without a geoid the track has no AMSL altitude,
// alt_source none, counted; with one, AMSL is HAE - N for the fixture
// point.
func TestAMSLWithAndWithoutGeoid(t *testing.T) {
	r := newRig(t, rigOpts{})
	one(t, r.take(clientA, nil, frame(snA, 1, t0)), OutcomeAccepted)
	b := r.pub.tracks(t)[0].Body
	if b.AltAMSLM != nil || b.AltSource != core.AltNone || b.UndulationM != nil || r.counters.Get(CounterAltNoGeoid) != 1 {
		t.Fatalf("no geoid: %v %s %v %v", b.AltAMSLM, b.AltSource, b.UndulationM, r.counters.Snapshot())
	}
	g := newRig(t, rigOpts{geoid: true})
	one(t, g.take(clientA, nil, frame(snA, 1, t0)), OutcomeAccepted)
	b = g.pub.tracks(t)[0].Body
	if b.AltAMSLM == nil || *b.AltAMSLM != 650-15.9 || b.AltSource != core.AltGeodetic || *b.UndulationM != 15.9 {
		t.Fatalf("geoid: %v %s", b.AltAMSLM, b.AltSource)
	}
	if g.counters.Get(CounterAltNoGeoid) != 0 {
		t.Fatal("counted with a geoid")
	}
}

// R-08: a poor geodetic altitude with a pressure altitude stands in on
// pressure and holds; a category worse than every code (VA150mPlus) is
// never used.
func TestPressureFallbackAndHold(t *testing.T) {
	r := newRig(t, rigOpts{geoid: true})
	f := frame(snA, 1, t0)
	f.AccuracyV, f.AltPressureM = f3411.VA150mPlus, f64(700)
	one(t, r.take(clientA, nil, f), OutcomeAccepted)
	b := r.pub.tracks(t)[0].Body
	if b.AltSource != core.AltPressure || *b.AltAMSLM != 700 {
		t.Fatalf("poor: %s %v", b.AltSource, b.AltAMSLM)
	}
	r.clk.add(time.Second)
	g := frame(snA, 2, t0.Add(time.Second))
	g.AltPressureM = f64(701)
	one(t, r.take(clientA, nil, g), OutcomeAccepted)
	if b := r.pub.tracks(t)[1].Body; b.AltSource != core.AltPressure {
		t.Fatalf("hold: %s", b.AltSource)
	}
	r.clk.add(20 * time.Second)
	h := frame(snA, 3, t0.Add(21*time.Second))
	h.AltPressureM = f64(702)
	one(t, r.take(clientA, nil, h), OutcomeAccepted)
	if b := r.pub.tracks(t)[2].Body; b.AltSource != core.AltGeodetic {
		t.Fatalf("after the hold: %s", b.AltSource)
	}
	if r.counters.Get(CounterAltPressure) != 2 {
		t.Fatalf("alt_source_pressure %d", r.counters.Get(CounterAltPressure))
	}
}

// 04 §3.2: the bound aircraft is identified through the registry
// projection, basis authenticated; without a fresh answer it is
// registry_unavailable, never registered; ident.v1 carries each change
// once.
func TestIdentificationThroughTheRegistry(t *testing.T) {
	r := newRig(t, rigOpts{})
	one(t, r.take(clientA, nil, frame(snA, 1, t0)), OutcomeAccepted)
	id := r.pub.tracks(t)[0].Body.Identification
	if id.Status != core.IdentUnknownOperator || id.Reason != core.ReasonRegistryUnavailable || id.Basis != core.BasisAuthenticated {
		t.Fatalf("no answer: %+v", id)
	}
	k := registry.Key{Entity: registry.EntityUAS, Key: serial.Normalize(snA)} //nolint:misspell // core's name
	r.reg.all[k] = registry.Entry{Key: k, KeyFold: serial.FoldKey(snA), Status: registry.StatusValid, FetchedAt: t0}
	r.clk.add(time.Second)
	one(t, r.take(clientA, nil, frame(snA, 2, t0.Add(time.Second))), OutcomeAccepted)
	r.clk.add(time.Second)
	one(t, r.take(clientA, nil, frame(snA, 3, t0.Add(2*time.Second))), OutcomeAccepted)
	id = r.pub.tracks(t)[2].Body.Identification
	if id.Status != core.IdentRegistered || id.Reason != core.ReasonSessionBinding {
		t.Fatalf("fresh answer: %+v", id)
	}
	waitFor(t, func() bool { return len(r.pub.kind(bus.KindIdent)) == 2 })
	if n := r.counters.Get(CounterIdentPublished); n != 2 {
		t.Fatalf("ident changes %d, want 2 (first, then registered)", n)
	}
	r.in.cfg.Registry = nil
	r.clk.add(time.Second)
	one(t, r.take(clientA, nil, frame(snA, 4, t0.Add(3*time.Second))), OutcomeAccepted)
	if id := r.pub.tracks(t)[3].Body.Identification; id.Reason != core.ReasonRegistryUnavailable {
		t.Fatalf("no registry: %+v", id)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5 s")
		}
		time.Sleep(time.Millisecond)
	}
}

// E-10: the aircraft bound refuses a new aircraft (counted) and keeps
// those it follows; the sweep forgets an idle one and makes room.
func TestAircraftBoundAndSweep(t *testing.T) {
	r := newRig(t, rigOpts{maxAircraft: 1})
	one(t, r.take(clientA, nil, frame(snA, 1, t0)), OutcomeAccepted)
	one(t, r.take(clientA, nil, frame(snB, 1, t0)), RefusedCapacity)
	if r.in.Aircraft() != 1 {
		t.Fatal(r.in.Aircraft())
	}
	if n := r.in.Sweep(time.Minute); n != 0 {
		t.Fatalf("swept %d active aircraft", n)
	}
	r.clk.add(2 * time.Minute)
	if n := r.in.Sweep(time.Minute); n != 1 {
		t.Fatalf("swept %d", n)
	}
	one(t, r.take(clientA, nil, frame(snB, 2, t0.Add(2*time.Minute))), OutcomeAccepted)
}

// An operator's end sample ends its flight.
func TestOperatorEndEndsTheFlight(t *testing.T) {
	r := newRig(t, rigOpts{})
	f := frame(snA, 1, t0)
	f.End = true
	one(t, r.take(clientA, nil, f), OutcomeAccepted)
	if len(r.flights.ends) != 1 || !strings.HasSuffix(r.flights.ends[0], ":operator_ended") {
		t.Fatalf("ends %v", r.flights.ends)
	}
}

// A sample with an invalid position never reaches the bus (cell.Key
// refuses it) and nothing panics.
func TestDecodeErrorsNeverPanic(t *testing.T) {
	r := newRig(t, rigOpts{})
	f := frame(snA, 1, t0)
	f.Position = Position{Lat: 95, Lng: 0}
	one(t, r.take(clientA, nil, f), RefusedInvalid)
}

// E-01 pair (06 T3): a sample 1 km from the previous one a second later
// is flagged anomaly teleport and counted, never dropped; one 50 m away
// is not flagged.
func TestTeleportFlaggedNeverDropped(t *testing.T) {
	r := newRig(t, rigOpts{})
	one(t, r.take(clientA, nil, frame(snA, 1, t0)), OutcomeAccepted)
	near := frame(snA, 2, t0.Add(time.Second))
	near.Position.Lat += 0.00045 // about 50 m north
	r.clk.add(time.Second)
	one(t, r.take(clientA, nil, near), OutcomeAccepted)
	far := frame(snA, 3, t0.Add(2*time.Second))
	far.Position.Lat += 0.009 // about 1 km north
	r.clk.add(time.Second)
	one(t, r.take(clientA, nil, far), OutcomeAccepted)
	trs := r.pub.tracks(t)
	if trs[1].Body.Anomaly != nil || trs[2].Body.Anomaly == nil || *trs[2].Body.Anomaly != AnomalyTeleport {
		t.Fatalf("anomalies %v %v", trs[1].Body.Anomaly, trs[2].Body.Anomaly)
	}
	if r.counters.Get(CounterTeleport) != 1 {
		t.Fatal("not counted")
	}
}

// A client that restarts its seq counter is not taken for a replay: its
// new run names a new epoch, and a run without an epoch still sends new
// samples (another ts) under the old seqs. Each pair's twin, a true
// replay (same epoch, seq and ts), is acknowledged and not published.
func TestRestartedClientReusingSeqsIsTaken(t *testing.T) {
	r := newRig(t, rigOpts{})
	run := func(epoch string, seq int64, ts time.Time, want string) {
		t.Helper()
		f := frame(snA, seq, ts)
		f.Epoch = epoch
		r.clk.set(ts.Add(200 * time.Millisecond))
		one(t, r.take(clientA, nil, f), want)
	}
	for i := range 3 {
		run("boot-1", int64(i), t0.Add(time.Duration(i)*time.Second), OutcomeAccepted)
	}
	run("boot-1", 2, t0.Add(2*time.Second), OutcomeDuplicate) // replay
	// The client restarts: a new epoch, seqs from 0 again.
	for i := range 3 {
		run("boot-2", int64(i), t0.Add(time.Duration(10+i)*time.Second), OutcomeAccepted)
	}
	run("boot-2", 1, t0.Add(11*time.Second), OutcomeDuplicate) // replay of the new run
	// A client that sends no epoch and restarts: new samples, new ts.
	run("", 7, t0.Add(20*time.Second), OutcomeAccepted)
	run("", 7, t0.Add(20*time.Second), OutcomeDuplicate)
	run("", 7, t0.Add(25*time.Second), OutcomeAccepted)
	if n := len(r.pub.tracks(t)); n != 8 {
		t.Fatalf("%d tracks, want 8", n)
	}
	if r.counters.Get(CounterSeqReused) != 1 {
		t.Fatalf("seq reuse not counted: %v", r.counters.Snapshot())
	}
}
