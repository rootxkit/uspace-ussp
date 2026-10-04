package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"

	"github.com/rootxkit/uspace-core/core"
	coresources "github.com/rootxkit/uspace-core/sources"

	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/sources"
	"github.com/rootxkit/uspace-ussp/internal/store"
	"github.com/rootxkit/uspace-ussp/internal/store/relational"
)

// PolicyPutter stores a policy version made on version base
// (policy.Service.PutOn): validated, with its events row and its KV
// projection in one transaction, a *policy.StaleBaseError when another
// version was stored since base (decided under the policy lock), a
// *policy.ProjectionError when the KV cannot take it.
type PolicyPutter interface {
	PutOn(ctx context.Context, base int64, actor, reason string, v policy.Values) (policy.Record, error)
}

// SourceSwitcher writes and lists the source switches (sources.Writer
// through SourcesOf).
type SourceSwitcher interface {
	Switch(ctx context.Context, actor, reason string, c coresources.Control) (coresources.State, error)
	List(ctx context.Context) ([]sources.Row, coresources.State, error)
	Rows(ctx context.Context) ([]SwitchRow, error)
}

// SourcesOf is SourceSwitcher over the writer of internal/sources.
type SourcesOf struct{ W *sources.Writer }

// Switch implements SourceSwitcher.
func (s SourcesOf) Switch(ctx context.Context, actor, reason string, c coresources.Control) (coresources.State, error) {
	return s.W.Switch(ctx, actor, reason, c)
}

// List implements SourceSwitcher.
func (s SourcesOf) List(ctx context.Context) ([]sources.Row, coresources.State, error) {
	return s.W.List(ctx)
}

// Rows implements SourceSwitcher.
func (s SourcesOf) Rows(ctx context.Context) ([]SwitchRow, error) {
	rows, _, err := s.W.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]SwitchRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, SwitchRow{SourceType: r.Control.SourceType, InstanceID: r.Control.InstanceID, Enabled: r.Control.Enabled,
			Actor: r.Actor, Reason: r.Reason, ChangedAt: r.ChangedAt})
	}
	return out, nil
}

// Change is one value that differs between two versions.
type Change struct {
	Field string `json:"field"`
	From  any    `json:"from"`
	To    any    `json:"to"`
}

// PolicyVersion is one version on the policy page.
type PolicyVersion struct {
	Version   int64          `json:"version"`
	CreatedAt *time.Time     `json:"created_at,omitempty"`
	Actor     *string        `json:"actor,omitempty"`
	Reason    *string        `json:"reason,omitempty"`
	Values    map[string]any `json:"values"`
	Changes   []Change       `json:"changes"`
}

// Policy is GET /v1/admin/policy.
type Policy struct {
	Current     PolicyVersion   `json:"current"`
	History     []PolicyVersion `json:"history"`
	PendingGCAA []string        `json:"pending_gcaa"`
}

// valuesMap is v by its JSON names.
func valuesMap(v policy.Values) map[string]any {
	raw, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return m
}

// changes lists the names whose values differ from before to after,
// sorted.
func changes(before, after map[string]any) []Change {
	out := []Change{}
	keys := make([]string, 0, len(after))
	for k := range after {
		keys = append(keys, k)
	}
	for k := range before {
		if _, ok := after[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	for _, k := range slices.Compact(keys) {
		if !reflect.DeepEqual(before[k], after[k]) {
			out = append(out, Change{Field: k, From: before[k], To: after[k]})
		}
	}
	return out
}

// Policy is the version in force (the defaults as version 0 while none
// is stored), the newest MaxPolicies versions each with its changes from
// the one before, and the names pending GCAA. The actor of each version
// is the staff username when the account is known.
func (s *Service) Policy(ctx context.Context) (Policy, error) {
	rows, err := s.Store.Queries().AdminPolicyHistory(ctx, MaxPolicies+1)
	if err != nil {
		return Policy{}, fmt.Errorf("policy history: %w", err)
	}
	actors := make([]string, 0, len(rows))
	for _, r := range rows {
		actors = append(actors, r.Actor)
	}
	return policyHistory(rows, s.staffNames(ctx, actors))
}

// policyHistory is the policy page of rows, the newest versions newest
// first as AdminPolicyHistory reads at most MaxPolicies+1 of them. Each
// version's changes are from the one before it; the first version ever
// stored (the oldest row when fewer than MaxPolicies+1 were read) has
// its changes from the defaults, version 0, as PutPolicy answered when
// it was stored. The version past MaxPolicies is read only for the
// changes of the oldest one shown.
func policyHistory(rows []relational.Policy, names func(string) string) (Policy, error) {
	maps := make([]map[string]any, len(rows))
	for i, r := range rows {
		// Decoded onto the defaults, as internal/store reads a version: a
		// value a version predates takes its default.
		v := policy.Defaults()
		if err := json.NewDecoder(bytes.NewReader(r.Values)).Decode(&v); err != nil {
			return Policy{}, fmt.Errorf("policy version %d: %w", r.Version, err)
		}
		maps[i] = valuesMap(v)
	}
	defaults := valuesMap(policy.Defaults())
	out := Policy{History: make([]PolicyVersion, 0, min(len(rows), MaxPolicies)), PendingGCAA: slices.Clone(policy.PendingGCAA)}
	for i, r := range rows {
		if i == MaxPolicies {
			break
		}
		var ch []Change
		if i+1 < len(rows) {
			ch = changes(maps[i+1], maps[i])
		} else {
			ch = changes(defaults, maps[i])
		}
		at := r.CreatedAt.UTC()
		actor, why := names(r.Actor), r.Reason
		out.History = append(out.History, PolicyVersion{Version: r.Version, CreatedAt: &at, Actor: &actor, Reason: &why, Values: maps[i], Changes: ch})
	}
	if len(out.History) > 0 {
		out.Current = out.History[0]
	} else {
		out.Current = PolicyVersion{Version: 0, Values: defaults, Changes: []Change{}}
	}
	return out, nil
}

// PolicyPut is a new version asked for.
type PolicyPut struct {
	BaseVersion int64
	Reason      string
	Values      map[string]any
}

// PutPolicy stores a new version: the values given replace those of the
// version in force (an unknown name is 400), base_version other than
// the version in force is 409 (permanent), and the result is validated
// before anything is written. The base is compared twice: here, to
// refuse early, and again under the policy lock as the version is
// inserted, which is the compare that decides; of two changes made on
// the same version the second is 409, never stored over the first. The
// answer is the new version with its changes from the one before.
func (s *Service) PutPolicy(ctx context.Context, staffID string, in PolicyPut) (PolicyVersion, error) {
	if err := reason("reason", in.Reason, MaxReason); err != nil {
		return PolicyVersion{}, err
	}
	if s.Policies == nil {
		return PolicyVersion{}, &Error{Status: 503, Slug: SlugUnavailable, Detail: "the policy cannot be stored on this process"}
	}
	// The base is the newest version stored, read from the database: a
	// version another replica stored is the one in force.
	cur, err := s.Store.NewestPolicy(ctx)
	switch {
	case errors.Is(err, policy.ErrNoPolicy):
		cur = policy.Record{Values: policy.Defaults()}
	case err != nil:
		return PolicyVersion{}, fmt.Errorf("policy in force: %w", err)
	}
	if in.BaseVersion != cur.Version {
		return PolicyVersion{}, policyChanged(cur.Version, in.BaseVersion)
	}
	if len(in.Values) == 0 {
		return PolicyVersion{}, core.Fieldf("values", "nothing to change")
	}
	raw, err := json.Marshal(in.Values)
	if err != nil {
		return PolicyVersion{}, core.Fieldf("values", "not a JSON object")
	}
	next := cur.Values
	// The slices of the current version are not shared with the new one.
	next.WeatherStationIDs = slices.Clone(next.WeatherStationIDs)
	next.EmergencyChecklist = slices.Clone(next.EmergencyChecklist)
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&next); err != nil {
		return PolicyVersion{}, core.Fieldf("values", "%s", decodeReason(err))
	}
	if err := next.Validate(); err != nil {
		return PolicyVersion{}, err
	}
	rec, err := s.Policies.PutOn(ctx, cur.Version, staffID, in.Reason, next)
	var stale *policy.StaleBaseError
	if errors.As(err, &stale) {
		return PolicyVersion{}, policyChanged(stale.Current, in.BaseVersion)
	}
	if err != nil {
		return PolicyVersion{}, err
	}
	at := rec.CreatedAt.UTC()
	actor, why := s.staffNames(ctx, []string{rec.Actor})(rec.Actor), rec.Reason
	after := valuesMap(rec.Values)
	return PolicyVersion{Version: rec.Version, CreatedAt: &at, Actor: &actor, Reason: &why, Values: after,
		Changes: changes(valuesMap(cur.Values), after)}, nil
}

// policyChanged is the 409 of a change made on a version that is no
// longer the one in force.
func policyChanged(current, base int64) error {
	return conflict(SlugPolicyChanged, fmt.Sprintf("the policy in force is version %d, not %d: read it again and make the change on it", current, base))
}

// decodeReason says why a values object does not decode, naming the
// field where the decoder does.
func decodeReason(err error) string {
	var te *json.UnmarshalTypeError
	if errors.As(err, &te) && te.Field != "" {
		return te.Field + ": wrong type"
	}
	return err.Error()
}

// SourceSwitchView is one stored switch on the inputs page.
type SourceSwitchView struct {
	SourceType string    `json:"source_type"`
	InstanceID *string   `json:"instance_id,omitempty"`
	Enabled    bool      `json:"enabled"`
	Reason     string    `json:"reason"`
	Actor      string    `json:"actor"`
	ChangedAt  time.Time `json:"changed_at"`
	Version    int64     `json:"version"`
}

// Switches is GET and POST /v1/admin/sources.
type Switches struct {
	Switches   []SourceSwitchView `json:"switches"`
	Version    uint64             `json:"version"`
	Epoch      string             `json:"epoch"`
	KnownTypes []string           `json:"known_types"`
}

// Sources lists the stored switches with who set them (the staff
// username when known) and the state they make.
func (s *Service) Sources(ctx context.Context) (Switches, error) {
	if s.Switches == nil {
		return Switches{}, &Error{Status: 503, Slug: SlugUnavailable, Detail: "the source switches are not configured on this process"}
	}
	rows, st, err := s.Switches.List(ctx)
	if err != nil {
		return Switches{}, fmt.Errorf("source switches: %w", err)
	}
	actors := make([]string, 0, len(rows))
	for _, r := range rows {
		actors = append(actors, r.Actor)
	}
	names := s.staffNames(ctx, actors)
	out := Switches{Switches: make([]SourceSwitchView, 0, len(rows)), Version: st.Version, Epoch: st.Epoch, KnownTypes: slices.Clone(KnownSourceTypes)}
	for _, r := range rows {
		out.Switches = append(out.Switches, SourceSwitchView{SourceType: r.Control.SourceType, InstanceID: r.Control.InstanceID,
			Enabled: r.Control.Enabled, Reason: r.Reason, Actor: names(r.Actor), ChangedAt: r.ChangedAt.UTC(), Version: r.Version})
	}
	return out, nil
}

// SwitchRequest is a switch asked for.
type SwitchRequest struct {
	SourceType string
	InstanceID *string
	Enabled    bool
	Reason     string
}

// MaxInstanceID bounds an instance id of a switch (E-10).
const MaxInstanceID = 512

// Switch sets one switch with a reason: validated first (a known type, a
// bounded instance, a reason), then the row, its events row and the KV
// projection in one transaction (internal/sources); a
// *policy.ProjectionError (503) when the KV cannot take it, and nothing
// changed. The answer is every switch after it.
func (s *Service) Switch(ctx context.Context, staffID string, in SwitchRequest) (Switches, error) {
	if !slices.Contains(KnownSourceTypes, in.SourceType) {
		return Switches{}, core.Fieldf("source_type", "not a source type of this USSP")
	}
	if in.InstanceID != nil && (*in.InstanceID == "" || len(*in.InstanceID) > MaxInstanceID) {
		return Switches{}, core.Fieldf("instance_id", "1 to %d bytes; leave it out to switch the whole type", MaxInstanceID)
	}
	if err := reason("reason", in.Reason, MaxReason); err != nil {
		return Switches{}, err
	}
	if s.Switches == nil {
		return Switches{}, &Error{Status: 503, Slug: SlugUnavailable, Detail: "the source switches are not configured on this process"}
	}
	if _, err := s.Switches.Switch(ctx, staffID, in.Reason, coresources.Control{SourceType: in.SourceType, InstanceID: in.InstanceID, Enabled: in.Enabled}); err != nil {
		return Switches{}, err
	}
	return s.Sources(ctx)
}

// staffNames resolves staff account ids to their usernames for display;
// an id that is not a staff account (a system actor, a bootstrap) is
// shown as it is stored.
func (s *Service) staffNames(ctx context.Context, ids []string) func(string) string {
	names := map[string]string{}
	q := s.Store.Queries()
	for _, id := range ids {
		if _, done := names[id]; done || len(names) >= 256 {
			continue
		}
		names[id] = id
		u, err := store.UUID("actor", id)
		if err != nil {
			continue
		}
		if st, err := q.StaffByID(ctx, u); err == nil {
			names[id] = st.Username
		}
	}
	return func(id string) string {
		if n, ok := names[id]; ok {
			return n
		}
		return id
	}
}
