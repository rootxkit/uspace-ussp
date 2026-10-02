package intent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3548"

	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/registry"
)

func statusOf(t *testing.T, err error) (int, string) {
	t.Helper()
	var se interface {
		HTTPStatus() int
		ProblemSlug() string
	}
	if !errors.As(err, &se) {
		t.Fatalf("not a status error: %v", err)
	}
	return se.HTTPStatus(), se.ProblemSlug()
}

func submit(t *testing.T, s *Service, m map[string]any) (Decision, bool, error) {
	t.Helper()
	return s.Submit(t.Context(), testClient, encode(t, m))
}

// E-02: the success path's response read whole and compared, field by
// field, with what the schema example promises; projected to
// intent_active and published on intent.v1.accepted.<id>.
func TestSubmitAuthorisesAndProjects(t *testing.T) {
	g := newRig()
	s, st, pr := newService(g)
	d, created, err := submit(t, s, baseRequest())
	if err != nil || !created {
		t.Fatalf("%v %v", created, err)
	}
	if d.Decision != DecisionAuthorised || d.State != StateAccepted || d.Version != 1 || d.ClientRef != "ref-1" ||
		d.DSSState == nil || *d.DSSState != "Accepted" || d.AuthorisationNumber == nil ||
		!strings.HasPrefix(*d.AuthorisationNumber, "USSP-DEV-GEOTESTOP0001-") || len(*d.AuthorisationNumber) != len("USSP-DEV-GEOTESTOP0001-")+26 ||
		d.DeviationThresholds == nil || *d.DeviationThresholds != (Thresholds{HM: 50, VM: 15, TS: 60}) ||
		string(d.Alternative) != "null" || d.PolicyVersion != 7 || d.CISVersionChecked == nil || *d.CISVersionChecked != freshBasis().CISVersion ||
		d.RegistryCheckedAt == nil || !d.RegistryCheckedAt.Equal(testNow) || d.WeatherCheckedRef != nil || d.ExemptArt13 || d.InUSpaceAirspace ||
		!d.ValidFrom.Equal(t0) || !d.ValidTo.Equal(t1) || !d.DecidedAt.Equal(testNow) || len(d.VolumesAMSL) != 1 || d.VolumesAMSL[0].LowerAMSLM != 480 ||
		len(d.Conflicts) != 0 || len(d.Conditions) != 1 || d.Conditions[0].Code != CondLocalDeconfliction || !validUUID(d.IntentID) {
		raw, _ := json.MarshalIndent(d, "", " ")
		t.Fatalf("decision:\n%s", raw)
	}
	if g.reg.purpose != registry.PurposeAuthorisation || g.reg.calls != 1 {
		t.Fatalf("registry purpose %s calls %d", g.reg.purpose, g.reg.calls)
	}
	kv, ok := pr.kv[d.IntentID]
	if !ok || kv.LocalState != StateAccepted || len(kv.CellSet) == 0 || kv.DeviationThresholds == nil || len(kv.Volumes) != 1 || kv.FlightID != nil {
		t.Fatalf("intent_active %+v", kv)
	}
	if pr.last() != "intent.v1.accepted."+d.IntentID || len(st.versions[d.IntentID]) != 1 {
		t.Fatalf("published %v versions %d", pr.subjects, len(st.versions[d.IntentID]))
	}
	// The response is the stored decision.
	got, err := s.Get(t.Context(), testClient, d.IntentID)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(got)
	b, _ := json.Marshal(d)
	if !bytes.Equal(a, b) {
		t.Fatalf("GET differs from POST:\n%s\n%s", a, b)
	}
}

// Idempotency on (client_id, client_ref): the same body answers the same
// decision (created false, nothing new); another body under the same
// reference is 409 (E-01 pair).
func TestSubmitIsIdempotentPerClientRef(t *testing.T) {
	s, st, _ := newService(newRig())
	first, _, err := submit(t, s, baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	again, created, err := submit(t, s, baseRequest())
	if err != nil || created || again.IntentID != first.IntentID || *again.AuthorisationNumber != *first.AuthorisationNumber {
		t.Fatalf("replay: %v %v %+v", created, err, again)
	}
	if len(st.byID) != 1 {
		t.Fatalf("%d intents", len(st.byID))
	}
	_, _, err = submit(t, s, with(baseRequest(), "endurance_s", 3601))
	if code, slug := statusOf(t, err); code != http.StatusConflict || slug != "idempotency_conflict" {
		t.Fatalf("%d %s", code, slug)
	}
}

// S-M1: two overlapping intents filed in either order give the same
// pair decision: the first filed is authorised, the second refused
// naming the first (Art. 10(9)).
func TestTwoOverlappingIntentsEitherOrder(t *testing.T) {
	a := with(baseRequest(), "client_ref", "a")
	b := with(baseRequest(), "client_ref", "b", "volumes", []any{wireVolumeJSON(squareWire(41.705, 44.805, 0.01), 520, 600, t0.Add(10*time.Minute), t1)})
	for _, order := range [][2]map[string]any{{a, b}, {b, a}} {
		s, st, _ := newService(newRig())
		first, _, err := submit(t, s, order[0])
		if err != nil {
			t.Fatal(err)
		}
		st.now = st.now.Add(time.Second)
		second, _, err := submit(t, s, order[1])
		if err != nil {
			t.Fatal(err)
		}
		if first.Decision != DecisionAuthorised || second.Decision != DecisionRejected ||
			len(second.Conflicts) != 1 || second.Conflicts[0].Ref != first.IntentID || second.Conflicts[0].Reason != ReasonIntentFirstCome ||
			second.Conflicts[0].Item == nil || *second.Conflicts[0].Item != 5 || second.Conflicts[0].Overlap == nil || second.AuthorisationNumber != nil {
			t.Fatalf("%s then %s: %+v", first.Decision, second.Decision, second.Conflicts)
		}
	}
}

// First come, first served never lets a new request take the space of
// an intent already accepted at its priority, whatever the two ranks
// say: a clock that steps back (or ties, broken on the id) must not grant
// the later request and flag the earlier one. Twin: the same pair one
// second apart in the usual order refuses the later one too, and a
// request that does not overlap is authorised.
func TestFirstComeNeverGrantsOverAnAcceptedIntent(t *testing.T) {
	s, st, _ := newService(newRig())
	first, _, err := submit(t, s, with(baseRequest(), "client_ref", "first"))
	if err != nil || first.Decision != DecisionAuthorised {
		t.Fatalf("first: %v %s", err, first.Decision)
	}
	st.now = st.now.Add(-time.Second) // the database clock stepped back
	second, _, err := submit(t, s, with(baseRequest(), "client_ref", "second"))
	if err != nil {
		t.Fatal(err)
	}
	if second.Decision != DecisionRejected || len(second.Conflicts) != 1 || second.Conflicts[0].Reason != ReasonIntentFirstCome ||
		second.Conflicts[0].Ref != first.IntentID || len(st.flags) != 0 {
		t.Fatalf("second: %s %+v flags %v", second.Decision, second.Conflicts, st.flags)
	}
	st.now = st.now.Add(2 * time.Second)
	third, _, err := submit(t, s, with(baseRequest(), "client_ref", "third"))
	if err != nil || third.Decision != DecisionRejected || third.Conflicts[0].Reason != ReasonIntentFirstCome {
		t.Fatalf("third: %v %s %+v", err, third.Decision, third.Conflicts)
	}
	clear := wireVolumeJSON(squareWire(42.70, 44.80, 0.01), 500, 550, t0, t1)
	if d, _, err := submit(t, s, with(baseRequest(), "client_ref", "clear", "volumes", []any{clear})); err != nil || d.Decision != DecisionAuthorised {
		t.Fatalf("clear: %v %s", err, d.Decision)
	}
}

// S-M1: a special operation wins priority over an authorised normal
// flight, which is flagged for an update; the normal flight filed after
// a special operation is refused.
func TestSpecialOperationWinsPriority(t *testing.T) {
	s, st, _ := newService(newRig())
	normal, _, err := submit(t, s, with(baseRequest(), "client_ref", "n"))
	if err != nil {
		t.Fatal(err)
	}
	special, _, err := submit(t, s, with(baseRequest(), "client_ref", "s", "flight_type", "special_operation"))
	if err != nil {
		t.Fatal(err)
	}
	if special.Decision != DecisionAuthorised || special.Priority != 100 || st.flags[normal.IntentID] != special.IntentID {
		t.Fatalf("special %s flags %v", special.Decision, st.flags)
	}
	late, _, err := submit(t, s, with(baseRequest(), "client_ref", "l"))
	if err != nil {
		t.Fatal(err)
	}
	if late.Decision != DecisionRejected || !strings.Contains(strings.Join(reasons(late), ","), ReasonIntentPriority) {
		t.Fatalf("late normal %s %v", late.Decision, reasons(late))
	}
}

// Every refusal of the service carries its problem; each has a twin that
// passes (E-01).
func TestSubmitRefusals(t *testing.T) {
	cases := map[string]struct {
		setup  func(*Service, *memStore, *memProjector)
		m      map[string]any
		status int
		slug   string
	}{
		"annex IV":         {nil, with(baseRequest(), "mode", "x"), 400, "annex_iv_invalid"},
		"malformed":        {nil, with(baseRequest(), "colour", "red"), 400, ""},
		"no geoid":         {func(s *Service, _ *memStore, _ *memProjector) { s.Geoid = nil }, baseRequest(), 503, "geoid_unavailable"},
		"other operator":   {nil, with(baseRequest(), "operator_reg", "GEOTESTOP0002"), 403, "operator_mismatch"},
		"serial not bound": {nil, with(baseRequest(), "uas_serial", "TEST0002"), 403, "serial_not_bound"},
		"inactive operator": {func(_ *Service, st *memStore, _ *memProjector) {
			o := st.owners[testClient]
			o.OperatorStatus = "suspended"
			st.owners[testClient] = o
		}, baseRequest(), 403, "operator_inactive"},
		"unknown client":   {func(_ *Service, st *memStore, _ *memProjector) { delete(st.owners, testClient) }, baseRequest(), 403, "client_unknown"},
		"projection fails": {func(_ *Service, _ *memStore, pr *memProjector) { pr.err = errors.New("nats: timeout") }, baseRequest(), 503, "projection_unavailable"},
		"database down":    {func(_ *Service, st *memStore, _ *memProjector) { st.failTx = errors.New("conn refused") }, baseRequest(), 503, "database_unavailable"},
		"clock down":       {func(_ *Service, st *memStore, _ *memProjector) { st.failNow = errors.New("conn refused") }, baseRequest(), 503, "database_unavailable"},
	}
	for name, c := range cases {
		s, st, pr := newService(newRig())
		if c.setup != nil {
			c.setup(s, st, pr)
		}
		_, _, err := submit(t, s, c.m)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if c.slug == "" {
			var fe *core.FieldError
			if !errors.As(err, &fe) {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		if code, slug := statusOf(t, err); code != c.status || slug != c.slug {
			t.Errorf("%s: %d %s, want %d %s", name, code, slug, c.status, c.slug)
		}
		if len(st.byID) != 0 {
			t.Errorf("%s: an intent was stored", name)
		}
	}
	var ie *Error
	s, _, _ := newService(newRig())
	_, _, err := submit(t, s, with(baseRequest(), "mode", "x", "endurance_s", 0))
	if !errors.As(err, &ie) || len(ie.FieldErrors()) != 2 || !strings.Contains(ie.FieldErrors()[0].Reason+ie.FieldErrors()[1].Reason, "annex_iv.2") {
		t.Fatalf("problems: %+v", err)
	}
}

// E-10: the operator's open-intent bound refuses the next one at the
// bound and takes one below it.
func TestOpenIntentBound(t *testing.T) {
	g := newRig()
	s, _, _ := newService(g)
	s.Policy = func() policy.Record {
		r := testPolicy()
		r.Values.IntentOpenMaxCount = 2
		return r
	}
	for i, ref := range []string{"a", "b"} {
		far := wireVolumeJSON(squareWire(41.0+float64(i), 44.8, 0.01), 500, 550, t0, t1)
		if _, _, err := submit(t, s, with(baseRequest(), "client_ref", ref, "volumes", []any{far})); err != nil {
			t.Fatal(err)
		}
	}
	_, _, err := submit(t, s, with(baseRequest(), "client_ref", "c", "volumes", []any{wireVolumeJSON(squareWire(43, 44.8, 0.01), 500, 550, t0, t1)}))
	if code, slug := statusOf(t, err); code != 429 || slug != "intent_bound_reached" {
		t.Fatalf("%d %s", code, slug)
	}
}

// A peer's intent in the DSS conflicts like a local one; one that
// cannot be judged refuses the decision (503), never passes.
func TestPeerIntents(t *testing.T) {
	raw := encode(t, map[string]any{"v": []any{wireVolumeJSON(squareWire(41.70, 44.80, 0.01), 500, 550, t0, t1)}})
	var w struct {
		V []f3548.Volume4D `json:"v"`
	}
	if err := json.Unmarshal(raw, &w); err != nil {
		t.Fatal(err)
	}
	vols := w.V
	s, st, _ := newService(newRig())
	st.peers = []PeerIntent{{EntityID: "peer-1", FetchedAt: testNow.Add(-time.Hour), Volumes: vols}}
	d, _, err := submit(t, s, baseRequest())
	if err != nil || d.Decision != DecisionRejected || d.Conflicts[0].Ref != "peer:peer-1" {
		t.Fatalf("%v %+v", err, d)
	}
	s, st, _ = newService(newRig())
	st.peers = []PeerIntent{{EntityID: "peer-2", FetchedAt: testNow}}
	_, _, err = submit(t, s, baseRequest())
	if code, slug := statusOf(t, err); code != 503 || slug != "deconfliction_not_judged" {
		t.Fatalf("%d %s", code, slug)
	}
}

func patch(t *testing.T, s *Service, id string, m map[string]any) (Decision, error) {
	t.Helper()
	return s.Change(t.Context(), testClient, id, encode(t, m))
}

// An authorisation flagged for an update (a later intent with
// precedence overlaps it, Art. 10(10)) is not activated until it is
// updated: 409 update_required, nothing written or projected. Twin: the
// same intent unflagged activates in the same window.
func TestActivationRefusedWhileUpdateRequired(t *testing.T) {
	s, st, pr := newService(newRig())
	flagged, _, err := submit(t, s, with(baseRequest(), "client_ref", "flagged"))
	if err != nil {
		t.Fatal(err)
	}
	clear := wireVolumeJSON(squareWire(42.70, 44.80, 0.01), 500, 550, t0, t1)
	other, _, err := submit(t, s, with(baseRequest(), "client_ref", "other", "volumes", []any{clear}))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InTx(t.Context(), func(ctx context.Context, tx Tx) error {
		return tx.FlagUpdate(ctx, []string{flagged.IntentID}, "00000000-0000-4000-8000-0000000000ff", testNow)
	}); err != nil {
		t.Fatal(err)
	}
	st.now = t0.Add(-5 * time.Minute)
	published := len(pr.subjects)
	_, err = patch(t, s, flagged.IntentID, map[string]any{"action": "activate"})
	if code, slug := statusOf(t, err); code != 409 || slug != "update_required" {
		t.Fatalf("flagged: %d %s", code, slug)
	}
	if r := st.byID[flagged.IntentID]; r.LocalState != StateAccepted || r.Version != 1 || len(pr.subjects) != published {
		t.Fatalf("written: %s v%d, %d published", r.LocalState, r.Version, len(pr.subjects)-published)
	}
	if a, err := patch(t, s, other.IntentID, map[string]any{"action": "activate"}); err != nil || a.State != StateActivated {
		t.Fatalf("unflagged: %v %+v", err, a)
	}
}

// Activation is confirmed in the response inside its window and refused
// outside it or in another state; end removes the intent from
// intent_active.
func TestActivateAndEnd(t *testing.T) {
	s, st, pr := newService(newRig())
	d, _, err := submit(t, s, baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	// testNow is 22 h before time_start: too early under a 600 s lead.
	_, err = patch(t, s, d.IntentID, map[string]any{"action": "activate"})
	if code, slug := statusOf(t, err); code != 409 || slug != "activation_refused" {
		t.Fatalf("early: %d %s", code, slug)
	}
	st.now = t0.Add(-5 * time.Minute)
	a, err := patch(t, s, d.IntentID, map[string]any{"action": "activate"})
	if err != nil || a.State != StateActivated || a.Version != 2 || a.DSSState == nil || *a.DSSState != "Activated" ||
		*a.AuthorisationNumber != *d.AuthorisationNumber || pr.kv[d.IntentID].LocalState != StateActivated {
		t.Fatalf("activate: %v %+v", err, a)
	}
	_, err = patch(t, s, d.IntentID, map[string]any{"action": "activate"})
	if code, _ := statusOf(t, err); code != 409 {
		t.Fatal("activated twice")
	}
	e, err := patch(t, s, d.IntentID, map[string]any{"action": "end", "change_reason": "landed"})
	if err != nil || e.State != StateEnded || e.DSSState != nil || e.ChangeReason == nil || *e.ChangeReason != "landed" {
		t.Fatalf("end: %v %+v", err, e)
	}
	if _, ok := pr.kv[d.IntentID]; ok || pr.last() != "intent.v1.ended."+d.IntentID {
		t.Fatalf("still in intent_active, or not published: %v", pr.subjects)
	}
	_, err = patch(t, s, d.IntentID, map[string]any{"action": "end"})
	if code, slug := statusOf(t, err); code != 409 || slug != "end_refused" {
		t.Fatalf("end twice: %d %s", code, slug)
	}
	// After time_end an accepted intent is not activated.
	d2, _, err := submit(t, s, with(baseRequest(), "client_ref", "x2", "volumes", []any{wireVolumeJSON(squareWire(41.70, 44.80, 0.01), 500, 550, t1.Add(time.Minute), t1.Add(time.Hour))}))
	if err != nil || d2.Decision != DecisionAuthorised {
		t.Fatalf("%v %s", err, d2.Decision)
	}
	st.now = t1.Add(2 * time.Hour)
	_, err = patch(t, s, d2.IntentID, map[string]any{"action": "activate"})
	if code, _ := statusOf(t, err); code != 409 {
		t.Fatal("activated after time_end")
	}
}

// Modify re-decides the new volumes as a new version: still authorised
// keeps the number; refused loses it and leaves intent_active.
func TestModify(t *testing.T) {
	g := newRig()
	s, _, pr := newService(g)
	d, _, err := submit(t, s, baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	moved := []any{wireVolumeJSON(squareWire(41.72, 44.80, 0.01), 500, 550, t0, t1)}
	m, err := patch(t, s, d.IntentID, map[string]any{"action": "modify", "volumes": moved})
	if err != nil || m.Decision != DecisionAuthorised || m.Version != 2 || *m.AuthorisationNumber != *d.AuthorisationNumber || m.IntentID != d.IntentID {
		t.Fatalf("modify: %v %+v", err, m)
	}
	g.cis.features = append(g.cis.features, feature{Dataset: "zones", ID: "TZ-P", Type: "PROHIBITED",
		Polygon: [][2]float64{{41.71, 44.79}, {41.71, 44.82}, {41.74, 44.82}, {41.74, 44.79}}}.candidate(t))
	r, err := patch(t, s, d.IntentID, map[string]any{"action": "modify", "volumes": moved})
	if err != nil || r.Decision != DecisionRejected || r.AuthorisationNumber != nil || r.Version != 3 {
		t.Fatalf("modify into a zone: %v %+v", err, r)
	}
	if _, ok := pr.kv[d.IntentID]; ok {
		t.Fatal("a refused modification stays in intent_active")
	}
	_, err = patch(t, s, d.IntentID, map[string]any{"action": "modify", "volumes": moved})
	if code, slug := statusOf(t, err); code != 409 || slug != "modify_refused" {
		t.Fatalf("modify a rejected intent: %d %s", code, slug)
	}
	for name, body := range map[string]map[string]any{
		"unknown action":     {"action": "pause"},
		"modify, no volumes": {"action": "modify"},
		"end with volumes":   {"action": "end", "volumes": moved},
	} {
		if _, err := patch(t, s, d.IntentID, body); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// Another operator's intent is not found, never forbidden; an id that
// is no UUID is not found either.
func TestOwnershipIsEnforced(t *testing.T) {
	s, st, _ := newService(newRig())
	d, _, err := submit(t, s, baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	st.owners["client-other"] = Owner{ClientID: "client-other", OperatorID: "00000000-0000-4000-8000-000000000002", OperatorKey: "GEOTESTOP0002", OperatorStatus: "active", ClientStatus: "active"}
	for _, id := range []string{d.IntentID, "not-a-uuid"} {
		if _, err := s.Get(t.Context(), "client-other", id); err == nil {
			t.Fatal("another operator read the intent")
		} else if code, _ := statusOf(t, err); code != 404 {
			t.Fatalf("%d", code)
		}
		if _, err := s.Change(t.Context(), "client-other", id, []byte(`{"action":"end"}`)); err == nil {
			t.Fatal("another operator ended the intent")
		}
	}
	list, err := s.List(t.Context(), testClient, ListFilter{})
	if err != nil || len(list) != 1 {
		t.Fatalf("own list %v %v", list, err)
	}
	if list, err := s.List(t.Context(), "client-other", ListFilter{}); err != nil || len(list) != 0 {
		t.Fatalf("other list %v %v", list, err)
	}
	if _, err := s.List(t.Context(), testClient, ListFilter{State: "flying"}); err == nil {
		t.Fatal("an unknown state filter")
	}
}

// The sweep ends what is past time_end and nothing else (E-01 pair).
func TestEndDue(t *testing.T) {
	s, st, pr := newService(newRig())
	d, _, err := submit(t, s, baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.EndDue(t.Context()); err != nil || n != 0 {
		t.Fatalf("before time_end: %d %v", n, err)
	}
	st.now = t1.Add(time.Second)
	if n, err := s.EndDue(t.Context()); err != nil || n != 1 {
		t.Fatalf("after time_end: %d %v", n, err)
	}
	if r := st.byID[d.IntentID]; r.LocalState != StateEnded || r.Decision.ChangeReason == nil || *r.Decision.ChangeReason != "time_end passed" {
		t.Fatalf("%+v", r.Decision)
	}
	if _, ok := pr.kv[d.IntentID]; ok {
		t.Fatal("still in intent_active")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	s.RunSweep(ctx, time.Hour) // returns at once on a cancelled context
}

// The decision when a dependency is absent: each one refuses or holds,
// never passes (CLAUDE.md rule 4).
func TestMissingDependenciesNeverAuthorise(t *testing.T) {
	cases := map[string]func(*Decider){
		"no CIS":       func(d *Decider) { d.CIS = nil },
		"no integrity": func(d *Decider) { d.Integrity = nil },
		"no registry":  func(d *Decider) { d.Registry = nil },
		"registry error": func(d *Decider) {
			d.Registry = &fakeRegistry{err: errors.New("bad query")}
		},
		"CIS error":    func(d *Decider) { d.CIS = &fakeCIS{basis: freshBasis(), err: "the envelope is not valid"} },
		"no system id": func(d *Decider) { d.SystemID = "" },
	}
	for name, mutate := range cases {
		g := newRig()
		mutate(g.decider)
		d, _ := g.decider.Decide(t.Context(), normalise(t, baseRequest()), testPolicy(), testNow, newID(), testNow, nil)
		if d.Decision == DecisionAuthorised || d.AuthorisationNumber != nil {
			t.Errorf("%s: %s", name, d.Decision)
		}
	}
	g := newRig()
	g.decider.DSS = nil
	in := feature{Dataset: "uspace_airspace", ID: "TSA", Type: "USPACE", Polygon: [][2]float64{{41, 44}, {41, 46}, {42, 46}, {42, 44}}}
	g.cis.features = append(g.cis.features, in.candidate(t))
	if d, _ := g.decider.Decide(t.Context(), normalise(t, baseRequest()), testPolicy(), testNow, newID(), testNow, nil); d.Decision != DecisionPendingDSS {
		t.Errorf("no DSS inside U-space airspace: %s", d.Decision)
	}
}

// The airspace's service performance may tighten the thresholds, never
// loosen them.
func TestAirspaceThresholdOverride(t *testing.T) {
	g := newRig()
	in := feature{Dataset: "uspace_airspace", ID: "TSA", Type: "USPACE", Polygon: [][2]float64{{41, 44}, {41, 46}, {42, 46}, {42, 44}},
		ServicePerf: map[string]float64{"deviation_h_m": 20, "deviation_v_m": 30, "deviation_t_s": 30}}
	g.cis.features = append(g.cis.features, in.candidate(t))
	d, _ := g.decider.Decide(t.Context(), normalise(t, baseRequest()), testPolicy(), testNow, newID(), testNow, nil)
	if d.DeviationThresholds == nil || *d.DeviationThresholds != (Thresholds{HM: 20, VM: 15, TS: 30}) {
		t.Fatalf("%+v %v", d.DeviationThresholds, reasons(d))
	}
}

// Every valid example of the three intent schemas round-trips through
// its Go type unchanged (E-03): the published examples are what the
// code reads and writes. The decoder is strict, so a member the type
// does not know fails.
func TestSchemaExamplesRoundTrip(t *testing.T) {
	n := 0
	for dir, newV := range map[string]func() any{
		"request":  func() any { return &Request{} },
		"decision": func() any { return &Decision{} },
		"state":    func() any { return &StateBody{} },
	} {
		files, err := filepath.Glob("../../schemas/intent/" + dir + "/v1/examples/*.json")
		if err != nil || len(files) == 0 {
			t.Fatalf("%s: no examples (%v)", dir, err)
		}
		for _, name := range files {
			raw, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			v := newV()
			if err := strictDecode(raw, v); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			back, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			if !jsonEqual(t, raw, back) {
				t.Fatalf("%s does not round-trip: %s / %s", name, raw, back)
			}
			n++
		}
	}
	t.Logf("%d examples round-trip", n)
}

func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	ja, _ := json.Marshal(x)
	jb, _ := json.Marshal(y)
	return bytes.Equal(ja, jb)
}
