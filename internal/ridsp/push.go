package ridsp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3411"

	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/ridsp/gen"
	"github.com/rootxkit/uspace-ussp/internal/stdapi/convert"
)

// Schemas and producer of the push.
const (
	SchemaAuthorityFlight = "authority/flight/v1"
	SchemaConsoleStatus   = "console/status/v1"
	Producer              = "ussp/rid-sp"
	// SlugPushGap is the degraded slug of a status frame that follows a
	// gap.
	SlugPushGap = "authority_push_gap"
)

// Bounds and periods of the push (spec 02 F7).
const (
	// PushBuffer is how long frames are kept for a client that is not
	// connected: ten minutes.
	PushBuffer = 10 * time.Minute
	// DefaultPushMaxFrames bounds the buffer in frames (E-10): ten
	// minutes of 200 airborne flights at 1 Hz, about 70 MB.
	DefaultPushMaxFrames = 120_000
	// pushWriteTimeout bounds one frame written to a client.
	pushWriteTimeout = 5 * time.Second
)

// Counters of the push.
const (
	CounterPushFrames     = "rid_sp_push_frames"
	CounterPushSent       = "rid_sp_push_frames_sent"
	CounterPushShed       = "rid_sp_push_frames_shed"
	CounterPushGaps       = "rid_sp_push_gaps"
	CounterPushSessions   = "rid_sp_push_sessions"
	CounterPushUnservable = "rid_sp_push_flight_unservable"
)

// PushAccess is the access entry of WS /v1/authority/flights: an
// upgrade authenticated by an ecosystem token granting
// rid.display_provider (the authority as a Display Provider).
var PushAccess = httpx.Access{WebSocket: true, Scopes: []string{string(f3411.ScopeDisplayProvider)}}

// PushAccessTable is the access table of the push route.
func PushAccessTable() map[string]httpx.Access {
	return map[string]httpx.Access{"GET /v1/authority/flights": PushAccess}
}

// FlightFrame is one authority/flight/v1 frame.
type FlightFrame struct {
	bus.Envelope
	Body f3411.RIDFlight `json:"body"`
}

// PushStatusBody is console/status/v1 as the push sends it, with the
// frames buffered as an extra.
type PushStatusBody struct {
	ConnectionID  string    `json:"connection_id"`
	ServerTS      bus.Stamp `json:"server_ts"`
	PolicyVersion string    `json:"policy_version"`
	StaleAfterS   float64   `json:"stale_after_s"`
	LiveMaxAgeS   float64   `json:"live_max_age_s"`
	DroppedFrames uint64    `json:"dropped_frames"`
	Degraded      []string  `json:"degraded"`
	Sources       []any     `json:"sources"`
	Buffered      int       `json:"buffered"`
}

// PushStatus is one console/status/v1 frame of the push.
type PushStatus struct {
	bus.Envelope
	Body PushStatusBody `json:"body"`
}

type pushFrame struct {
	seq    uint64
	made   time.Time
	flight f3411.RIDFlight
}

// Push is the optional WS /v1/authority/flights (see the package
// documentation): every Every it makes one frame per airborne flight of
// the window, keeps them PushBuffer (at most MaxFrames), and hands them
// to the connected clients; frames made while no client is connected
// go to the next one as backlog. A frame shed before any client had it,
// or before a slow client had it, is a gap: counted, logged and shown
// on that client's next status frame. Safe for concurrent use.
type Push struct {
	Window *Window
	WS     *auth.WSAuth
	Policy func() policy.Record
	// Degraded lists the process's degraded dependencies.
	Degraded func() []string
	// Every (1 s), StatusEvery (2 s), MaxFrames (DefaultPushMaxFrames).
	Every       time.Duration
	StatusEvery time.Duration
	MaxFrames   int
	Now         func() time.Time
	// Ctx ends every connection when the process stops.
	Ctx      context.Context
	Counters *core.Counters
	Logger   *slog.Logger

	mu        sync.Mutex
	frames    []pushFrame
	next      uint64
	delivered uint64
	lost      uint64 // shed while no client was connected, not yet reported
	conns     int
	wake      chan struct{}
	// last is the captured_at of each flight's newest frame: a flight
	// whose state did not change since has no new frame (its silence
	// shows as the frames' age, not as repeats).
	last map[string]time.Time
}

var _ gen.ServerInterface = (*Push)(nil)

func (p *Push) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Push) count(name string, n uint64) {
	if p.Counters != nil && n > 0 {
		p.Counters.Add(name, n)
	}
}

func (p *Push) logger() *slog.Logger {
	if p.Logger == nil {
		return obs.Discard()
	}
	return p.Logger
}

func (p *Push) every() time.Duration {
	if p.Every > 0 {
		return p.Every
	}
	return time.Second
}

func (p *Push) maxFrames() int {
	if p.MaxFrames > 0 {
		return p.MaxFrames
	}
	return DefaultPushMaxFrames
}

// wakeLocked returns the channel closed at the next frame.
func (p *Push) wakeLocked() chan struct{} {
	if p.wake == nil {
		p.wake = make(chan struct{})
	}
	return p.wake
}

// Tick makes the frames of now: one per airborne flight of the window
// (core's airborne rule: only Ground is not) with a state newer than its
// last frame, each checked by core before it is kept; it returns how
// many it made.
func (p *Push) Tick() int {
	now := p.now()
	var made []pushFrame
	all := p.Window.All()
	p.mu.Lock()
	if p.last == nil {
		p.last = map[string]time.Time{}
	}
	seen := make(map[string]time.Time, len(all))
	for _, v := range all {
		seen[v.ID] = p.last[v.ID]
	}
	p.last = seen // flights that left the window are forgotten (E-10)
	p.mu.Unlock()
	for _, v := range all {
		if !v.Current.CapturedAt.After(seen[v.ID]) {
			continue
		}
		f := FlightOf(View{ID: v.ID, Current: v.Current})
		if !f.CurrentState.Airborne() {
			continue
		}
		if _, err := convert.RIDFlightToWire(&f); err != nil {
			p.count(CounterPushUnservable, 1)
			continue
		}
		made = append(made, pushFrame{made: now, flight: f})
		seen[v.ID] = v.Current.CapturedAt
	}
	p.mu.Lock()
	for i := range made {
		p.next++
		made[i].seq = p.next
	}
	p.frames = append(p.frames, made...)
	shed := 0
	for shed < len(p.frames) && (len(p.frames)-shed > p.maxFrames() || now.Sub(p.frames[shed].made) > PushBuffer) {
		if p.conns == 0 && p.frames[shed].seq > p.delivered {
			p.lost++
		}
		shed++
	}
	p.frames = slices.Delete(p.frames, 0, shed)
	if p.wake != nil {
		close(p.wake)
		p.wake = nil
	}
	p.mu.Unlock()
	p.count(CounterPushFrames, uint64(len(made)))
	p.count(CounterPushShed, uint64(shed))
	return len(made)
}

// Buffered is the frames held.
func (p *Push) Buffered() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.frames)
}

// Run makes frames every Every until ctx ends.
func (p *Push) Run(ctx context.Context) {
	t := time.NewTicker(p.every())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.Tick()
		}
	}
}

func connectionID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "push-" + hex.EncodeToString(b[:])
}

// session is one client's cursor.
type session struct {
	id string
	// since is when the client connected: a frame made before it is
	// history to it (backlog).
	since   time.Time
	cursor  uint64
	dropped uint64
	gap     uint64 // frames lost since the last status frame
}

// OpenAuthorityFlights implements gen.ServerInterface: WS
// /v1/authority/flights.
func (p *Push) OpenAuthorityFlights(w http.ResponseWriter, r *http.Request) {
	conn, _, err := p.WS.AcceptWS(w, r, PushAccess)
	if err != nil {
		return
	}
	p.count(CounterPushSessions, 1)
	ctx := conn.CloseRead(r.Context())
	if p.Ctx != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		stop := context.AfterFunc(p.Ctx, cancel)
		defer stop()
	}
	s := &session{id: connectionID(), since: p.now()}
	p.mu.Lock()
	// What was shed before this client came is lost (reported once, to
	// it); it starts at the oldest frame not yet delivered.
	s.cursor = p.delivered + 1
	if len(p.frames) > 0 {
		s.cursor = max(s.cursor, p.frames[0].seq)
	}
	s.gap, p.lost = p.lost, 0
	p.conns++
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.conns--; p.mu.Unlock() }()
	if err := p.serve(ctx, conn, s); err != nil {
		_ = conn.Close(websocket.StatusInternalError, "write failed")
		return
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
}

// nextBatch is every frame from the session's cursor on, and the
// channel closed at the next frame. Frames shed before the session had
// them are its gap, and its cursor moves to the oldest frame held.
func (p *Push) nextBatch(s *session) ([]pushFrame, chan struct{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	wake := p.wakeLocked()
	if len(p.frames) == 0 {
		return nil, wake
	}
	if first := p.frames[0].seq; s.cursor < first {
		s.gap += first - s.cursor
		s.cursor = first
	}
	i, _ := slices.BinarySearchFunc(p.frames, s.cursor, func(f pushFrame, seq uint64) int {
		switch {
		case f.seq < seq:
			return -1
		case f.seq > seq:
			return 1
		}
		return 0
	})
	return slices.Clone(p.frames[i:]), wake
}

// serve sends status frames every StatusEvery and every frame from the
// session's cursor on until ctx ends; an error is a failed write.
func (p *Push) serve(ctx context.Context, conn *websocket.Conn, s *session) error {
	every := p.StatusEvery
	if every <= 0 {
		every = 2 * time.Second
	}
	if err := p.sendStatus(ctx, conn, s); err != nil {
		return err
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		batch, wake := p.nextBatch(s)
		if s.gap > 0 {
			if err := p.sendStatus(ctx, conn, s); err != nil {
				return err
			}
		}
		for _, f := range batch {
			if err := p.sendFrame(ctx, conn, f, f.made.Before(s.since)); err != nil {
				return err
			}
			s.cursor = f.seq + 1
			p.mu.Lock()
			p.delivered = max(p.delivered, f.seq)
			p.mu.Unlock()
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := p.sendStatus(ctx, conn, s); err != nil {
				return err
			}
		case <-wake:
		}
	}
}

func (p *Push) write(ctx context.Context, conn *websocket.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, pushWriteTimeout)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, data)
}

func (p *Push) sendFrame(ctx context.Context, conn *websocket.Conn, f pushFrame, backlog bool) error {
	env := bus.SystemEnvelope(SchemaAuthorityFlight, Producer, f.made)
	env.Backlog = backlog
	if err := p.write(ctx, conn, &FlightFrame{Envelope: env, Body: f.flight}); err != nil {
		return err
	}
	p.count(CounterPushSent, 1)
	return nil
}

// sendStatus sends the status frame; a gap since the last one is in
// dropped_frames and the degraded slug authority_push_gap, logged and
// counted.
func (p *Push) sendStatus(ctx context.Context, conn *websocket.Conn, s *session) error {
	now := p.now()
	pol := policy.Record{Values: policy.Defaults()}
	if p.Policy != nil {
		pol = p.Policy()
	}
	degraded := []string{}
	if p.Degraded != nil {
		degraded = append(degraded, p.Degraded()...)
	}
	if s.gap > 0 {
		p.count(CounterPushGaps, 1)
		p.logger().LogAttrs(ctx, slog.LevelWarn, "authority push gap: frames shed before they were sent",
			slog.String("connection_id", s.id), slog.Uint64("frames", s.gap))
		s.dropped += s.gap
		s.gap = 0
		degraded = append(degraded, SlugPushGap)
	}
	slices.Sort(degraded)
	degraded = slices.Compact(degraded)
	body := PushStatusBody{
		ConnectionID: s.id, ServerTS: bus.Stamp{Time: now.UTC()}, PolicyVersion: strconv.FormatInt(pol.Version, 10),
		StaleAfterS: pol.Values.TelemetryLostS, LiveMaxAgeS: pol.Values.BacklogAfterS,
		DroppedFrames: s.dropped, Degraded: degraded, Sources: []any{}, Buffered: p.Buffered(),
	}
	return p.write(ctx, conn, &PushStatus{Envelope: bus.SystemEnvelope(SchemaConsoleStatus, Producer, now), Body: body})
}

// RegisterPush serves WS /v1/authority/flights on mux behind guard; the
// error lists a route without an entry or an entry without a route.
func RegisterPush(mux *http.ServeMux, p *Push, guard httpx.Guard) error {
	g := httpx.NewGuardedMux(mux, PushAccessTable(), guard, auth.ValidateAccess)
	gen.HandlerWithOptions(p, gen.StdHTTPServerOptions{BaseRouter: g})
	return g.Err()
}
