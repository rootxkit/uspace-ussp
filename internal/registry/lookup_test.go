package registry

import (
	"context"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/identify"
	"github.com/rootxkit/uspace-core/odid"
)

var (
	opID   = "op-1"
	fleet1 = Fleet{
		Operators: []FleetOperator{{ID: opID, RegistrationNumber: opNumber}},
		Aircraft:  []FleetAircraft{{DroneID: "d-1", Serial: snA, OperatorID: &opID}},
	}
	defaultTTL = TTL{Positive: 24 * time.Hour, Negative: 5 * time.Minute}
)

func fresh(e EntityType, key string, s Status, ageS float64) Cached {
	return Cached{Entry: Entry{Key: Key{e, key}, KeyFold: entryKeyFold(e, key), Status: s}, AgeS: ageS}
}

func ptr(s string) *string { return &s }

func want(t *testing.T, got core.Identification, status core.IdentStatus, reason core.IdentReason) {
	t.Helper()
	if got.Status != status || got.Reason != reason {
		t.Fatalf("%s/%s, want %s/%s", got.Status, got.Reason, status, reason)
	}
}

// Our aircraft and its operator with fresh valid answers resolve
// registered on both bases (the presence half of the pairs below).
func TestLookupResolvesWithFreshAnswers(t *testing.T) {
	l := NewLookup(fleet1, []Cached{fresh(EntityOperator, opNumber, StatusValid, 10), fresh(EntityUAS, snA, StatusValid, 10)}, true, defaultTTL)
	want(t, l.ResolveBound("d-1"), core.IdentRegistered, core.ReasonSessionBinding)
	want(t, l.ResolveBroadcast(ptr("test-sn-a"), ptr(opSecret)), core.IdentRegistered, core.ReasonMatched)
	want(t, l.ResolveRemoteID(identify.RemoteIDIdentity{Identified: true, UAID: snA, IDType: odid.IDTypeSerial, OperatorID: ptr(opNumber)}),
		core.IdentRegistered, core.ReasonMatched)
	if !l.Available() || !l.SerialIsOurs("test-sn-a") || l.SerialIsOurs("TEST-SN-Z") {
		t.Fatal("ours")
	}
	if o, ok := l.Operator(opID); !ok || o.Status != identify.StatusActive {
		t.Fatalf("operator %+v", o)
	}
}

// A missing projection is registry_unavailable on every basis, never
// unidentified and never registered (G-08, SC-22); a broadcast without
// a serial needs no registry and stays unidentified / no_serial.
func TestMissingProjectionIsUnavailable(t *testing.T) {
	for name, l := range map[string]*Lookup{
		"nil source":   FromProjection(fleet1, nil, defaultTTL, time.Now()),
		"not loaded":   FromProjection(fleet1, &MemoryProjector{}, defaultTTL, time.Now()),
		"table failed": NewLookup(fleet1, nil, false, defaultTTL),
	} {
		t.Run(name, func(t *testing.T) {
			if l.Available() {
				t.Fatal("available")
			}
			want(t, l.ResolveBound("d-1"), core.IdentUnknownOperator, core.ReasonRegistryUnavailable)
			want(t, l.ResolveBroadcast(ptr(snA), nil), core.IdentUnknownOperator, core.ReasonRegistryUnavailable)
			want(t, l.ResolveBroadcast(ptr("TEST-SN-STRANGER"), nil), core.IdentUnknownOperator, core.ReasonRegistryUnavailable)
			want(t, l.ResolveRemoteID(identify.RemoteIDIdentity{Identified: true, UAID: snA, IDType: odid.IDTypeSerial}),
				core.IdentUnknownOperator, core.ReasonRegistryUnavailable)
			want(t, l.ResolveBroadcast(nil, ptr(opNumber)), core.IdentUnidentified, core.ReasonNoSerial)
		})
	}
}

// A fleet key without a fresh answer (never fetched, or beyond its TTL)
// is registry_unavailable; the aircraft's or its operator's alone is
// enough. Through identify directly (bypassing Resolve) it is still
// never registered.
func TestFleetKeyWithoutAFreshAnswerIsUnavailable(t *testing.T) {
	cases := map[string][]Cached{
		"no uas answer":      {fresh(EntityOperator, opNumber, StatusValid, 10)},
		"no operator answer": {fresh(EntityUAS, snA, StatusValid, 10)},
		"uas expired":        {fresh(EntityOperator, opNumber, StatusValid, 10), fresh(EntityUAS, snA, StatusValid, 25*3600)},
		"unknown expired":    {fresh(EntityOperator, opNumber, StatusUnknown, 301), fresh(EntityUAS, snA, StatusValid, 10)},
		"negative age":       {fresh(EntityOperator, opNumber, StatusValid, -1), fresh(EntityUAS, snA, StatusValid, 10)},
	}
	for name, cs := range cases {
		t.Run(name, func(t *testing.T) {
			l := NewLookup(fleet1, cs, true, defaultTTL)
			want(t, l.ResolveBound("d-1"), core.IdentUnknownOperator, core.ReasonRegistryUnavailable)
			want(t, l.ResolveBroadcast(ptr(snA), ptr(opNumber)), core.IdentUnknownOperator, core.ReasonRegistryUnavailable)
			if got := identify.ResolveBroadcast(l, ptr(snA), ptr(opNumber)); got.Status == core.IdentRegistered {
				t.Fatalf("registered through identify directly: %+v", got)
			}
			if got := identify.ResolveBound(l, "d-1"); got.Status == core.IdentRegistered {
				t.Fatalf("bound registered through identify directly: %+v", got)
			}
		})
	}
	// A serial that is not ours needs no answer of ours: serial_unknown.
	l := NewLookup(fleet1, nil, true, defaultTTL)
	want(t, l.ResolveBroadcast(ptr("TEST-SN-STRANGER"), nil), core.IdentUnknownOperator, core.ReasonSerialUnknown)
}

// What the registry answered maps onto identify: suspended and revoked
// are suspended, unknown is not in the registry (an aircraft) or
// owner_unknown (an operator).
func TestLookupMapsTheF8Statuses(t *testing.T) {
	for name, tc := range map[string]struct {
		op, uas Status
		status  core.IdentStatus
		reason  core.IdentReason
	}{
		"uas suspended": {StatusValid, StatusSuspended, core.IdentSuspended, core.ReasonUASSuspended},
		"uas revoked":   {StatusValid, StatusRevoked, core.IdentSuspended, core.ReasonUASRevoked},
		"uas unknown":   {StatusValid, StatusUnknown, core.IdentUnknownOperator, core.ReasonNotInRegistry},
		"op suspended":  {StatusSuspended, StatusValid, core.IdentSuspended, core.ReasonOperatorSuspended},
		"op revoked":    {StatusRevoked, StatusValid, core.IdentSuspended, core.ReasonOperatorRevoked},
		"op unknown":    {StatusUnknown, StatusValid, core.IdentUnknownOperator, core.ReasonOwnerUnknown},
		"op garbage":    {"pending", StatusValid, core.IdentUnknownOperator, core.ReasonOwnerUnknown},
	} {
		t.Run(name, func(t *testing.T) {
			l := NewLookup(fleet1, []Cached{fresh(EntityOperator, opNumber, tc.op, 1), fresh(EntityUAS, snA, tc.uas, 1)}, true, defaultTTL)
			want(t, l.ResolveBroadcast(ptr(snA), ptr(opNumber)), tc.status, tc.reason)
		})
	}
}

// The hot path's Lookup from the projection: ages on the process clock
// from fetched_at.
func TestFromProjection(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	p := NewMemoryProjector()
	_ = p.ProjectRegistry(t.Context(), []Entry{
		{Key: Key{EntityOperator, opNumber}, KeyFold: opNumber, Status: StatusValid, FetchedAt: now.Add(-time.Hour)},
		{Key: Key{EntityUAS, snA}, KeyFold: snA, Status: StatusValid, FetchedAt: now.Add(-23 * time.Hour)},
	}, nil)
	want(t, FromProjection(fleet1, p, defaultTTL, now).ResolveBound("d-1"), core.IdentRegistered, core.ReasonSessionBinding)
	want(t, FromProjection(fleet1, p, defaultTTL, now.Add(2*time.Hour)).ResolveBound("d-1"), core.IdentUnknownOperator, core.ReasonRegistryUnavailable)
	_ = p.ProjectRegistry(t.Context(), nil, []Key{{EntityUAS, snA}})
	want(t, FromProjection(fleet1, p, defaultTTL, now).ResolveBound("d-1"), core.IdentUnknownOperator, core.ReasonRegistryUnavailable)
}

// api's Lookup from the table: a table that cannot be read is
// unavailable and says why.
func TestFromStore(t *testing.T) {
	c := newClock()
	st := newMemStore(c)
	_, _ = st.Save(t.Context(), []Entry{
		{Key: Key{EntityOperator, opNumber}, KeyFold: opNumber, Status: StatusValid},
		{Key: Key{EntityUAS, snA}, KeyFold: snA, Status: StatusValid},
	}, 0, noProjection)
	l, err := FromStore(t.Context(), fleet1, st, defaultTTL)
	if err != nil {
		t.Fatal(err)
	}
	want(t, l.ResolveBound("d-1"), core.IdentRegistered, core.ReasonSessionBinding)
	st.failRead = errDown
	l, err = FromStore(t.Context(), fleet1, st, defaultTTL)
	if err == nil || l.Available() {
		t.Fatal("an unreadable table was available")
	}
	if l, err := FromStore(t.Context(), fleet1, nil, defaultTTL); err != nil || l.Available() {
		t.Fatal("no store was available")
	}
}

func noProjection(context.Context, []Entry) error { return nil }

// The adapter WP-8 calls: times before now become seconds on now's
// clock, capture lag becomes behind_s, positions are copied.
func TestFleetInputMapping(t *testing.T) {
	l := NewLookup(fleet1, nil, true, defaultTTL)
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	pos := core.LatLon{LatDeg: 41.7, LonDeg: 44.8}
	in := l.FleetInput("test-sn-a", []FleetRow{
		{HeardAt: now.Add(-2 * time.Second), CapturedAt: now.Add(-2500 * time.Millisecond), Pos: &pos},
		{HeardAt: now.Add(-time.Second), Backlog: true, Source: "remote_id"},
	}, core.LatLon{LatDeg: 41.71, LonDeg: 44.81}, now, FleetThresholds{LiveForS: 5, SpoofDistanceM: 300})
	if !in.SerialIsOurs || in.NowS != 0 || len(in.Rows) != 2 || in.Rows[0].HeardAtS != -2 || in.Rows[0].BehindS != 0.5 ||
		in.Rows[0].Pos == &pos || in.Rows[0].Pos.LatDeg != 41.7 || in.Rows[1].BehindS != 0 || !in.Rows[1].Backlog || in.Rows[1].Pos != nil ||
		in.LiveForS != 5 || in.SpoofDistanceM != 300 {
		t.Fatalf("%+v", in)
	}
	if l.FleetInput("TEST-SN-Z", nil, pos, now, FleetThresholds{}).SerialIsOurs {
		t.Fatal("a stranger is ours")
	}
}

// Keys names exactly the entries the Lookup reads: with fresh answers
// under them the bound aircraft resolves registered; without the
// aircraft's it is registry_unavailable (E-01 pair).
func TestKeysAreWhatTheLookupReads(t *testing.T) {
	keys := Keys(fleet1)
	if len(keys) != 2 || keys[0].Entity != EntityOperator || keys[1].Entity != EntityUAS {
		t.Fatalf("keys %+v", keys)
	}
	cs := make([]Cached, 0, len(keys))
	for _, k := range keys {
		cs = append(cs, fresh(k.Entity, k.Key, StatusValid, 1))
	}
	want(t, NewLookup(fleet1, cs, true, defaultTTL).ResolveBound("d-1"), core.IdentRegistered, core.ReasonSessionBinding)
	want(t, NewLookup(fleet1, cs[:1], true, defaultTTL).ResolveBound("d-1"), core.IdentUnknownOperator, core.ReasonRegistryUnavailable)
}
