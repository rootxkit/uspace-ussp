package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/admin"
	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/national/gen"
	"github.com/rootxkit/uspace-ussp/internal/obs"
)

func sp(s string) *string       { return &s }
func tp(t time.Time) *time.Time { return &t }
func fp(f float64) *float64     { return &f }

// Every answer of internal/admin, with every optional member set,
// decodes into its contract type with unknown fields refused: a name
// that drifted from api/openapi.yaml fails here, not on the console.
func TestConsoleAnswersMatchTheContract(t *testing.T) {
	now := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	id := "6f1c2b9e-1d2a-4c3b-8a7d-0e1f2a3b4c5d"
	alert := admin.Alert{AlertID: id, Kind: "proximity", Severity: "critical", State: "raised", ClearReason: sp("resolved"), FlightID: sp(id),
		IntentID: sp(id), AuthorisationNumber: sp("A-1"), RaisedAt: now, UpdatedAt: now, ClearedAt: tp(now), AckedAt: tp(now), AckedBy: sp("c"),
		EscalatedAt: tp(now), EscalatedBy: sp("s"), EscalationReason: sp("r"), ClosedAt: tp(now), ClosedBy: sp("s"), CloseReason: sp("r"),
		MessagesRecorded: 3, PolicyVersion: 2, Detail: map[string]any{"d_cpa_h_m": 12.5}}
	cases := []struct {
		name string
		v    any
		into func([]byte) error
	}{
		{"flights", admin.Flights{Flights: []admin.Flight{{FlightID: id, IntentID: sp(id), AuthorisationNumber: sp("A"), UASSerial: "TEST1", OperatorReg: sp("GEO"),
			ClientID: sp("op-1"), StartedAt: now, LastState: sp("airborne"), Emergency: true, IntentState: sp("activated"), DSSState: sp("Activated"),
			Conformance: &admin.Conformance{State: "conforming", At: now, Reason: sp("x")}, LastSampleAt: tp(now), LastSampleAgeS: fp(1), EmergencyCaseOpen: true}},
			Samples: admin.Samples{Available: false, Detail: sp("down")}}, decodeInto[gen.AdminFlights]},
		{"alerts", admin.Alerts{Alerts: []admin.Alert{alert}, Truncated: true, EscalationAfterS: 30, EscalationRepeatS: 10, PolicyVersion: 2}, decodeInto[gen.AdminAlerts]},
		{"dss", admin.DSS{DSSState: admin.DSSState{USSAvailability: sp("Normal"), SetBy: sp("x"), SetAt: tp(now), DSSReachableSince: tp(now), DSSUnreachableSince: tp(now)},
			Readiness:     obs.DependencyStatus{State: obs.StateDown, Required: false, Since: now, AgeS: fp(3), Detail: "x"},
			Outbox:        admin.Outbox{Pending: 2, ByKind: map[string]int64{"oir_put": 2}, OldestCreatedAt: tp(now), Retrying: 1},
			LastErrors:    []admin.OutboxError{{Kind: "oir_put", EntityID: id, Attempts: 2, LastError: "503", NextAt: now, DoneAt: tp(now)}},
			Subscriptions: []admin.Subscription{{SubscriptionID: "s", Kind: "utm", TimeEnd: now, RenewedAt: tp(now), NotificationIndex: 4}}}, decodeInto[gen.AdminDSS]},
		{"inputs", admin.InputsView{CheckedAt: now, Bus: admin.Bus{State: "disconnected", Since: now, Detail: sp("x")}, Switches: admin.SwitchesState{State: admin.SwitchesUnavailable, Detail: sp("x")},
			Sources: []admin.Input{{Source: "operator_ws", SourceInstance: sp("op-1"), State: "disabled", Since: tp(now), LastHeardAt: tp(now), AgeS: fp(1), LagS: fp(2),
				Disabled: &admin.Disabled{By: "instance", ByWho: "admin.a", At: now, Reason: "r"}, Detail: sp("x"), Counters: map[string]uint64{"accepted": 1}}},
			SourcesTruncated: true,
			Monitor: admin.Monitor{State: "down", LastHeardAt: tp(now), Detail: "stopped", Instances: []admin.MonitorInstance{{MonitorStatus: bus.MonitorStatus{
				Instance: "m1", At: now, Workers: 1, FlightsTracked: 2, States: map[string]int{"conforming": 2}, EvaluationPeriodS: 1, CPAEvaluationPeriodS: 1,
				OutboxDepth: 0, PolicyVersion: 2, IntentActiveAgeS: fp(0), CISLoaded: true, CISVersion: "v", CISAgeS: 3, CISStale: false, Terrain: true, Geoid: true}, AgeS: 4}}},
			Dependencies: map[string]obs.DependencyStatus{"nats": {State: obs.StateUp, Since: now, AgeS: fp(0)}}}, decodeInto[gen.AdminInputs]},
		{"policy", admin.Policy{Current: admin.PolicyVersion{Version: 2, CreatedAt: tp(now), Actor: sp("a"), Reason: sp("r"), Values: map[string]any{"cis_stale_s": 300.0},
			Changes: []admin.Change{{Field: "cis_stale_s", From: 200.0, To: 300.0}}}, History: []admin.PolicyVersion{}, PendingGCAA: []string{"lost_link_s"}}, decodeInto[gen.AdminPolicy]},
		{"sources", admin.Switches{Switches: []admin.SourceSwitchView{{SourceType: "operator_ws", InstanceID: sp("op-1"), Enabled: false, Reason: "r", Actor: "a",
			ChangedAt: now, Version: 3}}, Version: 3, Epoch: "e", KnownTypes: []string{"operator_ws"}}, decodeInto[gen.SourceSwitches]},
		{"cases", admin.Cases{Cases: []admin.Case{{CaseID: id, FlightID: id, IntentID: sp(id), AuthorisationNumber: sp("A"), UASSerial: "TEST1", OpenedAt: now,
			OpenedBy: "s", Reason: "r", ContactRef: sp("EC-TEST-1"), ContactProcedure: "p",
			Checklist: []admin.Step{{Step: "operator_contacted", DoneAt: tp(now), DoneBy: sp("s")}},
			Notes:     []admin.Note{{At: now, Author: "s", Step: sp("operator_contacted"), Text: "t"}}, ClosedAt: tp(now), ClosedBy: sp("s"), Outcome: sp("o"),
			RecordLink: "/v1/records/flights/" + id}}, Truncated: true}, decodeInto[gen.EmergencyCases]},
		{"days", admin.RecordDays{Days: []admin.RecordDay{{Date: "2026-10-04", BuiltAt: now, ContentHash: strings.Repeat("a", 64), Flights: 2}}, Missing: []string{"2026-10-03"}},
			decodeInto[gen.RecordDays]},
		{"events", admin.Events{Events: []admin.Event{{ID: 1, TS: now, ActorType: "staff", ActorID: "s", EntityType: "alert", EntityID: sp(id), EventType: "alert_closed",
			Payload: map[string]any{"reason": "r"}}}, Truncated: true}, decodeInto[gen.AdminEvents]},
	}
	for _, c := range cases {
		raw, err := json.Marshal(c.v)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.into(raw); err != nil {
			t.Errorf("%s: %v\n%s", c.name, err, raw)
		}
	}
	// The presence twin: a member the contract does not have is refused.
	if _, err := asGen[gen.AdminFlights](map[string]any{"flights": []any{}, "truncated": false, "samples": map[string]any{"available": true}, "extra": 1}, nil); err == nil {
		t.Fatal("an unknown member passed the contract check")
	}
}

func decodeInto[T any](raw []byte) error {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	_, err := asGen[T](v, nil)
	return err
}
