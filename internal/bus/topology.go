package bus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// Stream names (docs/PLAN.md §7).
const (
	StreamTRK     = "TRK"
	StreamMAN     = "MAN"
	StreamPEER    = "PEER"
	StreamALRT    = "ALRT"
	StreamCONF    = "CONF"
	StreamIDENT   = "IDENT"
	StreamINTENT  = "INTENT"
	StreamCIS     = "CIS"
	StreamTRAFFIC = "TRAFFIC"
	StreamINGEST  = "INGEST"
)

// Bucket names (docs/PLAN.md D6, §7); written by api only.
const (
	BucketCISCurrent       = "cis_current"
	BucketPolicy           = "policy"
	BucketSourceControl    = "source_control"
	BucketRegistryValidity = "registry_validity"
	BucketClientBindings   = "client_bindings"
	BucketIntentActive     = "intent_active"
)

// Bounds (E-10). The server's max_payload is 1 MiB by default, so no
// stream admits more; a value larger than its bucket's bound is refused
// by the server and the projection fails (503, B-09).
const (
	MaxPayloadBytes = 1 << 20
	// TrackMsgBytes bounds one track message (TRK, MAN, PEER).
	TrackMsgBytes = 64 << 10
	// IngestMaxBytes is the INGEST work queue's size: ten minutes of
	// 1000 drones at 1 Hz of about 600 bytes is 360 MB; the queue holds
	// 256 MiB and refuses new messages beyond (the producer counts them).
	IngestMaxBytes = 256 << 20
	// RegistryTTL is registry_validity's TTL: the longest an F8 answer
	// lives (24 h, spec 02 F8); readers check their own shorter TTLs.
	RegistryTTL = 24 * time.Hour
)

// DuplicateWindow is every stream's dedupe window: a durable publish
// retried with the same msg_id inside it is stored once.
const DuplicateWindow = 2 * time.Minute

// Topology is every stream and bucket of this system.
type Topology struct {
	Streams []jetstream.StreamConfig
	Buckets []jetstream.KeyValueConfig
}

// DefaultTopology is docs/PLAN.md §7. TRK, MAN and PEER capture the core
// subjects the hot path publishes (it never waits for an
// acknowledgement); tsdb-writer reads them durably.
func DefaultTopology() Topology {
	stream := func(name, subject, desc string, age time.Duration, maxMsg int32) jetstream.StreamConfig {
		return jetstream.StreamConfig{
			Name: name, Description: desc, Subjects: []string{subject}, Retention: jetstream.LimitsPolicy,
			MaxAge: age, MaxMsgs: -1, MaxBytes: -1, MaxMsgSize: maxMsg, Storage: jetstream.FileStorage,
			Discard: jetstream.DiscardOld, Duplicates: DuplicateWindow, Replicas: 1,
		}
	}
	ingest := stream(StreamINGEST, SubjectIngestAll,
		"telemetry-ingest work queue under backpressure (10 min); full refuses new messages, counted by the producer",
		10*time.Minute, TrackMsgBytes)
	ingest.Retention, ingest.Discard, ingest.MaxBytes = jetstream.WorkQueuePolicy, jetstream.DiscardNew, IngestMaxBytes
	bucket := func(name, desc string, maxValue int32, ttl time.Duration) jetstream.KeyValueConfig {
		return jetstream.KeyValueConfig{
			Bucket: name, Description: desc, History: 1, TTL: ttl, MaxValueSize: maxValue, MaxBytes: -1,
			Storage: jetstream.FileStorage, Replicas: 1,
		}
	}
	return Topology{
		Streams: []jetstream.StreamConfig{
			stream(StreamTRK, SubjectTrkAll, "tracks of the hot path (1 h): restart replay and tsdb-writer", time.Hour, TrackMsgBytes),
			stream(StreamMAN, SubjectManAll, "manned tracks (1 h): tsdb-writer", time.Hour, TrackMsgBytes),
			stream(StreamPEER, SubjectPeerAll, "peer flights (1 h): tsdb-writer", time.Hour, TrackMsgBytes),
			stream(StreamALRT, SubjectAlrtAll, "alerts raised, refreshed and cleared (7 d)", 7*24*time.Hour, 256<<10),
			stream(StreamCONF, SubjectConfAll, "conformance state changes (30 d)", 30*24*time.Hour, 256<<10),
			stream(StreamIDENT, SubjectIdentAll, "identification changes (24 h)", 24*time.Hour, 64<<10),
			stream(StreamINTENT, SubjectIntentAll, "intent states (30 d)", 30*24*time.Hour, 256<<10),
			stream(StreamCIS, SubjectCISAll, "CIS changes (30 d)", 30*24*time.Hour, 256<<10),
			stream(StreamTRAFFIC, SubjectTrafficAll, "traffic products sampled for the record (1 d)", 24*time.Hour, 512<<10),
			ingest,
		},
		Buckets: []jetstream.KeyValueConfig{
			bucket(BucketCISCurrent, "CIS zones per cell5 and the large-zone entry", MaxPayloadBytes, 0),
			bucket(BucketPolicy, "the current policy version", 64<<10, 0),
			bucket(BucketSourceControl, "the source-control state (B-09)", 256<<10, 0),
			bucket(BucketRegistryValidity, "F8 answers by entity and key, statuses only", 16<<10, RegistryTTL),
			bucket(BucketClientBindings, "client id -> bound serial fold keys", 64<<10, 0),
			bucket(BucketIntentActive, "active intents: volumes AMSL, thresholds, flight, cells", 256<<10, 0),
		},
	}
}

// Stream is the configuration of the stream name.
func (t Topology) Stream(name string) (jetstream.StreamConfig, bool) {
	i := slices.IndexFunc(t.Streams, func(c jetstream.StreamConfig) bool { return c.Name == name })
	if i < 0 {
		return jetstream.StreamConfig{}, false
	}
	return t.Streams[i], true
}

// Bucket is the configuration of the bucket name.
func (t Topology) Bucket(name string) (jetstream.KeyValueConfig, bool) {
	i := slices.IndexFunc(t.Buckets, func(c jetstream.KeyValueConfig) bool { return c.Bucket == name })
	if i < 0 {
		return jetstream.KeyValueConfig{}, false
	}
	return t.Buckets[i], true
}

// Ensure creates every stream and bucket of t that does not exist and
// leaves an existing one as it is: changing a live stream is an
// operator's decision, which Verify reports. It returns the names it
// created. Idempotent; safe from every process at once (a create that
// lost the race is not an error).
func Ensure(ctx context.Context, js jetstream.JetStream, t Topology) ([]string, error) {
	var created []string
	for i := range t.Streams {
		want := &t.Streams[i]
		_, err := js.Stream(ctx, want.Name)
		if errors.Is(err, jetstream.ErrStreamNotFound) {
			_, err = js.CreateStream(ctx, *want)
			if err == nil {
				created = append(created, "stream "+want.Name)
			} else if errors.Is(err, jetstream.ErrStreamNameAlreadyInUse) {
				err = nil
			}
		}
		if err != nil {
			return created, fmt.Errorf("stream %s: %w", want.Name, err)
		}
	}
	for i := range t.Buckets {
		want := &t.Buckets[i]
		_, err := js.KeyValue(ctx, want.Bucket)
		if errors.Is(err, jetstream.ErrBucketNotFound) {
			_, err = js.CreateKeyValue(ctx, *want)
			if err == nil {
				created = append(created, "bucket "+want.Bucket)
			} else if errors.Is(err, jetstream.ErrBucketExists) || errors.Is(err, jetstream.ErrStreamNameAlreadyInUse) {
				err = nil
			}
		}
		if err != nil {
			return created, fmt.Errorf("bucket %s: %w", want.Bucket, err)
		}
	}
	return created, nil
}

func norm(v int64) int64 {
	if v <= 0 {
		return -1
	}
	return v
}

// StreamDrift names the settings in which have differs from want.
func StreamDrift(have, want jetstream.StreamConfig) []string {
	var d []string
	if !slices.Equal(have.Subjects, want.Subjects) {
		d = append(d, "subjects")
	}
	if have.Retention != want.Retention {
		d = append(d, "retention")
	}
	if have.MaxAge != want.MaxAge {
		d = append(d, "max_age")
	}
	if norm(have.MaxBytes) != norm(want.MaxBytes) {
		d = append(d, "max_bytes")
	}
	if norm(int64(have.MaxMsgSize)) != norm(int64(want.MaxMsgSize)) {
		d = append(d, "max_msg_size")
	}
	if have.Storage != want.Storage {
		d = append(d, "storage")
	}
	if have.Discard != want.Discard {
		d = append(d, "discard")
	}
	if have.Duplicates != want.Duplicates {
		d = append(d, "duplicate_window")
	}
	return d
}

// bucketDrift compares a bucket's backing stream KV_<bucket> with want:
// history is the stream's per-subject limit, the TTL its max age.
func bucketDrift(have jetstream.StreamConfig, want jetstream.KeyValueConfig) []string {
	var d []string
	if have.MaxMsgsPerSubject != int64(want.History) {
		d = append(d, "history")
	}
	if have.MaxAge != want.TTL {
		d = append(d, "ttl")
	}
	if norm(int64(have.MaxMsgSize)) != norm(int64(want.MaxValueSize)) {
		d = append(d, "max_value_size")
	}
	if have.Storage != want.Storage {
		d = append(d, "storage")
	}
	return d
}

// Verify compares what exists with t: a missing stream or bucket, and
// every managed setting that differs, is one drift line such as
// "stream TRK: max_age". An error is a server that did not answer.
func Verify(ctx context.Context, js jetstream.JetStream, t Topology) ([]string, error) {
	var drift []string
	for i := range t.Streams {
		want := &t.Streams[i]
		s, err := js.Stream(ctx, want.Name)
		if errors.Is(err, jetstream.ErrStreamNotFound) {
			drift = append(drift, "stream "+want.Name+": missing")
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("stream %s: %w", want.Name, err)
		}
		if d := StreamDrift(s.CachedInfo().Config, *want); len(d) > 0 {
			drift = append(drift, "stream "+want.Name+": "+strings.Join(d, ","))
		}
	}
	for i := range t.Buckets {
		want := &t.Buckets[i]
		s, err := js.Stream(ctx, "KV_"+want.Bucket)
		if errors.Is(err, jetstream.ErrStreamNotFound) {
			drift = append(drift, "bucket "+want.Bucket+": missing")
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("bucket %s: %w", want.Bucket, err)
		}
		if d := bucketDrift(s.CachedInfo().Config, *want); len(d) > 0 {
			drift = append(drift, "bucket "+want.Bucket+": "+strings.Join(d, ","))
		}
	}
	return drift, nil
}

// TopologyState is the last check of the topology.
type TopologyState struct {
	// Checked is false until a check completed.
	Checked bool
	At      time.Time
	Drift   []string
	Created []string
}

// Maintainer keeps the topology in place: it creates what is missing
// (Ensure) and records what differs (Verify), once connected and then
// every Interval, and on demand from the readiness probe.
type Maintainer struct {
	JS       jetstream.JetStream
	Topology Topology
	Logger   *slog.Logger
	// Interval is the period of Run (default 30 s).
	Interval time.Duration

	run   sync.Mutex // one check at a time; never held by readers
	mu    sync.Mutex
	state TopologyState
}

// Check runs Ensure and Verify once and records the result.
func (m *Maintainer) Check(ctx context.Context) (TopologyState, error) {
	m.run.Lock()
	defer m.run.Unlock()
	return m.check(ctx)
}

// ErrCheckRunning is TryCheck's answer while another check runs.
var ErrCheckRunning = errors.New("a check of the streams and buckets is running")

// TryCheck is Check unless another check is running, when it returns
// the last state and ErrCheckRunning at once (a readiness probe never
// waits on the background check).
func (m *Maintainer) TryCheck(ctx context.Context) (TopologyState, error) {
	if !m.run.TryLock() {
		return m.State(), ErrCheckRunning
	}
	defer m.run.Unlock()
	return m.check(ctx)
}

func (m *Maintainer) check(ctx context.Context) (TopologyState, error) {
	created, err := Ensure(ctx, m.JS, m.Topology)
	if err != nil {
		return m.State(), err
	}
	drift, err := Verify(ctx, m.JS, m.Topology)
	if err != nil {
		return m.State(), err
	}
	st := TopologyState{Checked: true, At: time.Now(), Drift: drift, Created: created}
	m.mu.Lock()
	prev := m.state
	m.state = st
	m.mu.Unlock()
	if m.Logger != nil {
		if len(created) > 0 {
			m.Logger.LogAttrs(ctx, slog.LevelInfo, "bus topology created", obs.Dependency("nats"), slog.Any("created", created))
		}
		if len(drift) > 0 && !slices.Equal(drift, prev.Drift) {
			m.Logger.LogAttrs(ctx, slog.LevelWarn, "bus topology differs from this build's", obs.Dependency("nats"), slog.Any("drift", drift))
		}
	}
	return st, nil
}

// State is the last check.
func (m *Maintainer) State() TopologyState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// Run checks at once and then every Interval until ctx ends; a failed
// check (NATS down) is retried at the next tick.
func (m *Maintainer) Run(ctx context.Context, timeout time.Duration) {
	every := m.Interval
	if every <= 0 {
		every = 30 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		cctx, cancel := context.WithTimeout(ctx, timeout)
		_, _ = m.Check(cctx)
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
