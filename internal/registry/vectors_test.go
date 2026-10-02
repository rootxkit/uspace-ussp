//nolint:misspell // serial.Normalize is uspace-core's API name (Go spelling)
package registry

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/identify"
	"github.com/rootxkit/uspace-core/odid"
	"github.com/rootxkit/uspace-core/regnum"
	"github.com/rootxkit/uspace-core/serial"
	"github.com/rootxkit/uspace-core/vectors"
)

// The vectors run through this repository's adapters; the judgement is
// uspace-core's (CLAUDE.md rule 3). identification_status: the fixture
// registry becomes our fleet (operators and aircraft as our records
// hold them) and the F8 answers the cache holds for them, the Lookup
// joins the two, and its Resolve methods are what core resolves
// through.

type vecOperator struct {
	OperatorID         string `json:"operator_id"`
	RegistrationNumber string `json:"registration_number"`
	Status             string `json:"status"`
}

type vecUAS struct {
	DroneID            string  `json:"drone_id"`
	Label              string  `json:"label"`
	Serial             string  `json:"serial"`
	RegistrationStatus string  `json:"registration_status"`
	UASOperatorID      *string `json:"uas_operator_id"`
	InRegistry         bool    `json:"in_registry"`
}

type vecRegistry struct {
	Operators []vecOperator `json:"operators"`
	UAS       []vecUAS      `json:"uas"`
	Notes     []string      `json:"notes"`
}

// f8Status is what F8 answers for a registry status: active (and the
// empty status, which the registry reads as active) is valid; an
// expired registration is revoked; a status the registry does not know
// is passed on as given, for the Lookup to refuse as unrecognised.
func f8Status(registry string) Status {
	switch registry {
	case "active", "":
		return StatusValid
	case "suspended":
		return StatusSuspended
	case "revoked", "expired":
		return StatusRevoked
	}
	return Status(registry)
}

// lookupOf writes the vector registry as our fleet and the cached F8
// answers about it, and joins them in a Lookup.
func lookupOf(r vecRegistry) *Lookup {
	var f Fleet
	var cs []Cached
	for _, o := range r.Operators {
		f.Operators = append(f.Operators, FleetOperator{ID: o.OperatorID, RegistrationNumber: o.RegistrationNumber})
		key, _ := operatorKey(o.RegistrationNumber)
		cs = append(cs, Cached{Entry: Entry{Key: Key{EntityOperator, key}, KeyFold: key, Status: f8Status(o.Status)}})
	}
	for _, u := range r.UAS {
		f.Aircraft = append(f.Aircraft, FleetAircraft{DroneID: u.DroneID, Serial: u.Serial, OperatorID: u.UASOperatorID})
		st := f8Status(u.RegistrationStatus)
		if !u.InRegistry {
			st = StatusUnknown
		}
		key, fold := serialKey(u.Serial)
		cs = append(cs, Cached{Entry: Entry{Key: Key{EntityUAS, key}, KeyFold: fold, Status: st}})
	}
	return NewLookup(f, cs, true, defaultTTL)
}

type vecRemoteID struct {
	Identified bool    `json:"identified"`
	UAID       string  `json:"ua_id"`
	IDType     uint8   `json:"id_type"`
	OperatorID *string `json:"operator_id"`
}

type vecIdentInput struct {
	Kind             string       `json:"kind"`
	Serial           *string      `json:"serial"`
	OperatorReg      *string      `json:"operator_reg"`
	RegistryOverride *vecRegistry `json:"registry_override"`
	RemoteID         *vecRemoteID `json:"remote_id"`
	DroneID          *string      `json:"drone_id"`
}

type vecIdentExpected struct {
	Status                string  `json:"status"`
	Reason                string  `json:"reason"`
	Serial                *string `json:"serial"`
	OperatorReg           *string `json:"operator_reg"`
	Mismatch              bool    `json:"mismatch"`
	RegisteredOperatorReg *string `json:"registered_operator_reg"`
	DroneID               *string `json:"drone_id"`
}

func TestVectorsIdentificationStatusThroughTheLookup(t *testing.T) {
	f := vectors.Load(t, "identification_status.json")
	var fx struct {
		Registry vecRegistry `json:"registry"`
	}
	vectors.Unmarshal(t, f.Fixtures, &fx)
	fixture := lookupOf(fx.Registry)
	ran := map[string]int{}
	f.RunOwned(t, "ussp", func(t *testing.T, c vectors.Case) {
		var in vecIdentInput
		var exp vecIdentExpected
		c.Decode(t, &in, &exp)
		l := fixture
		if in.RegistryOverride != nil {
			l = lookupOf(*in.RegistryOverride)
		}
		var got core.Identification
		basis := core.BasisAsBroadcast
		switch in.Kind {
		case "broadcast":
			got = l.ResolveBroadcast(in.Serial, in.OperatorReg)
		case "remote_id_block":
			got = l.ResolveRemoteID(identify.RemoteIDIdentity{
				Identified: in.RemoteID.Identified, UAID: in.RemoteID.UAID,
				IDType: odid.IDType(in.RemoteID.IDType), OperatorID: in.RemoteID.OperatorID,
			})
		case "bound":
			got = l.ResolveBound(*in.DroneID)
			basis = core.BasisAuthenticated
		case "serial_conflict":
			got = identify.SerialConflict(*in.Serial, in.OperatorReg)
		default:
			t.Fatalf("unknown kind %q", in.Kind)
		}
		ran[in.Kind]++
		if got.Status != core.IdentStatus(exp.Status) || got.Reason != core.IdentReason(exp.Reason) {
			t.Errorf("%s/%s, want %s/%s", got.Status, got.Reason, exp.Status, exp.Reason)
		}
		vectors.EqualStrPtr(t, "serial", got.Serial, exp.Serial)
		vectors.EqualStrPtr(t, "operator_reg", got.OperatorReg, exp.OperatorReg)
		vectors.EqualStrPtr(t, "registered_operator_reg", got.RegisteredOperatorReg, exp.RegisteredOperatorReg)
		vectors.EqualStrPtr(t, "drone_id", got.RegistryUASID, exp.DroneID)
		if got.Mismatch != exp.Mismatch {
			t.Errorf("mismatch %v, want %v", got.Mismatch, exp.Mismatch)
		}
		if got.Basis != basis {
			t.Errorf("basis %s, want %s", got.Basis, basis)
		}
	})
	// Every kind ran (E-01: a table that skips a kind proves nothing).
	for _, k := range []string{"broadcast", "remote_id_block", "bound", "serial_conflict"} {
		if ran[k] == 0 {
			t.Errorf("no %s case ran", k)
		}
	}
	t.Logf("ran %v", ran)
}

type vecSerialInput struct {
	Kind       string  `json:"kind"`
	Serial     *string `json:"serial"`
	ClassLabel *string `json:"class_label"`
	Value      string  `json:"value"`
	Pattern    string  `json:"pattern"`
}

type vecSerialExpected struct {
	Valid           *bool   `json:"valid"`
	Problem         *string `json:"problem"`
	ProblemContains string  `json:"problem_contains"`
	Public          string  `json:"public"`
	CompareKey      string  `json:"compare_key"`
	FoldKey         string  `json:"fold_key"`
}

// serials_and_registration through the cache's key derivation: the
// operator number a query names becomes the public part asked of the
// authority and the compare key it is cached under (G-04); a serial
// becomes the fold key the change feed invalidates (G-05, G-12).
func TestVectorsSerialsAndRegistrationThroughTheCacheKeys(t *testing.T) {
	f := vectors.Load(t, "serials_and_registration.json")
	f.RunOwned(t, "ussp", func(t *testing.T, c vectors.Case) {
		var in vecSerialInput
		var exp vecSerialExpected
		c.Decode(t, &in, &exp)
		switch in.Kind {
		case "public_registration_number":
			if in.Pattern != regnum.DefaultPattern {
				t.Fatalf("pattern %q: the cache derives keys under regnum.DefaultPattern", in.Pattern)
			}
			ns, err := checkQueries([]Query{{Operator: in.Value}}, PurposeIdentification)
			if err != nil {
				t.Fatal(err)
			}
			if ns[0].operatorPublic != exp.Public || ns[0].operatorKey != exp.CompareKey {
				t.Errorf("public %q key %q, want %q %q", ns[0].operatorPublic, ns[0].operatorKey, exp.Public, exp.CompareKey)
			}
			if ns[0].keys()[0] != (Key{EntityOperator, exp.CompareKey}) {
				t.Errorf("cached under %v", ns[0].keys())
			}
		case "serial_fold":
			ns, err := checkQueries([]Query{{Serial: *in.Serial}}, PurposeIdentification)
			if err != nil {
				t.Fatal(err)
			}
			if ns[0].serialFold != exp.FoldKey || entryKeyFold(EntityUAS, ns[0].serial) != exp.FoldKey {
				t.Errorf("fold %q, want %q", ns[0].serialFold, exp.FoldKey)
			}
			if ns[0].serial != serial.Normalize(*in.Serial) || strings.TrimSpace(*in.Serial) != ns[0].serial {
				t.Errorf("key %q", ns[0].serial)
			}
		default:
			t.Fatalf("kind %q is not the ussp's", in.Kind)
		}
	})
}

type vecRelayRow struct {
	HeardAtS float64  `json:"heard_at_s"`
	LatDeg   *float64 `json:"lat_deg"`
	LonDeg   *float64 `json:"lon_deg"`
	Backlog  bool     `json:"backlog"`
	BehindS  float64  `json:"behind_s"`
	Source   string   `json:"source"`
}

type vecFleetInput struct {
	SerialIsOurs      bool          `json:"serial_is_ours"`
	RelayRows         []vecRelayRow `json:"relay_rows"`
	BroadcastPosition [2]float64    `json:"broadcast_position"`
	NowS              float64       `json:"now_s"`
	LiveForS          float64       `json:"live_for_s"`
	SpoofDistanceM    float64       `json:"spoof_distance_m"`
}

type vecFleetExpected struct {
	Verdict            string   `json:"verdict"`
	ApartM             *float64 `json:"apart_m"`
	IgnoredHistoryRows int      `json:"ignored_history_rows"`
	Problem            *string  `json:"problem"`
}

// at is the instant s seconds after t0.
func at(t0 time.Time, s float64) time.Time { return t0.Add(time.Duration(math.Round(s * 1e9))) }

// fleet_match through Lookup.FleetInput, the adapter telemetry-ingest
// calls: our rows (receipt and capture times, positions) become core's
// AuthRows, and whether the serial is ours is the Lookup's own unique
// match; the vector's serial_is_ours picks a serial of our fleet or a
// stranger's.
func TestVectorsFleetMatchThroughTheAdapter(t *testing.T) {
	f := vectors.Load(t, "fleet_match.json")
	tol, ok := f.FloatTolerance("apart_m")
	if !ok {
		t.Fatal("no apart_m tolerance")
	}
	ours := "SN-OURS"
	l := NewLookup(Fleet{Aircraft: []FleetAircraft{{DroneID: "d-ours", Serial: ours}}}, nil, true, defaultTTL)
	t0 := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	verdicts := map[string]int{}
	f.RunOwned(t, "ussp", func(t *testing.T, c vectors.Case) {
		var in vecFleetInput
		var exp vecFleetExpected
		c.Decode(t, &in, &exp)
		sn := "SN-STRANGER"
		if in.SerialIsOurs {
			sn = strings.ToLower(ours) // a folded spelling of ours is ours (G-05)
		}
		rows := make([]FleetRow, 0, len(in.RelayRows))
		for i, r := range in.RelayRows {
			row := FleetRow{HeardAt: at(t0, r.HeardAtS), Backlog: r.Backlog, Source: r.Source}
			if r.BehindS != 0 {
				row.CapturedAt = at(t0, r.HeardAtS-r.BehindS)
			}
			switch {
			case r.LatDeg != nil && r.LonDeg != nil:
				row.Pos = &core.LatLon{LatDeg: *r.LatDeg, LonDeg: *r.LonDeg}
			case r.LatDeg != nil || r.LonDeg != nil:
				t.Fatalf("relay_rows[%d] has one coordinate", i)
			}
			rows = append(rows, row)
		}
		input := l.FleetInput(sn, rows, core.LatLon{LatDeg: in.BroadcastPosition[0], LonDeg: in.BroadcastPosition[1]},
			at(t0, in.NowS), FleetThresholds{LiveForS: in.LiveForS, SpoofDistanceM: in.SpoofDistanceM})
		if input.SerialIsOurs != in.SerialIsOurs {
			t.Fatalf("serial_is_ours %v, want %v", input.SerialIsOurs, in.SerialIsOurs)
		}
		got := identify.JudgeFleet(input)
		if got.Verdict != identify.Verdict(exp.Verdict) {
			t.Errorf("verdict %q, want %q", got.Verdict, exp.Verdict)
		}
		vectors.NearPtr(t, "apart_m", got.ApartM, exp.ApartM, tol)
		if got.IgnoredHistoryRows != exp.IgnoredHistoryRows {
			t.Errorf("ignored_history_rows %d, want %d", got.IgnoredHistoryRows, exp.IgnoredHistoryRows)
		}
		switch {
		case exp.Problem == nil && got.Problem != nil:
			t.Errorf("problem %v, want none", got.Problem)
		case exp.Problem != nil && (got.Problem == nil || got.Problem.Field != *exp.Problem):
			t.Errorf("problem %v, want one naming %q", got.Problem, *exp.Problem)
		}
		verdicts[exp.Verdict]++
	})
	for _, v := range []identify.Verdict{identify.VerdictStranger, identify.VerdictWithhold, identify.VerdictAsOurs, identify.VerdictConflict} {
		if verdicts[string(v)] == 0 {
			t.Errorf("no case expects verdict %q", v)
		}
	}
}
