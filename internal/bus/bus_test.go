package bus

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/rootxkit/uspace-core/core"
	coresources "github.com/rootxkit/uspace-core/sources"
)

const tbs = "c5:1317:2248" // Tbilisi

func TestBuilders(t *testing.T) {
	cases := []struct {
		got  func() (string, error)
		want string
	}{
		{func() (string, error) { return Trk(tbs, "trk-1") }, "trk.v1.c3:131:224.c5:1317:2248.trk-1"},
		{func() (string, error) { return Man(tbs, "4ca7b5") }, "man.v1.c3:131:224.c5:1317:2248.4ca7b5"},
		{func() (string, error) { return Peer(tbs, "f-1") }, "peer.v1.c3:131:224.c5:1317:2248.f-1"},
		{func() (string, error) { return Alrt("proximity", tbs, "a-1") }, "alrt.v1.proximity.c5:1317:2248.a-1"},
		{func() (string, error) { return Conf("fl-1") }, "conf.v1.fl-1"},
		{func() (string, error) { return Ident("trk-1") }, "ident.v1.trk-1"},
		{func() (string, error) { return Intent("accepted", "in-1") }, "intent.v1.accepted.in-1"},
		{func() (string, error) { return CIS("zones") }, "cis.v1.zones"},
		{func() (string, error) { return TrafficProduct("client-1") }, "traffic.product.v1.client-1"},
		{func() (string, error) { return Ingest("c3:131:224") }, "ingest.v1.c3:131:224"},
		{func() (string, error) { return Src("operator_ws", "ti-1") }, "src.v1.operator_ws.ti-1"},
		{func() (string, error) { return TrkCell3("c3:131:224") }, "trk.v1.c3:131:224.>"},
	}
	for _, c := range cases {
		got, err := c.got()
		if err != nil || got != c.want {
			t.Errorf("got %q %v, want %q", got, err, c.want)
			continue
		}
		if strings.HasSuffix(got, ">") {
			continue
		}
		if _, err := Parse(got); err != nil {
			t.Errorf("Parse(%q): %v", got, err)
		}
	}
}

func TestBuildersRefuse(t *testing.T) {
	long := strings.Repeat("x", MaxTokenBytes+1)
	for name, f := range map[string]func() (string, error){
		"trk bad cell":     func() (string, error) { return Trk("c3:131:224", "x") },
		"trk dotted id":    func() (string, error) { return Trk(tbs, "a.b") },
		"trk wildcard id":  func() (string, error) { return Trk(tbs, "*") },
		"trk tail id":      func() (string, error) { return Trk(tbs, "a>") },
		"trk empty id":     func() (string, error) { return Trk(tbs, "") },
		"trk space id":     func() (string, error) { return Trk(tbs, "a b") },
		"trk control id":   func() (string, error) { return Trk(tbs, "a\x00") },
		"trk long id":      func() (string, error) { return Trk(tbs, long) },
		"trk not utf8":     func() (string, error) { return Trk(tbs, "\xff") },
		"alrt bad kind":    func() (string, error) { return Alrt("", tbs, "a") },
		"alrt bad cell":    func() (string, error) { return Alrt("k", "c5:x", "a") },
		"alrt bad id":      func() (string, error) { return Alrt("k", tbs, "a.b") },
		"intent bad state": func() (string, error) { return Intent("a.b", "x") },
		"intent bad id":    func() (string, error) { return Intent("a", "") },
		"ingest c5":        func() (string, error) { return Ingest(tbs) },
		"trkcell3 c5":      func() (string, error) { return TrkCell3(tbs) },
		"conf empty":       func() (string, error) { return Conf("") },
	} {
		got, err := f()
		var fe *core.FieldError
		if !errors.As(err, &fe) || got != "" {
			t.Errorf("%s: %q %v", name, got, err)
		}
	}
}

func TestParse(t *testing.T) {
	s, err := Parse("trk.v1.c3:131:224.c5:1317:2248.trk-1")
	if err != nil || s.Kind != KindTrk || s.Cell3 != "c3:131:224" || s.Cell5 != tbs || s.ID != "trk-1" {
		t.Fatalf("%+v %v", s, err)
	}
	s, err = Parse("alrt.v1.zone.c5:1317:2248.a-1")
	if err != nil || s.Sub != "zone" || s.Cell5 != tbs || s.ID != "a-1" {
		t.Fatalf("%+v %v", s, err)
	}
	if s, err = Parse("cis.v1.zones"); err != nil || s.Sub != "zones" {
		t.Fatalf("%+v %v", s, err)
	}
	if s, err = Parse("conf.v1.f"); err != nil || s.ID != "f" {
		t.Fatalf("%+v %v", s, err)
	}
	if s, err = Parse("intent.v1.accepted.i"); err != nil || s.Sub != "accepted" || s.ID != "i" {
		t.Fatalf("%+v %v", s, err)
	}
	if s, err = Parse("src.v1.adsb_rx.r1"); err != nil || s.Sub != "adsb_rx" || s.Instance != "r1" {
		t.Fatalf("%+v %v", s, err)
	}
	if s, err = Parse("flight.v1.started.f-1"); err != nil || s.Kind != KindFlight || s.Sub != "started" || s.ID != "f-1" {
		t.Fatalf("%+v %v", s, err)
	}
	if f, err := Flight("ended", "f-1"); err != nil || f != "flight.v1.ended.f-1" {
		t.Fatalf("%q %v", f, err)
	}
	if f, err := Flight("ended", "a.b"); err == nil {
		t.Fatalf("flight id with a dot: %q", f)
	}
	if s, err = Parse("ingest.v1.c3:131:224"); err != nil || s.Cell3 != "c3:131:224" {
		t.Fatalf("%+v %v", s, err)
	}
	if s, err = Parse("traffic.product.v1.c1"); err != nil || s.Kind != KindTraffic || s.ID != "c1" {
		t.Fatalf("%+v %v", s, err)
	}
	if s, err = Parse(CtlSources); err != nil || s.Kind != KindCtl || s.Sub != "sources" {
		t.Fatalf("%+v %v", s, err)
	}
	// Wrong token counts, unknown kinds, bad cells: errors, never panics.
	for _, bad := range []string{
		"", "trk", "trk.v1", "trk.v1.c3:131:224.c5:1317:2248", "trk.v1.c3:131:224.c5:1317:2248.a.b",
		"trk.v2.c3:131:224.c5:1317:2248.a", "trk.v1.c3:131:225.c5:1317:2248.a", "trk.v1.c3:131:224.c5:x.a",
		"alrt.v1.k.c5:x.a", "ingest.v1.c5:1317:2248", "nope.v1.a", "conf.v1..", "conf.v1.a.b", "conf.v1.*",
		"flight.v1.started", "traffic.product.v1", "traffic.product.v1.a.b", "traffic.product.v1.*", "ctl.other", "x.v1.a",
		strings.Repeat("a.", MaxSubjectBytes),
	} {
		if s, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) = %+v", bad, s)
		}
	}
}

func TestDurable(t *testing.T) {
	for _, k := range []string{KindTrk, KindMan, KindPeer, KindSrc, KindCtl} {
		if Durable(k) {
			t.Errorf("%s durable", k)
		}
	}
	for _, k := range []string{KindAlrt, KindConf, KindIdent, KindIntent, KindCIS, KindTraffic, KindIngest, KindFlight} {
		if !Durable(k) {
			t.Errorf("%s not durable", k)
		}
	}
}

func TestULID(t *testing.T) {
	at := time.Date(2026, 10, 2, 9, 15, 5, 500e6, time.UTC)
	a, b := NewULID(at), NewULID(at)
	if !ValidULID(a) || !ValidULID(b) || a == b {
		t.Fatalf("%q %q", a, b)
	}
	if got, ok := ULIDTime(a); !ok || !got.Equal(at) {
		t.Fatalf("ULIDTime = %v %v", got, ok)
	}
	// The lab's example decodes to a plausible time.
	if got, ok := ULIDTime("01K6CD1GDJFMGT83PXT891WB09"); !ok || got.Year() != 2025 && got.Year() != 2026 {
		t.Fatalf("lab ULID time %v", got)
	}
	for _, bad := range []string{"", "8ZZZZZZZZZZZZZZZZZZZZZZZZZ", "01K6CD1GDJFMGT83PXT891WB0", "01K6CD1GDJFMGT83PXT891WB0I"} {
		if ValidULID(bad) {
			t.Errorf("%q valid", bad)
		}
		if _, ok := ULIDTime(bad); ok {
			t.Errorf("ULIDTime(%q) ok", bad)
		}
	}
}

type trackMsg struct {
	Envelope
	Body struct {
		TrackID string `json:"track_id"`
	} `json:"body"`
}

func TestEnvelopeRoundTripAndTimes(t *testing.T) {
	ts := time.Date(2026, 10, 2, 9, 15, 5, 500123456, time.UTC)
	rx := ts.Add(112 * time.Millisecond)
	m := trackMsg{Envelope: NewEnvelope("track/telemetry/v1", "ussp/telemetry-ingest",
		core.Times{TS: &ts, RxTS: rx, CapturedAt: ts, Source: core.TimeSourceClock, Backlog: true})}
	m.Body.TrackID = "t1"
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, want := range []string{`"schema":"track/telemetry/v1"`, `"ts":"2026-10-02T09:15:05.500Z"`, `"rx_ts":"2026-10-02T09:15:05.612Z"`,
		`"time_source":"source_clock"`, `"backlog":true`, `"body":{"track_id":"t1"}`} {
		if !strings.Contains(s, want) {
			t.Errorf("%s lacks %s", s, want)
		}
	}
	var back trackMsg
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	tm := back.Times()
	if tm.TS == nil || !tm.TS.Equal(ts.Truncate(time.Millisecond)) || !tm.RxTS.Equal(rx.Truncate(time.Millisecond)) ||
		tm.Source != core.TimeSourceClock || !tm.Backlog || back.MsgID != m.MsgID || back.Body.TrackID != "t1" {
		t.Fatalf("%+v %+v", back, tm)
	}
	// No source clock: ts is null and stays nil.
	none := NewEnvelope("track/manned/v1", "ussp/monitor", core.Times{RxTS: rx, CapturedAt: rx, Source: core.TimeReceiver})
	data, _ = json.Marshal(&none)
	if !strings.Contains(string(data), `"ts":null`) {
		t.Fatal(string(data))
	}
	var nb Envelope
	if err := json.Unmarshal(data, &nb); err != nil || nb.Times().TS != nil {
		t.Fatalf("%v %+v", err, nb)
	}
	sys := SystemEnvelope("alert/v1", "ussp/monitor", rx)
	if sys.TimeSource != core.TimeSystem || sys.Backlog || sys.TS == nil || sys.Validate() != nil {
		t.Fatalf("%+v", sys)
	}
	if sys.Head() != &sys {
		t.Fatal("Head is not the envelope")
	}
}

func TestEnvelopeValidateRefuses(t *testing.T) {
	good := SystemEnvelope("alert/v1", "ussp/monitor", time.Now())
	for name, mut := range map[string]func(*Envelope){
		"schema":      func(e *Envelope) { e.Schema = "alert" },
		"msg_id":      func(e *Envelope) { e.MsgID = "x" },
		"producer":    func(e *Envelope) { e.Producer = "someone/else" },
		"time_source": func(e *Envelope) { e.TimeSource = "guess" },
		"rx_ts":       func(e *Envelope) { e.RxTS = Stamp{} },
		"captured_at": func(e *Envelope) { e.CapturedAt = Stamp{} },
	} {
		e := good
		mut(&e)
		var fe *core.FieldError
		if err := e.Validate(); !errors.As(err, &fe) || fe.Field != name {
			t.Errorf("%s: %v", name, err)
		}
	}
	var s Stamp
	for _, bad := range []string{`1`, `"yesterday"`, `"2026-10-02"`} {
		if err := json.Unmarshal([]byte(bad), &s); err == nil {
			t.Errorf("Stamp accepted %s", bad)
		}
	}
	if err := json.Unmarshal([]byte(`"2026-10-02T13:15:05.5+04:00"`), &s); err != nil || s.Location() != time.UTC || s.Hour() != 9 {
		t.Fatalf("%v %v", s, err)
	}
}

func TestKeys(t *testing.T) {
	for _, k := range []string{"current", "c5.1317.2248", "a/b_c=d-e"} {
		if !ValidKey(k) {
			t.Errorf("%q invalid", k)
		}
	}
	for _, k := range []string{"", "c5:1317:2248", "a b", ".a", "a.", "*", strings.Repeat("a", MaxKeyBytes+1)} {
		if ValidKey(k) {
			t.Errorf("%q valid", k)
		}
	}
	for _, s := range []string{"TEST-SERIAL/1", "GEO-TEST-OP 123", "ü.*>", ""} {
		tok := KeyToken(s)
		if s != "" && !ValidKey(tok) {
			t.Errorf("KeyToken(%q) = %q invalid", s, tok)
		}
		if back, err := KeyFromToken(tok); err != nil || back != s {
			t.Errorf("round trip %q: %q %v", s, back, err)
		}
	}
	if _, err := KeyFromToken("!!"); err == nil {
		t.Error("bad token decoded")
	}
}

func TestSourcesWire(t *testing.T) {
	inst := "ti-1"
	s := coresources.State{Controls: []coresources.Control{{SourceType: "operator_ws", Enabled: false}, {SourceType: "adsb_rx", InstanceID: &inst, Enabled: true}},
		Version: 7, Epoch: "e1"}
	data, err := EncodeSources(s)
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeSources(data)
	if err != nil || back.Version != 7 || back.Epoch != "e1" || len(back.Controls) != 2 || *back.Controls[1].InstanceID != "ti-1" || back.Controls[0].Enabled {
		t.Fatalf("%+v %v", back, err)
	}
	for _, bad := range []string{`x`, `{"version":1}`, `{"epoch":"e","controls":[{"enabled":true}]}`} {
		if _, err := DecodeSources([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestTopologyShape(t *testing.T) {
	top := DefaultTopology()
	names := map[string]bool{}
	for _, s := range top.Streams {
		names[s.Name] = true
		if s.MaxMsgSize <= 0 || s.MaxMsgSize > MaxPayloadBytes || s.Duplicates != DuplicateWindow {
			t.Errorf("%s: unbounded or without dedupe window: %+v", s.Name, s)
		}
	}
	for _, n := range []string{StreamTRK, StreamMAN, StreamPEER, StreamALRT, StreamCONF, StreamIDENT, StreamINTENT, StreamCIS, StreamTRAFFIC, StreamINGEST, StreamFLIGHT} {
		if !names[n] {
			t.Errorf("stream %s missing", n)
		}
	}
	ing, _ := top.Stream(StreamINGEST)
	if ing.Retention != jetstream.WorkQueuePolicy || ing.MaxBytes != IngestMaxBytes || ing.Discard != jetstream.DiscardNew || ing.MaxAge != 10*time.Minute {
		t.Errorf("INGEST %+v", ing)
	}
	trk, _ := top.Stream(StreamTRK)
	if trk.MaxAge != time.Hour || trk.Subjects[0] != SubjectTrkAll {
		t.Errorf("TRK %+v", trk)
	}
	for _, b := range top.Buckets {
		if b.History != 1 || b.MaxValueSize <= 0 {
			t.Errorf("%s: %+v", b.Bucket, b)
		}
		if (b.TTL != 0) != (b.Bucket == BucketRegistryValidity || b.Bucket == BucketTelemetrySeen || b.Bucket == BucketISANotifications ||
			b.Bucket == BucketConformanceState) {
			t.Errorf("%s TTL %v: only registry_validity, telemetry_seen, rid_isa_notifications and conformance_state have one", b.Bucket, b.TTL)
		}
	}
	if _, ok := top.Bucket(BucketIntentActive); !ok {
		t.Error("intent_active missing")
	}
	if _, ok := top.Stream("NOPE"); ok {
		t.Error("unknown stream found")
	}
	if _, ok := top.Bucket("nope"); ok {
		t.Error("unknown bucket found")
	}
}

func TestDrift(t *testing.T) {
	want, _ := DefaultTopology().Stream(StreamTRK)
	if d := StreamDrift(want, want); len(d) != 0 {
		t.Fatal(d)
	}
	have := want
	have.MaxAge, have.Subjects, have.Storage, have.Discard, have.Retention, have.MaxBytes, have.MaxMsgSize, have.Duplicates =
		time.Minute, []string{"x"}, jetstream.MemoryStorage, jetstream.DiscardNew, jetstream.InterestPolicy, 5, 5, time.Second
	if d := StreamDrift(have, want); strings.Join(d, ",") != "subjects,retention,max_age,max_bytes,max_msg_size,storage,discard,duplicate_window" {
		t.Fatal(d)
	}
	b, _ := DefaultTopology().Bucket(BucketRegistryValidity)
	kv := jetstream.StreamConfig{MaxMsgsPerSubject: 1, MaxAge: b.TTL, MaxMsgSize: b.MaxValueSize, Storage: b.Storage}
	if d := bucketDrift(kv, b); len(d) != 0 {
		t.Fatal(d)
	}
	kv = jetstream.StreamConfig{MaxMsgsPerSubject: 5, MaxAge: 0, MaxMsgSize: -1, Storage: jetstream.MemoryStorage}
	if d := bucketDrift(kv, b); strings.Join(d, ",") != "history,ttl,max_value_size,storage" {
		t.Fatal(d)
	}
}

// fakeCore records core publishes.
type fakeCore struct {
	msgs []*nats.Msg
	err  error
}

func (f *fakeCore) PublishMsg(m *nats.Msg) error {
	if f.err != nil {
		return f.err
	}
	f.msgs = append(f.msgs, m)
	return nil
}

// fakeJS records JetStream publishes; every other method is the nil
// interface's (unused here).
type fakeJS struct {
	jetstream.JetStream
	msgs []*nats.Msg
	err  error
}

func (f *fakeJS) PublishMsg(ctx context.Context, m *nats.Msg, _ ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("unbounded publish")
	}
	if f.err != nil {
		return nil, f.err
	}
	f.msgs = append(f.msgs, m)
	return &jetstream.PubAck{}, nil
}

func TestPublisherRoutes(t *testing.T) {
	c, js := &fakeCore{}, &fakeJS{}
	p := &Publisher{Core: c, JS: js, Counters: &core.Counters{}}
	m := trackMsg{Envelope: SystemEnvelope("track/telemetry/v1", "ussp/telemetry-ingest", time.Now())}
	trk, _ := Trk(tbs, "t1")
	if err := p.Publish(context.Background(), trk, &m); err != nil {
		t.Fatal(err)
	}
	conf, _ := Conf("f1")
	if err := p.Publish(context.Background(), conf, &m); err != nil {
		t.Fatal(err)
	}
	if len(c.msgs) != 1 || c.msgs[0].Subject != trk || c.msgs[0].Header.Get(jetstream.MsgIDHeader) != "" {
		t.Fatalf("core: %+v", c.msgs)
	}
	if len(js.msgs) != 1 || js.msgs[0].Header.Get(jetstream.MsgIDHeader) != m.MsgID {
		t.Fatalf("jetstream: %+v", js.msgs)
	}
	if p.Counters.Get("published_trk") != 1 || p.Counters.Get("published_conf") != 1 {
		t.Fatal(p.Counters.Snapshot())
	}
	tp, _ := TrafficProduct("c1")
	if err := p.Publish(context.Background(), tp, &m); err != nil || p.Counters.Get("published_traffic_product") != 1 {
		t.Fatal(err, p.Counters.Snapshot())
	}
	if err := p.PublishControl(CtlSources, Version{Version: 3, Epoch: "e"}); err != nil || len(c.msgs) != 2 || string(c.msgs[1].Data) != `{"version":3,"epoch":"e"}` {
		t.Fatal(err)
	}
}

func TestPublisherRefusesAndCounts(t *testing.T) {
	c, js := &fakeCore{}, &fakeJS{}
	p := &Publisher{Core: c, JS: js, Counters: &core.Counters{}}
	good := trackMsg{Envelope: SystemEnvelope("track/telemetry/v1", "ussp/telemetry-ingest", time.Now())}
	trk, _ := Trk(tbs, "t1")
	if err := p.Publish(context.Background(), "trk.v1.bad", &good); err == nil || p.Counters.Get("publish_failed_invalid_subject") != 1 {
		t.Fatal(err)
	}
	bad := good
	bad.MsgID = "nope"
	if err := p.Publish(context.Background(), trk, &bad); err == nil || p.Counters.Get("publish_failed_trk") != 1 {
		t.Fatal(err)
	}
	big := good
	big.Body.TrackID = strings.Repeat("x", MaxPayloadBytes)
	if err := p.Publish(context.Background(), trk, &big); err == nil || p.Counters.Get("publish_failed_trk") != 2 {
		t.Fatal(err)
	}
	c.err = errors.New("nats: connection closed")
	if err := p.Publish(context.Background(), trk, &good); err == nil || p.Counters.Get("publish_failed_trk") != 3 {
		t.Fatal(err)
	}
	js.err = errors.New("nats: timeout")
	conf, _ := Conf("f1")
	if err := p.Publish(context.Background(), conf, &good); err == nil || p.Counters.Get("publish_failed_conf") != 1 {
		t.Fatal(err)
	}
	if err := p.PublishControl(trk, 1); err == nil {
		t.Fatal("a track subject as control")
	}
	if err := p.PublishControl(CtlPolicy, 1); err == nil || p.Counters.Get("publish_failed_ctl") != 1 {
		t.Fatal(err)
	}
	if len(c.msgs)+len(js.msgs) != 0 {
		t.Fatal("something was sent")
	}
}

func TestPullConsumerRefusesUnbounded(t *testing.T) {
	if _, _, err := PullConsumer(context.Background(), nil, DefaultTopology(), StreamTRK, PullSpec{Durable: "x"}); err == nil {
		t.Fatal("unbounded consumer accepted")
	}
	if _, _, err := PullConsumer(context.Background(), nil, DefaultTopology(), "NOPE", PullSpec{Durable: "x", MaxAckPending: 1}); err == nil {
		t.Fatal("unknown stream accepted")
	}
}

func TestHolesOf(t *testing.T) {
	got := holesOf([]Jump{{After: 5, Before: 20}, {After: 30, Before: 40}, {After: 50, Before: 52}}, 15, []uint64{33, 35, 51, 60})
	want := []Hole{{FromSeq: 6, ToSeq: 14, Count: 9}, {FromSeq: 33, ToSeq: 35, Count: 2}, {FromSeq: 51, ToSeq: 51, Count: 1}}
	if !slices.Equal(got, want) {
		t.Fatalf("%+v", got)
	}
	// Nothing removed: a jump over messages still in the stream is no
	// hole (the twin).
	if got := holesOf([]Jump{{After: 5, Before: 9}}, 1, nil); got[0] != (Hole{}) {
		t.Fatalf("%+v", got)
	}
}

// E-10: CONF is bounded in time and in bytes (the 30-day record of
// conformance is TimescaleDB's conformance_samples, not the stream),
// and both bounds are configurable; an option left zero keeps the
// default.
func TestConfStreamCapped(t *testing.T) {
	conf, _ := DefaultTopology().Stream(StreamCONF)
	if conf.MaxAge != DefaultConfMaxAge || conf.MaxBytes != DefaultConfMaxBytes || conf.Discard != jetstream.DiscardOld {
		t.Fatalf("CONF %v %d %v", conf.MaxAge, conf.MaxBytes, conf.Discard)
	}
	if DefaultConfMaxAge != 48*time.Hour || DefaultConfMaxBytes != 4<<30 {
		t.Fatalf("defaults %v %d", DefaultConfMaxAge, DefaultConfMaxBytes)
	}
	conf, _ = TopologyWith(TopologyOptions{ConfMaxAge: 6 * time.Hour, ConfMaxBytes: 1 << 30}).Stream(StreamCONF)
	if conf.MaxAge != 6*time.Hour || conf.MaxBytes != 1<<30 {
		t.Fatalf("CONF configured %v %d", conf.MaxAge, conf.MaxBytes)
	}
	conf, _ = TopologyWith(TopologyOptions{}).Stream(StreamCONF)
	if conf.MaxAge != DefaultConfMaxAge || conf.MaxBytes != DefaultConfMaxBytes {
		t.Fatalf("CONF zero options %v %d", conf.MaxAge, conf.MaxBytes)
	}
}
