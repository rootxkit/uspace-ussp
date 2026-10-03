package intent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/f3548"
)

// allLocalStates is every operational_intents.local_state.
var allLocalStates = []string{StatePendingValidation, StatePendingDSS, StatePendingAuthority, StateAccepted, StateActivated,
	StateNonconforming, StateContingent, StateEnded, StateRejected, StateWithdrawn}

// The DSS state written is only ever one of the four F3548 states or
// none, for every local state and whatever the decision names (spec 04
// §4: local states never leave); pending_dss writes Accepted, and only an
// intent the DSS must hold writes anything.
func TestDSSStateWrittenIsOneOfTheFourOrNone(t *testing.T) {
	names := []*string{nil, ptr("Accepted"), ptr("Activated"), ptr("Nonconforming"), ptr("Contingent"), ptr("pending_dss"), ptr("Flying"), ptr("")}
	for _, local := range allLocalStates {
		for _, ds := range names {
			for _, managed := range []bool{true, false} {
				r := &Record{LocalState: local, Decision: Decision{DSSState: ds, InUSpaceAirspace: managed}}
				got, write := DSSDesired(r, false)
				switch {
				case !write && got != "":
					t.Fatalf("%s/%v: no write but %q", local, ds, got)
				case write && !slices.Contains(f3548.DSSStates, got):
					t.Fatalf("%s/%v: wrote %q, not an F3548 state", local, ds, got)
				case write && !managed:
					t.Fatalf("%s: an intent outside U-space airspace written", local)
				case write && local == StatePendingDSS && got != f3548.Accepted:
					t.Fatalf("pending_dss writes %s", got)
				case write && !slices.Contains(append(slices.Clone(ActiveStates), StatePendingDSS), local):
					t.Fatalf("%s is written to the DSS", local)
				}
			}
		}
	}
	// The pair: an activated intent inside U-space airspace is written
	// Activated, and with USSP_DSS_FOR_ALL one outside too.
	r := &Record{LocalState: StateActivated, Decision: Decision{DSSState: ptr("Activated")}}
	if s, ok := DSSDesired(r, true); !ok || s != f3548.Activated {
		t.Fatalf("%s %v", s, ok)
	}
	if _, ok := DSSDesired(&Record{Exempt: true, LocalState: StateAccepted, Decision: Decision{DSSState: ptr("Accepted")}}, true); ok {
		t.Fatal("an exempt intent written")
	}
}

// insideRig is a service whose requests fall in a U-space airspace.
func insideRig(t *testing.T) (*rig, *Service, *memStore) {
	t.Helper()
	g := newRig()
	in := feature{Dataset: "uspace_airspace", ID: "TSA", Type: "USPACE", Polygon: [][2]float64{{41, 44}, {41, 46}, {42, 46}, {42, 44}}}
	g.cis.features = append(g.cis.features, in.candidate(t))
	s, st, _ := newService(g)
	return g, s, st
}

func oirItems(st *memStore, id string) int {
	n := 0
	for _, o := range st.outbox {
		if o.Kind == OutboxOIR && o.EntityID == id {
			n++
		}
	}
	return n
}

// Inside U-space airspace an intent the local checks authorise waits
// pending_dss (dss_write_pending, no number, its thresholds kept) with
// its DSS work queued; outside, it is authorised at once and nothing is
// queued (E-01 pair); with USSP_DSS_FOR_ALL outside waits too.
func TestSubmitWaitsForTheDSSInsideUSpaceAirspace(t *testing.T) {
	_, s, st := insideRig(t)
	d, _, err := submit(t, s, baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	if d.Decision != DecisionPendingDSS || d.State != StatePendingDSS || d.AuthorisationNumber != nil || d.DSSState != nil ||
		d.DeviationThresholds == nil || !slices.Contains(reasons(d), ReasonDSSWritePending) {
		t.Fatalf("%+v %v", d, reasons(d))
	}
	if oirItems(st, d.IntentID) != 1 {
		t.Fatalf("DSS work not queued: %+v", st.outbox)
	}
	g := newRig()
	s2, st2, _ := newService(g)
	d2, _, err := submit(t, s2, baseRequest())
	if err != nil || d2.Decision != DecisionAuthorised || oirItems(st2, d2.IntentID) != 0 {
		t.Fatalf("outside: %s, %d items, %v", d2.Decision, oirItems(st2, d2.IntentID), err)
	}
	s2.DSSForAll = true
	d3, _, err := submit(t, s2, with(baseRequest(), "client_ref", "ref-2", "volumes", []any{wireVolumeJSON(squareWire(41.80, 44.90, 0.01), 500, 550, t0, t1)}))
	if err != nil || d3.Decision != DecisionPendingDSS || oirItems(st2, d3.IntentID) != 1 || slices.ContainsFunc(d3.Conditions, func(c Condition) bool { return c.Code == CondLocalDeconfliction }) {
		t.Fatalf("for all: %s %+v %v", d3.Decision, d3.Conditions, err)
	}
}

// The reason the intent waits names why the DSS cannot be written: down,
// our availability Down, or no DSS at all.
func TestSubmitNamesWhyTheDSSCannotBeWritten(t *testing.T) {
	for _, c := range []struct {
		dss  DSS
		want string
	}{
		{fakeDSS{ok: false, reason: "the DSS does not answer"}, ReasonDSSUnavailable},
		{fakeDSS{ok: false, reason: ReasonUSSAvailabilityDown + ": the authority set it Down"}, ReasonUSSAvailabilityDown},
	} {
		g, s, _ := insideRig(t)
		g.decider.DSS = c.dss
		d, _, err := submit(t, s, baseRequest())
		if err != nil || d.State != StatePendingDSS || !slices.Contains(reasons(d), c.want) {
			t.Fatalf("%s: %+v %v", c.want, reasons(d), err)
		}
	}
	// outside with USSP_DSS_FOR_ALL: the service names it.
	for _, c := range []struct {
		dss  DSS
		want string
	}{
		{nil, ReasonDSSUnavailable},
		{fakeDSS{ok: false, reason: ReasonUSSAvailabilityDown + ": Down"}, ReasonUSSAvailabilityDown},
		{fakeDSS{ok: false, reason: "no answer"}, ReasonDSSUnavailable},
	} {
		g := newRig()
		s, _, _ := newService(g)
		s.DSSForAll = true
		g.decider.DSS = c.dss
		d, _, err := submit(t, s, baseRequest())
		if err != nil || d.State != StatePendingDSS || !slices.Contains(reasons(d), c.want) {
			t.Fatalf("for all %s: %+v %v", c.want, reasons(d), err)
		}
	}
}

// authorisingWriter authorises every intent it is asked to write, as
// the DSS writer does after the DSS took it.
type authorisingWriter struct {
	s    *Service
	fail error
}

func (w authorisingWriter) WriteNow(ctx context.Context, id string) error {
	if w.fail != nil {
		return w.fail
	}
	r, err := w.s.Store.Get(ctx, id)
	if err != nil || r == nil {
		return err
	}
	_, err = w.s.DSSAuthorise(ctx, id, r.Version, DSSHeld{State: f3548.Accepted, OVN: "ovn-1", Version: 1, SubscriptionID: "sub-1"}, nil)
	return err
}

// The request path writes the intent to the DSS after its commit and
// answers the decision as it then stands: authorised, with its number,
// once the DSS took it; still pending_dss when the write did not finish
// (the outbox goes on; E-01 pair).
func TestSubmitWritesInTheRequestPath(t *testing.T) {
	_, s, st := insideRig(t)
	s.Writer = authorisingWriter{s: s}
	d, _, err := submit(t, s, baseRequest())
	if err != nil || d.Decision != DecisionAuthorised || d.State != StateAccepted || d.AuthorisationNumber == nil ||
		!strings.HasPrefix(*d.AuthorisationNumber, testSystem+"-") || d.DSSState == nil || *d.DSSState != "Accepted" ||
		slices.Contains(reasons(d), ReasonDSSWritePending) {
		t.Fatalf("%+v %v", d, err)
	}
	if h := st.held[d.IntentID]; h == nil || h.OVN != "ovn-1" {
		t.Fatalf("held %+v", h)
	}
	_, s2, _ := insideRig(t)
	s2.Writer = authorisingWriter{s: s2, fail: errors.New("the DSS is slow")}
	d2, _, err := submit(t, s2, baseRequest())
	if err != nil || d2.State != StatePendingDSS || d2.AuthorisationNumber != nil || s2.Counters.Get("intent_dss_write_deferred") != 1 {
		t.Fatalf("%+v %v", d2, err)
	}
}

// A modification of a DSS-held authorisation waits for the DSS again and
// keeps its authorisation number when authorised (Art. 6(6)).
func TestModifyKeepsTheNumberThroughTheDSS(t *testing.T) {
	_, s, _ := insideRig(t)
	s.Writer = authorisingWriter{s: s}
	d, _, err := submit(t, s, baseRequest())
	if err != nil || d.State != StateAccepted {
		t.Fatalf("%+v %v", d, err)
	}
	vols := []any{wireVolumeJSON(squareWire(41.71, 44.81, 0.01), 500, 550, t0, t1)}
	raw := encode(t, map[string]any{"action": "modify", "volumes": vols})
	m, err := s.Change(t.Context(), testClient, d.IntentID, raw)
	if err != nil || m.State != StateAccepted || m.AuthorisationNumber == nil || *m.AuthorisationNumber != *d.AuthorisationNumber {
		t.Fatalf("%+v %v", m, err)
	}
}

// PeerCheck: a peer's intent that came first refuses the pending intent
// (committed rejected, naming peer:<id>); a constraint refuses it; a
// peer the intent outranks is displaced, nothing written; a peer that
// cannot be judged and a stale version change nothing; the CIS as it is
// now still refuses.
func TestPeerCheck(t *testing.T) {
	peerVol := func() []f3548.Volume4D {
		n := normalise(t, baseRequest())
		return []f3548.Volume4D{n.Volumes[0].Wire}
	}
	ctx := context.Background()
	t.Run("peer first", func(t *testing.T) {
		_, s, st := insideRig(t)
		d, _, _ := submit(t, s, baseRequest())
		st.peers = []PeerIntent{{EntityID: "p1", FetchedAt: testNow, Volumes: peerVol()}}
		out, err := s.PeerCheck(ctx, d.IntentID, d.Version)
		if err != nil || out.Outcome != PeerCheckRejected || out.Conflicts[0].Ref != PeerPrefix+"p1" || out.Conflicts[0].Reason != ReasonIntentFirstCome {
			t.Fatalf("%+v %v", out, err)
		}
		r, _ := st.Get(ctx, d.IntentID)
		if r.LocalState != StateRejected || r.Decision.Decision != DecisionRejected || slices.Contains(reasons(r.Decision), ReasonDSSWritePending) ||
			!strings.Contains(r.ChangeReason, "peer:p1") {
			t.Fatalf("%+v", r.Decision)
		}
		if out, _ := s.PeerCheck(ctx, d.IntentID, d.Version); out.Outcome != PeerCheckStale {
			t.Fatalf("checked again: %+v", out)
		}
	})
	t.Run("constraint", func(t *testing.T) {
		_, s, st := insideRig(t)
		d, _, _ := submit(t, s, baseRequest())
		st.constraints = []PeerIntent{{EntityID: "c1", FetchedAt: testNow, Volumes: peerVol()}}
		out, err := s.PeerCheck(ctx, d.IntentID, d.Version)
		if err != nil || out.Outcome != PeerCheckRejected || out.Conflicts[0].Kind != KindConstraint || out.Conflicts[0].Ref != "c1" {
			t.Fatalf("%+v %v", out, err)
		}
	})
	t.Run("outranks", func(t *testing.T) {
		_, s, st := insideRig(t)
		d, _, _ := submit(t, s, baseRequest())
		st.byID[d.IntentID].Priority = 100
		st.peers = []PeerIntent{{EntityID: "p1", FetchedAt: testNow, Volumes: peerVol()}}
		out, err := s.PeerCheck(ctx, d.IntentID, d.Version)
		if err != nil || out.Outcome != PeerCheckOK || !slices.Equal(out.Displaced, []string{"p1"}) {
			t.Fatalf("%+v %v", out, err)
		}
	})
	t.Run("clear", func(t *testing.T) {
		_, s, _ := insideRig(t)
		d, _, _ := submit(t, s, baseRequest())
		out, err := s.PeerCheck(ctx, d.IntentID, d.Version)
		if err != nil || out.Outcome != PeerCheckOK || len(out.Displaced) != 0 {
			t.Fatalf("%+v %v", out, err)
		}
	})
	t.Run("not judged", func(t *testing.T) {
		_, s, st := insideRig(t)
		d, _, _ := submit(t, s, baseRequest())
		st.peers = []PeerIntent{{EntityID: "p1", FetchedAt: testNow}}
		out, err := s.PeerCheck(ctx, d.IntentID, d.Version)
		if err != nil || out.Outcome != PeerCheckNotJudged || out.Reason != ReasonDeconflictNotJudged {
			t.Fatalf("%+v %v", out, err)
		}
		st.peers = nil
		st.constraints = []PeerIntent{{EntityID: "c1", FetchedAt: testNow}}
		if out, _ := s.PeerCheck(ctx, d.IntentID, d.Version); out.Outcome != PeerCheckNotJudged {
			t.Fatalf("%+v", out)
		}
		if r, _ := st.Get(ctx, d.IntentID); r.LocalState != StatePendingDSS {
			t.Fatalf("a check that did not run changed the intent: %s", r.LocalState)
		}
	})
	t.Run("cis", func(t *testing.T) {
		g, s, _ := insideRig(t)
		d, _, _ := submit(t, s, baseRequest())
		z := feature{Dataset: "zones", ID: "TZP", Type: "PROHIBITED", Polygon: [][2]float64{{41.6, 44.7}, {41.6, 44.9}, {41.8, 44.9}, {41.8, 44.7}}}
		g.cis.features = append(g.cis.features, z.candidate(t))
		out, err := s.PeerCheck(ctx, d.IntentID, d.Version)
		if err != nil || out.Outcome != PeerCheckRejected || out.Conflicts[0].Ref != "TZP" {
			t.Fatalf("%+v %v", out, err)
		}
		g.cis.basis.Stale = true
		d2, _, _ := submit(t, s, with(baseRequest(), "client_ref", "ref-2", "volumes", []any{wireVolumeJSON(squareWire(41.9, 45.5, 0.01), 500, 550, t0, t1)}))
		if d2.State == StatePendingDSS {
			if out, _ := s.PeerCheck(ctx, d2.IntentID, d2.Version); out.Outcome != PeerCheckNotJudged {
				t.Fatalf("%+v", out)
			}
		}
	})
}

// DSSAuthorise records what the DSS holds and the notifications in one
// transaction; only the pending_dss version asked about is authorised.
func TestDSSAuthoriseAndRecord(t *testing.T) {
	_, s, st := insideRig(t)
	ctx := context.Background()
	d, _, _ := submit(t, s, baseRequest())
	notes := []OutboxSpec{{Kind: "peer_notify", EntityID: d.IntentID + "/s", Version: 1, Payload: map[string]any{}}}
	ok, err := s.DSSAuthorise(ctx, d.IntentID, d.Version+1, DSSHeld{State: f3548.Accepted, OVN: "o"}, notes)
	if err != nil || ok {
		t.Fatalf("a stale version authorised: %v %v", ok, err)
	}
	if st.held[d.IntentID] == nil || len(st.outbox) < 2 {
		t.Fatal("what the DSS holds not recorded")
	}
	if ok, err := s.DSSAuthorise(ctx, d.IntentID, d.Version, DSSHeld{State: f3548.Activated, OVN: "o"}, nil); err != nil || ok {
		t.Fatalf("authorised on a state other than Accepted: %v", err)
	}
	ok, err = s.DSSAuthorise(ctx, d.IntentID, d.Version, DSSHeld{State: f3548.Accepted, OVN: "o2", Version: 2}, nil)
	if err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	r, _ := st.Get(ctx, d.IntentID)
	if r.LocalState != StateAccepted || r.Decision.AuthorisationNumber == nil || !strings.Contains(r.ChangeReason, "DSS") {
		t.Fatalf("%+v", r.Decision)
	}
	if h, _ := s.Held(ctx, d.IntentID); h == nil || h.OVN != "o2" {
		t.Fatalf("held %+v", h)
	}
	if err := s.DSSRecord(ctx, d.IntentID, nil, nil); err != nil || st.held[d.IntentID] != nil {
		t.Fatal("not cleared")
	}
	if ok, err := s.DSSAuthorise(ctx, "00000000-0000-4000-8000-0000000000ff", 1, DSSHeld{}, nil); ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	if r, err := s.Record(ctx, "not-a-uuid"); r != nil || err != nil {
		t.Fatal("a record for an id that is no UUID")
	}
}

// DSSHold writes a new version only when the reason or its detail
// changes; an intent not pending, or another version, is left alone.
func TestDSSHold(t *testing.T) {
	_, s, st := insideRig(t)
	ctx := context.Background()
	d, _, _ := submit(t, s, baseRequest())
	if err := s.DSSHold(ctx, d.IntentID, d.Version, ReasonDSSUnavailable, "down"); err != nil {
		t.Fatal(err)
	}
	r, _ := st.Get(ctx, d.IntentID)
	if r.Version != 2 || !slices.Contains(reasons(r.Decision), ReasonDSSUnavailable) || slices.Contains(reasons(r.Decision), ReasonDSSWritePending) {
		t.Fatalf("%d %v", r.Version, reasons(r.Decision))
	}
	if oirItems(st, d.IntentID) != 2 {
		t.Fatalf("the new version's DSS work not queued")
	}
	if err := s.DSSHold(ctx, d.IntentID, 2, ReasonDSSUnavailable, "down"); err != nil {
		t.Fatal(err)
	}
	if r, _ := st.Get(ctx, d.IntentID); r.Version != 2 {
		t.Fatal("the same reason wrote a version")
	}
	if err := s.DSSHold(ctx, d.IntentID, 1, ReasonDSSKeyConflict, "x"); err != nil {
		t.Fatal(err)
	}
	if r, _ := st.Get(ctx, d.IntentID); r.Version != 2 {
		t.Fatal("an older version held")
	}
}

// A version of an intent the DSS still holds is queued even when it no
// longer needs the DSS (its reference is to be deleted), and one it
// never held is not.
func TestQueuedWhileHeld(t *testing.T) {
	g := newRig()
	s, st, _ := newService(g)
	ctx := context.Background()
	d, _, _ := submit(t, s, baseRequest())
	end := encode(t, map[string]any{"action": "end"})
	if _, err := s.Change(ctx, testClient, d.IntentID, end); err != nil {
		t.Fatal(err)
	}
	if oirItems(st, d.IntentID) != 0 {
		t.Fatal("an intent the DSS never held queued")
	}
	d2, _, _ := submit(t, s, with(baseRequest(), "client_ref", "ref-2", "volumes", []any{wireVolumeJSON(squareWire(41.80, 44.90, 0.01), 500, 550, t0, t1)}))
	st.held[d2.IntentID] = &DSSHeld{State: f3548.Accepted, OVN: "o"}
	if _, err := s.Change(ctx, testClient, d2.IntentID, end); err != nil {
		t.Fatal(err)
	}
	if oirItems(st, d2.IntentID) != 1 {
		t.Fatal("the delete of a held reference not queued")
	}
}

// PeerConflicts: our active intents a peer's intent meets, and whether
// the peer outranks them; PeerIntentDisplaced withdraws an accepted one
// with the peer named.
func TestPeerConflictsAndDisplacement(t *testing.T) {
	g := newRig()
	s, st, _ := newService(g)
	ctx := context.Background()
	d, _, _ := submit(t, s, baseRequest())
	n := normalise(t, baseRequest())
	vols := []f3548.Volume4D{n.Volumes[0].Wire}
	cs, err := s.PeerConflicts(ctx, PeerIntent{EntityID: "p1", Priority: 0, FetchedAt: testNow, Volumes: vols})
	if err != nil || len(cs) != 1 || cs[0].IntentID != d.IntentID || cs[0].PeerWinsByPriority {
		t.Fatalf("%+v %v", cs, err)
	}
	cs, err = s.PeerConflicts(ctx, PeerIntent{EntityID: "p1", Priority: 100, FetchedAt: testNow, Volumes: vols})
	if err != nil || len(cs) != 1 || !cs[0].PeerWinsByPriority {
		t.Fatalf("%+v %v", cs, err)
	}
	far := normalise(t, with(baseRequest(), "volumes", []any{wireVolumeJSON(squareWire(10, 10, 0.01), 500, 550, t0, t1)}))
	if cs, _ := s.PeerConflicts(ctx, PeerIntent{EntityID: "p2", FetchedAt: testNow, Volumes: []f3548.Volume4D{far.Volumes[0].Wire}}); len(cs) != 0 {
		t.Fatalf("%+v", cs)
	}
	if _, err := s.PeerConflicts(ctx, PeerIntent{EntityID: "p3", FetchedAt: testNow}); err == nil {
		t.Fatal("a peer intent without volumes judged")
	}
	res, err := s.PeerIntentDisplaced(ctx, d.IntentID, "p1")
	if err != nil || res.Outcome != RecheckWithdrawn {
		t.Fatalf("%+v %v", res, err)
	}
	if r, _ := st.Get(ctx, d.IntentID); r.LocalState != StateWithdrawn || !strings.Contains(r.ChangeReason, "peer:p1") {
		t.Fatalf("%s %s", r.LocalState, r.ChangeReason)
	}
	_ = time.Second
}
