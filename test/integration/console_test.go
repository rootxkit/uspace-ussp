//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	coresources "github.com/rootxkit/uspace-core/sources"

	"github.com/rootxkit/uspace-ussp/internal/admin"
	"github.com/rootxkit/uspace-ussp/internal/alerts"
	"github.com/rootxkit/uspace-ussp/internal/app/api"
	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/national"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/sources"
)

// republished records the console's republishes.
type republished struct {
	mu  sync.Mutex
	ids []string
}

func (r *republished) Republish(_ context.Context, st alerts.Stored) {
	r.mu.Lock()
	r.ids = append(r.ids, st.AlertID)
	r.mu.Unlock()
}

func (r *republished) n() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.ids) }

// switchableProjector refuses every projection while err is set.
type switchableProjector struct {
	mu  sync.Mutex
	err error
}

func (p *switchableProjector) set(err error) { p.mu.Lock(); p.err = err; p.mu.Unlock() }
func (p *switchableProjector) fail() error   { p.mu.Lock(); defer p.mu.Unlock(); return p.err }
func (p *switchableProjector) ProjectPolicy(context.Context, policy.Record) error {
	return p.fail()
}
func (p *switchableProjector) ProjectSources(context.Context, coresources.State) error {
	return p.fail()
}

// samples answers the last samples, or err.
type samples struct {
	err error
	at  time.Time
}

func (s samples) LastSamples(_ context.Context, ids []string) (map[string]time.Time, error) {
	if s.err != nil {
		return nil, s.err
	}
	out := map[string]time.Time{}
	for _, id := range ids {
		out[id] = s.at
	}
	return out, nil
}

// consoleRig is the api's console routes on the real database, with
// staff of each role signed in.
type consoleRig struct {
	s                       *stack
	svc                     *admin.Service
	pol                     *policy.Service
	rep                     *republished
	proj                    *switchableProjector
	supervisor, support, ad string
	superID, adminID        string
}

func newConsoleRig(t *testing.T) *consoleRig {
	t.Helper()
	c := newClock()
	g := &consoleRig{rep: &republished{}, proj: &switchableProjector{}}
	st := appStore(t)
	g.pol = policy.New(st, g.proj, nil)
	if _, err := g.pol.Load(context.Background()); err != nil && !errors.Is(err, policy.ErrNoPolicy) {
		t.Fatal(err)
	}
	g.svc = &admin.Service{Store: st, Republisher: g.rep, Policies: g.pol,
		Switches: admin.SourcesOf{W: &sources.Writer{Store: st, Projector: g.proj}}, Inputs: &admin.Inputs{},
		Current: func() policy.Record {
			if r, ok := g.pol.Current(); ok {
				return r
			}
			return policy.Record{Values: policy.Defaults()}
		}, Logger: quiet()}
	g.s = newStackWith(t, c, &logBuffer{}, nil, func(ns *national.Server) { ns.Admin = api.ConsoleAPI(g.svc) })
	u := unique()
	sign := func(role string) (string, string) {
		user, pass := role+".console."+u, "staff-password-"+u
		created, err := g.s.svc.CreateStaff(context.Background(), user, pass, role, "test")
		if err != nil {
			t.Fatal(err)
		}
		code := ""
		if role == auth.RoleAdmin {
			code = totpNow(t, created.TOTPSecret, c.Now())
		}
		r := g.s.login(auth.RealmConsole, user, pass, code)
		if r.status != 200 {
			t.Fatalf("%s sign-in: %d %s", role, r.status, r.raw)
		}
		return r.str("token"), created.ID
	}
	g.supervisor, g.superID = sign(auth.RoleSupervisor)
	g.support, _ = sign(auth.RoleSupport)
	g.ad, g.adminID = sign(auth.RoleAdmin)
	return g
}

func (g *consoleRig) as(tok string) func(*testing.T, string, string, any) resp {
	return func(t *testing.T, method, path string, body any) resp {
		t.Helper()
		return g.s.call(method, path, body, bearer(tok))
	}
}

func list(r resp, key string) []map[string]any {
	raw, _ := r.body[key].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, x := range raw {
		if m, ok := x.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func byID(rows []map[string]any, key, id string) map[string]any {
	for _, r := range rows {
		if r[key] == id {
			return r
		}
	}
	return nil
}

// The console's alerts (WP-18): support reads and may not act; a
// supervisor's escalation writes its events row once (a repeat writes
// nothing and republishes nothing) and puts the alert on the
// escalations; a close takes it off, never clears it, and a second close
// is 409; every refusal leaves no row.
func TestIntegrationConsoleAlerts(t *testing.T) {
	g := newConsoleRig(t)
	seed := seedFlight(t, "activated", nil, time.Now().Add(-10*time.Minute))
	id := seedProximity(t, seed, "pair-"+unique(), 12.0, 3.0, time.Now().Add(-time.Minute))
	support, super := g.as(g.support), g.as(g.supervisor)

	r := support(t, "GET", "/v1/admin/alerts", nil)
	a := byID(list(r, "alerts"), "alert_id", id)
	if r.status != 200 || a == nil || a["state"] != "raised" || a["messages_recorded"] != 1.0 || r.body["escalation_after_s"] == nil {
		t.Fatalf("alerts: %d %s", r.status, r.raw)
	}
	if r := support(t, "GET", "/v1/admin/alerts?view=everything", nil); r.status != 400 {
		t.Fatalf("an unknown view: %d", r.status)
	}
	if r := support(t, "POST", "/v1/admin/alerts/"+id+"/escalate", map[string]any{"reason": "support tries"}); r.status != 403 {
		t.Fatalf("support escalates: %d %s", r.status, r.raw)
	}
	if r := super(t, "POST", "/v1/admin/alerts/"+id+"/escalate", map[string]any{"reason": ""}); r.status != 400 {
		t.Fatalf("no reason: %d %s", r.status, r.raw)
	}
	if n := events(t, admin.EntityAlert, id, admin.EventAlertEscalated); n != 0 || g.rep.n() != 0 {
		t.Fatalf("a refusal wrote %d events, %d republishes", n, g.rep.n())
	}
	r = super(t, "POST", "/v1/admin/alerts/"+id+"/escalate", map[string]any{"reason": "operator does not answer"})
	if r.status != 200 || r.str("escalated_by") != g.superID || r.str("escalation_reason") != "operator does not answer" || r.str("escalated_at") == "" {
		t.Fatalf("escalate: %d %s", r.status, r.raw)
	}
	if n := events(t, admin.EntityAlert, id, admin.EventAlertEscalated); n != 1 || g.rep.n() != 1 {
		t.Fatalf("escalation: %d events, %d republishes", n, g.rep.n())
	}
	if r := super(t, "POST", "/v1/admin/alerts/"+id+"/escalate", map[string]any{"reason": "again"}); r.status != 200 || r.str("escalation_reason") != "operator does not answer" {
		t.Fatalf("a second escalation: %d %s", r.status, r.raw)
	}
	if n := events(t, admin.EntityAlert, id, admin.EventAlertEscalated); n != 1 || g.rep.n() != 1 {
		t.Fatalf("a repeat wrote: %d events, %d republishes", n, g.rep.n())
	}
	if e := byID(list(support(t, "GET", "/v1/admin/escalations", nil), "alerts"), "alert_id", id); e == nil {
		t.Fatal("the escalation is not listed")
	}
	if r := super(t, "POST", "/v1/admin/alerts/"+id+"/close", map[string]any{"reason": "operator landed, confirmed by phone"}); r.status != 200 ||
		r.str("closed_by") != g.superID || r.str("state") != "raised" {
		t.Fatalf("close: %d %s", r.status, r.raw)
	}
	if r := super(t, "POST", "/v1/admin/alerts/"+id+"/close", map[string]any{"reason": "again"}); r.status != 409 || r.slug() != admin.SlugAlreadyClosed {
		t.Fatalf("a second close: %d %s", r.status, r.raw)
	}
	if n := events(t, admin.EntityAlert, id, admin.EventAlertClosed); n != 1 {
		t.Fatalf("close events %d", n)
	}
	if e := byID(list(support(t, "GET", "/v1/admin/escalations", nil), "alerts"), "alert_id", id); e != nil {
		t.Fatal("a closed escalation is still listed")
	}
	var state string
	if err := appPool(t).QueryRow(context.Background(), "SELECT state FROM alerts WHERE id = $1::uuid", id).Scan(&state); err != nil || state != "raised" {
		t.Fatalf("the close changed the alert's state: %s %v", state, err)
	}
	if r := super(t, "POST", "/v1/admin/alerts/"+strings.Repeat("0", 8)+"-0000-4000-8000-000000000000/close", map[string]any{"reason": "x"}); r.status != 404 {
		t.Fatalf("unknown alert: %d", r.status)
	}
	// The audit walk: every row carries the actor and the reason.
	ev := g.as(g.support)(t, "GET", "/v1/admin/events?entity_type=alert&entity_id="+id, nil)
	rows := list(ev, "events")
	if ev.status != 200 || len(rows) != 2 {
		t.Fatalf("events: %d %s", ev.status, ev.raw)
	}
	for _, row := range rows {
		p, _ := row["payload"].(map[string]any)
		if row["actor_id"] != g.superID || row["actor_type"] != "staff" || p["reason"] == "" || p["reason"] == nil {
			t.Fatalf("an audit row without actor or reason: %+v", row)
		}
	}
	if r := support(t, "GET", "/v1/admin/events?entity_type=login&entity_id=x", nil); r.status != 400 {
		t.Fatalf("sign-in rows served: %d", r.status)
	}
}

// The emergency workflow (S11): a supervisor opens a case for a flight
// (201), notes tick the checklist with the time, a second open is 409, the
// close ends it, a note after the close is 409; every step is an events
// row; support reads and may not act; the case shows the intent's
// contact reference and the record link, never more.
func TestIntegrationConsoleEmergency(t *testing.T) {
	g := newConsoleRig(t)
	seed := seedFlight(t, "activated", nil, time.Now().Add(-5*time.Minute))
	if _, err := appPool(t).Exec(context.Background(), "UPDATE operational_intents SET emergency_contact_ref = 'EC-TEST-1' WHERE id = $1::uuid", seed.intentID); err != nil {
		t.Fatal(err)
	}
	super, support := g.as(g.supervisor), g.as(g.support)
	path := "/v1/admin/emergency/" + seed.flightID
	if r := support(t, "GET", path, nil); r.status != 404 {
		t.Fatalf("no case yet: %d", r.status)
	}
	if r := support(t, "POST", path, map[string]any{"action": "open", "reason": "x"}); r.status != 403 {
		t.Fatalf("support opens: %d", r.status)
	}
	if r := super(t, "POST", path, map[string]any{"action": "note", "text": "before"}); r.status != 409 || r.slug() != admin.SlugNoOpenCase {
		t.Fatalf("a note without a case: %d %s", r.status, r.raw)
	}
	r := super(t, "POST", path, map[string]any{"action": "open", "reason": "the remote pilot declared an emergency"})
	if r.status != 201 || r.str("contact_ref") != "EC-TEST-1" || r.str("record_link") != "/v1/records/flights/"+seed.flightID || r.str("contact_procedure") == "" {
		t.Fatalf("open: %d %s", r.status, r.raw)
	}
	caseID := r.str("case_id")
	if r := super(t, "POST", path, map[string]any{"action": "open", "reason": "again"}); r.status != 409 || r.slug() != admin.SlugCaseOpen {
		t.Fatalf("a second open: %d %s", r.status, r.raw)
	}
	if r := super(t, "POST", path, map[string]any{"action": "note", "text": "x", "step": "not_a_step"}); r.status != 400 {
		t.Fatalf("an unknown step: %d", r.status)
	}
	r = super(t, "POST", path, map[string]any{"action": "note", "text": "called the operator's number of record", "step": "operator_contacted"})
	steps := list(r, "checklist")
	if r.status != 200 || len(list(r, "notes")) != 1 || byID(steps, "step", "operator_contacted")["done_at"] == nil ||
		byID(steps, "step", "authority_informed")["done_at"] != nil {
		t.Fatalf("note: %d %s", r.status, r.raw)
	}
	if r := super(t, "POST", path, map[string]any{"action": "close", "outcome": "landed safely"}); r.status != 200 || r.str("outcome") != "landed safely" || r.str("closed_at") == "" {
		t.Fatalf("close: %d %s", r.status, r.raw)
	}
	if r := super(t, "POST", path, map[string]any{"action": "note", "text": "late"}); r.status != 409 {
		t.Fatalf("a note after the close: %d", r.status)
	}
	if r := super(t, "POST", "/v1/admin/emergency/"+strings.Repeat("0", 8)+"-0000-4000-8000-000000000000", map[string]any{"action": "open", "reason": "x"}); r.status != 404 {
		t.Fatalf("unknown flight: %d", r.status)
	}
	for _, et := range []string{admin.EventCaseOpened, admin.EventCaseNote, admin.EventCaseClosed} {
		if n := events(t, admin.EntityCase, caseID, et); n != 1 {
			t.Fatalf("%s: %d events", et, n)
		}
	}
	if c := byID(list(support(t, "GET", "/v1/admin/emergency", nil), "cases"), "case_id", caseID); c == nil || c["closed_at"] == nil {
		t.Fatalf("the closed case is not listed: %+v", c)
	}
	// The flight shows on the flights page; its last sample from the record.
	g.svc.Samples = samples{at: time.Now().Add(-2 * time.Second)}
	f := byID(list(support(t, "GET", "/v1/admin/flights", nil), "flights"), "flight_id", seed.flightID)
	if f == nil || f["last_sample_at"] == nil || f["intent_state"] != "activated" {
		t.Fatalf("flight %+v", f)
	}
	// The record unreadable: listed without the sample, and said so.
	g.svc.Samples = samples{err: errors.New("timescale down")}
	r = support(t, "GET", "/v1/admin/flights", nil)
	f = byID(list(r, "flights"), "flight_id", seed.flightID)
	sm, _ := r.body["samples"].(map[string]any)
	if f == nil || f["last_sample_at"] != nil || sm["available"] != false || !strings.Contains(fmt.Sprint(sm["detail"]), "since") {
		t.Fatalf("flights without the record: %s", r.raw)
	}
}

// The policy (INV-03): every role reads it with its history and the
// values pending GCAA; a supervisor may not change it; an admin's change
// needs the base version in force (409 otherwise), names only known
// values (400) and is refused whole with 503 when the KV cannot take it;
// a change that is accepted is a new version with its changes and its
// events row.
func TestIntegrationConsolePolicy(t *testing.T) {
	g := newConsoleRig(t)
	admin_, super := g.as(g.ad), g.as(g.supervisor)
	r := super(t, "GET", "/v1/admin/policy", nil)
	if r.status != 200 || len(list(r, "history")) > admin.MaxPolicies {
		t.Fatalf("policy: %d %s", r.status, r.raw)
	}
	cur, _ := r.body["current"].(map[string]any)
	base := cur["version"]
	if p, _ := r.body["pending_gcaa"].([]any); len(p) == 0 {
		t.Fatal("no value is marked pending GCAA")
	}
	body := func(v any, values map[string]any) map[string]any {
		return map[string]any{"base_version": v, "reason": "WP-18 test", "values": values}
	}
	if r := super(t, "PUT", "/v1/admin/policy", body(base, map[string]any{"cis_stale_s": 301})); r.status != 403 {
		t.Fatalf("supervisor: %d", r.status)
	}
	if r := admin_(t, "PUT", "/v1/admin/policy", body(-1+base.(float64), map[string]any{"cis_stale_s": 301})); r.status != 409 {
		t.Fatalf("an old base: %d %s", r.status, r.raw)
	}
	if r := admin_(t, "PUT", "/v1/admin/policy", body(base, map[string]any{"not_a_value_s": 1})); r.status != 400 {
		t.Fatalf("an unknown value: %d %s", r.status, r.raw)
	}
	if r := admin_(t, "PUT", "/v1/admin/policy", body(base, map[string]any{"cis_stale_s": -1})); r.status != 400 {
		t.Fatalf("an invalid value: %d %s", r.status, r.raw)
	}
	before := count(t, appPool(t), "SELECT count(*) FROM policy")
	g.proj.set(errors.New("kv: bucket policy unavailable"))
	r = admin_(t, "PUT", "/v1/admin/policy", body(base, map[string]any{"cis_stale_s": 301}))
	g.proj.set(nil)
	if r.status != 503 || r.header.Get("Retry-After") == "" || count(t, appPool(t), "SELECT count(*) FROM policy") != before {
		t.Fatalf("KV refusing: %d %s", r.status, r.raw)
	}
	r = admin_(t, "PUT", "/v1/admin/policy", body(base, map[string]any{"cis_stale_s": 301}))
	ch := list(r, "changes")
	if r.status != 201 || len(ch) != 1 || ch[0]["field"] != "cis_stale_s" || ch[0]["to"] != 301.0 {
		t.Fatalf("put: %d %s", r.status, r.raw)
	}
	v := fmt.Sprint(r.body["version"])
	if n := events(t, "policy", v, store_EventPolicyPut); n != 1 {
		t.Fatalf("policy events %d", n)
	}
	h := list(super(t, "GET", "/v1/admin/policy", nil), "history")
	if len(h) == 0 || fmt.Sprint(h[0]["version"]) != v || h[0]["actor"] == g.adminID {
		t.Fatalf("history head %+v (the actor is shown by username)", h)
	}
	// Leave the defaults as they were for the other tests.
	if r := admin_(t, "PUT", "/v1/admin/policy", body(r.body["version"], map[string]any{"cis_stale_s": policy.Defaults().CISStaleS})); r.status != 201 {
		t.Fatalf("restore: %d %s", r.status, r.raw)
	}
}

const store_EventPolicyPut = "policy_put"

// The source switches (SC-08): a support viewer gets 403 and nothing is
// written; an admin's switch with a reason is a row, an events row and a
// projection, refused whole with 503 when the KV cannot take it; the
// inputs page labels the disabled instance with who and when; switching
// it on again is reversible.
func TestIntegrationConsoleSources(t *testing.T) {
	g := newConsoleRig(t)
	admin_, support := g.as(g.ad), g.as(g.support)
	inst := "op-console-" + unique()
	req := map[string]any{"source_type": "operator_ws", "instance_id": inst, "enabled": false, "reason": "client sends garbage"}
	rows := func() int64 {
		return count(t, appPool(t), "SELECT count(*) FROM source_controls WHERE source_type = 'operator_ws' AND instance_id = $1", inst)
	}
	if r := support(t, "POST", "/v1/admin/sources", req); r.status != 403 || rows() != 0 {
		t.Fatalf("support switch: %d, rows %d", r.status, rows())
	}
	if r := admin_(t, "POST", "/v1/admin/sources", map[string]any{"source_type": "sitl", "enabled": false, "reason": "x"}); r.status != 400 {
		t.Fatalf("an unknown type: %d %s", r.status, r.raw)
	}
	g.proj.set(errors.New("kv: bucket source_control unavailable"))
	r := admin_(t, "POST", "/v1/admin/sources", req)
	g.proj.set(nil)
	if r.status != 503 || r.header.Get("Retry-After") == "" || rows() != 0 {
		t.Fatalf("KV refusing: %d %s rows %d", r.status, r.raw, rows())
	}
	r = admin_(t, "POST", "/v1/admin/sources", req)
	if r.status != 200 || rows() != 1 {
		t.Fatalf("switch: %d %s", r.status, r.raw)
	}
	sw := byID(list(r, "switches"), "instance_id", inst)
	if sw == nil || sw["enabled"] != false || sw["reason"] != "client sends garbage" || strings.Contains(fmt.Sprint(sw["actor"]), g.adminID) {
		t.Fatalf("switch row %+v", sw)
	}
	if n := events(t, "source_control", "operator_ws/"+inst, sources.EventSourceSwitched); n != 1 {
		t.Fatalf("switch events %d", n)
	}
	in := support(t, "GET", "/v1/admin/inputs", nil)
	got := byID(list(in, "sources"), "source_instance", inst)
	dis, _ := got["disabled"].(map[string]any)
	if in.status != 200 || got["state"] != "disabled" || dis["by"] != "instance" || dis["by_who"] != sw["actor"] || dis["at"] == nil {
		t.Fatalf("inputs: %d %+v", in.status, got)
	}
	if m, _ := in.body["monitor"].(map[string]any); m["state"] != "unknown" {
		t.Fatalf("no monitor status read on this rig: %+v", m)
	}
	req["enabled"], req["reason"] = true, "fixed"
	if r := admin_(t, "POST", "/v1/admin/sources", req); r.status != 200 {
		t.Fatalf("switch on: %d", r.status)
	}
	got = byID(list(support(t, "GET", "/v1/admin/inputs", nil), "sources"), "source_instance", inst)
	if got["state"] != "never_heard" || got["disabled"] != nil {
		t.Fatalf("switched on: %+v", got)
	}
}

// The monitor's status on the inputs page (05 §6): up while an instance
// wrote it within monitor_status_missing_s, down ("alerts stopped
// since") when the newest is older, down when none was ever written.
func TestIntegrationConsoleMonitorStatus(t *testing.T) {
	ensureSchemas(t)
	ctx := context.Background()
	nc := busConn(t, mustEnv(t, "USSP_TEST_NATS_URL"), true)
	if _, err := bus.Ensure(ctx, nc.JetStream(), bus.DefaultTopology()); err != nil {
		t.Fatal(err)
	}
	kv := bus.KVStore{JS: nc.JetStream(), Bucket: bus.BucketMonitorStatus}
	purge := func() {
		k, err := nc.JetStream().KeyValue(ctx, bus.BucketMonitorStatus)
		if err != nil {
			t.Fatal(err)
		}
		keys, _ := k.Keys(ctx)
		for _, key := range keys {
			_ = k.Purge(ctx, key)
		}
	}
	purge()
	t.Cleanup(purge)
	now := time.Now()
	js := nc.JetStream()
	st := admin.Service{Store: appStore(t), Inputs: &admin.Inputs{Link: nc.Link,
		Monitors: func(ctx context.Context) ([]bus.MonitorEntry, error) { return bus.MonitorStatuses(ctx, js) }}, Now: func() time.Time { return now },
		Current: func() policy.Record { return policy.Record{Values: policy.Defaults()} }}
	v, err := st.InputsView(ctx)
	if err != nil || v.Monitor.State != "down" || !strings.Contains(v.Monitor.Detail, "no monitor") {
		t.Fatalf("never written: %+v %v", v.Monitor, err)
	}
	raw, _ := bus.EncodeMonitorStatus(bus.MonitorStatus{Instance: "mon-it", At: now, Workers: 1, States: map[string]int{}, EvaluationPeriodS: 1.1, Geoid: true})
	if err := kv.Put(ctx, bus.KeyToken("mon-it"), raw); err != nil {
		t.Fatal(err)
	}
	v, _ = st.InputsView(ctx)
	if v.Monitor.State != "up" || len(v.Monitor.Instances) != 1 || v.Monitor.Instances[0].EvaluationPeriodS != 1.1 || !v.Monitor.Instances[0].Geoid {
		t.Fatalf("written: %+v", v.Monitor)
	}
	now = now.Add(time.Duration(policy.Defaults().MonitorStatusMissingS+1) * time.Second)
	v, _ = st.InputsView(ctx)
	if v.Monitor.State != "down" || !strings.Contains(v.Monitor.Detail, "alerts stopped since") {
		t.Fatalf("silent: %+v", v.Monitor)
	}
	if v.Bus.State != "connected" {
		t.Fatalf("bus %+v", v.Bus)
	}
}
