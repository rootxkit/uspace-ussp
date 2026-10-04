package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/admin"
	"github.com/rootxkit/uspace-ussp/internal/alerts"
	"github.com/rootxkit/uspace-ussp/internal/app/proc"
	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/national"
	"github.com/rootxkit/uspace-ussp/internal/national/gen"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/sources"
)

// asGen is v in the contract's type T: v's JSON decoded into T with
// unknown fields refused, so an answer of internal/admin that drifted
// from api/openapi.yaml fails instead of being sent (and its tests say
// so).
func asGen[T any](v any, err error) (T, error) {
	var out T
	if err != nil {
		return out, err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return out, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return out, fmt.Errorf("console answer does not match the contract (%T): %w", out, err)
	}
	return out, nil
}

// adminAPI is national.Admin over internal/admin.
type adminAPI struct{ S *admin.Service }

var _ national.Admin = adminAPI{}

// Flights implements national.Admin.
func (a adminAPI) Flights(ctx context.Context) (gen.AdminFlights, error) {
	v, err := a.S.Flights(ctx)
	return asGen[gen.AdminFlights](v, err)
}

// Alerts implements national.Admin.
func (a adminAPI) Alerts(ctx context.Context, recent bool) (gen.AdminAlerts, error) {
	v, err := a.S.Alerts(ctx, recent)
	return asGen[gen.AdminAlerts](v, err)
}

// Escalations implements national.Admin.
func (a adminAPI) Escalations(ctx context.Context) (gen.AdminAlerts, error) {
	v, err := a.S.Escalations(ctx)
	return asGen[gen.AdminAlerts](v, err)
}

// Escalate implements national.Admin.
func (a adminAPI) Escalate(ctx context.Context, staffID, alertID, reason string) (gen.AdminAlert, error) {
	v, err := a.S.Escalate(ctx, staffID, alertID, reason)
	return asGen[gen.AdminAlert](v, err)
}

// CloseAlert implements national.Admin.
func (a adminAPI) CloseAlert(ctx context.Context, staffID, alertID, reason string) (gen.AdminAlert, error) {
	v, err := a.S.CloseAlert(ctx, staffID, alertID, reason)
	return asGen[gen.AdminAlert](v, err)
}

// DSS implements national.Admin.
func (a adminAPI) DSS(ctx context.Context) (gen.AdminDSS, error) {
	v, err := a.S.DSS(ctx)
	return asGen[gen.AdminDSS](v, err)
}

// Inputs implements national.Admin.
func (a adminAPI) Inputs(ctx context.Context) (gen.AdminInputs, error) {
	v, err := a.S.InputsView(ctx)
	return asGen[gen.AdminInputs](v, err)
}

// Policy implements national.Admin.
func (a adminAPI) Policy(ctx context.Context) (gen.AdminPolicy, error) {
	v, err := a.S.Policy(ctx)
	return asGen[gen.AdminPolicy](v, err)
}

// PutPolicy implements national.Admin.
func (a adminAPI) PutPolicy(ctx context.Context, staffID string, in gen.PolicyPut) (gen.PolicyVersion, error) {
	v, err := a.S.PutPolicy(ctx, staffID, admin.PolicyPut{BaseVersion: in.BaseVersion, Reason: in.Reason, Values: in.Values})
	return asGen[gen.PolicyVersion](v, err)
}

// Sources implements national.Admin.
func (a adminAPI) Sources(ctx context.Context) (gen.SourceSwitches, error) {
	v, err := a.S.Sources(ctx)
	return asGen[gen.SourceSwitches](v, err)
}

// Switch implements national.Admin.
func (a adminAPI) Switch(ctx context.Context, staffID string, in gen.SourceSwitchRequest) (gen.SourceSwitches, error) {
	v, err := a.S.Switch(ctx, staffID, admin.SwitchRequest{SourceType: string(in.SourceType), InstanceID: in.InstanceId, Enabled: in.Enabled, Reason: in.Reason})
	return asGen[gen.SourceSwitches](v, err)
}

// Cases implements national.Admin.
func (a adminAPI) Cases(ctx context.Context) (gen.EmergencyCases, error) {
	v, err := a.S.Cases(ctx)
	return asGen[gen.EmergencyCases](v, err)
}

// Case implements national.Admin.
func (a adminAPI) Case(ctx context.Context, flightID string) (gen.EmergencyCase, error) {
	v, err := a.S.Case(ctx, flightID)
	return asGen[gen.EmergencyCase](v, err)
}

// Act implements national.Admin.
func (a adminAPI) Act(ctx context.Context, staffID, flightID string, in gen.EmergencyAction) (gen.EmergencyCase, bool, error) {
	deref := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	v, opened, err := a.S.Act(ctx, staffID, flightID, admin.CaseAction{Action: string(in.Action), Reason: deref(in.Reason), Text: deref(in.Text),
		Step: deref(in.Step), Outcome: deref(in.Outcome)})
	out, err := asGen[gen.EmergencyCase](v, err)
	return out, opened, err
}

// RecordDays implements national.Admin.
func (a adminAPI) RecordDays(ctx context.Context) (gen.RecordDays, error) {
	v, err := a.S.RecordDays(ctx)
	return asGen[gen.RecordDays](v, err)
}

// Events implements national.Admin.
func (a adminAPI) Events(ctx context.Context, entityType, entityID string, limit int) (gen.AdminEvents, error) {
	v, err := a.S.Events(ctx, entityType, entityID, limit)
	return asGen[gen.AdminEvents](v, err)
}

// DepSourceControl is api's readiness entry of the source switches'
// projection.
const DepSourceControl = "source_control"

// startAdmin runs the console's side of api (WP-18): the source
// switches' writer and its republication of source_control (B-09
// repair), the source statuses heard on src.v1 and the monitor's read
// from monitor_status, and the console service.
func startAdmin(ctx context.Context, rt *proc.Runtime, pol *policy.Service, kv *bus.Projector, alertSvc *alerts.Service) *admin.Service {
	counters := &core.Counters{}
	proc.Publish(rt, "admin", counters)
	logger := rt.Logger.With("component", "admin")
	writer := &sources.Writer{Store: rt.Store, Projector: kv, Counters: counters, Logger: logger}
	rt.Go(ctx, writer.Run)
	js := rt.Bus.JetStream()
	inputs := &admin.Inputs{Link: rt.Bus.Link, Counters: counters, Logger: logger,
		Monitors: func(ctx context.Context) ([]bus.MonitorEntry, error) { return bus.MonitorStatuses(ctx, js) }}
	rt.Go(ctx, func(ctx context.Context) { inputs.Run(ctx, rt.Bus.Listen) })
	svc := &admin.Service{
		Store: rt.Store, Republisher: alertSvc, Policies: pol, Switches: admin.SourcesOf{W: writer}, Inputs: inputs,
		Current: func() policy.Record {
			if r, ok := pol.Current(); ok {
				return r
			}
			return policy.Record{Values: policy.Defaults()}
		},
		Health:   rt.Health.Check,
		Counters: counters, Logger: logger, Now: time.Now,
	}
	if rt.Store.TS != nil {
		svc.Samples = admin.TSSamples{S: rt.Store}
	}
	return svc
}

// ConsoleAPI is national.Admin over s, for the integration tests that
// serve the console's routes without the whole process.
func ConsoleAPI(s *admin.Service) national.Admin { return adminAPI{S: s} }
