//go:build integration

package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/rootxkit/uspace-core/core"
	coresources "github.com/rootxkit/uspace-core/sources"

	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
)

func natsURL(t *testing.T) string {
	t.Helper()
	u := os.Getenv("USSP_TEST_NATS_URL")
	if u == "" {
		t.Fatal("USSP_TEST_NATS_URL is not set; see the Makefile's integration target")
	}
	return u
}

func connect(t *testing.T) *Conn {
	t.Helper()
	c, err := Connect(natsURL(t), "", "bus-test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	deadline := time.Now().Add(5 * time.Second)
	for !c.IsConnected() {
		if time.Now().After(deadline) {
			t.Fatal("not connected")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return c
}

var seq atomic.Int64

func unique(prefix string) string {
	return fmt.Sprintf("%s_%d_%d", prefix, time.Now().UnixNano()%1e9, seq.Add(1))
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

// Ensure creates what is missing and is idempotent; Verify reports
// nothing on what Ensure made and names every difference after a
// stream was changed behind its back, and a missing stream and bucket.
func TestIntegrationTopologyEnsureVerifyDrift(t *testing.T) {
	c := connect(t)
	js := c.JetStream()
	name := strings.ToUpper(unique("DRIFT"))
	bucket := unique("drift")
	top := Topology{
		Streams: []jetstream.StreamConfig{{Name: name, Subjects: []string{strings.ToLower(name) + ".>"}, MaxAge: time.Hour,
			MaxMsgSize: 1024, Storage: jetstream.FileStorage, Duplicates: DuplicateWindow, Retention: jetstream.LimitsPolicy}},
		Buckets: []jetstream.KeyValueConfig{{Bucket: bucket, History: 1, MaxValueSize: 1024, Storage: jetstream.FileStorage}},
	}
	t.Cleanup(func() {
		_ = js.DeleteStream(context.Background(), name)
		_ = js.DeleteKeyValue(context.Background(), bucket)
	})
	drift, err := Verify(ctx(t), js, top)
	if err != nil || !slices.Equal(drift, []string{"stream " + name + ": missing", "bucket " + bucket + ": missing"}) {
		t.Fatalf("before: %v %v", drift, err)
	}
	created, err := Ensure(ctx(t), js, top)
	if err != nil || len(created) != 2 {
		t.Fatalf("created %v %v", created, err)
	}
	if created, err = Ensure(ctx(t), js, top); err != nil || len(created) != 0 {
		t.Fatalf("second Ensure created %v %v", created, err)
	}
	if drift, err = Verify(ctx(t), js, top); err != nil || len(drift) != 0 {
		t.Fatalf("after Ensure: %v %v", drift, err)
	}
	changed := top.Streams[0]
	changed.MaxAge = 30 * time.Minute
	if _, err := js.UpdateStream(ctx(t), changed); err != nil {
		t.Fatal(err)
	}
	if drift, err = Verify(ctx(t), js, top); err != nil || !slices.Equal(drift, []string{"stream " + name + ": max_age"}) {
		t.Fatalf("drift %v %v", drift, err)
	}
	// Ensure never changes an existing stream back: an operator does.
	if _, err := Ensure(ctx(t), js, top); err != nil {
		t.Fatal(err)
	}
	if drift, _ = Verify(ctx(t), js, top); len(drift) != 1 {
		t.Fatalf("Ensure changed a live stream: %v", drift)
	}
}

// The readiness probe: up once the default topology is in place
// (E-02: the success path, read), degraded naming the drift when a
// stream differs, and down with its reason when NATS is gone.
func TestIntegrationProbeReportsTopology(t *testing.T) {
	c := connect(t)
	m := c.Maintain(DefaultTopology(), nil)
	if st, d := c.Probe()(ctx(t)); st != obs.StateUp || d != "" {
		t.Fatalf("probe %s %q", st, d)
	}
	if st := m.State(); !st.Checked || len(st.Drift) != 0 {
		t.Fatalf("state %+v", st)
	}
	// Drift: the maintainer is told to expect a longer TRK.
	m.Topology = DefaultTopology()
	m.Topology.Streams[0].MaxAge = 2 * time.Hour
	if _, err := m.Check(ctx(t)); err != nil {
		t.Fatal(err)
	}
	st, d := c.Probe()(ctx(t))
	if st != obs.StateDegraded || !strings.Contains(d, "stream TRK: max_age") {
		t.Fatalf("probe %s %q", st, d)
	}
	c.Close()
	if st, d := c.Probe()(ctx(t)); st != obs.StateDown || !strings.Contains(d, "not connected") {
		t.Fatalf("closed: %s %q", st, d)
	}
}

func TestIntegrationMaintainerRuns(t *testing.T) {
	c := connect(t)
	m := &Maintainer{JS: c.JetStream(), Topology: DefaultTopology(), Interval: 50 * time.Millisecond}
	rctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(rctx, 5*time.Second); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for !m.State().Checked && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if !m.State().Checked {
		t.Fatal("never checked")
	}
}

func newProjector(_ *testing.T, c *Conn) *Projector { return NewProjector(c, nil) }

// The projector writes, deletes and lists within its bound; a key that
// is not one is refused before any I/O, and a value over the bucket's
// bound is refused by the server (E-10).
func TestIntegrationProjectorPutDeleteKeys(t *testing.T) {
	c := connect(t)
	p := newProjector(t, c)
	bucket := unique("proj")
	p.Topology.Buckets = append(p.Topology.Buckets, jetstream.KeyValueConfig{Bucket: bucket, History: 1, MaxValueSize: 64, Storage: jetstream.FileStorage})
	t.Cleanup(func() { _ = c.JetStream().DeleteKeyValue(context.Background(), bucket) })
	if err := p.PutJSON(ctx(t), bucket, "a", map[string]int{"x": 1}); err != nil {
		t.Fatal(err)
	}
	if err := p.Put(ctx(t), bucket, "b", []byte("2")); err != nil {
		t.Fatal(err)
	}
	keys, err := p.Keys(ctx(t), bucket)
	slices.Sort(keys)
	if err != nil || !slices.Equal(keys, []string{"a", "b"}) {
		t.Fatalf("keys %v %v", keys, err)
	}
	if err := p.Delete(ctx(t), bucket, "a"); err != nil {
		t.Fatal(err)
	}
	if keys, _ = p.Keys(ctx(t), bucket); !slices.Equal(keys, []string{"b"}) {
		t.Fatalf("after delete %v", keys)
	}
	if err := p.Put(ctx(t), bucket, "c5:1:1", []byte("x")); err == nil {
		t.Fatal("a colon key accepted")
	}
	if err := p.Delete(ctx(t), bucket, ""); err == nil {
		t.Fatal("an empty key deleted")
	}
	if err := p.Put(ctx(t), bucket, "big", make([]byte, 65)); err == nil {
		t.Fatal("a value over the bucket bound accepted")
	}
	if err := p.Put(ctx(t), "not_in_topology_"+bucket, "a", nil); err == nil {
		t.Fatal("an unknown bucket created")
	}
	if p.Counters.Get(CounterKVPut) != 2 || p.Counters.Get(CounterKVPutFailed) != 3 || p.Counters.Get(CounterKVDelete) != 1 || p.Counters.Get(CounterKVDeleteFailed) != 1 {
		t.Fatal(p.Counters.Snapshot())
	}
	// Empty bucket: no keys, no error.
	if err := p.Delete(ctx(t), bucket, "b"); err != nil {
		t.Fatal(err)
	}
	if keys, err := p.Keys(ctx(t), bucket); err != nil || len(keys) != 0 {
		t.Fatalf("%v %v", keys, err)
	}
}

// With NATS gone the projector fails within its bound, so the writer's
// transaction rolls back and answers 503 (B-09).
func TestIntegrationProjectorFailsWithinItsBound(t *testing.T) {
	c := connect(t)
	p := newProjector(t, c)
	p.Timeout = 300 * time.Millisecond
	c.Close()
	start := time.Now()
	err := p.ProjectPolicy(context.Background(), policy.Record{Version: 1, Values: policy.Defaults()})
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
	if err := p.ProjectSources(context.Background(), coresources.State{Version: 1, Epoch: "e"}); err == nil {
		t.Fatal("sources projected without NATS")
	}
}

func follow[T any](t *testing.T, c *Conn, bucket, key string, decode func([]byte) (T, error)) *Follower[T] {
	f := &Follower[T]{JS: c.JetStream(), Bucket: bucket, Key: key, Decode: decode, Retry: 100 * time.Millisecond, Reread: time.Hour}
	fctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.Run(fctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return f
}

func waitFor(t *testing.T, within time.Duration, cond func() bool) time.Duration {
	t.Helper()
	start := time.Now()
	for !cond() {
		if time.Since(start) > within {
			t.Fatalf("not within %v", within)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return time.Since(start)
}

// KV missing at follower start: Value says ok false (the caller's
// default applies); the bucket appears: the value within 1 s. Then an
// update arrives by the watch, a delete keeps the last value, and a
// value that does not decode is counted and the last one kept.
func TestIntegrationFollowerMissingThenAppears(t *testing.T) {
	c := connect(t)
	bucket := unique("follow")
	t.Cleanup(func() { _ = c.JetStream().DeleteKeyValue(context.Background(), bucket) })
	f := follow(t, c, bucket, "current", func(b []byte) (int, error) {
		var v int
		err := json.Unmarshal(b, &v)
		return v, err
	})
	time.Sleep(300 * time.Millisecond)
	if v, age, ok := f.Value(); ok || v != 0 || age != 0 {
		t.Fatalf("value before the bucket exists: %v %v %v", v, age, ok)
	}
	kv, err := c.JetStream().CreateKeyValue(ctx(t), jetstream.KeyValueConfig{Bucket: bucket, History: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kv.Put(ctx(t), "current", []byte("41")); err != nil {
		t.Fatal(err)
	}
	took := waitFor(t, time.Second, func() bool { v, _, ok := f.Value(); return ok && v == 41 })
	t.Logf("value read %v after the bucket appeared", took)

	if _, err := kv.Put(ctx(t), "current", []byte("42")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { v, _, _ := f.Value(); return v == 42 })
	if err := kv.Delete(ctx(t), "current"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return f.Counters.Get(CounterFollowDeleted) == 1 })
	if v, _, ok := f.Value(); !ok || v != 42 {
		t.Fatalf("a delete dropped the value: %v %v", v, ok)
	}
	if _, err := kv.Put(ctx(t), "current", []byte("not a number")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return f.Counters.Get(CounterFollowDecodeFailed) == 1 })
	if v, age, ok := f.Value(); !ok || v != 42 || age < 0 {
		t.Fatalf("after a bad value: %v %v %v", v, age, ok)
	}
}

// The age grows while nothing confirms the value and drops when a push
// on the control subject makes the follower read the bucket again.
func TestIntegrationFollowerAgeAndPush(t *testing.T) {
	c := connect(t)
	bucket := unique("age")
	t.Cleanup(func() { _ = c.JetStream().DeleteKeyValue(context.Background(), bucket) })
	kv, err := c.JetStream().CreateKeyValue(ctx(t), jetstream.KeyValueConfig{Bucket: bucket, History: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kv.Put(ctx(t), "k", []byte(`"v1"`)); err != nil {
		t.Fatal(err)
	}
	push := "ctl." + unique("push")
	var applied atomic.Int64
	f := &Follower[string]{JS: c.JetStream(), Bucket: bucket, Key: "k", Core: c.Conn, Push: push, Reread: time.Hour,
		Retry: 100 * time.Millisecond, Apply: func(string) { applied.Add(1) }}
	fctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.Run(fctx); close(done) }()
	defer func() { cancel(); <-done }()
	waitFor(t, time.Second, func() bool { _, _, ok := f.Value(); return ok })
	time.Sleep(600 * time.Millisecond)
	_, before, _ := f.Value()
	if before < 0.5 {
		t.Fatalf("age %v did not grow", before)
	}
	if err := c.Publish(push, []byte(`{"version":2}`)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { _, age, _ := f.Value(); return age < before })
	if applied.Load() != 1 {
		t.Fatalf("a re-read of the same revision applied again: %d", applied.Load())
	}
}

// ProjectPolicy and ProjectSources write what the followers read, and
// announce each version on its ctl subject.
func TestIntegrationProjectorPolicyAndSources(t *testing.T) {
	c := connect(t)
	p := newProjector(t, c)
	ann := make(chan *nats.Msg, 4)
	sub, err := c.ChanSubscribe("ctl.*", ann)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	if err := c.Flush(); err != nil {
		t.Fatal(err)
	}
	pf := follow(t, c, BucketPolicy, KeyPolicy, func(b []byte) (policy.Record, error) {
		var r policy.Record
		err := json.Unmarshal(b, &r)
		return r, err
	})
	rec := policy.Record{Version: time.Now().UnixNano(), Actor: "test", Reason: "integration", Values: policy.Defaults()}
	if err := p.ProjectPolicy(ctx(t), rec); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, func() bool { r, _, ok := pf.Value(); return ok && r.Version == rec.Version })

	inst := "ti-1"
	st := coresources.State{Controls: []coresources.Control{{SourceType: "operator_ws", InstanceID: &inst, Enabled: false}},
		Version: uint64(time.Now().UnixNano()), Epoch: "test-epoch"}
	sf := follow(t, c, BucketSourceControl, KeySources, DecodeSources)
	if err := p.ProjectSources(ctx(t), st); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, func() bool { s, _, ok := sf.Value(); return ok && s.Version == st.Version })
	got := map[string]string{}
	for len(got) < 2 {
		select {
		case m := <-ann:
			got[m.Subject] = string(m.Data)
		case <-time.After(2 * time.Second):
			t.Fatalf("announcements %v", got)
		}
	}
	if got[CtlSources] != fmt.Sprintf(`{"version":%d,"epoch":"test-epoch"}`, st.Version) || !strings.HasPrefix(got[CtlPolicy], `{"version":`) {
		t.Fatalf("announcements %v", got)
	}
}

// A durable pull consumer on a stream: created with the stream when it
// is missing, explicit ack, bounded pending.
func TestIntegrationPullConsumer(t *testing.T) {
	c := connect(t)
	name := strings.ToUpper(unique("PULL"))
	subj := strings.ToLower(name)
	top := Topology{Streams: []jetstream.StreamConfig{{Name: name, Subjects: []string{subj + ".>"}, Storage: jetstream.MemoryStorage}}}
	t.Cleanup(func() { _ = c.JetStream().DeleteStream(context.Background(), name) })
	s, cons, err := PullConsumer(ctx(t), c.JetStream(), top, name, PullSpec{Durable: "d", MaxAckPending: 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.JetStream().Publish(ctx(t), subj+".a", []byte("x")); err != nil {
		t.Fatal(err)
	}
	b, err := cons.Fetch(1, jetstream.FetchMaxWait(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for m := range b.Messages() {
		n++
		if err := m.Ack(); err != nil {
			t.Fatal(err)
		}
	}
	info, err := cons.Info(ctx(t))
	if n != 1 || err != nil || info.Config.MaxAckPending != 10 || info.Config.AckPolicy != jetstream.AckExplicitPolicy || s.CachedInfo().Config.Name != name {
		t.Fatalf("%d %v %+v", n, err, info)
	}
}

// The publisher end to end: a durable publish is acknowledged and the
// same msg_id inside the duplicate window is stored once; a core publish
// is captured by its stream.
func TestIntegrationPublisherDedupe(t *testing.T) {
	c := connect(t)
	if _, err := Ensure(ctx(t), c.JetStream(), DefaultTopology()); err != nil {
		t.Fatal(err)
	}
	p := NewPublisher(c, nil)
	m := trackMsg{Envelope: SystemEnvelope("conformance/state/v1", "ussp/monitor", time.Now())}
	flight := unique("f")
	subj, _ := Conf(flight)
	for range 3 {
		if err := p.Publish(ctx(t), subj, &m); err != nil {
			t.Fatal(err)
		}
	}
	s, err := c.JetStream().Stream(ctx(t), StreamCONF)
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.Info(ctx(t), jetstream.WithSubjectFilter(subj))
	if err != nil || info.State.Subjects[subj] != 1 {
		t.Fatalf("stored %v times, %v", info.State.Subjects[subj], err)
	}
	if p.Counters.Get("published_conf") != 3 {
		t.Fatal(p.Counters.Snapshot())
	}
}

// The mirror against the real bucket: never read (no bucket) is not
// loaded; created, it reads what is there, follows puts and deletes, and
// a key written before it started is in its first read (SC-22, D6).
func TestIntegrationMirror(t *testing.T) {
	c := connect(t)
	js := c.JetStream()
	bucket := unique("mirror")
	t.Cleanup(func() { _ = js.DeleteKeyValue(context.Background(), bucket) })
	m := &Mirror[string]{JS: js, Bucket: bucket, Retry: 50 * time.Millisecond}
	runCtx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.Run(runCtx) }()
	t.Cleanup(func() { stop(); <-done })
	time.Sleep(200 * time.Millisecond)
	if _, _, loaded := m.Snapshot(); loaded {
		t.Fatal("loaded without a bucket")
	}
	kv, err := js.CreateKeyValue(ctx(t), jetstream.KeyValueConfig{Bucket: bucket, History: 1, Storage: jetstream.FileStorage})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kv.Put(ctx(t), "a", []byte(`"1"`)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { v, ok, _, loaded := m.Get("a"); return loaded && ok && v == "1" })
	if _, err := kv.Put(ctx(t), "b", []byte(`"2"`)); err != nil {
		t.Fatal(err)
	}
	if err := kv.Delete(ctx(t), "a"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		vals, _, _ := m.Snapshot()
		_, hasA := vals["a"]
		return !hasA && vals["b"] == "2"
	})
}

// NeverDelivered observed on a real work queue (05 §5, B-13): messages
// the stream ages out before its consumer was delivered them are the
// range it reports; with nothing removed it reports none (E-01 pair).
func TestIntegrationNeverDeliveredOnAWorkQueue(t *testing.T) {
	c := connect(t)
	js := c.JetStream()
	name := strings.ToUpper(unique("WQ"))
	subject := strings.ToLower(name) + ".x"
	cfg := jetstream.StreamConfig{Name: name, Subjects: []string{strings.ToLower(name) + ".>"}, Retention: jetstream.WorkQueuePolicy,
		MaxAge: time.Second, Storage: jetstream.FileStorage, Duplicates: 500 * time.Millisecond}
	top := Topology{Streams: []jetstream.StreamConfig{cfg}}
	t.Cleanup(func() { _ = js.DeleteStream(context.Background(), name) })
	src := &StreamSource{Open: PullOpener(js, top, name, PullSpec{Durable: "wq", FilterSubject: strings.ToLower(name) + ".>", MaxAckPending: 10})}
	from, to, err := src.NeverDelivered(ctx(t))
	if err != nil || from <= to {
		t.Fatalf("empty queue: %d..%d %v", from, to, err)
	}
	for i := range 3 {
		if _, err := js.Publish(ctx(t), subject, []byte(fmt.Sprint(i))); err != nil {
			t.Fatal(err)
		}
	}
	if from, to, _ := src.NeverDelivered(ctx(t)); from <= to {
		t.Fatalf("held messages reported lost: %d..%d", from, to)
	}
	time.Sleep(2500 * time.Millisecond) // the stream's MaxAge removes them unread
	from, to, err = src.NeverDelivered(ctx(t))
	if err != nil || from != 1 || to != 3 {
		t.Fatalf("aged out: %d..%d %v", from, to, err)
	}
}

// SeenWindow on a real bucket: a key put through Run is read back by
// another reader (another replica, or the process after a restart), a
// key never put is not found, and a bucket that does not exist is an
// error, never "not found" (the ingest counts it and takes the sample).
func TestIntegrationSeenWindow(t *testing.T) {
	c := connect(t)
	js := c.JetStream()
	bucket := unique("seen")
	t.Cleanup(func() { _ = js.DeleteKeyValue(context.Background(), bucket) })
	missing := &SeenWindow{JS: js, Bucket: bucket}
	if _, _, err := missing.Get(ctx(t), "k.1"); err == nil {
		t.Fatal("no bucket read as an answer")
	}
	cfg, _ := DefaultTopology().Bucket(BucketTelemetrySeen)
	cfg.Bucket = bucket
	if _, err := js.CreateKeyValue(ctx(t), cfg); err != nil {
		t.Fatal(err)
	}
	w := &SeenWindow{JS: js, Bucket: bucket}
	runCtx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.Run(runCtx) }()
	t.Cleanup(func() { stop(); <-done })
	w.Put("abc.1", []byte("0123456789abcdef"))
	other := &SeenWindow{JS: js, Bucket: bucket}
	waitFor(t, 5*time.Second, func() bool {
		v, ok, err := other.Get(ctx(t), "abc.1")
		return err == nil && ok && string(v) == "0123456789abcdef"
	})
	if _, ok, err := other.Get(ctx(t), "abc.2"); err != nil || ok {
		t.Fatalf("a key never put: found %v err %v", ok, err)
	}
	if n := w.Counters.Get(CounterSeenPutFailed); n != 0 {
		t.Fatalf("%d puts failed", n)
	}
}

// The revision writes the monitor's conformance_state rests on, on a
// real bucket: a create of a key that exists and an update on a stale
// revision are conflicts (E-01 pair with the writes that succeed); a
// deleted key can be created again; All reads every current value.
func TestIntegrationKVStoreRevisions(t *testing.T) {
	c := connect(t)
	js := c.JetStream()
	bucket := unique("rev")
	t.Cleanup(func() { _ = js.DeleteKeyValue(context.Background(), bucket) })
	if _, err := js.CreateKeyValue(ctx(t), jetstream.KeyValueConfig{Bucket: bucket, History: 1, Storage: jetstream.FileStorage}); err != nil {
		t.Fatal(err)
	}
	s := KVStore{JS: js, Bucket: bucket}
	if _, found, err := s.GetRev(ctx(t), "a"); err != nil || found {
		t.Fatalf("%v %v", found, err)
	}
	r1, err := s.PutRev(ctx(t), "a", []byte("1"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutRev(ctx(t), "a", []byte("x"), 0); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("create of an existing key: %v", err)
	}
	r2, err := s.PutRev(ctx(t), "a", []byte("2"), r1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutRev(ctx(t), "a", []byte("x"), r1); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("update on a stale revision: %v", err)
	}
	if e, found, err := s.GetRev(ctx(t), "a"); err != nil || !found || string(e.Value) != "2" || e.Rev != r2 {
		t.Fatalf("%+v %v %v", e, found, err)
	}
	if _, err := s.PutRev(ctx(t), "b", []byte("3"), 0); err != nil {
		t.Fatal(err)
	}
	all, err := s.All(ctx(t), 10)
	if err != nil || len(all) != 2 {
		t.Fatalf("%+v %v", all, err)
	}
	if err := s.Delete(ctx(t), "a"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx(t), "nothing"); err != nil {
		t.Fatalf("deleting a missing key: %v", err)
	}
	if _, err := s.PutRev(ctx(t), "a", []byte("4"), 0); err != nil {
		t.Fatalf("create after a delete: %v", err)
	}
	if all, err := s.All(ctx(t), 1); err != nil || len(all) != 1 {
		t.Fatalf("bound: %+v %v", all, err)
	}
}

// The captures of the hot path (audit B1, N6): a core publish of trk
// cannot see the stream refuse it, so the maintainer counts what TRK,
// MAN and PEER captured (their last sequence's growth between checks)
// to stand beside published_trk. Three trk messages published are three
// captured; nothing published moves nothing.
func TestIntegrationMaintainerCountsCaptures(t *testing.T) {
	c := connect(t)
	m := c.Maintain(DefaultTopology(), nil)
	if _, err := m.Check(ctx(t)); err != nil {
		t.Fatal(err)
	}
	if n := m.Counters().Get(CounterCapturedPrefix + KindTrk); n != 0 {
		t.Fatalf("captured %d before any publish", n)
	}
	subject, err := Trk(tbs, unique("trk"))
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := c.Publish(subject, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Flush(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for m.Counters().Get(CounterCapturedPrefix+KindTrk) < 3 && time.Now().Before(deadline) {
		if _, err := m.Check(ctx(t)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := m.Counters().Get(CounterCapturedPrefix + KindTrk); n != 3 {
		t.Fatalf("captured_trk %d, want 3", n)
	}
	if _, err := m.Check(ctx(t)); err != nil {
		t.Fatal(err)
	}
	if n := m.Counters().Get(CounterCapturedPrefix + KindTrk); n != 3 {
		t.Fatalf("captured_trk %d after a check with nothing published", n)
	}
}

// Interior deletes beyond MaxDeletedDetails are not read, so a hole
// across them counts the head losses only: that is now counted as
// hole_undercounted (audit N5). Within the bound the deletes are read
// and counted in the hole, and nothing is undercounted (E-01 pair).
func TestIntegrationHoleUndercountedBeyondTheDeletedBound(t *testing.T) {
	c := connect(t)
	js := c.JetStream()
	name := strings.ToUpper(unique("HOLE"))
	subject := strings.ToLower(name) + ".x"
	cfg := jetstream.StreamConfig{Name: name, Subjects: []string{strings.ToLower(name) + ".>"}, Storage: jetstream.FileStorage,
		Duplicates: 500 * time.Millisecond}
	top := Topology{Streams: []jetstream.StreamConfig{cfg}}
	t.Cleanup(func() { _ = js.DeleteStream(context.Background(), name) })
	if _, err := Ensure(ctx(t), js, top); err != nil {
		t.Fatal(err)
	}
	for i := range 6 {
		if _, err := js.Publish(ctx(t), subject, []byte(fmt.Sprint(i))); err != nil {
			t.Fatal(err)
		}
	}
	st, err := js.Stream(ctx(t), name)
	if err != nil {
		t.Fatal(err)
	}
	for _, seq := range []uint64{3, 4} {
		if err := st.DeleteMsg(ctx(t), seq); err != nil {
			t.Fatal(err)
		}
	}
	jump := []Jump{{After: 2, Before: 5}}
	for _, c := range []struct {
		bound int
		count uint64
		under uint64
	}{{10, 2, 0}, {1, 0, 1}} {
		counters := &core.Counters{}
		src := &StreamSource{MaxDeletedDetails: c.bound, Counters: counters,
			Open: PullOpener(js, top, name, PullSpec{Durable: "h" + fmt.Sprint(c.bound), MaxAckPending: 10})}
		holes, err := src.Holes(ctx(t), jump)
		if err != nil || len(holes) != 1 || holes[0].Count != c.count || counters.Get(CounterHoleUndercounted) != c.under {
			t.Fatalf("bound %d: %+v %v, undercounted %d", c.bound, holes, err, counters.Get(CounterHoleUndercounted))
		}
	}
}

// StreamHas finds a message on its subject and answers no for a subject
// the stream does not hold; a stream that does not exist is an error,
// never a no.
func TestIntegrationStreamHas(t *testing.T) {
	c := connect(t)
	js := c.JetStream()
	name := unique("HAS")
	prefix := strings.ToLower(name) + "."
	t.Cleanup(func() { _ = js.DeleteStream(context.Background(), name) })
	if _, err := js.CreateStream(ctx(t), jetstream.StreamConfig{Name: name, Subjects: []string{prefix + ">"}, Storage: jetstream.FileStorage}); err != nil {
		t.Fatal(err)
	}
	if _, err := js.Publish(ctx(t), prefix+"ended.f1", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	h := &StreamHas{JS: js, Stream: name}
	if has, err := h.Has(ctx(t), prefix+"ended.f1"); err != nil || !has {
		t.Fatalf("a published subject: %v %v", has, err)
	}
	if has, err := h.Has(ctx(t), prefix+"ended.f2"); err != nil || has {
		t.Fatalf("a subject never published: %v %v", has, err)
	}
	missing := &StreamHas{JS: js, Stream: unique("NONE")}
	if _, err := missing.Has(ctx(t), prefix+"ended.f1"); err == nil {
		t.Fatal("a stream that does not exist answered")
	}
}
