package manned

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/core"
	coresources "github.com/rootxkit/uspace-core/sources"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/sources"
)

// Unit conversions of the ADS-B feeds (feet, knots, feet per minute).
const (
	// KnotsToMS is one international knot in metres per second (1852 m
	// an hour, exact).
	KnotsToMS = 1852.0 / 3600.0
	// FeetPerMinToMS is one foot per minute in metres per second.
	FeetPerMinToMS = core.FeetToMetres / 60
)

// The feed kinds of USSP_ADSB_SOURCE.
const (
	FeedHTTP = "http"
	FeedSBS  = "sbs"
	FeedFile = "file"
)

// Bounds and periods of the receiver adapter (E-10).
const (
	// MaxDocBytes bounds one aircraft.json document (and one replay line).
	MaxDocBytes = 4 << 20
	// MaxDocAircraft bounds the aircraft taken from one document.
	MaxDocAircraft = 5000
	// MaxSBSLineBytes bounds one BaseStation line.
	MaxSBSLineBytes = 1024
	// MaxSBSAircraft bounds the aircraft whose BaseStation state is kept.
	MaxSBSAircraft = 5000
	// MaxSeenS is the oldest position taken: older than the network
	// placement's 60 s is never current (timeplace.DefaultNetworkPolicy).
	MaxSeenS = 60
	// DefaultPollEvery is aircraft.json's poll period (1 Hz).
	DefaultPollEvery = time.Second
	// pollTimeout bounds one aircraft.json read.
	pollTimeout = 3 * time.Second
	// sbsHoldS is how long a BaseStation velocity or identity is joined
	// to the positions that follow it.
	sbsHoldS = 10
)

// Counters of the receiver adapter.
const (
	CounterAdsbPolls        = "adsb_rx_polls"
	CounterAdsbPollFailed   = "adsb_rx_poll_failed"
	CounterAdsbRecords      = "adsb_rx_records"
	CounterAdsbRefused      = "adsb_rx_record_refused"
	CounterAdsbNoPosition   = "adsb_rx_record_no_position"
	CounterAdsbNonICAO      = "adsb_rx_record_non_icao"
	CounterAdsbTooOld       = "adsb_rx_record_too_old"
	CounterAdsbOverBound    = "adsb_rx_over_bound"
	CounterAdsbPressureOnly = "adsb_rx_record_pressure_only"
	CounterAdsbUnreadable   = "adsb_rx_unreadable"
	CounterAdsbSwitchedOff  = "adsb_rx_stopped_switched_off"
	CounterAdsbStatusFailed = "adsb_rx_status_publish_failed"
)

// ParseFeed reads USSP_ADSB_SOURCE: http(s)://... (aircraft.json),
// sbs://host:port (BaseStation) or file://path (a recorded replay, one
// aircraft.json document per line). Anything else is refused naming the
// variable.
func ParseFeed(s string) (kind, target string, err error) {
	switch {
	case strings.HasPrefix(s, "file://"):
		p := strings.TrimPrefix(s, "file://")
		if p == "" {
			return "", "", core.Fieldf("USSP_ADSB_SOURCE", "file:// needs a path")
		}
		return FeedFile, p, nil
	case strings.HasPrefix(s, "sbs://"):
		u, err := url.Parse(s)
		if err != nil || u.Host == "" || u.Port() == "" {
			return "", "", core.Fieldf("USSP_ADSB_SOURCE", "sbs:// needs host:port")
		}
		return FeedSBS, u.Host, nil
	case strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://"):
		u, err := url.Parse(s)
		if err != nil || u.Host == "" {
			return "", "", core.Fieldf("USSP_ADSB_SOURCE", "not an absolute http(s) URL")
		}
		return FeedHTTP, u.String(), nil
	}
	return "", "", core.Fieldf("USSP_ADSB_SOURCE", "must be http(s)://.../aircraft.json, sbs://host:port or file://path")
}

// Econspicuity is this USSP's own e-conspicuity receiver adapter; see
// the package documentation.
type Econspicuity struct {
	// Source is USSP_ADSB_SOURCE (ParseFeed).
	Source string
	// ReceiverID is the source_instance of its tracks and the instance
	// of its adsb_rx switch (a slug).
	ReceiverID string
	HTTP       *http.Client
	// Dial opens a BaseStation connection (net.Dialer by default).
	Dial     func(ctx context.Context, network, addr string) (net.Conn, error)
	Sink     Sink
	Own      Own
	Gate     Gate
	Policy   func() policy.Values
	Counters *core.Counters
	Logger   *slog.Logger
	Now      func() time.Time
	// Every (1 s), StatusEvery (2 s), GateEvery (250 ms) and RetryMin
	// (1 s) are what a test shortens.
	Every       time.Duration
	StatusEvery time.Duration
	GateEvery   time.Duration
	RetryMin    time.Duration

	mu         sync.Mutex
	started    time.Time
	lastOK     time.Time
	lastRecord time.Time
	lastErr    string
	feedNow    float64
	feedMoved  time.Time
	finished   time.Time
	accepted   uint64
	refused    uint64
	echoes     uint64
	agg        string
	aggSince   time.Time
	offSince   time.Time
	once       sync.Once
	sbs        map[string]*sbsAircraft
}

func (e *Econspicuity) init() {
	e.once.Do(func() {
		if e.Counters == nil {
			e.Counters = &core.Counters{}
		}
		e.mu.Lock()
		e.started = e.now()
		e.mu.Unlock()
	})
}

func (e *Econspicuity) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Econspicuity) logger() *slog.Logger {
	if e.Logger == nil {
		return obs.Discard()
	}
	return e.Logger
}

func (e *Econspicuity) policy() policy.Values {
	if e.Policy == nil {
		return policy.Defaults()
	}
	return e.Policy()
}

func (e *Econspicuity) decision() coresources.Decision {
	if e.Gate == nil {
		return coresources.Decision{Enabled: true}
	}
	inst := e.ReceiverID
	return e.Gate.Query(SourceAdsbRx, &inst)
}

func (e *Econspicuity) failed(err error) {
	e.Counters.Inc(CounterAdsbPollFailed)
	e.mu.Lock()
	e.lastErr = clipErr(err)
	e.mu.Unlock()
}

func (e *Econspicuity) ok(feedNow float64) {
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lastOK, e.lastErr = now, ""
	if feedNow == 0 || feedNow != e.feedNow || e.feedMoved.IsZero() {
		e.feedNow, e.feedMoved = feedNow, now
	}
}

// Run reads the feed until ctx ends (a replay until its file ends) and
// publishes the receiver's status every StatusEvery.
func (e *Econspicuity) Run(ctx context.Context) {
	e.init()
	kind, target, err := ParseFeed(e.Source)
	if err != nil {
		e.failed(err)
		e.statusLoop(ctx)
		return
	}
	go e.statusLoop(ctx)
	switch kind {
	case FeedHTTP:
		e.runHTTP(ctx, target)
	case FeedSBS:
		e.runSBS(ctx, target)
	case FeedFile:
		e.runFile(ctx, target)
		<-ctx.Done()
	}
}

// waitEnabled waits while the receiver is switched off; false when ctx
// ended.
func (e *Econspicuity) waitEnabled(ctx context.Context) bool {
	for !e.decision().Enabled {
		if !sleep(ctx, every(e.GateEvery, DefaultGateEvery)) {
			return false
		}
	}
	return ctx.Err() == nil
}

func (e *Econspicuity) runHTTP(ctx context.Context, target string) {
	t := time.NewTicker(every(e.Every, DefaultPollEvery))
	defer t.Stop()
	for {
		if !e.waitEnabled(ctx) {
			return
		}
		e.pollOnce(ctx, target)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// pollOnce reads aircraft.json once.
func (e *Econspicuity) pollOnce(ctx context.Context, target string) {
	e.Counters.Inc(CounterAdsbPolls)
	cctx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, target, nil)
	if err != nil {
		e.failed(err)
		return
	}
	hc := e.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	res, err := hc.Do(req)
	if err != nil {
		e.failed(err)
		return
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(res.Body, MaxDocBytes+1))
	switch {
	case err != nil:
		e.failed(err)
		return
	case res.StatusCode != http.StatusOK:
		e.failed(fmt.Errorf("aircraft.json answered %d", res.StatusCode))
		return
	case len(body) > MaxDocBytes:
		e.failed(fmt.Errorf("aircraft.json over %d bytes", MaxDocBytes))
		return
	}
	if err := e.TakeDocument(ctx, body, e.now()); err != nil {
		e.Counters.Inc(CounterAdsbUnreadable)
		e.failed(err)
	}
}

// Document is aircraft.json (readsb/dump1090 README-json.md): the
// receiver's clock and the aircraft.
type Document struct {
	Now      float64          `json:"now"`
	Aircraft []AircraftRecord `json:"aircraft"`
}

// AircraftRecord is one aircraft.json entry: the keys the schema's
// members come from (and r, a registration, read by the echo guard only).
type AircraftRecord struct {
	Hex       string          `json:"hex"`
	Flight    *string         `json:"flight"`
	R         *string         `json:"r"`
	AltBaro   json.RawMessage `json:"alt_baro"`
	AltGeom   *float64        `json:"alt_geom"`
	GS        *float64        `json:"gs"`
	Track     *float64        `json:"track"`
	BaroRate  *float64        `json:"baro_rate"`
	GeomRate  *float64        `json:"geom_rate"`
	Squawk    *string         `json:"squawk"`
	Emergency *string         `json:"emergency"`
	Lat       *float64        `json:"lat"`
	Lon       *float64        `json:"lon"`
	SeenPos   *float64        `json:"seen_pos"`
	Seen      *float64        `json:"seen"`
	MLAT      []string        `json:"mlat"`
	NIC       *int            `json:"nic"`
	NACp      *int            `json:"nac_p"`
}

// TakeDocument reads one aircraft.json document arrived at rx and
// publishes its aircraft; an error is a document that does not read.
func (e *Econspicuity) TakeDocument(ctx context.Context, data []byte, rx time.Time) error {
	e.init()
	var d Document
	if err := json.Unmarshal(data, &d); err != nil {
		return core.Fieldf("aircraft.json", "does not read: %s", clipErr(err))
	}
	if d.Aircraft == nil {
		return core.Fieldf("aircraft.json", "no aircraft list")
	}
	if !core.IsFinite(d.Now) || d.Now < 0 {
		return core.Fieldf("aircraft.json.now", "not a time")
	}
	e.ok(d.Now)
	if len(d.Aircraft) > MaxDocAircraft {
		e.Counters.Add(CounterAdsbOverBound, uint64(len(d.Aircraft)-MaxDocAircraft))
		d.Aircraft = d.Aircraft[:MaxDocAircraft]
	}
	for i := range d.Aircraft {
		e.takeRecord(ctx, &d.Aircraft[i], d.Now, rx)
	}
	return nil
}

func ptr[T any](v T) *T { return &v }

func times(v *float64, k float64) *float64 {
	if v == nil || !core.IsFinite(*v) {
		return nil
	}
	return ptr(*v * k)
}

// emergencySquawks are the Mode A codes of an emergency: unlawful
// interference, radio failure, general emergency.
var emergencySquawks = map[string]bool{"7500": true, "7600": true, "7700": true}

// emergencyOf is the emergency flag of a squawk and the feed's own flag:
// true for 7500/7600/7700 or a flag other than none, false for a known
// normal state, nil when the feed says neither.
func emergencyOf(squawk, flag *string) *bool {
	if squawk != nil && emergencySquawks[*squawk] {
		return ptr(true)
	}
	if flag != nil {
		return ptr(*flag != "none" && *flag != "")
	}
	if squawk != nil {
		return ptr(false)
	}
	return nil
}

// callsignOf is a broadcast callsign trimmed, nil when empty or longer
// than the schema allows.
func callsignOf(s *string) *string {
	if s == nil {
		return nil
	}
	c := strings.TrimSpace(*s)
	if c == "" || len(c) > MaxCallsignBytes {
		return nil
	}
	return &c
}

// RecordBody maps one aircraft.json entry onto the schema's body: the
// icao24 lower-cased (a non-ICAO address, "~" first, is refused), the
// callsign trimmed, the position, alt_pressure_m from alt_baro in feet
// (none on the ground), alt_wgs84_m from alt_geom in feet, gs_ms from
// knots, track_deg, vrate_ms from the barometric (else geometric) rate
// in feet per minute, the emergency from the squawk or the feed's flag,
// the source class (Mode S multilateration, else ADS-B) and NIC/NACp as
// the quality; trust broadcast, source adsb_rx, state live. The second
// value is the position's own age in seconds (seen_pos, else seen).
func RecordBody(a *AircraftRecord, receiverID string) (Body, float64, error) {
	hex := strings.ToLower(strings.TrimSpace(a.Hex))
	if strings.HasPrefix(hex, "~") {
		return Body{}, 0, errNonICAO
	}
	if a.Lat == nil || a.Lon == nil {
		return Body{}, 0, errNoPosition
	}
	b := Body{
		ICAO24: hex, Callsign: callsignOf(a.Flight), Position: Position{Lat: *a.Lat, Lng: *a.Lon},
		AltWGS84M: times(a.AltGeom, core.FeetToMetres), GSMS: times(a.GS, KnotsToMS), TrackDeg: a.Track,
		VRateMS: times(a.BaroRate, FeetPerMinToMS), Emergency: emergencyOf(a.Squawk, a.Emergency),
		SourceClass: "ads_b", Trust: core.TrustBroadcast, Source: SourceAdsbRx, SourceInstance: receiverID, State: StateLive,
	}
	if b.VRateMS == nil {
		b.VRateMS = times(a.GeomRate, FeetPerMinToMS)
	}
	if a.Squawk != nil && squawkRe.MatchString(*a.Squawk) {
		b.Squawk = a.Squawk
	}
	var baro float64
	if json.Unmarshal(a.AltBaro, &baro) == nil && core.IsFinite(baro) {
		b.AltPressureM = ptr(baro * core.FeetToMetres)
	}
	for _, m := range a.MLAT {
		if m == "lat" || m == "lon" {
			b.SourceClass = "mode_s"
		}
	}
	if a.NIC != nil || a.NACp != nil {
		q, _ := json.Marshal(struct {
			NIC  *int `json:"nic,omitempty"`
			NACp *int `json:"nac_p,omitempty"`
		}{a.NIC, a.NACp})
		b.Quality = q
	}
	if b.TrackDeg != nil && *b.TrackDeg == 360 {
		b.TrackDeg = ptr(0.0)
	}
	age := 0.0
	switch {
	case a.SeenPos != nil:
		age = *a.SeenPos
	case a.Seen != nil:
		age = *a.Seen
	}
	if !core.IsFinite(age) || age < 0 {
		age = 0
	}
	if err := b.Check(); err != nil {
		return Body{}, 0, err
	}
	return b, age, nil
}

var (
	errNonICAO    = errors.New("not an ICAO address")
	errNoPosition = errors.New("no position")
)

// takeRecord publishes one aircraft.json entry arrived at rx, placed at
// arrival less its own age (T-12), with the receiver's time of it as ts.
func (e *Econspicuity) takeRecord(ctx context.Context, a *AircraftRecord, feedNow float64, rx time.Time) {
	e.Counters.Inc(CounterAdsbRecords)
	b, age, err := RecordBody(a, e.ReceiverID)
	switch {
	case errors.Is(err, errNonICAO):
		e.Counters.Inc(CounterAdsbNonICAO)
		return
	case errors.Is(err, errNoPosition):
		e.Counters.Inc(CounterAdsbNoPosition)
		return
	case err != nil:
		e.Counters.Inc(CounterAdsbRefused)
		e.mu.Lock()
		e.refused++
		e.mu.Unlock()
		return
	case age > MaxSeenS:
		e.Counters.Inc(CounterAdsbTooOld)
		return
	}
	captured := rx.Add(-time.Duration(age * float64(time.Second)))
	t := core.Times{RxTS: rx, CapturedAt: captured, Source: core.TimeReceiver}
	if feedNow > 0 {
		sec, frac := math.Modf(feedNow - age)
		ts := time.Unix(int64(sec), int64(frac*1e9)).UTC()
		t.TS = &ts
	}
	e.emit(ctx, b, a.R, t)
}

func (e *Econspicuity) emit(ctx context.Context, b Body, registration *string, t core.Times) {
	if b.AltWGS84M == nil && b.AltPressureM != nil {
		e.Counters.Inc(CounterAdsbPressureOnly)
	}
	out := &Track{Envelope: bus.NewEnvelope(SchemaManned, ProducerEcon, t), Body: b}
	echo, err := publish(ctx, e.Sink, e.Own, e.Counters, out, registration)
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case echo != "":
		e.echoes++
	case err != nil:
		e.refused++
	default:
		e.accepted++
		e.lastRecord = t.RxTS
	}
}

// runFile replays a recorded file: one aircraft.json document per line,
// each sent when as much time has passed since the first as passed
// between their own now, every one placed on our clock (re-based).
func (e *Econspicuity) runFile(ctx context.Context, path string) {
	f, err := os.Open(path) // #nosec G304 -- the operator's own configured replay file
	if err != nil {
		e.failed(fmt.Errorf("replay file: %w", err))
		return
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), MaxDocBytes)
	var first float64
	start := e.now()
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var probe struct {
			Now float64 `json:"now"`
		}
		if json.Unmarshal(line, &probe) != nil || !core.IsFinite(probe.Now) {
			e.Counters.Inc(CounterAdsbUnreadable)
			continue
		}
		if first == 0 {
			first = probe.Now
		}
		if wait := time.Duration((probe.Now-first)*float64(time.Second)) - e.now().Sub(start); wait > 0 && !sleep(ctx, wait) {
			return
		}
		if !e.waitEnabled(ctx) {
			return
		}
		if err := e.replayLine(ctx, line, first, start); err != nil {
			e.Counters.Inc(CounterAdsbUnreadable)
		}
	}
	if err := sc.Err(); err != nil {
		e.failed(fmt.Errorf("replay file: %w", err))
	}
	e.mu.Lock()
	e.finished = e.now()
	e.mu.Unlock()
	e.logger().LogAttrs(ctx, slog.LevelInfo, "e-conspicuity replay finished", slog.String("file", path))
}

// replayLine takes one recorded document with its now re-based onto our
// clock (start + its offset from the first).
func (e *Econspicuity) replayLine(ctx context.Context, line []byte, first float64, start time.Time) error {
	var d map[string]json.RawMessage
	if err := json.Unmarshal(line, &d); err != nil {
		return err
	}
	var now float64
	if err := json.Unmarshal(d["now"], &now); err != nil {
		return err
	}
	rebased := float64(start.UnixNano())/1e9 + (now - first)
	d["now"], _ = json.Marshal(rebased)
	doc, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return e.TakeDocument(ctx, doc, e.now())
}

// sbsAircraft is what the BaseStation lines said of one aircraft.
type sbsAircraft struct {
	callsign, squawk   *string
	emergency          *bool
	gsMS, trackDeg, vr *float64
	velAt, idAt, seen  time.Time
}

// runSBS reads a BaseStation stream, reconnecting with backoff; a
// switched-off receiver closes the connection at once.
func (e *Econspicuity) runSBS(ctx context.Context, addr string) {
	retryMin := every(e.RetryMin, ansPRetryMin)
	retry := retryMin
	for ctx.Err() == nil {
		if !e.waitEnabled(ctx) {
			return
		}
		read, err := e.sbsSession(ctx, addr)
		if ctx.Err() != nil {
			return
		}
		if read {
			retry = retryMin
		}
		e.failed(err)
		if !sleep(ctx, retry) {
			return
		}
		retry = min(retry*2, ansPRetryMax)
	}
}

func (e *Econspicuity) sbsSession(ctx context.Context, addr string) (read bool, err error) {
	dial := e.Dial
	if dial == nil {
		d := &net.Dialer{Timeout: ansPDialTimeout}
		dial = d.DialContext
	}
	conn, err := dial(ctx, "tcp", addr)
	if err != nil {
		return false, err
	}
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		t := time.NewTicker(every(e.GateEvery, DefaultGateEvery))
		defer t.Stop()
		for {
			select {
			case <-sctx.Done():
				_ = conn.Close()
				return
			case <-t.C:
			}
			if !e.decision().Enabled {
				e.Counters.Inc(CounterAdsbSwitchedOff)
				_ = conn.Close()
				return
			}
		}
	}()
	r := bufio.NewReaderSize(conn, MaxSBSLineBytes)
	silence := time.Duration(e.policy().MannedUnavailableS * float64(time.Second))
	for {
		_ = conn.SetReadDeadline(e.now().Add(silence))
		line, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			e.Counters.Inc(CounterAdsbUnreadable)
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = r.ReadSlice('\n')
			}
			continue
		}
		if err != nil {
			return read, err
		}
		read = true
		e.ok(0)
		e.TakeSBS(sctx, string(line), e.now())
	}
}

// TakeSBS reads one BaseStation line arrived at rx: identity (MSG 1),
// velocity (MSG 4) and squawk (MSG 6) are kept per aircraft for
// sbsHoldS and joined to the positions (MSG 2 and 3), which are
// published, placed at arrival (the lines' local times carry no zone,
// T-12). A line that does not read is counted.
func (e *Econspicuity) TakeSBS(ctx context.Context, line string, rx time.Time) {
	e.init()
	f := strings.Split(strings.TrimRight(line, "\r\n"), ",")
	if len(f) < 22 || f[0] != "MSG" {
		e.Counters.Inc(CounterAdsbUnreadable)
		return
	}
	hex := strings.ToLower(strings.TrimSpace(f[4]))
	if !icao24Re.MatchString(hex) {
		e.Counters.Inc(CounterAdsbNonICAO)
		return
	}
	num := func(i int) *float64 {
		v, err := strconv.ParseFloat(strings.TrimSpace(f[i]), 64)
		if err != nil || !core.IsFinite(v) {
			return nil
		}
		return &v
	}
	flag := func(i int) *bool {
		switch strings.TrimSpace(f[i]) {
		case "-1", "1":
			return ptr(true)
		case "0":
			return ptr(false)
		}
		return nil
	}
	e.mu.Lock()
	if e.sbs == nil {
		e.sbs = map[string]*sbsAircraft{}
	}
	ac := e.sbs[hex]
	if ac == nil {
		if len(e.sbs) >= MaxSBSAircraft {
			e.forgetSBSLocked(rx)
		}
		if len(e.sbs) >= MaxSBSAircraft {
			e.mu.Unlock()
			e.Counters.Inc(CounterAdsbOverBound)
			return
		}
		ac = &sbsAircraft{}
		e.sbs[hex] = ac
	}
	ac.seen = rx
	switch strings.TrimSpace(f[1]) {
	case "1":
		ac.callsign, ac.idAt = callsignOf(&f[10]), rx
	case "4":
		ac.gsMS, ac.trackDeg, ac.vr, ac.velAt = times(num(12), KnotsToMS), num(13), times(num(16), FeetPerMinToMS), rx
	case "6":
		if sq := strings.TrimSpace(f[17]); squawkRe.MatchString(sq) {
			ac.squawk = &sq
		}
		if em := flag(19); em != nil {
			ac.emergency = em
		}
		ac.idAt = rx
	case "2", "3":
		lat, lon := num(14), num(15)
		if lat == nil || lon == nil {
			e.mu.Unlock()
			e.Counters.Inc(CounterAdsbNoPosition)
			return
		}
		b := Body{ICAO24: hex, Position: Position{Lat: *lat, Lng: *lon}, AltPressureM: times(num(11), core.FeetToMetres),
			SourceClass: "ads_b", Trust: core.TrustBroadcast, Source: SourceAdsbRx, SourceInstance: e.ReceiverID, State: StateLive}
		if em := flag(19); em != nil {
			ac.emergency = em
		}
		if rx.Sub(ac.idAt) <= sbsHoldS*time.Second {
			b.Callsign, b.Squawk = ac.callsign, ac.squawk
		}
		b.Emergency = ac.emergency
		if ac.squawk != nil && emergencySquawks[*ac.squawk] {
			b.Emergency = ptr(true)
		}
		if rx.Sub(ac.velAt) <= sbsHoldS*time.Second {
			b.GSMS, b.TrackDeg, b.VRateMS = ac.gsMS, ac.trackDeg, ac.vr
		}
		if gs := times(num(12), KnotsToMS); gs != nil {
			b.GSMS = gs
		}
		if td := num(13); td != nil {
			b.TrackDeg = td
		}
		e.mu.Unlock()
		e.Counters.Inc(CounterAdsbRecords)
		if err := b.Check(); err != nil {
			e.Counters.Inc(CounterAdsbRefused)
			e.mu.Lock()
			e.refused++
			e.mu.Unlock()
			return
		}
		e.emit(ctx, b, nil, core.Times{RxTS: rx, CapturedAt: rx, Source: core.TimeSystem})
		return
	}
	e.mu.Unlock()
}

// forgetSBSLocked drops the aircraft not heard for sbsHoldS.
func (e *Econspicuity) forgetSBSLocked(now time.Time) {
	for k, a := range e.sbs {
		if now.Sub(a.seen) > sbsHoldS*time.Second {
			delete(e.sbs, k)
		}
	}
}

// Status is the receiver's src.v1 body now: disabled while switched
// off; down (unavailable) once manned_unavailable_s passed without a
// read, since the last one (or the start), and once a replay ended;
// stale while the receiver answers with a clock that does not move;
// live otherwise.
func (e *Econspicuity) Status() sources.StatusBody {
	e.init()
	dec := e.decision()
	now := e.now()
	silence := time.Duration(e.policy().MannedUnavailableS * float64(time.Second))
	inst := e.ReceiverID
	e.mu.Lock()
	defer e.mu.Unlock()
	b := sources.StatusBody{Source: SourceAdsbRx, SourceInstance: &inst,
		Counters: map[string]uint64{"accepted": e.accepted, "refused": e.refused, "echo_own_flight": e.echoes}}
	if !e.lastRecord.IsZero() {
		b.AgeS = ptr(max(0, now.Sub(e.lastRecord).Seconds()))
	}
	var since time.Time
	switch {
	case b.Disabled(dec):
		if e.offSince.IsZero() {
			e.offSince = now
		}
		since, b.Detail = e.offSince, "adsb_rx is switched off: the receiver is not read"
	case !e.finished.IsZero():
		b.State, since, b.Detail = sources.StateDown, e.finished, "the replay file ended"
	case e.lastOK.IsZero():
		b.State, since, b.Detail = sources.StateDown, e.started, "unavailable: the receiver has not answered yet"
		if e.lastErr != "" {
			b.Detail += " (" + e.lastErr + ")"
		}
	case now.Sub(e.lastOK) > silence:
		b.State, since = sources.StateDown, e.lastOK
		b.Detail = fmt.Sprintf("unavailable: no answer from the receiver for %.0f s", now.Sub(e.lastOK).Seconds())
		if e.lastErr != "" {
			b.Detail += " (" + e.lastErr + ")"
		}
	case e.feedNow > 0 && now.Sub(e.feedMoved) > silence:
		b.State, since, b.Detail = sources.StateStale, e.feedMoved, "the receiver answers, but its clock has not moved"
	default:
		b.State, b.Detail = sources.StateLive, "receiving"
	}
	if dec.Enabled {
		e.offSince = time.Time{}
	}
	if b.State != e.agg {
		e.agg, e.aggSince = b.State, now
	}
	if since.IsZero() {
		since = e.aggSince
	}
	b.Since = bus.Stamp{Time: since.UTC()}
	return b
}

func (e *Econspicuity) statusLoop(ctx context.Context) {
	t := time.NewTicker(every(e.StatusEvery, DefaultStatusEvery))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if e.Sink == nil {
			continue
		}
		if err := sources.PublishStatus(ctx, e.Sink, ProducerEcon, e.now(), e.Status()); err != nil {
			e.Counters.Inc(CounterAdsbStatusFailed)
		}
	}
}

// Probe is the readiness of the receiver (adsb_rx on /readyz).
func (e *Econspicuity) Probe(context.Context) (obs.State, string) {
	b := e.Status()
	e.mu.Lock()
	tail := fmt.Sprintf("; %d tracks published on man.v1 as broadcast, %d refused, %d echoes of own flights left out", e.accepted, e.refused, e.echoes)
	e.mu.Unlock()
	since := b.Since.UTC().Format(time.RFC3339)
	switch b.State {
	case sources.StateDown:
		return obs.StateDown, "unavailable since " + since + ": " + b.Detail + tail
	case sources.StateDisabled, sources.StateStale:
		return obs.StateDegraded, b.State + " since " + since + ": " + b.Detail + tail
	}
	return obs.StateUp, "live since " + since + tail
}
