package trafficws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/cis"
	"github.com/rootxkit/uspace-ussp/internal/geo"
	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/traffic"
	"github.com/rootxkit/uspace-ussp/internal/traffic/gen"
)

// Schemas of the console frame (M29).
const (
	SchemaStatus    = "console/status/v1"
	SchemaSnapshot  = "console/snapshot/v1"
	SchemaSubscribe = "console/subscribe/v1"
)

// Periods and bounds of a connection.
const (
	ProductEvery = time.Second
	StatusEvery  = 2 * time.Second
	// SendQueueLen bounds the frames waiting for one connection; a full
	// queue drops the frame, counted in dropped_frames (05 §5).
	SendQueueLen = 64
	// MaxClientFrameBytes bounds a frame from the client.
	MaxClientFrameBytes = 8 << 10
	writeTimeout        = 5 * time.Second
)

// Counters of the streams.
const (
	CounterConnections      = "traffic_ws_connections"
	CounterFramesSent       = "traffic_ws_frames_sent"
	CounterFramesDropped    = "traffic_ws_frames_dropped"
	CounterClearsResent     = "traffic_ws_clears_resent"
	CounterResync           = "traffic_ws_resync"
	CounterTracksHeldBack   = "traffic_ws_tracks_held_back"
	CounterSubscribes       = "traffic_ws_subscribes"
	CounterSubscribeRefused = "traffic_ws_subscribe_refused"
	CounterAlertsSent       = "traffic_ws_alerts_sent"
	CounterAlertsRepeated   = "traffic_ws_alerts_repeated"
	CounterExpired          = "traffic_ws_credential_expired"
	CounterGeoChanges       = "traffic_ws_geo_changes_sent"
)

// The access of the operations (06 §3): an operator machine token of
// this USSP granting ussp.traffic, or a console session (staff) for the
// traffic picture; the alert stream is the operator's.
var (
	TrafficAccess  = httpx.Access{WebSocket: true, Scopes: []string{auth.ScopeTraffic}, Sessions: []httpx.SessionAccess{{Realm: auth.RealmConsole}}}
	SnapshotAccess = httpx.Access{Scopes: []string{auth.ScopeTraffic}, Sessions: []httpx.SessionAccess{{Realm: auth.RealmConsole}}}
	AlertsAccess   = httpx.Access{WebSocket: true, Scopes: []string{auth.ScopeTraffic}}
)

// AccessTable is the access entry of every operation traffic-ws serves
// (the GuardedMux fails closed).
func AccessTable() map[string]httpx.Access {
	public := httpx.Access{Public: true}
	return map[string]httpx.Access{
		"GET /healthz":             public,
		"GET /readyz":              public,
		"GET /v1/traffic":          TrafficAccess,
		"GET /v1/traffic/snapshot": SnapshotAccess,
		"GET /v1/alerts":           AlertsAccess,
	}
}

// Health serves the two health operations.
type Health interface {
	GetHealthz(w http.ResponseWriter, r *http.Request)
	GetReadyz(w http.ResponseWriter, r *http.Request)
}

// Server serves the traffic and alert streams and the snapshot
// (gen.ServerInterface).
type Server struct {
	Health
	Hub *Hub
	WS  *auth.WSAuth
	// Ctx ends every open socket when the process stops.
	Ctx context.Context
	// ProductEvery, StatusEvery and RecordEvery override the periods
	// (tests); zero keeps 1 s, 2 s and the policy's record period.
	ProductEvery, StatusEvery, RecordEvery time.Duration
	// RepeatEvery overrides the policy's escalation_repeat_s (tests).
	RepeatEvery time.Duration
	// Degraded lists the process's degraded dependencies (/readyz).
	Degraded func() []string
	// Geo is the geo change push (brief WP-12): every installed CIS
	// version is a geo/changed/v1 frame on the traffic stream, so the
	// subscriber refetches GET /v1/geo; nil sends none.
	Geo      *geo.Changes
	Counters *core.Counters
	Logger   *slog.Logger
}

var _ gen.ServerInterface = (*Server)(nil)

func (s *Server) count(name string) {
	if s.Counters != nil {
		s.Counters.Inc(name)
	}
}

func (s *Server) logger() *slog.Logger {
	if s.Logger == nil {
		return obs.Discard()
	}
	return s.Logger
}

// Register registers every operation on mux behind guard; the error
// lists every route without a valid access entry and every entry
// without a route.
func Register(mux *http.ServeMux, s *Server, guard httpx.Guard) error {
	g := httpx.NewGuardedMux(mux, AccessTable(), guard, auth.ValidateAccess)
	gen.HandlerWithOptions(s, gen.StdHTTPServerOptions{BaseRouter: g, ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, _ error) {
		httpx.NewProblem(http.StatusBadRequest, httpx.SlugValidation, "Invalid request", "", core.Fieldf("parameter", "malformed")).Write(w, r)
	}})
	return g.Err()
}

// subOf is the subscription a caller asks for, or why it is refused
// (answered before any upgrade).
func (s *Server) subOf(p auth.Principal, intentID *string, bbox *string, staffAllowed bool) (*Sub, *RefusalError) {
	if p.Session {
		if !staffAllowed || p.Claims.Realm != auth.RealmConsole {
			return nil, &RefusalError{http.StatusForbidden, httpx.SlugForbidden, "a console session subscribes to the traffic picture only"}
		}
		sub := &Sub{ClientID: p.Claims.Subject, Staff: true}
		if intentID != nil {
			return nil, &RefusalError{http.StatusBadRequest, httpx.SlugValidation, "a console session subscribes by bbox"}
		}
		if bbox != nil {
			b, err := ParseBBox(*bbox)
			if err != nil {
				return nil, &RefusalError{http.StatusBadRequest, httpx.SlugValidation, err.Error()}
			}
			sub.BBox = b
		}
		return sub, nil
	}
	if intentID == nil || bbox != nil {
		return nil, &RefusalError{http.StatusBadRequest, httpx.SlugValidation, "an operator client subscribes by intent_id"}
	}
	if _, ref := s.Hub.Owns(p.Claims.Subject, *intentID); ref != nil {
		return nil, ref
	}
	return &Sub{ClientID: p.Claims.Subject, IntentID: *intentID}, nil
}

func writeRefusal(w http.ResponseWriter, r *http.Request, ref *RefusalError) {
	if ref.status == http.StatusServiceUnavailable {
		httpx.RetryAfter(w, 5*time.Second)
	}
	httpx.NewProblem(ref.status, ref.slug, "", ref.detail).Write(w, r)
}

func uuidString(u fmt.Stringer) *string {
	s := u.String()
	return &s
}

// GetTrafficSnapshot implements gen.ServerInterface: one product.
func (s *Server) GetTrafficSnapshot(w http.ResponseWriter, r *http.Request, params gen.GetTrafficSnapshotParams) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.NewProblem(http.StatusUnauthorized, httpx.SlugUnauthenticated, "", "no caller").Write(w, r)
		return
	}
	var intentID *string
	if params.IntentId != nil {
		intentID = uuidString(params.IntentId)
	}
	sub, ref := s.subOf(p, intentID, params.Bbox, true)
	if ref != nil {
		writeRefusal(w, r, ref)
		return
	}
	c := &connState{sub: sub}
	prod := s.product(c, 0)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(prod)
}

// connState is one connection's subscription and counters.
type connState struct {
	id      string
	mu      sync.Mutex
	sub     *Sub
	dropped atomic.Uint64
	out     chan []byte
	// sent is the state and severity of each alert last queued, and
	// repeatAt when an unacknowledged critical one is due again. unsent
	// are the clears the queue could not take, sent again each tick (no
	// republish follows a clear).
	sent     map[string]string
	repeatAt map[string]time.Time
	unsent   map[string]traffic.Entry
}

func (c *connState) subscription() *Sub {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := *c.sub
	return &cp
}

// enqueue queues one frame and reports whether it was queued; a full
// queue drops it, counted.
func (s *Server) enqueue(c *connState, v any) bool {
	data, err := json.Marshal(v)
	if err != nil {
		s.logger().Error("frame not encoded", obs.Err(err))
		return false
	}
	select {
	case c.out <- data:
		return true
	default:
		c.dropped.Add(1)
		s.count(CounterFramesDropped)
		return false
	}
}

// OpenTrafficStream implements gen.ServerInterface: the traffic stream.
func (s *Server) OpenTrafficStream(w http.ResponseWriter, r *http.Request, params gen.OpenTrafficStreamParams) {
	var sub *Sub
	ws := *s.WS
	ws.Admit = func(w http.ResponseWriter, r *http.Request, p auth.Principal) bool {
		var intentID *string
		if params.IntentId != nil {
			intentID = uuidString(params.IntentId)
		}
		var ref *RefusalError
		sub, ref = s.subOf(p, intentID, params.Bbox, true)
		if ref != nil {
			writeRefusal(w, r, ref)
			return false
		}
		return true
	}
	conn, p, err := ws.AcceptWS(w, r, TrafficAccess)
	if err != nil {
		return
	}
	s.serve(r.Context(), conn, sub, p.Claims.ExpiresAt, false)
}

// OpenAlertStream implements gen.ServerInterface: the alert stream.
func (s *Server) OpenAlertStream(w http.ResponseWriter, r *http.Request, params gen.OpenAlertStreamParams) {
	var sub *Sub
	ws := *s.WS
	ws.Admit = func(w http.ResponseWriter, r *http.Request, p auth.Principal) bool {
		var ref *RefusalError
		sub, ref = s.subOf(p, uuidString(params.IntentId), nil, false)
		if ref != nil {
			writeRefusal(w, r, ref)
			return false
		}
		return true
	}
	conn, p, err := ws.AcceptWS(w, r, AlertsAccess)
	if err != nil {
		return
	}
	s.serve(r.Context(), conn, sub, p.Claims.ExpiresAt, true)
}

func (s *Server) period(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// serve runs one connection: the writer, the reader (console/subscribe
// from staff), the status frames, the snapshot and the products (the
// traffic stream) or the alerts and their repeats (the alert stream).
// The socket closes with 4401 (sign in again) when the credential it
// was opened with expires: this process cannot read the session rows,
// so a session lives here no longer than its token.
func (s *Server) serve(rctx context.Context, conn *websocket.Conn, sub *Sub, exp time.Time, alertsOnly bool) {
	s.count(CounterConnections)
	conn.SetReadLimit(MaxClientFrameBytes)
	ctx, cancel := context.WithCancel(rctx)
	defer cancel()
	if s.Ctx != nil {
		stop := context.AfterFunc(s.Ctx, cancel)
		defer stop()
	}
	c := &connState{id: bus.NewULID(time.Now()), sub: sub, out: make(chan []byte, SendQueueLen), sent: map[string]string{}, repeatAt: map[string]time.Time{}}
	book := s.Hub.Book.Subscribe()
	defer book.Cancel()
	var geoCh <-chan cis.Change
	if s.Geo != nil && !alertsOnly {
		ch, cancelGeo := s.Geo.Subscribe()
		defer cancelGeo()
		geoCh = ch
	}
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Go(func() { s.write(ctx, cancel, conn, c) })
	resubscribed := make(chan struct{}, 1)
	wg.Go(func() { s.read(ctx, cancel, conn, c, resubscribed) })

	s.enqueue(c, s.status(c))
	if alertsOnly {
		s.sendActive(ctx, c)
	} else {
		s.enqueue(c, s.snapshot(c))
	}
	product := time.NewTicker(s.period(s.ProductEvery, ProductEvery))
	status := time.NewTicker(s.period(s.StatusEvery, StatusEvery))
	record := time.NewTicker(s.period(s.RecordEvery, time.Duration(s.Hub.policy().Values.TrafficRecordEveryS*float64(time.Second))))
	defer product.Stop()
	defer status.Stop()
	defer record.Stop()
	expired := make(<-chan time.Time)
	if !exp.IsZero() {
		t := time.NewTimer(time.Until(exp))
		defer t.Stop()
		expired = t.C
	}
	var tick uint64
	for {
		select {
		case <-ctx.Done():
			return
		case <-expired:
			s.count(CounterExpired)
			_ = conn.Close(auth.CloseRelogin, "the credential expired: sign in again")
			cancel()
			return
		case <-resubscribed:
			s.enqueue(c, s.snapshot(c))
		case e, ok := <-book.C:
			if ok {
				s.forward(ctx, c, &e, alertsOnly)
			}
		case <-book.Missed:
			if s.missed(ctx, c, book, alertsOnly) {
				s.count(CounterResync)
				_ = conn.Close(websocket.StatusTryAgainLater, "alert clears missed: reconnect for a new snapshot")
				cancel()
				return
			}
		case ch := <-geoCh:
			if s.enqueue(c, s.frame(geo.SchemaChanged, ch)) {
				s.count(CounterGeoChanges)
			}
		case <-status.C:
			s.enqueue(c, s.status(c))
		case <-product.C:
			tick++
			s.resend(ctx, c)
			if alertsOnly {
				s.repeat(ctx, c)
				continue
			}
			s.enqueue(c, s.product(c, tick))
		case <-record.C:
			if !alertsOnly {
				s.record(ctx, c)
			}
		}
	}
}

// write is the only writer of the socket.
func (s *Server) write(ctx context.Context, cancel context.CancelFunc, conn *websocket.Conn, c *connState) {
	for {
		select {
		case <-ctx.Done():
			_ = conn.Close(websocket.StatusNormalClosure, "")
			return
		case data := <-c.out:
			wctx, wcancel := context.WithTimeout(ctx, writeTimeout)
			err := conn.Write(wctx, websocket.MessageText, data)
			wcancel()
			if err != nil {
				cancel()
				return
			}
			s.count(CounterFramesSent)
		}
	}
}

// subscribeFrame is console/subscribe/v1.
type subscribeFrame struct {
	Schema string `json:"schema"`
	Body   *struct {
		BBox   []float64 `json:"bbox"`
		Layers []string  `json:"layers"`
	} `json:"body"`
}

var layersKnown = map[string]bool{"tracks": true, "manned": true, "alerts": true, "zones": true}

// read takes the client's frames: a staff session's console/subscribe/v1
// changes its bbox and layers and is answered with a snapshot; anything
// else is ignored and counted.
func (s *Server) read(ctx context.Context, cancel context.CancelFunc, conn *websocket.Conn, c *connState, resubscribed chan<- struct{}) {
	defer cancel()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var f subscribeFrame
		if json.Unmarshal(data, &f) != nil || f.Schema != SchemaSubscribe || f.Body == nil || len(f.Body.BBox) != 4 {
			s.count(CounterSubscribeRefused)
			continue
		}
		c.mu.Lock()
		staff := c.sub.Staff
		c.mu.Unlock()
		if !staff {
			s.count(CounterSubscribeRefused)
			continue
		}
		b, err := checkBBox([4]float64(f.Body.BBox))
		if err != nil {
			s.count(CounterSubscribeRefused)
			continue
		}
		layers := map[string]bool{}
		bad := false
		for _, l := range f.Body.Layers {
			if !layersKnown[l] || layers[l] {
				bad = true
			}
			layers[l] = true
		}
		if bad {
			s.count(CounterSubscribeRefused)
			continue
		}
		c.mu.Lock()
		c.sub.BBox, c.sub.Layers = b, layers
		c.mu.Unlock()
		s.count(CounterSubscribes)
		select {
		case resubscribed <- struct{}{}:
		default:
		}
	}
}

// ThresholdsBody is the CPA and traffic thresholds in force (INV-03:
// the console never defaults them).
type ThresholdsBody struct {
	CPATCPAMaxS         float64 `json:"cpa_tcpa_max_s"`
	CPAHorizontalMinM   float64 `json:"cpa_horizontal_min_m"`
	CPAVerticalMinM     float64 `json:"cpa_vertical_min_m"`
	CPANeighbourRadiusM float64 `json:"cpa_neighbour_radius_m"`
	CPAClearAfterS      float64 `json:"cpa_clear_after_s"`
	TrafficRadiusM      float64 `json:"traffic_radius_m"`
}

// StatusBody is console/status/v1 with this stream's extras.
type StatusBody struct {
	ConnectionID  string            `json:"connection_id"`
	ServerTS      bus.Stamp         `json:"server_ts"`
	PolicyVersion string            `json:"policy_version"`
	StaleAfterS   float64           `json:"stale_after_s"`
	LiveMaxAgeS   float64           `json:"live_max_age_s"`
	DroppedFrames uint64            `json:"dropped_frames"`
	Degraded      []string          `json:"degraded"`
	Sources       []json.RawMessage `json:"sources"`
	CISVersion    *string           `json:"cis_version,omitempty"`
	CISAgeS       *float64          `json:"cis_age_s,omitempty"`
	// EvaluationPeriodS is the CPA evaluation period of the newest
	// proximity alert of the subscription; absent while there is none
	// (traffic-ws learns it from the monitor's alerts).
	EvaluationPeriodS *float64           `json:"evaluation_period_s,omitempty"`
	Thresholds        ThresholdsBody     `json:"thresholds"`
	DegradedInputs    []traffic.Degraded `json:"degraded_inputs"`
	Subscription      traffic.For        `json:"subscription"`
}

// Frame is one frame with the envelope and a body.
type Frame struct {
	bus.Envelope
	Body any `json:"body"`
}

func (s *Server) frame(schema string, body any) *Frame {
	return &Frame{Envelope: bus.SystemEnvelope(schema, traffic.ProducerWS, s.Hub.now()), Body: body}
}

func forOf(sub *Sub) traffic.For {
	f := traffic.For{BBox: sub.BBox}
	if sub.IntentID != "" {
		id := sub.IntentID
		f.IntentID = &id
	}
	return f
}

// status is the connection's console/status/v1.
func (s *Server) status(c *connState) *Frame {
	sub := c.subscription()
	now := s.Hub.now()
	pol := s.Hub.policy()
	v := pol.Values
	ds := s.Hub.Degraded(now)
	slugs := []string{}
	if s.Degraded != nil {
		slugs = append(slugs, s.Degraded()...)
	}
	for k := range ds {
		slugs = append(slugs, k)
	}
	slices.Sort(slugs)
	slugs = slices.Compact(slugs)
	b := StatusBody{
		ConnectionID: c.id, ServerTS: bus.Stamp{Time: now.UTC()}, PolicyVersion: strconv.FormatInt(pol.Version, 10),
		StaleAfterS: v.TrafficStaleAfterS, LiveMaxAgeS: v.TrafficLiveMaxAgeS, DroppedFrames: c.dropped.Load(),
		Degraded: slugs, Sources: s.Hub.Sources(sub), DegradedInputs: traffic.DegradedSorted(ds), Subscription: forOf(sub),
		Thresholds: ThresholdsBody{CPATCPAMaxS: v.CPATCPAMaxS, CPAHorizontalMinM: v.CPAHorizontalMinM, CPAVerticalMinM: v.CPAVerticalMinM,
			CPANeighbourRadiusM: v.CPANeighbourRadiusM, CPAClearAfterS: v.CPAClearAfterS, TrafficRadiusM: v.TrafficRadiusM},
	}
	if s.Hub.CIS != nil {
		if basis, ok := s.Hub.CIS(); ok && basis.CISVersion != "" {
			ver, age := basis.CISVersion, basis.CISAgeS+max(0, now.Sub(basis.At).Seconds())
			b.CISVersion, b.CISAgeS = &ver, &age
		}
	}
	a, flight, _ := s.Hub.Area(sub)
	prox := s.Hub.AlertsFor(sub, a, flight, traffic.KindProximity)
	for i := range prox {
		e := &prox[i]
		var d struct {
			Detail struct {
				EvaluationPeriodS *float64 `json:"evaluation_period_s"`
			} `json:"detail"`
		}
		if json.Unmarshal(e.Body, &d) == nil && d.Detail.EvaluationPeriodS != nil {
			b.EvaluationPeriodS = d.Detail.EvaluationPeriodS
			break
		}
	}
	return s.frame(SchemaStatus, b)
}

// SnapshotBody is console/snapshot/v1.
type SnapshotBody struct {
	Tracks       []json.RawMessage `json:"tracks"`
	Alerts       []json.RawMessage `json:"alerts"`
	Manned       []json.RawMessage `json:"manned"`
	ZonesVersion *string           `json:"zones_version"`
}

func (sub *Sub) wants(layer string) bool { return sub.Layers == nil || sub.Layers[layer] }

// snapshot is the connection's console/snapshot/v1: every track and
// manned aircraft of the subscription as its message, its active
// alerts, the CIS version.
func (s *Server) snapshot(c *connState) *Frame {
	sub := c.subscription()
	a, flight, _ := s.Hub.Area(sub)
	b := SnapshotBody{Tracks: []json.RawMessage{}, Alerts: []json.RawMessage{}, Manned: []json.RawMessage{}}
	ts := s.Hub.Picture.Tracks(a, s.Hub.now(), s.Hub.policy().Values, s.Hub)
	for i := range ts {
		t := &ts[i]
		switch {
		case t.Manned && sub.wants("manned"):
			b.Manned = append(b.Manned, t.Raw)
		case !t.Manned && sub.wants("tracks"):
			b.Tracks = append(b.Tracks, t.Raw)
		}
	}
	if sub.wants("alerts") {
		es := s.Hub.AlertsFor(sub, a, flight, "")
		for i := range es {
			b.Alerts = append(b.Alerts, es[i].Message)
		}
	}
	if s.Hub.CIS != nil {
		if basis, ok := s.Hub.CIS(); ok && basis.CISVersion != "" {
			v := basis.CISVersion
			b.ZonesVersion = &v
		}
	}
	return s.frame(SchemaSnapshot, b)
}

// product is the subscription's traffic/product/v1 at tick: the tracks
// (throttled above traffic_throttle_track_count on a stream; tick 0, a
// snapshot or a record sample, is never throttled), the active
// proximity alerts, the degraded inputs.
func (s *Server) product(c *connState, tick uint64) *traffic.Product {
	sub := c.subscription()
	now := s.Hub.now()
	pol := s.Hub.policy()
	a, flight, _ := s.Hub.Area(sub)
	sel := s.Hub.Picture.Tracks(a, now, pol.Values, s.Hub)
	if !sub.wants("tracks") || !sub.wants("manned") {
		sel = slices.DeleteFunc(sel, func(t traffic.Selected) bool {
			return (t.Manned && !sub.wants("manned")) || (!t.Manned && !sub.wants("tracks"))
		})
	}
	kept, held := sel, 0
	if tick > 0 {
		kept, held = traffic.Throttle(sel, pol.Values.TrafficThrottleTracks, tick)
	}
	if held > 0 {
		c.dropped.Add(uint64(held))
		if s.Counters != nil {
			s.Counters.Add(CounterTracksHeldBack, uint64(held))
		}
	}
	b := traffic.ProductBody{
		At: bus.Stamp{Time: now.UTC()}, For: forOf(sub), Tracks: traffic.Tracked(kept), Alerts: []json.RawMessage{},
		Degraded: traffic.DegradedSorted(s.Hub.Degraded(now)), PolicyVersion: pol.Version, DroppedFrames: c.dropped.Load(),
		Throttled: held > 0,
	}
	if sub.wants("alerts") {
		es := s.Hub.AlertsFor(sub, a, flight, traffic.KindProximity)
		for i := range es {
			b.Alerts = append(b.Alerts, es[i].Body)
		}
	}
	if s.Hub.CIS != nil {
		if basis, ok := s.Hub.CIS(); ok && basis.CISVersion != "" {
			v := basis.CISVersion
			b.CISVersion = &v
		}
	}
	return &traffic.Product{Envelope: bus.SystemEnvelope(traffic.SchemaProduct, traffic.ProducerWS, now), Body: b}
}

// record publishes a sample of the connection's product for the record
// (traffic.product.v1.<client_id>, 03 §3).
func (s *Server) record(ctx context.Context, c *connState) {
	if s.Hub.Pub == nil {
		return
	}
	p := s.product(c, 0)
	p.Body.ClientID = c.subscription().ClientID
	subject, err := bus.TrafficProduct(p.Body.ClientID)
	if err == nil {
		pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err = s.Hub.Pub.Publish(pctx, subject, p)
		cancel()
	}
	if err != nil {
		s.Hub.counters().Inc(CounterRecordFailed)
		if !errors.Is(err, context.Canceled) {
			s.logger().LogAttrs(ctx, slog.LevelWarn, "traffic product sample not recorded", obs.Err(err))
		}
		return
	}
	s.Hub.counters().Inc(CounterRecordPublished)
}

// matches reports whether an alert belongs to the connection's
// subscription.
func (s *Server) matches(c *connState, e *traffic.Entry) bool {
	sub := c.subscription()
	if !sub.Staff {
		if e.FlightID == "" && e.IntentID != "" && e.IntentID == sub.IntentID {
			// A notice of the subscribed intent (restriction_activated,
			// WP-12; ownership was checked at the upgrade) reaches it even
			// once the intent left intent_active (withdrawn).
			return true
		}
		b, ref := s.Hub.Owns(sub.ClientID, sub.IntentID)
		if ref != nil {
			return false
		}
		return (b.FlightID != nil && e.FlightID == *b.FlightID) || e.IntentID == sub.IntentID
	}
	if !sub.wants("alerts") {
		return false
	}
	a, _, _ := s.Hub.Area(sub)
	ts := s.Hub.Picture.Tracks(a, s.Hub.now(), s.Hub.policy().Values, nil)
	for i := range ts {
		if ts[i].Track.TrackID == e.FlightID {
			return true
		}
	}
	return false
}

// changeKey is what makes an alert message news to a connection: its
// state (raised and updated are one: the republish of an active alert),
// severity and acknowledgement.
func changeKey(e *traffic.Entry) string {
	st := e.State
	if st == "raised" || st == "updated" {
		st = "active"
	}
	return st + "|" + string(e.Severity) + "|" + strconv.FormatBool(e.Acked) + "|" + strconv.FormatBool(e.Escalated)
}

// forward sends an alert of the subscription when it says something new
// to the connection: a raise, a clear, a severity change or an
// acknowledgement (the 1 s republish of an unchanged alert is carried by
// the product's numbers, and by the repeat on the alert stream).
func (s *Server) forward(ctx context.Context, c *connState, e *traffic.Entry, alertsOnly bool) {
	if !s.matches(c, e) {
		return
	}
	key := changeKey(e)
	if c.sent[e.AlertID] == key {
		return
	}
	// Marked only once queued: a dropped raise or change goes with the
	// next republish, a dropped clear with the next tick (resend).
	queued := s.send(ctx, c, e, alertsOnly)
	switch {
	case e.State == "cleared" && queued:
		delete(c.sent, e.AlertID)
		delete(c.unsent, e.AlertID)
	case e.State == "cleared":
		if c.unsent == nil {
			c.unsent = map[string]traffic.Entry{}
		}
		c.unsent[e.AlertID] = *e
	case queued:
		c.sent[e.AlertID] = key
	}
}

// missed sends the clears the subscription could not take when they
// came, and reports whether more were missed than the book kept: the
// client must then resync (reconnect to a new snapshot) rather than miss
// one silently.
func (s *Server) missed(ctx context.Context, c *connState, book *traffic.Subscription, alertsOnly bool) bool {
	missed, resync := book.TakeMissed()
	for i := range missed {
		s.forward(ctx, c, &missed[i], alertsOnly)
	}
	return resync
}

// resend queues again the clears the connection's queue could not take.
func (s *Server) resend(ctx context.Context, c *connState) {
	for _, id := range slices.Sorted(maps.Keys(c.unsent)) {
		e := c.unsent[id]
		if !s.matches(c, &e) {
			// No longer the subscription's (a staff bbox moved).
			delete(c.unsent, id)
			continue
		}
		s.count(CounterClearsResent)
		s.forward(ctx, c, &e, true)
	}
}

// send queues one alert/v1 frame and records the first send to an
// operator; an unacknowledged critical alert is due again after the
// repeat period. It reports whether the frame was queued (nothing is
// recorded of one that was not).
func (s *Server) send(ctx context.Context, c *connState, e *traffic.Entry, alertsOnly bool) bool {
	if !s.enqueue(c, e.Message) {
		return false
	}
	s.count(CounterAlertsSent)
	sub := c.subscription()
	if !sub.Staff {
		s.Hub.Delivered(ctx, e, sub.ClientID)
	}
	if alertsOnly && e.State != "cleared" && e.Severity == core.SeverityCritical && !e.Acked {
		c.repeatAt[e.AlertID] = s.Hub.now().Add(s.repeatEvery())
	} else {
		delete(c.repeatAt, e.AlertID)
	}
	return true
}

func (s *Server) repeatEvery() time.Duration {
	if s.RepeatEvery > 0 {
		return s.RepeatEvery
	}
	return time.Duration(s.Hub.policy().Values.EscalationRepeatS * float64(time.Second))
}

// sendActive sends every active alert of the subscription (on connect).
func (s *Server) sendActive(ctx context.Context, c *connState) {
	sub := c.subscription()
	a, flight, _ := s.Hub.Area(sub)
	es := s.Hub.AlertsFor(sub, a, flight, "")
	for i := range es {
		// One the queue cannot take is sent with its next republish.
		if s.send(ctx, c, &es[i], true) {
			c.sent[es[i].AlertID] = changeKey(&es[i])
		}
	}
}

// repeat sends again every unacknowledged critical alert whose repeat
// is due, with its current numbers (02 F5: every 10 s until it is
// acknowledged or cleared).
func (s *Server) repeat(ctx context.Context, c *connState) {
	now := s.Hub.now()
	sub := c.subscription()
	a, flight, _ := s.Hub.Area(sub)
	active := map[string]*traffic.Entry{}
	es := s.Hub.AlertsFor(sub, a, flight, "")
	for i := range es {
		active[es[i].AlertID] = &es[i]
	}
	for id, at := range c.repeatAt {
		e, ok := active[id]
		if !ok || e.Acked {
			delete(c.repeatAt, id)
			continue
		}
		if now.Before(at) {
			continue
		}
		s.count(CounterAlertsRepeated)
		s.send(ctx, c, e, true)
	}
}
