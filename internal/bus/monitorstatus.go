package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/rootxkit/uspace-core/core"
)

// MonitorStatusEvery is how often a monitor instance writes its status
// to monitor_status: its status line's period.
const MonitorStatusEvery = 10 * time.Second

// MonitorStatus is one monitor instance's status line as it writes it to
// the KV bucket monitor_status under KeyToken(instance) (WP-18): what
// the conformance, CPA and zone paths rest on, for the console. It is
// internal (the bucket never leaves this system); At is the instance's
// clock, and a reader ages the entry by the KV's own time.
type MonitorStatus struct {
	Instance       string         `json:"instance"`
	At             time.Time      `json:"at"`
	Workers        int            `json:"workers"`
	FlightsTracked int            `json:"flights_tracked"`
	States         map[string]int `json:"states"`
	// EvaluationPeriodS is the longest conformance tick period measured
	// in the last status period (05 §3: widened, never skipped);
	// CPAEvaluationPeriodS the CPA path's.
	EvaluationPeriodS    float64 `json:"evaluation_period_s"`
	CPAEvaluationPeriodS float64 `json:"cpa_evaluation_period_s"`
	OutboxDepth          int     `json:"outbox_depth"`
	PolicyVersion        int64   `json:"policy_version"`
	// IntentActiveAgeS is nil while intent_active was never read: every
	// flight is unknown and nothing is judged (SC-22).
	IntentActiveAgeS *float64 `json:"intent_active_age_s"`
	CISLoaded        bool     `json:"cis_loaded"`
	CISVersion       string   `json:"cis_version"`
	CISAgeS          float64  `json:"cis_age_s"`
	CISStale         bool     `json:"cis_stale"`
	Terrain          bool     `json:"terrain"`
	Geoid            bool     `json:"geoid"`
	// InputDownSince is set while the instance's own input (its bus
	// link) is down: a monitoring outage, no lost_link judged (PLAN
	// §15.2 Q35). InputBackAt is when it last came back, and
	// LostLinkSuspendedUntil, while set, the end of the grace after that
	// return (lost_link_s) before a flight still silent loses its link.
	InputDownSince         *time.Time `json:"input_down_since,omitempty"`
	InputBackAt            *time.Time `json:"input_back_at,omitempty"`
	LostLinkSuspendedUntil *time.Time `json:"lost_link_suspended_until,omitempty"`
}

// EncodeMonitorStatus is s as the bucket holds it.
func EncodeMonitorStatus(s MonitorStatus) ([]byte, error) { return json.Marshal(s) }

// DecodeMonitorStatus reads one stored status, refusing a value over
// MonitorStatusBytes, an unknown field, and a status without its
// instance or time.
func DecodeMonitorStatus(data []byte) (MonitorStatus, error) {
	var s MonitorStatus
	if len(data) > MonitorStatusBytes {
		return s, core.Fieldf("monitor_status", "longer than %d bytes", MonitorStatusBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return MonitorStatus{}, core.Fieldf("monitor_status", "not a monitor status")
	}
	if s.Instance == "" {
		return MonitorStatus{}, core.Fieldf("instance", "required")
	}
	if s.At.IsZero() {
		return MonitorStatus{}, core.Fieldf("at", "required")
	}
	return s, nil
}

// MaxMonitorStatuses bounds the instances MonitorStatuses reads (E-10).
const MaxMonitorStatuses = 64

// MonitorEntry is one stored status with when the KV stored it (the
// NATS server's clock).
type MonitorEntry struct {
	Status MonitorStatus
	Stored time.Time
}

// MonitorStatuses reads every instance's status from monitor_status, at
// most MaxMonitorStatuses, skipping a value that does not decode; none
// when the bucket holds no key, an error when it cannot be read.
func MonitorStatuses(ctx context.Context, js jetstream.JetStream) ([]MonitorEntry, error) {
	kv, err := js.KeyValue(ctx, BucketMonitorStatus)
	if err != nil {
		return nil, err
	}
	lister, err := kv.ListKeys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = lister.Stop() }()
	var out []MonitorEntry
	for k := range lister.Keys() {
		if len(out) >= MaxMonitorStatuses {
			break
		}
		e, err := kv.Get(ctx, k)
		if err != nil {
			continue
		}
		st, err := DecodeMonitorStatus(e.Value())
		if err != nil {
			continue
		}
		out = append(out, MonitorEntry{Status: st, Stored: e.Created()})
	}
	return out, nil
}
