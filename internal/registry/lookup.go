//nolint:misspell // serial.Normalize is uspace-core's API name (Go spelling)
package registry

import (
	"context"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/identify"
	"github.com/rootxkit/uspace-core/odid"
	"github.com/rootxkit/uspace-core/regnum"
	"github.com/rootxkit/uspace-core/serial"
)

// FleetOperator is a UAS operator this USSP serves: its id in our
// records and the registration number it registered with.
type FleetOperator struct {
	ID                 string
	RegistrationNumber string
}

// FleetAircraft is an aircraft bound to one of our clients: the id our
// records give it, its serial and its operator (nil: none recorded).
type FleetAircraft struct {
	DroneID    string
	Serial     string
	OperatorID *string
}

// Fleet is what this USSP knows of its own customers (operator
// accounts and client bindings, WP-2): the registry's answers about
// them come from the cache.
type Fleet struct {
	Operators []FleetOperator
	Aircraft  []FleetAircraft
}

// The registration statuses the Lookup hands to uspace-core for an F8
// answer that is not one of valid, suspended and revoked: identify
// reads any status it does not recognise as never active (an aircraft
// not_in_registry, an owner owner_unknown), and an empty one as active,
// so neither is ever empty.
const (
	// identUnavailable is a fleet key without a fresh answer.
	identUnavailable = "registry_unavailable"
	// identUnknown is an operator the registry answered unknown for.
	identUnknown = "unknown"
)

// identStatus maps an F8 status onto identify's registration status;
// unknown is not in the registry.
func identStatus(s Status) (status string, inRegistry bool) {
	switch s {
	case StatusValid:
		return identify.StatusActive, true
	case StatusSuspended:
		return identify.StatusSuspended, true
	case StatusRevoked:
		return identify.StatusRevoked, true
	case StatusUnknown:
		return identUnknown, false
	}
	return identUnavailable, true
}

// Lookup is identify.Lookup over our fleet and the cached answers: the
// hot path builds it from the KV projection (FromProjection), api from
// the table (FromStore). Identification itself is uspace-core's; the
// Resolve methods only make sure that a registry that could not be
// consulted resolves as identify.Unavailable (registry_unavailable),
// never as registered and never as unidentified. A Lookup is immutable
// and safe for concurrent use.
type Lookup struct {
	snap      *identify.Snapshot
	available bool
	// missingUAS and missingOp are the fleet's drone and operator ids
	// without a fresh answer.
	missingUAS map[string]bool
	missingOp  map[string]bool
}

var _ identify.Lookup = (*Lookup)(nil)

// NewLookup joins the fleet with the cached answers: an answer older
// than its TTL is not used. loaded is false when the projection does not
// exist (or the table could not be read): every resolution is then
// identify.Unavailable.
func NewLookup(f Fleet, cached []Cached, loaded bool, ttl TTL) *Lookup {
	l := &Lookup{available: loaded, missingUAS: map[string]bool{}, missingOp: map[string]bool{}}
	fresh := make(map[Key]Entry, len(cached))
	for i := range cached {
		c := &cached[i]
		if c.AgeS >= 0 && time.Duration(c.AgeS*float64(time.Second)) < ttl.For(c.Entry) {
			fresh[c.Key] = c.Entry
		}
	}
	ops := make([]identify.OperatorFacts, 0, len(f.Operators))
	for _, o := range f.Operators {
		of := identify.OperatorFacts{OperatorID: o.ID, RegistrationNumber: o.RegistrationNumber, Status: identUnavailable}
		if e, ok := fresh[Key{EntityOperator, regnum.CompareKey(o.RegistrationNumber)}]; ok {
			of.Status, _ = identStatus(e.Status)
		} else {
			l.missingOp[o.ID] = true
		}
		ops = append(ops, of)
	}
	uas := make([]identify.UASFacts, 0, len(f.Aircraft))
	for _, a := range f.Aircraft {
		u := identify.UASFacts{DroneID: a.DroneID, Serial: a.Serial, OperatorID: a.OperatorID, RegistrationStatus: identUnavailable, InRegistry: true}
		if e, ok := fresh[Key{EntityUAS, serial.Normalize(a.Serial)}]; ok {
			u.RegistrationStatus, u.InRegistry = identStatus(e.Status)
		} else {
			l.missingUAS[a.DroneID] = true
		}
		uas = append(uas, u)
	}
	l.snap = identify.NewSnapshot(ops, uas)
	return l
}

// ProjectionSource is the KV projection as the hot path reads it
// (MemoryProjector until WP-6).
type ProjectionSource interface {
	Snapshot() ([]Entry, bool)
}

// FromProjection builds the hot path's Lookup: ages on the process
// clock now (the projection carries fetched_at); a missing projection
// (src nil or not loaded) makes every resolution unavailable.
func FromProjection(f Fleet, src ProjectionSource, ttl TTL, now time.Time) *Lookup {
	if src == nil {
		return NewLookup(f, nil, false, ttl)
	}
	es, loaded := src.Snapshot()
	cs := make([]Cached, 0, len(es))
	for i := range es {
		cs = append(cs, Cached{Entry: es[i], AgeS: now.Sub(es[i].FetchedAt).Seconds()})
	}
	return NewLookup(f, cs, loaded, ttl)
}

// MaxLookupRows bounds the table read of FromStore (E-10).
const MaxLookupRows = 100_000

// FromStore builds api's Lookup from the table, ages on the database
// clock; a table that cannot be read makes every resolution
// unavailable, and the error says why.
func FromStore(ctx context.Context, f Fleet, st Store, ttl TTL) (*Lookup, error) {
	if st == nil {
		return NewLookup(f, nil, false, ttl), nil
	}
	cs, err := st.All(ctx, MaxLookupRows)
	if err != nil {
		return NewLookup(f, nil, false, ttl), err
	}
	return NewLookup(f, cs, true, ttl), nil
}

// Available reports whether the projection exists.
func (l *Lookup) Available() bool { return l.available }

// UASBySerial implements identify.Lookup (exact, else a unique folded
// match, G-05).
func (l *Lookup) UASBySerial(sn string) (identify.UASFacts, identify.Match) {
	return l.snap.UASBySerial(sn)
}

// UASByID implements identify.Lookup.
func (l *Lookup) UASByID(droneID string) (identify.UASFacts, bool) { return l.snap.UASByID(droneID) }

// Operator implements identify.Lookup.
func (l *Lookup) Operator(operatorID string) (identify.OperatorFacts, bool) {
	return l.snap.Operator(operatorID)
}

// unconsulted reports whether the aircraft a serial lookup found, or
// its operator, has no fresh answer.
func (l *Lookup) unconsulted(u identify.UASFacts, m identify.Match) bool {
	if m != identify.MatchExact && m != identify.MatchFolded {
		return false
	}
	return l.missingUAS[u.DroneID] || (u.OperatorID != nil && l.missingOp[*u.OperatorID])
}

// ResolveBroadcast is identify.ResolveBroadcast, or identify.Unavailable
// when the projection is missing or the aircraft the serial names (or
// its operator) has no fresh answer.
func (l *Lookup) ResolveBroadcast(sn, operatorReg *string) core.Identification {
	if sn != nil && serial.Normalize(*sn) != "" {
		if !l.available {
			return identify.Unavailable(sn, operatorReg)
		}
		if u, m := l.UASBySerial(*sn); l.unconsulted(u, m) {
			return identify.Unavailable(sn, operatorReg)
		}
	}
	return identify.ResolveBroadcast(l, sn, operatorReg)
}

// ResolveRemoteID is identify.ResolveRemoteID with ResolveBroadcast's
// availability for a serial identity; any other identity needs no
// registry.
func (l *Lookup) ResolveRemoteID(id identify.RemoteIDIdentity) core.Identification {
	if id.Identified && id.IDType == odid.IDTypeSerial && serial.Normalize(id.UAID) != "" {
		return l.ResolveBroadcast(&id.UAID, id.OperatorID)
	}
	return identify.ResolveRemoteID(l, id)
}

// ResolveBound is identify.ResolveBound, resolved against no registry
// (unknown_operator / registry_unavailable) when the projection is
// missing or the bound aircraft or its operator has no fresh answer.
func (l *Lookup) ResolveBound(droneID string) core.Identification {
	if !l.available {
		return identify.ResolveBound(nil, droneID)
	}
	if u, ok := l.UASByID(droneID); ok && (l.missingUAS[u.DroneID] || (u.OperatorID != nil && l.missingOp[*u.OperatorID])) {
		return identify.ResolveBound(nil, droneID)
	}
	return identify.ResolveBound(l, droneID)
}

// SerialIsOurs reports whether a broadcast serial names one of our
// fleet's aircraft: a unique match, exact or case-folded (G-05), so
// that "sn-fleet" cannot pass as a stranger while our "SN-FLEET" flies.
// Every aircraft of a Lookup is bound to one of our clients, whatever
// its registry status.
func (l *Lookup) SerialIsOurs(sn string) bool {
	_, m := l.UASBySerial(sn)
	return m == identify.MatchExact || m == identify.MatchFolded
}

// FleetRow is one authenticated telemetry row of the aircraft a
// broadcast claims, as the bus delivered it.
type FleetRow struct {
	// HeardAt is when the row was received; CapturedAt when it was
	// captured (zero: unknown).
	HeardAt    time.Time
	CapturedAt time.Time
	// Pos is where the row places the aircraft; nil when it carries none.
	Pos *core.LatLon
	// Backlog marks a row delivered from a queue after an outage.
	Backlog bool
	// Source is "" for authenticated telemetry, else the broadcast
	// source it came from.
	Source string
}

// FleetThresholds are the spoofing guard's thresholds (policy).
type FleetThresholds struct {
	LiveForS       float64
	SpoofDistanceM float64
}

// FleetInput is the input of identify.JudgeFleet for a broadcast of sn
// at broadcast, heard at now, against rows: what telemetry-ingest (WP-8)
// calls. Times become seconds before now on now's clock.
func (l *Lookup) FleetInput(sn string, rows []FleetRow, broadcast core.LatLon, now time.Time, th FleetThresholds) identify.FleetInput {
	out := identify.FleetInput{SerialIsOurs: l.SerialIsOurs(sn), Broadcast: broadcast, NowS: 0,
		LiveForS: th.LiveForS, SpoofDistanceM: th.SpoofDistanceM, Rows: make([]identify.AuthRow, 0, len(rows))}
	for _, r := range rows {
		row := identify.AuthRow{HeardAtS: r.HeardAt.Sub(now).Seconds(), Backlog: r.Backlog, Source: r.Source}
		if !r.CapturedAt.IsZero() {
			row.BehindS = r.HeardAt.Sub(r.CapturedAt).Seconds()
		}
		if r.Pos != nil {
			p := *r.Pos
			row.Pos = &p
		}
		out.Rows = append(out.Rows, row)
	}
	return out
}
