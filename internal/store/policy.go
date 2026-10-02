package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/store/relational"
)

// Event types written by this package's mutating paths.
const (
	EventPolicyPut = "policy_put"
)

var _ policy.Store = (*Store)(nil)

// NewestPolicy is the highest stored policy version, or policy.ErrNoPolicy.
func (s *Store) NewestPolicy(ctx context.Context) (policy.Record, error) {
	row, err := s.Queries().NewestPolicy(ctx)
	if IsNoRows(err) {
		return policy.Record{}, policy.ErrNoPolicy
	}
	if err != nil {
		return policy.Record{}, fmt.Errorf("newest policy: %w", err)
	}
	return policyRecord(row)
}

// InsertPolicy stores v as the next version (policy_version_seq, under
// LockPolicy), writes its policy_put event, and calls beforeCommit with
// the stored record, all in one transaction. An error from beforeCommit
// rolls it all back. The version number taken is never reused.
func (s *Store) InsertPolicy(ctx context.Context, actor, reason string, v policy.Values, beforeCommit func(context.Context, policy.Record) error) (policy.Record, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return policy.Record{}, fmt.Errorf("policy values: %w", err)
	}
	var out policy.Record
	err = s.Tx(ctx, func(q *relational.Queries) error {
		if err := Lock(ctx, q, LockPolicy); err != nil {
			return err
		}
		version, err := q.NextPolicyVersion(ctx)
		if err != nil {
			return fmt.Errorf("policy version: %w", err)
		}
		row, err := q.InsertPolicy(ctx, relational.InsertPolicyParams{Version: version, Actor: actor, Reason: reason, PolicyValues: raw})
		if err != nil {
			return fmt.Errorf("insert policy: %w", err)
		}
		if out, err = policyRecord(row); err != nil {
			return err
		}
		if _, err := Audit(ctx, q, Event{
			ActorType: ActorStaff, ActorID: actor,
			EntityType: "policy", EntityID: strconv.FormatInt(version, 10), EventType: EventPolicyPut,
			Payload: map[string]any{"policy_version": version, "reason": reason, "values": v},
		}); err != nil {
			return err
		}
		return beforeCommit(ctx, out)
	})
	if err != nil {
		return policy.Record{}, fmt.Errorf("put policy: %w", err)
	}
	return out, nil
}

// policyRecord decodes a stored row strictly: an unknown field is an
// error, never silently dropped.
func policyRecord(row relational.Policy) (policy.Record, error) {
	var v policy.Values
	dec := json.NewDecoder(bytes.NewReader(row.Values))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return policy.Record{}, fmt.Errorf("policy version %d values: %w", row.Version, err)
	}
	return policy.Record{Version: row.Version, CreatedAt: row.CreatedAt, Actor: row.Actor, Reason: row.Reason, Values: v}, nil
}
