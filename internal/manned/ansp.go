package manned

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/geodesy"
	coresources "github.com/rootxkit/uspace-core/sources"
	"github.com/rootxkit/uspace-core/timeplace"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/sources"
)

// ScopeANSPTraffic is the scope of a token for the ANSP's stream (M23).
const ScopeANSPTraffic = "ansp.traffic"

// Schemas of the ANSP's stream besides track/manned/v1 (M12, M29).
const (
	SchemaConsoleStatus   = "console/status/v1"
	SchemaConsoleSnapshot = "console/snapshot/v1"
)

// Bounds and periods of the stream reader (E-10).
const (
	// ANSPFrameBytes bounds one frame read from the ANSP: a
	// console/snapshot/v1 carries every relevant aircraft.
	ANSPFrameBytes = 1 << 20
	// ANSPSnapshotBytes bounds the GET /v1/manned-traffic/snapshot body.
	ANSPSnapshotBytes = 8 << 20
	// MaxANSPAdapters bounds the ANSP adapters whose state is kept.
	MaxANSPAdapters = 64
	// MaxSnapshotTracks bounds the aircraft taken from one snapshot.
	MaxSnapshotTracks = 10_000
	// DefaultStatusEvery is the period of src.v1 (spec 04 §3.6: 2 s).
	DefaultStatusEvery = 2 * time.Second
	// DefaultGateEvery is how often a session checks the switches and
	// the bbox: a type switched off closes the stream within it.
	DefaultGateEvery = 250 * time.Millisecond
	ansPRetryMin     = time.Second
	ansPRetryMax     = 30 * time.Second
	ansPDialTimeout  = 10 * time.Second
	// MaxDetailBytes bounds an error or detail kept for a person.
	MaxDetailBytes = 200
)

// Counters of the stream reader.
const (
	CounterANSPConnects        = "ansp_feed_connects"
	CounterANSPDisconnects     = "ansp_feed_disconnects"
	CounterANSPFrames          = "ansp_feed_frames"
	CounterANSPTracks          = "ansp_feed_tracks"
	CounterANSPTrackRefused    = "ansp_feed_track_refused"
	CounterANSPTooOld          = "ansp_feed_track_too_old"
	CounterANSPAtReceipt       = "ansp_feed_track_placed_at_receipt"
	CounterANSPInstanceOff     = "ansp_feed_track_instance_disabled"
	CounterANSPStatusFrames    = "ansp_feed_status_frames"
	CounterANSPSnapshotFrames  = "ansp_feed_snapshot_frames"
	CounterANSPUnknownSchema   = "ansp_feed_unknown_schema"
	CounterANSPUnreadable      = "ansp_feed_unreadable"
	CounterANSPSnapshotFailed  = "ansp_feed_snapshot_failed"
	CounterANSPSnapshotTaken   = "ansp_feed_snapshot_taken"
	CounterANSPSwitchedOff     = "ansp_feed_closed_switched_off"
	CounterANSPAdaptersOver    = "ansp_feed_adapters_over_bound"
	CounterANSPStatusFailed    = "ansp_feed_status_publish_failed"
	CounterANSPStatusPublished = "ansp_feed_status_published"
)

// TokenSource hands out an ecosystem token whose audience is the host of
// baseURL (auth.Outgoing; M18).
type TokenSource interface {
	Token(ctx context.Context, baseURL string, scopes ...string) (string, error)
}

// adapterState is what the ANSP says of one of its adapters.
type adapterState struct {
	state, disabledBy string
	since             time.Time
}

// ANSPStream reads the ANSP's manned-traffic stream (spec 02 F4) and
// publishes its aircraft on man.v1; see the package documentation.
type ANSPStream struct {
	// URL is USSP_ANSP_STREAM_URL (ws or wss, .../v1/manned-traffic/stream).
	URL    string
	Tokens TokenSource
	HTTP   *http.Client
	Sink   Sink
	Own    Own
	Gate   Gate
	Policy func() policy.Values
	// BBox is the box asked of the ANSP (the U-space airspaces padded by
	// manned_margin_m, or USSP_TRAFFIC_INPUT_BBOX); false while it is not
	// known: the ANSP's own relevance filter then applies.
	BBox     func() (geodesy.BBox, bool)
	Counters *core.Counters
	Logger   *slog.Logger
	Now      func() time.Time
	// MTLSOff makes every status period log, at error level, that the
	// stream is read without a client certificate (M25); MTLSOffEvery is
	// that period (the process's status interval).
	MTLSOff      bool
	MTLSOffEvery time.Duration
	// RetryMin (1 s), StatusEvery (2 s) and GateEvery (250 ms) are what a
	// test shortens.
	RetryMin    time.Duration
	StatusEvery time.Duration
	GateEvery   time.Duration

	mu        sync.Mutex
	started   time.Time
	connected bool
	connSince time.Time
	lastFrame time.Time
	lastTrack time.Time
	lastErr   string
	adapters  map[string]adapterState
	degraded  []string
	accepted  uint64
	refused   uint64
	echoes    uint64
	agg       string
	aggSince  time.Time
	offSince  time.Time
	once      sync.Once
}

func (f *ANSPStream) init() {
	f.once.Do(func() {
		if f.Counters == nil {
			f.Counters = &core.Counters{}
		}
		f.mu.Lock()
		f.started = f.now()
		f.mu.Unlock()
	})
}

func (f *ANSPStream) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func (f *ANSPStream) logger() *slog.Logger {
	if f.Logger == nil {
		return obs.Discard()
	}
	return f.Logger
}

func (f *ANSPStream) policy() policy.Values {
	if f.Policy == nil {
		return policy.Defaults()
	}
	return f.Policy()
}

func (f *ANSPStream) silence() time.Duration {
	return time.Duration(f.policy().MannedUnavailableS * float64(time.Second))
}

func every(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// decision is the switch of the type (instance nil) or of one adapter.
func (f *ANSPStream) decision(instance *string) coresources.Decision {
	if f.Gate == nil {
		return coresources.Decision{Enabled: true}
	}
	return f.Gate.Query(SourceANSPFeed, instance)
}

// baseURL is the stream URL's origin (the ANSP's published base URL,
// whose host is the token's audience).
func (f *ANSPStream) baseURL() string {
	u, err := url.Parse(f.URL)
	if err != nil {
		return f.URL
	}
	scheme := "https"
	if u.Scheme == "ws" || u.Scheme == "http" {
		scheme = "http"
	}
	return scheme + "://" + u.Host
}

// bboxQuery is the bbox parameter (west,south,east,north) and whether
// there is one.
func (f *ANSPStream) bboxQuery() (string, bool) {
	if f.BBox == nil {
		return "", false
	}
	b, ok := f.BBox()
	if !ok {
		return "", false
	}
	n := func(v float64) string { return strconv.FormatFloat(v, 'f', 6, 64) }
	return n(b.MinLon) + "," + n(b.MinLat) + "," + n(b.MaxLon) + "," + n(b.MaxLat), true
}

// withBBox is u with the bbox parameter set (or removed).
func withBBox(u, bbox string, ok bool) string {
	p, err := url.Parse(u)
	if err != nil {
		return u
	}
	q := p.Query()
	q.Del("bbox")
	if ok {
		q.Set("bbox", bbox)
	}
	p.RawQuery = q.Encode()
	return p.String()
}

// SnapshotURL is the snapshot of the stream: the same base, its path's
// last element snapshot instead of stream, over http(s); "" when the
// stream URL does not end in /stream.
func SnapshotURL(stream string) string {
	u, err := url.Parse(stream)
	if err != nil || !strings.HasSuffix(u.Path, "/stream") {
		return ""
	}
	switch u.Scheme {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	}
	u.Path = strings.TrimSuffix(u.Path, "/stream") + "/snapshot"
	return u.String()
}

func clip(s string) string {
	if len(s) > MaxDetailBytes {
		s = s[:MaxDetailBytes]
		for len(s) > 0 && !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return s
}

func clipErr(err error) string {
	if err == nil {
		return "closed"
	}
	return clip(err.Error())
}

func (f *ANSPStream) setConnected(connected bool, why string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.connected != connected || f.connSince.IsZero() {
		f.connected, f.connSince = connected, f.now()
		if connected {
			// A new connection forgets what the previous one said of the
			// ANSP's adapters: its first console/status/v1 says it again.
			f.adapters, f.degraded = nil, nil
		}
	}
	f.lastErr = why
}

// Run reads the stream until ctx ends, reconnecting with backoff, and
// publishes the feed's status every StatusEvery. While the type is
// switched off nothing is dialled.
func (f *ANSPStream) Run(ctx context.Context) {
	f.init()
	f.setConnected(false, "not connected yet")
	go f.statusLoop(ctx)
	retryMin := every(f.RetryMin, ansPRetryMin)
	retry := retryMin
	for ctx.Err() == nil {
		if !f.decision(nil).Enabled {
			f.setConnected(false, "switched off")
			if !sleep(ctx, every(f.GateEvery, DefaultGateEvery)) {
				return
			}
			retry = retryMin
			continue
		}
		connected, err := f.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if connected {
			retry = retryMin
			f.Counters.Inc(CounterANSPDisconnects)
		}
		f.setConnected(false, clipErr(err))
		if errors.Is(err, errSwitchedOff) || errors.Is(err, errBBoxChanged) {
			continue
		}
		f.logger().LogAttrs(ctx, slog.LevelWarn, "ANSP manned-traffic stream lost; reconnecting", slog.Duration("in", retry), obs.Err(err))
		if !sleep(ctx, retry) {
			return
		}
		retry = min(retry*2, ansPRetryMax)
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

var (
	errSwitchedOff = errors.New("ansp_feed switched off: the stream is closed")
	errBBoxChanged = errors.New("the bbox changed: reconnecting with the new one")
)

// session is one connection: the snapshot, then the stream's frames
// until it ends, the type is switched off or the bbox changes;
// connected is whether the dial succeeded.
func (f *ANSPStream) session(ctx context.Context) (connected bool, err error) {
	dctx, cancel := context.WithTimeout(ctx, ansPDialTimeout)
	defer cancel()
	tok, err := f.Tokens.Token(dctx, f.baseURL(), ScopeANSPTraffic)
	if err != nil {
		return false, fmt.Errorf("no token for the ANSP: %w", err)
	}
	bbox, hasBBox := f.bboxQuery()
	conn, resp, err := websocket.Dial(dctx, withBBox(f.URL, bbox, hasBBox), &websocket.DialOptions{HTTPClient: f.HTTP,
		HTTPHeader: http.Header{"Authorization": {"Bearer " + tok}}})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return false, fmt.Errorf("dial: %w", err)
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(ANSPFrameBytes)
	f.Counters.Inc(CounterANSPConnects)
	// The connection is no frame: lastFrame stays the last one the ANSP
	// sent (Take), so the feed is unavailable since then until one comes.
	f.setConnected(true, "")
	f.logger().LogAttrs(ctx, slog.LevelInfo, "ANSP manned-traffic stream connected", slog.String("bbox", bbox))
	f.snapshot(ctx, tok, bbox, hasBBox)

	sctx, scancel := context.WithCancelCause(ctx)
	defer scancel(nil)
	go func() {
		t := time.NewTicker(every(f.GateEvery, DefaultGateEvery))
		defer t.Stop()
		for {
			select {
			case <-sctx.Done():
				return
			case <-t.C:
			}
			if !f.decision(nil).Enabled {
				f.Counters.Inc(CounterANSPSwitchedOff)
				scancel(errSwitchedOff)
				return
			}
			if b, ok := f.bboxQuery(); ok != hasBBox || b != bbox {
				scancel(errBBoxChanged)
				return
			}
		}
	}()
	for {
		rctx, rcancel := context.WithTimeout(sctx, f.silence())
		_, data, err := conn.Read(rctx)
		rcancel()
		if err != nil {
			if cause := context.Cause(sctx); cause != nil && ctx.Err() == nil && !errors.Is(cause, context.Canceled) {
				_ = conn.Close(websocket.StatusNormalClosure, "")
				return true, cause
			}
			if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				return true, fmt.Errorf("no frame for %s", f.silence())
			}
			return true, err
		}
		f.Take(ctx, data)
	}
}

// snapshot bootstraps from GET /v1/manned-traffic/snapshot: every
// aircraft it lists is taken as a frame of the stream would be. A failed
// snapshot is counted and logged; the stream's own console/snapshot/v1
// follows on connect anyway.
func (f *ANSPStream) snapshot(ctx context.Context, tok, bbox string, hasBBox bool) {
	u := SnapshotURL(f.URL)
	if u == "" {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, ansPDialTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, withBBox(u, bbox, hasBBox), nil)
	if err != nil {
		f.Counters.Inc(CounterANSPSnapshotFailed)
		return
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	hc := f.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	res, err := hc.Do(req)
	if err != nil {
		f.Counters.Inc(CounterANSPSnapshotFailed)
		f.logger().LogAttrs(ctx, slog.LevelWarn, "ANSP manned-traffic snapshot not read; the stream's snapshot follows", obs.Err(err))
		return
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(res.Body, ANSPSnapshotBytes+1))
	if err != nil || res.StatusCode != http.StatusOK || len(body) > ANSPSnapshotBytes {
		f.Counters.Inc(CounterANSPSnapshotFailed)
		f.logger().LogAttrs(ctx, slog.LevelWarn, "ANSP manned-traffic snapshot refused or too large; the stream's snapshot follows",
			slog.Int("status", res.StatusCode))
		return
	}
	if !f.takeSnapshotBody(ctx, body, f.now()) {
		f.Counters.Inc(CounterANSPSnapshotFailed)
		return
	}
	f.Counters.Inc(CounterANSPSnapshotTaken)
}

// snapshotBody is the part of console/snapshot/v1 (and of the ANSP's
// MannedSnapshot) read here.
type snapshotBody struct {
	Manned   []json.RawMessage `json:"manned"`
	Degraded []string          `json:"degraded"`
	Adapters []adapterWire     `json:"adapters"`
}

func (f *ANSPStream) takeSnapshotBody(ctx context.Context, body []byte, rx time.Time) bool {
	var s snapshotBody
	if err := json.Unmarshal(body, &s); err != nil || s.Manned == nil {
		return false
	}
	if len(s.Manned) > MaxSnapshotTracks {
		f.Counters.Add(CounterANSPTrackRefused, uint64(len(s.Manned)-MaxSnapshotTracks))
		s.Manned = s.Manned[:MaxSnapshotTracks]
	}
	for _, m := range s.Manned {
		f.takeTrack(ctx, m, rx)
	}
	if s.Adapters != nil {
		f.takeAdapters(nil, s.Adapters, s.Degraded)
	}
	return true
}

// Take handles one frame of the stream: dispatched on its schema (M29);
// an unknown schema is counted and skipped, a frame that does not read
// is counted. Every frame, whatever it holds, shows the stream alive.
func (f *ANSPStream) Take(ctx context.Context, data []byte) {
	f.init()
	rx := f.now()
	var fr struct {
		Schema string          `json:"schema"`
		Body   json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(data, &fr); err != nil || fr.Schema == "" {
		f.Counters.Inc(CounterANSPUnreadable)
		return
	}
	f.Counters.Inc(CounterANSPFrames)
	f.mu.Lock()
	f.lastFrame = rx
	f.mu.Unlock()
	switch fr.Schema {
	case SchemaManned:
		f.takeTrack(ctx, data, rx)
	case SchemaConsoleStatus:
		f.Counters.Inc(CounterANSPStatusFrames)
		var b struct {
			Degraded []string      `json:"degraded"`
			Sources  []sourceWire  `json:"sources"`
			Adapters []adapterWire `json:"adapters"`
		}
		if json.Unmarshal(fr.Body, &b) != nil {
			f.Counters.Inc(CounterANSPUnreadable)
			return
		}
		f.takeAdapters(b.Sources, b.Adapters, b.Degraded)
	case SchemaConsoleSnapshot:
		f.Counters.Inc(CounterANSPSnapshotFrames)
		if !f.takeSnapshotBody(ctx, fr.Body, rx) {
			f.Counters.Inc(CounterANSPUnreadable)
		}
	default:
		f.Counters.Inc(CounterANSPUnknownSchema)
	}
}

// sourceWire is an item of console/status/v1 sources[] (source/status/v1
// bodies); adapterWire one of the ANSP's adapters[].
type sourceWire struct {
	Source         string     `json:"source"`
	SourceInstance *string    `json:"source_instance"`
	State          string     `json:"state"`
	Since          *time.Time `json:"since"`
	DisabledBy     *string    `json:"disabled_by"`
}

type adapterWire struct {
	ID          string     `json:"id"`
	State       string     `json:"state"`
	LastFrameAt *time.Time `json:"last_frame_at"`
}

var sourceStates = map[string]bool{sources.StateLive: true, sources.StateStale: true, sources.StateDisabled: true,
	sources.StateDown: true, sources.StateUnknown: true}

// takeAdapters keeps what the ANSP says of its adapters: sources[] when
// it lists the ansp_feed adapters (with their since), else adapters[]
// (since = the adapter's last frame). An item that does not read is
// left out.
func (f *ANSPStream) takeAdapters(srcs []sourceWire, ads []adapterWire, degraded []string) {
	got := map[string]adapterState{}
	add := func(id string, st adapterState) {
		if len(got) >= MaxANSPAdapters {
			f.Counters.Inc(CounterANSPAdaptersOver)
			return
		}
		if len(id) > 64 || !slugRe.MatchString(id) || !sourceStates[st.state] {
			return
		}
		got[id] = st
	}
	for _, s := range srcs {
		if s.Source != SourceANSPFeed || s.SourceInstance == nil {
			continue
		}
		st := adapterState{state: s.State}
		if s.Since != nil {
			st.since = s.Since.UTC()
		}
		if s.DisabledBy != nil {
			st.disabledBy = *s.DisabledBy
		}
		add(*s.SourceInstance, st)
	}
	if len(got) == 0 {
		for _, a := range ads {
			st := adapterState{state: a.State}
			if a.LastFrameAt != nil {
				st.since = a.LastFrameAt.UTC()
			}
			add(a.ID, st)
		}
	}
	if len(degraded) > MaxANSPAdapters {
		degraded = degraded[:MaxANSPAdapters]
	}
	for i := range degraded {
		degraded[i] = clip(degraded[i])
	}
	f.mu.Lock()
	f.adapters, f.degraded = got, slices.Clone(degraded)
	f.mu.Unlock()
}

// placementPolicy is the bound of a manned record's own time: uspace-core's
// network defaults with the ingest's ahead tolerance.
func (f *ANSPStream) placementPolicy() timeplace.NetworkPolicy {
	p := timeplace.DefaultNetworkPolicy()
	p.ToleranceS = f.policy().TelemetryAheadToleranceS
	return p
}

// takeTrack handles one track/manned/v1 message: checked, placed by
// PlaceNetwork against its own time (ts, else its captured_at) on our
// clock, and published unless its adapter is switched off here or it is
// an echo of one of this USSP's own flights.
func (f *ANSPStream) takeTrack(ctx context.Context, data []byte, rx time.Time) {
	f.Counters.Inc(CounterANSPTracks)
	refuse := func() {
		f.Counters.Inc(CounterANSPTrackRefused)
		f.mu.Lock()
		f.refused++
		f.mu.Unlock()
	}
	var m struct {
		bus.Envelope
		Body json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(data, &m); err != nil || m.Schema != SchemaManned || m.Validate() != nil {
		refuse()
		return
	}
	var b Body
	if err := json.Unmarshal(m.Body, &b); err != nil {
		refuse()
		return
	}
	if strings.TrimSpace(string(b.Quality)) == "null" {
		b.Quality = nil
	}
	if b.Check() != nil || b.Trust != core.TrustSurveillance || b.Source != SourceANSPFeed {
		refuse()
		return
	}
	if inst := b.SourceInstance; !f.decision(&inst).Enabled {
		f.Counters.Inc(CounterANSPInstanceOff)
		return
	}
	state := m.CapturedAt.Time
	if m.TS != nil {
		state = m.TS.Time
	}
	p, note, shown := timeplace.PlaceNetwork(state, nil, rx, f.placementPolicy())
	if !shown {
		f.Counters.Inc(CounterANSPTooOld)
		return
	}
	if note != timeplace.NoteNone {
		f.Counters.Inc(CounterANSPAtReceipt)
	}
	out := &Track{Envelope: bus.NewEnvelope(SchemaManned, ProducerANSP, timeplace.Times(p, rx, false)), Body: b}
	echo, err := publish(ctx, f.Sink, f.Own, f.Counters, out, nil)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case echo != "":
		f.echoes++
	case err != nil:
		f.refused++
	default:
		f.accepted++
		f.lastTrack = rx
	}
}

// Aggregate is the feed's state as a whole now: disabled while the type
// is switched off here; down (unavailable) since the last frame while
// the stream is cut (not connected) or once manned_unavailable_s passed
// without a frame on an open one, and since the start when none came;
// stale since the ANSP's own time while the ANSP says one of its
// adapters is stale, down or switched off; live otherwise.
type Aggregate struct {
	State      string
	Since      time.Time
	Detail     string
	DisabledBy *string
}

// aggregate computes the Aggregate at now (f.mu held), keeping when each
// state was entered.
func (f *ANSPStream) aggregateLocked(now time.Time, dec coresources.Decision) Aggregate {
	var a Aggregate
	last := f.lastFrame
	switch {
	case !dec.Enabled:
		why := string(coresources.WhyType)
		if dec.WhyDisabled != nil {
			why = string(*dec.WhyDisabled)
		}
		if f.offSince.IsZero() {
			f.offSince = now
		}
		a = Aggregate{State: sources.StateDisabled, Since: f.offSince, DisabledBy: &why,
			Detail: "ansp_feed is switched off (" + why + "): the stream is closed, manned traffic is not shown"}
	case last.IsZero():
		why := f.lastErr
		if f.connected {
			why = "connected, no frame read"
		}
		a = Aggregate{State: sources.StateDown, Since: f.started, Detail: "unavailable: no frame from the ANSP yet (" + why + ")"}
	case !f.connected:
		// Cut: the stream closed and is not open again; unavailable since
		// its last frame at once (a reconnect that succeeds clears it).
		a = Aggregate{State: sources.StateDown, Since: last, Detail: "unavailable: the stream is not connected (" + f.lastErr + ")"}
	case now.Sub(last) > f.silence():
		detail := fmt.Sprintf("unavailable: no frame from the ANSP for %.0f s", now.Sub(last).Seconds())
		if f.lastErr != "" {
			detail += " (" + f.lastErr + ")"
		}
		a = Aggregate{State: sources.StateDown, Since: last, Detail: detail}
	default:
		var worst []string
		var since time.Time
		for _, id := range slices.Sorted(maps.Keys(f.adapters)) {
			st := f.adapters[id]
			if st.state != sources.StateStale && st.state != sources.StateDown && st.state != sources.StateDisabled {
				continue
			}
			worst = append(worst, id+" "+st.state)
			if !st.since.IsZero() && (since.IsZero() || st.since.Before(since)) {
				since = st.since
			}
		}
		if len(worst) > 0 {
			if since.IsZero() {
				since = now
			}
			a = Aggregate{State: sources.StateStale, Since: since, Detail: clip("the ANSP says " + strings.Join(worst, ", "))}
		} else {
			a = Aggregate{State: sources.StateLive, Detail: "connected"}
		}
	}
	if dec.Enabled {
		f.offSince = time.Time{}
	}
	if a.State != f.agg {
		f.agg, f.aggSince = a.State, now
	}
	if a.Since.IsZero() {
		a.Since = f.aggSince
	}
	return a
}

// State is the Aggregate now.
func (f *ANSPStream) State() Aggregate {
	f.init()
	dec := f.decision(nil)
	now := f.now()
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.aggregateLocked(now, dec)
}

// Statuses are the src.v1 bodies of the feed now: the type as a whole
// (source_instance null) and each ANSP adapter with the ANSP's state and
// time, switched off here when its instance is, down with the feed when
// the feed is.
func (f *ANSPStream) Statuses() []sources.StatusBody {
	f.init()
	dec := f.decision(nil)
	now := f.now()
	f.mu.Lock()
	a := f.aggregateLocked(now, dec)
	var age *float64
	if !f.lastTrack.IsZero() {
		v := max(0, now.Sub(f.lastTrack).Seconds())
		age = &v
	}
	counters := map[string]uint64{"accepted": f.accepted, "refused": f.refused, "echo_own_flight": f.echoes}
	adapters := maps.Clone(f.adapters)
	f.mu.Unlock()
	out := make([]sources.StatusBody, 0, 1+len(adapters))
	out = append(out, sources.StatusBody{Source: SourceANSPFeed, State: a.State, Since: bus.Stamp{Time: a.Since.UTC()}, AgeS: age,
		DisabledBy: a.DisabledBy, Counters: counters, Detail: a.Detail})
	for _, id := range slices.Sorted(maps.Keys(adapters)) {
		st := adapters[id]
		inst := id
		b := sources.StatusBody{Source: SourceANSPFeed, SourceInstance: &inst, State: st.state, Since: bus.Stamp{Time: st.since.UTC()},
			Counters: map[string]uint64{}, Detail: "as the ANSP reports its adapter"}
		if st.since.IsZero() {
			b.Since = bus.Stamp{Time: now.UTC()}
		}
		switch {
		case b.Disabled(f.decision(&inst)):
			b.Detail = "switched off here: its tracks are not published"
		case a.State == sources.StateDown || a.State == sources.StateDisabled:
			b.State, b.Since, b.Detail = sources.StateDown, bus.Stamp{Time: a.Since.UTC()}, a.Detail
		case st.state == sources.StateDisabled:
			why := st.disabledBy
			if why == "" {
				why = string(coresources.WhyInstance)
			}
			b.DisabledBy = &why
			b.Detail = "switched off at the ANSP"
		}
		out = append(out, b)
	}
	return out
}

func (f *ANSPStream) statusLoop(ctx context.Context) {
	t := time.NewTicker(every(f.StatusEvery, DefaultStatusEvery))
	defer t.Stop()
	var lastWarn time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := f.now()
		for _, b := range f.Statuses() {
			if f.Sink == nil {
				break
			}
			if err := sources.PublishStatus(ctx, f.Sink, ProducerANSP, now, b); err != nil {
				f.Counters.Inc(CounterANSPStatusFailed)
				continue
			}
			f.Counters.Inc(CounterANSPStatusPublished)
		}
		if f.MTLSOff && (lastWarn.IsZero() || now.Sub(lastWarn) >= every(f.MTLSOffEvery, time.Minute)) {
			lastWarn = now
			f.logger().LogAttrs(ctx, slog.LevelError, "USSP_MTLS_MODE=off: the ANSP's manned-traffic stream is read without a client certificate (the lab and staging only)")
		}
	}
}

// Probe is the readiness of the stream (ansp_feed on /readyz): down
// while manned traffic is unavailable, degraded while it is switched off
// or the ANSP says it is stale, up otherwise; each with since when and
// what was published.
func (f *ANSPStream) Probe(context.Context) (obs.State, string) {
	a := f.State()
	f.mu.Lock()
	tail := fmt.Sprintf("; %d tracks published on man.v1, %d refused, %d echoes of own flights left out", f.accepted, f.refused, f.echoes)
	f.mu.Unlock()
	since := a.Since.UTC().Format(time.RFC3339)
	switch a.State {
	case sources.StateDown:
		return obs.StateDown, "unavailable since " + since + ": " + a.Detail + tail
	case sources.StateDisabled:
		return obs.StateDegraded, "switched off since " + since + ": " + a.Detail + tail
	case sources.StateStale:
		return obs.StateDegraded, "stale since " + since + ": " + a.Detail + tail
	}
	return obs.StateUp, "live since " + since + tail
}
