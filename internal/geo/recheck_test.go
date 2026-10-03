package geo

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/geodesy"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/cell"
	"github.com/rootxkit/uspace-ussp/internal/cis"
	"github.com/rootxkit/uspace-ussp/internal/intent"
)

// A notice is published as alert/v1 restriction_activated (critical) on
// alrt.v1 under the intent's first cell, flight_id null, and validates;
// an intent with no cell or a bus that fails is an error.
func TestNoticeBusPublishesAValidAlert(t *testing.T) {
	at := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	no := "USSP-DEV-GEOTESTOP0001-X"
	c5, _, err := cell.Key(core.LatLon{LatDeg: 41.705, LonDeg: 44.805})
	if err != nil {
		t.Fatal(err)
	}
	r := &intent.Record{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Cells: []string{c5}, Version: 2,
		Decision: intent.Decision{AuthorisationNumber: &no, PolicyVersion: 7}}
	starts, ends := at, at.Add(2*time.Hour)
	n := &intent.Notice{Cause: intent.CauseRestriction, Ref: "TRS001", RestrictionID: "TRS001", Reason: intent.ReasonRestrictionActive,
		Decision: intent.RecheckWithdrawn, Withdrawn: true, PreviousState: intent.StateAccepted, IntentState: intent.StateWithdrawn,
		AffectedIntents: []string{r.ID}, Window: &intent.Window{StartsAt: &starts, EndsAt: &ends}, ChangeReason: "restriction TRS001",
		Version: 2, At: at, AlertID: "4d6f0f7e-8d7c-4c1a-9e2b-3a4b5c6d7e82", Conflicts: []intent.Conflict{}}
	p := &pubRec{}
	nb := NoticeBus{Pub: p, Now: func() time.Time { return at }}
	if err := nb.PublishNotice(t.Context(), r, n); err != nil {
		t.Fatal(err)
	}
	subject, _ := bus.Alrt(KindRestrictionActivated, c5, n.AlertID)
	if len(p.subjects) != 1 || p.subjects[0] != subject {
		t.Fatalf("subjects %v", p.subjects)
	}
	raw := validate(t, schema(t, "alert/v1"), p.msgs[0])
	if !bytes.Contains(raw, []byte(`"flight_id":null`)) || !bytes.Contains(raw, []byte(`"severity":"critical"`)) {
		t.Fatalf("%s", raw)
	}
	if err := nb.PublishNotice(t.Context(), &intent.Record{ID: r.ID}, n); !errors.Is(err, ErrNoCell) {
		t.Fatalf("no cell: %v", err)
	}
	p.err = errors.New("down")
	if err := nb.PublishNotice(t.Context(), r, n); err == nil {
		t.Fatal("a failed publish not reported")
	}
}

type fakeIntents struct {
	mu     sync.Mutex
	calls  []call
	err    error
	result []intent.RecheckResult
}

type call struct {
	boxes []geodesy.BBox
	cause intent.Cause
}

func (f *fakeIntents) RecheckAll(_ context.Context, boxes []geodesy.BBox, _, _ *time.Time, cause intent.Cause) ([]intent.RecheckResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{boxes, cause})
	return f.result, f.err
}

func (f *fakeIntents) all() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

// A change re-checks the intents near the changed features with the
// dataset's cause; a truncated or warm change re-checks every active
// intent; a removed feature or ussp_list re-checks nothing; a full queue
// owes a sweep (E-01 pairs).
func TestRecheckerHandlesChanges(t *testing.T) {
	g := newCISRig(t)
	g.publish(t, cis.Restrictions, amslZone("TRS001", "PROHIBITED"))
	fi := &fakeIntents{}
	re := NewRechecker(fi, g.eval, nil, nil)
	ctx := t.Context()
	re.Handle(ctx, cis.Change{Dataset: cis.Restrictions, Version: 1, FeatureIDs: []string{"TRS001"}, Reason: cis.ChangeInstalled, CISVersion: "v"})
	cs := fi.all()
	if len(cs) != 1 || cs[0].cause.Kind != intent.CauseRestriction || len(cs[0].boxes) != 1 || cs[0].cause.CISVersion != "v" ||
		!(cs[0].boxes[0].MinLon < zoneBox[0] && cs[0].boxes[0].MaxLat > zoneBox[3]) {
		t.Fatalf("restriction change: %+v", cs)
	}
	// Gone from the version: nothing to re-check.
	re.Handle(ctx, cis.Change{Dataset: cis.Restrictions, Version: 2, FeatureIDs: []string{"TRS999"}, Reason: cis.ChangeInstalled})
	if n := len(fi.all()); n != 1 {
		t.Fatalf("a removed feature re-checked: %d", n)
	}
	re.Handle(ctx, cis.Change{Dataset: cis.Zones, Version: 3, Truncated: true, Reason: cis.ChangeInstalled})
	re.Handle(ctx, cis.Change{Dataset: cis.USpaceAirspace, Version: 3, Reason: cis.ChangeWarm})
	cs = fi.all()
	if len(cs) != 3 || cs[1].boxes != nil || cs[1].cause.Kind != intent.CauseZone || cs[2].cause.Kind != intent.CauseUSpaceAirspace {
		t.Fatalf("truncated and warm: %+v", cs[1:])
	}
	// ussp_list is not geo-awareness.
	re.Changed(ctx, cis.Change{Dataset: cis.USSPList, Version: 1})
	if len(re.ch) != 0 {
		t.Fatal("ussp_list queued")
	}
	for range ChangeQueue + 1 {
		re.Changed(ctx, cis.Change{Dataset: cis.Zones, Version: 1})
	}
	if !re.owed.Load() || re.Counters.Get(CounterRecheckQueued) != 1 {
		t.Fatal("a full queue owes no sweep")
	}
}

// Run takes the queued changes, sweeps every period, and a pass that
// fails or could not judge owes the next sweep at once.
func TestRecheckerRunSweepsAndRetries(t *testing.T) {
	g := newCISRig(t)
	g.publish(t, cis.Zones, amslZone("TZP001", "PROHIBITED"))
	fi := &fakeIntents{err: errors.New("database down")}
	re := NewRechecker(fi, g.eval, nil, nil)
	re.SweepEvery = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go re.Run(ctx)
	re.Changed(ctx, cis.Change{Dataset: cis.Zones, Version: 1, FeatureIDs: []string{"TZP001"}, Reason: cis.ChangeInstalled})
	deadline := time.Now().Add(5 * time.Second)
	for {
		cs := fi.all()
		sweeps := 0
		for _, c := range cs {
			if c.cause.Kind == intent.CauseSweep {
				sweeps++
			}
		}
		if len(cs) > 0 && cs[0].cause.Kind == intent.CauseZone && sweeps >= 2 && re.Counters.Get(CounterRecheckFailed) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("calls %+v", cs)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Constraint (WP-13's call) re-checks as a restriction.
	fi.err = nil
	rs := re.Constraint(ctx, "CON-1", []geodesy.BBox{{MinLat: 41, MinLon: 44, MaxLat: 42, MaxLon: 45}}, nil, nil)
	if rs != nil {
		t.Fatalf("results %v", rs)
	}
	last := fi.all()[len(fi.all())-1]
	if last.cause.Kind != intent.CauseRestriction || last.cause.Ref != "CON-1" {
		t.Fatalf("constraint %+v", last)
	}
	if rs := re.Constraint(ctx, "CON-2", nil, nil, nil); rs != nil {
		t.Fatal("a constraint without an extent re-checked")
	}
}

type pubRec struct {
	mu       sync.Mutex
	err      error
	subjects []string
	msgs     []bus.Enveloped
}

func (p *pubRec) Publish(_ context.Context, subject string, m bus.Enveloped) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	p.subjects = append(p.subjects, subject)
	p.msgs = append(p.msgs, m)
	return nil
}
