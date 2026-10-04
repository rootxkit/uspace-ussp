package ansp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/bus"
)

// producer is the envelope producer of the fake's frames.
const producer = "ansp/manned-feed"

// Aircraft is one manned aircraft the fake streams.
type Aircraft struct {
	ICAO24       string
	Callsign     *string
	Position     core.LatLon
	AltPressureM *float64
	AltWGS84M    *float64
	GSMS         float64
	TrackDeg     float64
	// Adapter is the ANSP adapter the aircraft comes from
	// (source_instance); "fake-adsb-1" when empty.
	Adapter string
}

// AdapterState is what the fake's console/status/v1 says of an adapter.
type AdapterState struct {
	ID, State string
	Since     time.Time
}

type streamState struct {
	aircraft    map[string]Aircraft
	adapters    map[string]AdapterState
	cut         bool
	tracksOff   bool
	conns       map[*websocket.Conn]context.CancelFunc
	connects    int
	auth        []string
	bboxes      []string
	trackEvery  time.Duration
	statusEvery time.Duration
}

// StreamURL is the fake's stream (USSP_ANSP_STREAM_URL).
func (f *Fake) StreamURL() string {
	return "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/v1/manned-traffic/stream"
}

// Periods sets how often the stream sends every aircraft and its
// status (1 s and 2 s by default); call before a client connects.
func (f *Fake) Periods(track, status time.Duration) {
	f.mu.Lock()
	f.stream.trackEvery, f.stream.statusEvery = track, status
	f.mu.Unlock()
}

// SetAircraft makes the stream send a (replacing one of its icao24).
func (f *Fake) SetAircraft(a Aircraft) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stream.aircraft == nil {
		f.stream.aircraft = map[string]Aircraft{}
	}
	f.stream.aircraft[a.ICAO24] = a
}

// RemoveAircraft stops sending the aircraft icao24.
func (f *Fake) RemoveAircraft(icao24 string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.stream.aircraft, icao24)
}

// SetAdapter sets what console/status/v1 says of one adapter.
func (f *Fake) SetAdapter(a AdapterState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stream.adapters == nil {
		f.stream.adapters = map[string]AdapterState{}
	}
	f.stream.adapters[a.ID] = a
}

// TracksOff makes the stream send only console/status/v1 frames (off
// true) or tracks again.
func (f *Fake) TracksOff(off bool) {
	f.mu.Lock()
	f.stream.tracksOff = off
	f.mu.Unlock()
}

// CutStream closes every open stream at once and refuses new ones with
// 503 until RestoreStream (the stream cut of 02 F4).
func (f *Fake) CutStream() {
	f.mu.Lock()
	f.stream.cut = true
	conns := f.stream.conns
	f.stream.conns = nil
	f.mu.Unlock()
	for c, cancel := range conns {
		cancel()
		_ = c.CloseNow()
	}
}

// RestoreStream ends CutStream.
func (f *Fake) RestoreStream() {
	f.mu.Lock()
	f.stream.cut = false
	f.mu.Unlock()
}

// StreamConnects is how many stream connections were accepted.
func (f *Fake) StreamConnects() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stream.connects
}

// StreamAuth is every Authorization header and bbox of the accepted
// connections, in order.
func (f *Fake) StreamAuth() (auth, bboxes []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.stream.auth...), append([]string(nil), f.stream.bboxes...)
}

func (f *Fake) envelope(schema string, now time.Time) bus.Envelope {
	return bus.NewEnvelope(schema, producer, core.Times{TS: &now, RxTS: now, CapturedAt: now, Source: core.TimeSourceClock})
}

// trackFrame is a's track/manned/v1 frame at now.
func (f *Fake) trackFrame(a Aircraft, now time.Time) map[string]any {
	adapter := a.Adapter
	if adapter == "" {
		adapter = "fake-adsb-1"
	}
	gs, td := a.GSMS, a.TrackDeg
	body := map[string]any{
		"icao24": a.ICAO24, "callsign": a.Callsign, "position": map[string]any{"lat": a.Position.LatDeg, "lng": a.Position.LonDeg},
		"alt_pressure_m": a.AltPressureM, "alt_wgs84_m": a.AltWGS84M, "gs_ms": gs, "track_deg": td, "vrate_ms": 0.0,
		"emergency": false, "source_class": "ads_b", "trust": "surveillance", "source": "ansp_feed", "source_instance": adapter,
		"state": "live", "relevant": true, "policy_version": "1", "age_s": 0.0,
	}
	e := f.envelope("track/manned/v1", now)
	return map[string]any{"schema": e.Schema, "msg_id": e.MsgID, "producer": e.Producer, "ts": e.TS, "rx_ts": e.RxTS,
		"captured_at": e.CapturedAt, "time_source": e.TimeSource, "backlog": false, "body": body}
}

func (f *Fake) statusFrame(now time.Time) map[string]any {
	f.mu.Lock()
	srcs, ads := make([]map[string]any, 0, len(f.stream.adapters)), make([]map[string]any, 0, len(f.stream.adapters))
	for _, a := range f.stream.adapters {
		id := a.ID
		srcs = append(srcs, map[string]any{"source": "ansp_feed", "source_instance": id, "state": a.State, "since": a.Since.UTC(),
			"age_s": nil, "disabled_by": nil, "counters": map[string]int{"accepted": 0, "refused": 0}})
		ads = append(ads, map[string]any{"id": id, "state": a.State, "enabled": true, "last_frame_at": a.Since.UTC()})
	}
	f.mu.Unlock()
	degraded := []string{}
	for _, s := range srcs {
		if s["state"] != "live" {
			degraded = append(degraded, "adsb_stale")
			break
		}
	}
	e := f.envelope("console/status/v1", now)
	body := map[string]any{"connection_id": "fake", "server_ts": now.UTC(), "policy_version": "1", "stale_after_s": 5, "live_max_age_s": 2,
		"dropped_frames": 0, "degraded": degraded, "sources": srcs, "adapters": ads}
	return map[string]any{"schema": e.Schema, "msg_id": e.MsgID, "producer": e.Producer, "ts": e.TS, "rx_ts": e.RxTS,
		"captured_at": e.CapturedAt, "time_source": e.TimeSource, "backlog": false, "body": body}
}

func (f *Fake) mannedNow(now time.Time) []map[string]any {
	f.mu.Lock()
	as := make([]Aircraft, 0, len(f.stream.aircraft))
	for _, a := range f.stream.aircraft {
		as = append(as, a)
	}
	off := f.stream.tracksOff
	f.mu.Unlock()
	out := []map[string]any{}
	if off {
		return out
	}
	for _, a := range as {
		out = append(out, f.trackFrame(a, now))
	}
	return out
}

func (f *Fake) streamAuthorized(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || len(r.Header.Get("Authorization")) <= len("Bearer ") {
		problem(w, http.StatusUnauthorized, "unauthenticated", "no bearer token")
		return false
	}
	return true
}

// serveSnapshot is GET /v1/manned-traffic/snapshot.
func (f *Fake) serveSnapshot(w http.ResponseWriter, r *http.Request) {
	if !f.streamAuthorized(w, r) {
		return
	}
	f.mu.Lock()
	cut := f.stream.cut
	f.mu.Unlock()
	if cut {
		w.Header().Set("Retry-After", "1")
		problem(w, http.StatusServiceUnavailable, "unavailable", "the manned feed is cut")
		return
	}
	now := time.Now()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"tracks": []any{}, "alerts": []any{}, "manned": f.mannedNow(now), "zones_version": nil,
		"degraded": []string{}, "adapters": []any{}, "cis_version": nil, "cis_age_s": nil, "policy_version": "1"})
}

// serveStream is GET /v1/manned-traffic/stream.
func (f *Fake) serveStream(w http.ResponseWriter, r *http.Request) {
	if !f.streamAuthorized(w, r) {
		return
	}
	f.mu.Lock()
	cut := f.stream.cut
	track, status := f.stream.trackEvery, f.stream.statusEvery
	f.mu.Unlock()
	if cut {
		w.Header().Set("Retry-After", "1")
		problem(w, http.StatusServiceUnavailable, "unavailable", "the manned feed is cut")
		return
	}
	if track <= 0 {
		track = time.Second
	}
	if status <= 0 {
		status = 2 * time.Second
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ctx, cancel := context.WithCancel(c.CloseRead(r.Context()))
	defer cancel()
	defer func() { _ = c.CloseNow() }()
	f.mu.Lock()
	if f.stream.conns == nil {
		f.stream.conns = map[*websocket.Conn]context.CancelFunc{}
	}
	f.stream.conns[c] = cancel
	f.stream.connects++
	f.stream.auth = append(f.stream.auth, r.Header.Get("Authorization"))
	f.stream.bboxes = append(f.stream.bboxes, r.URL.Query().Get("bbox"))
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.stream.conns, c)
		f.mu.Unlock()
	}()
	send := func(v any) bool {
		b, err := json.Marshal(v)
		if err != nil {
			return false
		}
		wctx, wcancel := context.WithTimeout(ctx, 2*time.Second)
		defer wcancel()
		return c.Write(wctx, websocket.MessageText, b) == nil
	}
	now := time.Now()
	e := f.envelope("console/snapshot/v1", now)
	snap := map[string]any{"schema": e.Schema, "msg_id": e.MsgID, "producer": e.Producer, "ts": e.TS, "rx_ts": e.RxTS,
		"captured_at": e.CapturedAt, "time_source": e.TimeSource, "backlog": false,
		"body": map[string]any{"tracks": []any{}, "alerts": []any{}, "manned": f.mannedNow(now), "zones_version": nil}}
	if !send(snap) || !send(f.statusFrame(now)) {
		return
	}
	tt, st := time.NewTicker(track), time.NewTicker(status)
	defer tt.Stop()
	defer st.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tt.C:
			for _, fr := range f.mannedNow(time.Now()) {
				if !send(fr) {
					return
				}
			}
		case <-st.C:
			if !send(f.statusFrame(time.Now())) {
				return
			}
		}
	}
}
