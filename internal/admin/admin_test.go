package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/policy"
)

var t0 = time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)

func status(source string, instance *string, state string, since time.Time, extra map[string]any) []byte {
	body := map[string]any{"source": source, "source_instance": instance, "state": state, "since": since.Format("2006-01-02T15:04:05.000Z"),
		"age_s": 0.5, "disabled_by": nil, "counters": map[string]any{"accepted": 7, "refused": 1}}
	for k, v := range extra {
		body[k] = v
	}
	raw, _ := json.Marshal(map[string]any{"schema": "source/status/v1", "msg_id": "m", "producer": "p", "ts": "2026-10-05T09:00:00.000Z",
		"rx_ts": "2026-10-05T09:00:00.000Z", "captured_at": "2026-10-05T09:00:00.000Z", "time_source": "system", "backlog": false, "body": body})
	return raw
}

func find(in []Input, source string, instance string) (Input, bool) {
	for _, i := range in {
		got := ""
		if i.SourceInstance != nil {
			got = *i.SourceInstance
		}
		if i.Source == source && got == instance {
			return i, true
		}
	}
	return Input{}, false
}

func ptr(s string) *string { return &s }

// Every known type is listed never_heard until heard; a status heard is
// healthy (live), lagging (live with a lag), stale or unreachable as the
// source says; silence beyond SourceSilentAfter is unreachable since it
// was last heard.
func TestInputsStates(t *testing.T) {
	now := t0
	in := &Inputs{Now: func() time.Time { return now }, Link: func() (bool, time.Time, string) { return true, t0.Add(-time.Hour), "" }}
	b, out, over := in.view(now, nil, func(s string) string { return s })
	if b.State != "connected" || over {
		t.Fatalf("bus %+v over %v", b, over)
	}
	for _, k := range KnownSourceTypes {
		if i, ok := find(out, k, ""); !ok || i.State != InputNeverHeard || i.LastHeardAt != nil {
			t.Errorf("%s never heard: %+v %v", k, i, ok)
		}
	}
	in.Take("src.v1.operator_ws.op-1", status("operator_ws", ptr("op-1"), "live", t0.Add(-time.Minute), nil))
	in.Take("src.v1.operator_ws.op-2", status("operator_ws", ptr("op-2"), "live", t0.Add(-time.Minute), map[string]any{"lag_s": 4.5}))
	in.Take("src.v1.ansp_feed._all", status("ansp_feed", nil, "down", t0.Add(-2*time.Minute), map[string]any{"detail": "stream cut"}))
	in.Take("src.v1.adsb_rx.r1", status("adsb_rx", ptr("r1"), "stale", t0.Add(-3*time.Minute), nil))
	_, out, _ = in.view(now, nil, func(s string) string { return s })
	for _, c := range []struct {
		src, inst, want string
	}{{"operator_ws", "op-1", InputHealthy}, {"operator_ws", "op-2", InputLagging}, {"ansp_feed", "", InputUnreachable}, {"adsb_rx", "r1", InputStale}} {
		i, ok := find(out, c.src, c.inst)
		if !ok || i.State != c.want || i.LastHeardAt == nil || i.Since == nil {
			t.Errorf("%s/%s: %+v (want %s)", c.src, c.inst, i, c.want)
		}
	}
	if i, _ := find(out, "ansp_feed", ""); i.Detail == nil || *i.Detail != "stream cut" {
		t.Errorf("detail %+v", i.Detail)
	}
	if i, _ := find(out, "operator_ws", "op-1"); i.Counters["accepted"] != 7 {
		t.Errorf("counters %+v", i.Counters)
	}
	// Silence: unreachable since it was last heard.
	now = t0.Add(SourceSilentAfter + time.Second)
	_, out, _ = in.view(now, nil, func(s string) string { return s })
	if i, _ := find(out, "operator_ws", "op-1"); i.State != InputUnreachable || !i.Since.Equal(t0) {
		t.Errorf("silent: %+v", i)
	}
}

// A switch disables what it names, labelled by whom, when and why; a
// type switch disables every instance of the type unless the instance
// has its own row; an enabled row changes nothing.
func TestInputsSwitches(t *testing.T) {
	in := &Inputs{Now: func() time.Time { return t0 }}
	in.Take("", status("operator_ws", ptr("op-1"), "live", t0, nil))
	in.Take("", status("operator_ws", ptr("op-2"), "live", t0, nil))
	in.Take("", status("adsb_rx", ptr("r1"), "live", t0, nil))
	names := func(id string) string { return map[string]string{"u1": "admin.a"}[id] }
	rows := []SwitchRow{
		{SourceType: "operator_ws", InstanceID: ptr("op-1"), Enabled: false, Actor: "u1", Reason: "client abuse", ChangedAt: t0.Add(-time.Minute)},
		{SourceType: "adsb_rx", Enabled: false, Actor: "u1", Reason: "receiver maintenance", ChangedAt: t0.Add(-2 * time.Minute)},
		{SourceType: "adsb_rx", InstanceID: ptr("r1"), Enabled: true, Actor: "u1", Reason: "on", ChangedAt: t0.Add(-3 * time.Minute)},
		{SourceType: "network_rid", Enabled: true, Actor: "u1", Reason: "on", ChangedAt: t0},
	}
	_, out, _ := in.view(t0, rows, names)
	i, _ := find(out, "operator_ws", "op-1")
	if i.State != InputDisabled || i.Disabled == nil || i.Disabled.By != "instance" || i.Disabled.ByWho != "admin.a" || i.Disabled.Reason != "client abuse" {
		t.Errorf("instance switch: %+v %+v", i, i.Disabled)
	}
	if i, _ := find(out, "operator_ws", "op-2"); i.State != InputHealthy || i.Disabled != nil {
		t.Errorf("another instance: %+v", i)
	}
	if i, _ := find(out, "adsb_rx", ""); i.State != InputDisabled || i.Disabled.By != "type" {
		t.Errorf("type switch: %+v", i)
	}
	// r1 has its own (enabled) row: the instance's row wins.
	if i, _ := find(out, "adsb_rx", "r1"); i.State != InputHealthy {
		t.Errorf("instance row under a type switch: %+v", i)
	}
	if i, _ := find(out, "network_rid", ""); i.State != InputNeverHeard || i.Disabled != nil {
		t.Errorf("enabled row: %+v", i)
	}
}

// While the bus is down every input that is not switched off is unknown
// since the bus went down, its last status kept.
func TestInputsBusDown(t *testing.T) {
	down := t0.Add(-30 * time.Second)
	in := &Inputs{Now: func() time.Time { return t0 }, Link: func() (bool, time.Time, string) { return false, down, "connection refused" }}
	in.Take("", status("operator_ws", ptr("op-1"), "live", t0, nil))
	rows := []SwitchRow{{SourceType: "ansp_feed", Enabled: false, Actor: "u1", Reason: "r", ChangedAt: t0}}
	b, out, _ := in.view(t0, rows, func(s string) string { return s })
	if b.State != "disconnected" || !b.Since.Equal(down) || b.Detail == nil {
		t.Fatalf("bus %+v", b)
	}
	for _, i := range out {
		if i.Source == "ansp_feed" {
			if i.State != InputDisabled {
				t.Errorf("a switched-off input stays disabled: %+v", i)
			}
			continue
		}
		if i.State != InputUnknown || i.Since == nil || !i.Since.Equal(down) {
			t.Errorf("%s: %+v", i.Source, i)
		}
	}
	if i, _ := find(out, "operator_ws", "op-1"); i.LastHeardAt == nil {
		t.Errorf("the last status is kept: %+v", i)
	}
}

// The tracker holds at most MaxSources instances and says when it was
// cut (E-10); an unreadable message is counted, not held.
func TestInputsBound(t *testing.T) {
	c := &core.Counters{}
	in := &Inputs{Now: func() time.Time { return t0 }, Counters: c}
	for i := range MaxSources + 3 {
		in.Take("", status("operator_ws", ptr(fmt.Sprint("op-", i)), "live", t0, nil))
	}
	in.Take("", []byte(`{"schema":"other/v1"}`))
	in.Take("", make([]byte, MaxStatusBytes+1))
	_, out, over := in.view(t0, nil, func(s string) string { return s })
	if !over || len(out) != MaxSources+len(KnownSourceTypes) {
		t.Fatalf("over %v, %d inputs", over, len(out))
	}
	snap := c.Snapshot()
	if snap[CounterSourcesOverBound] != 3 || snap[CounterSourceUnreadable] != 2 {
		t.Fatalf("counters %+v", snap)
	}
}

func TestStateOf(t *testing.T) {
	lag, none := 2.0, 0.0
	for _, c := range []struct {
		in   string
		lag  *float64
		want string
	}{{"live", nil, InputHealthy}, {"live", &none, InputHealthy}, {"live", &lag, InputLagging}, {"stale", nil, InputStale},
		{"down", nil, InputUnreachable}, {"disabled", nil, InputDisabled}, {"unknown", nil, InputUnknown}, {"other", nil, InputUnknown}} {
		if got := stateOf(c.in, c.lag); got != c.want {
			t.Errorf("%s: %s, want %s", c.in, got, c.want)
		}
	}
}

// changes names exactly the values that differ, both ways.
func TestPolicyChanges(t *testing.T) {
	a, b := valuesMap(policy.Defaults()), valuesMap(policy.Defaults())
	if ch := changes(a, b); len(ch) != 0 {
		t.Fatalf("no change: %+v", ch)
	}
	v := policy.Defaults()
	v.CISStaleS = 600
	v.EmergencyChecklist = append(v.EmergencyChecklist, "police_informed")
	ch := changes(a, valuesMap(v))
	if len(ch) != 2 || ch[0].Field != "cis_stale_s" || ch[0].From != 300.0 || ch[0].To != 600.0 || ch[1].Field != "emergency_checklist_ids" {
		t.Fatalf("changes %+v", ch)
	}
}

// Every refusal of an action is decided before anything is written.
func TestValidateAction(t *testing.T) {
	s := &Service{}
	long := string(make([]byte, MaxNote+1))
	for _, c := range []struct {
		a     CaseAction
		field string
	}{
		{CaseAction{Action: "open"}, "reason"},
		{CaseAction{Action: "note"}, "text"},
		{CaseAction{Action: "note", Text: long}, "text"},
		{CaseAction{Action: "note", Text: "t", Step: "not_a_step"}, "step"},
		{CaseAction{Action: "close"}, "outcome"},
		{CaseAction{Action: "command"}, "action"},
	} {
		err := s.validateAction(c.a)
		var fe *core.FieldError
		if !errors.As(err, &fe) || fe.Field != c.field {
			t.Errorf("%+v: %v", c.a, err)
		}
	}
	for _, a := range []CaseAction{{Action: "open", Reason: "r"}, {Action: "note", Text: "t"}, {Action: "note", Text: "t", Step: "operator_contacted"},
		{Action: "close", Outcome: "o"}} {
		if err := s.validateAction(a); err != nil {
			t.Errorf("%+v: %v", a, err)
		}
	}
}

// The refusals are the problems the console reads.
func TestErrorsAreProblems(t *testing.T) {
	for _, c := range []struct {
		err  error
		code int
		slug string
	}{{notFound("alert"), http.StatusNotFound, SlugNotFound}, {conflict(SlugAlreadyClosed, "x"), http.StatusConflict, SlugAlreadyClosed},
		{reason("reason", "", MaxReason), http.StatusBadRequest, httpx.SlugValidation}} {
		p := httpx.ProblemFromError(c.err)
		if p.Status != c.code || p.Type != httpx.ProblemTypeBase+c.slug {
			t.Errorf("%v: %d %s", c.err, p.Status, p.Type)
		}
	}
}
