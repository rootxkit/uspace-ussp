package bus

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/rootxkit/uspace-core/core"
	coresources "github.com/rootxkit/uspace-core/sources"

	"github.com/rootxkit/uspace-ussp/internal/policy"
)

// Keys of the single-key buckets.
const (
	// KeyPolicy holds the current policy.Record in bucket policy.
	KeyPolicy = "current"
	// KeySources holds the whole source-control state in source_control.
	KeySources = "state"
)

// DefaultKVTimeout bounds one KV write (B-09: a write the KV cannot
// take within it fails, and the writer refuses with 503).
const DefaultKVTimeout = 2 * time.Second

// Counters of the projector.
const (
	CounterKVPut          = "kv_put"
	CounterKVPutFailed    = "kv_put_failed"
	CounterKVDelete       = "kv_delete"
	CounterKVDeleteFailed = "kv_delete_failed"
	CounterPushFailed     = "ctl_push_failed"
)

var kvKey = regexp.MustCompile(`^[-/_=.a-zA-Z0-9]+$`)

// MaxKeyBytes bounds a KV key (E-10).
const MaxKeyBytes = 512

// ValidKey reports whether k is a NATS KV key: non-empty, at most
// MaxKeyBytes, of [-/_=.a-zA-Z0-9], without a leading or trailing dot.
func ValidKey(k string) bool {
	return k != "" && len(k) <= MaxKeyBytes && kvKey.MatchString(k) && k[0] != '.' && k[len(k)-1] != '.'
}

// KeyToken is s as one KV key token: unpadded base64url, so any id (a
// serial, a registration number, a client id) is a valid key and two
// ids never share one.
func KeyToken(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// KeyFromToken reverses KeyToken.
func KeyFromToken(t string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(t)
	if err != nil {
		return "", core.Fieldf("key", "%q is not a key token", t)
	}
	return string(b), nil
}

// Projector writes the KV projections (docs/PLAN.md D6, §3.2). Writers
// call it inside the transaction that changes the rows (a tx hook):
// every write is bounded by Timeout, and an error makes the writer roll
// back and answer 503 (B-09). It implements policy.Projector and
// sources.Projector itself; cis, registry and the client bindings
// project through Put and Delete.
type Projector struct {
	JS       jetstream.JetStream
	Push     *Publisher
	Topology Topology
	Counters *core.Counters
	// Timeout bounds each KV operation (DefaultKVTimeout).
	Timeout time.Duration
}

// NewProjector is a projector on c.
func NewProjector(c *Conn, counters *core.Counters) *Projector {
	if counters == nil {
		counters = &core.Counters{}
	}
	return &Projector{JS: c.JetStream(), Push: NewPublisher(c, counters), Topology: DefaultTopology(), Counters: counters}
}

var _ policy.Projector = (*Projector)(nil)

func (p *Projector) timeout() time.Duration {
	if p.Timeout > 0 {
		return p.Timeout
	}
	return DefaultKVTimeout
}

// bucket opens a bucket, creating it from the topology when it does not
// exist yet (a writer may run before any Ensure).
func (p *Projector) bucket(ctx context.Context, name string) (jetstream.KeyValue, error) {
	kv, err := p.JS.KeyValue(ctx, name)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		cfg, ok := p.Topology.Bucket(name)
		if !ok {
			return nil, core.Fieldf("bucket", "%q is not in the topology", name)
		}
		kv, err = p.JS.CreateKeyValue(ctx, cfg)
		if errors.Is(err, jetstream.ErrBucketExists) || errors.Is(err, jetstream.ErrStreamNameAlreadyInUse) {
			kv, err = p.JS.KeyValue(ctx, name)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("bucket %s: %w", name, err)
	}
	return kv, nil
}

// Put writes value under key in bucket and returns once JetStream
// stored it, within Timeout.
func (p *Projector) Put(ctx context.Context, bucket, key string, value []byte) error {
	if !ValidKey(key) {
		p.Counters.Inc(CounterKVPutFailed)
		return core.Fieldf("key", "%q is not a KV key", key)
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout())
	defer cancel()
	kv, err := p.bucket(ctx, bucket)
	if err == nil {
		_, err = kv.Put(ctx, key, value)
	}
	if err != nil {
		p.Counters.Inc(CounterKVPutFailed)
		return fmt.Errorf("kv %s/%s: %w", bucket, key, err)
	}
	p.Counters.Inc(CounterKVPut)
	return nil
}

// PutJSON is Put of v as JSON.
func (p *Projector) PutJSON(ctx context.Context, bucket, key string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		p.Counters.Inc(CounterKVPutFailed)
		return fmt.Errorf("kv %s/%s: %w", bucket, key, err)
	}
	return p.Put(ctx, bucket, key, data)
}

// Delete removes key from bucket (a delete marker the watchers see).
func (p *Projector) Delete(ctx context.Context, bucket, key string) error {
	if !ValidKey(key) {
		p.Counters.Inc(CounterKVDeleteFailed)
		return core.Fieldf("key", "%q is not a KV key", key)
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout())
	defer cancel()
	kv, err := p.bucket(ctx, bucket)
	if err == nil {
		err = kv.Delete(ctx, key)
	}
	if err != nil {
		p.Counters.Inc(CounterKVDeleteFailed)
		return fmt.Errorf("kv %s/%s: %w", bucket, key, err)
	}
	p.Counters.Inc(CounterKVDelete)
	return nil
}

// MaxListedKeys bounds Keys (E-10).
const MaxListedKeys = 100_000

// Keys lists the keys of bucket (none when it is empty), at most
// MaxListedKeys; more is an error, never a truncated list.
func (p *Projector) Keys(ctx context.Context, bucket string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*p.timeout())
	defer cancel()
	kv, err := p.bucket(ctx, bucket)
	if err != nil {
		return nil, err
	}
	lister, err := kv.ListKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("kv %s keys: %w", bucket, err)
	}
	defer func() { _ = lister.Stop() }()
	var out []string
	for k := range lister.Keys() {
		if len(out) == MaxListedKeys {
			return nil, fmt.Errorf("kv %s holds more than %d keys", bucket, MaxListedKeys)
		}
		out = append(out, k)
	}
	return out, nil
}

// Version is the announcement on a ctl.* subject: the version the KV
// now holds (and the epoch of source_control).
type Version struct {
	Version uint64 `json:"version"`
	Epoch   string `json:"epoch,omitempty"`
}

func (p *Projector) push(subject string, v Version) {
	if p.Push == nil {
		return
	}
	if err := p.Push.PublishControl(subject, v); err != nil {
		p.Counters.Inc(CounterPushFailed) // the watch and the re-read still carry it
	}
}

// ProjectPolicy writes r to policy/current and announces it on
// ctl.policy (policy.Projector).
func (p *Projector) ProjectPolicy(ctx context.Context, r policy.Record) error {
	if err := p.PutJSON(ctx, BucketPolicy, KeyPolicy, r); err != nil {
		return err
	}
	p.push(CtlPolicy, Version{Version: uint64(max(r.Version, 0))})
	return nil
}

// ControlWire is one switch on the wire.
type ControlWire struct {
	SourceType string  `json:"source_type"`
	InstanceID *string `json:"instance_id"`
	Enabled    bool    `json:"enabled"`
}

// SourcesWire is the source-control state as source_control/state holds
// it (core's State has no JSON names of its own).
type SourcesWire struct {
	Controls    []ControlWire `json:"controls"`
	DefaultDeny bool          `json:"default_deny"`
	Version     uint64        `json:"version"`
	Epoch       string        `json:"epoch"`
}

// EncodeSources is s on the wire.
func EncodeSources(s coresources.State) ([]byte, error) {
	w := SourcesWire{Controls: make([]ControlWire, len(s.Controls)), DefaultDeny: s.DefaultDeny, Version: s.Version, Epoch: s.Epoch}
	for i, c := range s.Controls {
		w.Controls[i] = ControlWire{SourceType: c.SourceType, InstanceID: c.InstanceID, Enabled: c.Enabled}
	}
	return json.Marshal(w)
}

// DecodeSources reads a state written by EncodeSources; a control
// without a source type, or a state without an epoch, is refused.
func DecodeSources(data []byte) (coresources.State, error) {
	var w SourcesWire
	if err := json.Unmarshal(data, &w); err != nil {
		return coresources.State{}, core.Fieldf("source_control", "not a source-control state")
	}
	if w.Epoch == "" {
		return coresources.State{}, core.Fieldf("epoch", "required")
	}
	s := coresources.State{Controls: make([]coresources.Control, len(w.Controls)), DefaultDeny: w.DefaultDeny, Version: w.Version, Epoch: w.Epoch}
	for i, c := range w.Controls {
		if c.SourceType == "" {
			return coresources.State{}, core.Fieldf("controls", "entry %d has no source_type", i)
		}
		s.Controls[i] = coresources.Control{SourceType: c.SourceType, InstanceID: c.InstanceID, Enabled: c.Enabled}
	}
	return s, nil
}

// ProjectSources writes s to source_control/state and announces it on
// ctl.sources (sources.Projector).
func (p *Projector) ProjectSources(ctx context.Context, s coresources.State) error {
	data, err := EncodeSources(s)
	if err != nil {
		p.Counters.Inc(CounterKVPutFailed)
		return err
	}
	if err := p.Put(ctx, BucketSourceControl, KeySources, data); err != nil {
		return err
	}
	p.push(CtlSources, Version{Version: s.Version, Epoch: s.Epoch})
	return nil
}
