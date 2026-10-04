// Package fakedss is an in-memory InterUSS DSS for tests (docs/PLAN.md
// §10; briefs WP-9, WP-13): the subset of the DSS API this USSP calls,
// with the DSS's version semantics, subscriber lists and a Down switch.
//
// WP-9 brings the F3411 side under /rid/v2/dss: identification service
// areas (create, read, update and delete by version, search by area and
// time) and subscriptions (create, read, update by version (WP-14),
// delete), each change answered with
// the subscribers to notify and their notification indexes, as the
// pinned standard file (api/standards/f3411-v22a.yaml) and core's f3411
// types shape them. WP-13 adds the F3548 side to the same fake (one
// fake, not two) under /dss/v1: operational intent references with ovn
// and key semantics, constraint references, subscriptions with their
// notification indexes, and the USS availability (f3548.go).
//
// It checks only what a test needs it to check: a bearer token on every
// call (recorded for the test to read), the version on an update or a
// delete, a start in the past, a missing end, more than MaxEntries
// entities. Geometry is the boxes core's Volume4DToZonesEnvelope gives.
package fakedss

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3411"
	"github.com/rootxkit/uspace-core/geodesy"
)

// MaxEntries bounds the ISAs and the subscriptions the fake holds.
const MaxEntries = 10_000

// maxBodyBytes bounds a request body read.
const maxBodyBytes = 1 << 20

// startTolerance is how far in the past a start may be before it is
// refused (the file: the DSS "may adjust very recent start times").
const startTolerance = 30 * time.Second

type isa struct {
	area  f3411.IdentificationServiceArea
	ext   f3411.Volume4D
	box   geodesy.BBox
	start time.Time
	end   time.Time
}

type sub struct {
	s     f3411.Subscription
	box   geodesy.BBox
	start time.Time
	end   time.Time
}

// Call is one request the fake answered.
type Call struct {
	Method, Path, Authorization string
	Status                      int
}

// DSS is the fake. Use New; Close when done.
type DSS struct {
	srv *httptest.Server
	// Owner is the owner recorded for every entity (a real DSS takes it
	// from the token's sub).
	Owner string
	Now   func() time.Time

	mu    sync.Mutex
	down  bool
	isas  map[string]*isa
	subs  map[string]*sub
	calls []Call
	seq   int
	// utm is the F3548 side (f3548.go); conflicts counts its 409s.
	utm       *utm
	conflicts int
}

// New starts the fake on a local port.
func New() *DSS {
	d := &DSS{Owner: "ussp-test", isas: map[string]*isa{}, subs: map[string]*sub{}, utm: newUTM()}
	d.srv = httptest.NewServer(http.HandlerFunc(d.serve))
	return d
}

// URL is the DSS base URL (USSP_DSS_BASE_URL): the F3411 operations are
// under /rid/v2, the F3548 ones under /dss/v1.
func (d *DSS) URL() string { return d.srv.URL }

// Close stops the fake.
func (d *DSS) Close() { d.srv.Close() }

// Down makes every call fail with 503 (down true) or be served again.
func (d *DSS) Down(down bool) {
	d.mu.Lock()
	d.down = down
	d.mu.Unlock()
}

// Calls returns the requests answered so far.
func (d *DSS) Calls() []Call {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]Call(nil), d.calls...)
}

// ISAs returns the ISAs held, by id.
func (d *DSS) ISAs() map[string]f3411.IdentificationServiceArea {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]f3411.IdentificationServiceArea, len(d.isas))
	for id, i := range d.isas {
		out[id] = i.area
	}
	return out
}

// Extents is the extents of the ISA id; false when it is not held.
func (d *DSS) Extents(id string) (f3411.Volume4D, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	i, ok := d.isas[id]
	if !ok {
		return f3411.Volume4D{}, false
	}
	return i.ext, true
}

func (d *DSS) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *DSS) version() string {
	d.seq++
	var b [4]byte
	_, _ = rand.Read(b[:])
	return strconv.Itoa(d.seq) + "-" + hex.EncodeToString(b[:])
}

const prefix = "/rid/v2/dss"

const prefix3548 = "/dss/v1"

func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, format string, a ...any) {
	m := fmt.Sprintf(format, a...)
	write(w, status, f3411.ErrorResponse{Message: &m})
}

type recorder struct {
	http.ResponseWriter
	status int
}

// WriteHeader records the status.
func (r *recorder) WriteHeader(s int) { r.status = s; r.ResponseWriter.WriteHeader(s) }

func (d *DSS) serve(w http.ResponseWriter, r *http.Request) {
	rec := &recorder{ResponseWriter: w, status: http.StatusOK}
	d.mu.Lock()
	defer func() {
		d.calls = append(d.calls, Call{Method: r.Method, Path: r.URL.Path, Authorization: r.Header.Get("Authorization"), Status: rec.status})
		if len(d.calls) > MaxEntries {
			d.calls = d.calls[len(d.calls)-MaxEntries:]
		}
		d.mu.Unlock()
	}()
	if d.down {
		fail(rec, http.StatusServiceUnavailable, "the fake DSS is down")
		return
	}
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		fail(rec, http.StatusUnauthorized, "no bearer token")
		return
	}
	if p, ok := strings.CutPrefix(r.URL.Path, prefix3548); ok {
		d.serve3548(rec, r, p)
		return
	}
	path, ok := strings.CutPrefix(r.URL.Path, prefix)
	if !ok {
		fail(rec, http.StatusNotFound, "not a DSS path")
		return
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case parts[0] == "identification_service_areas" && len(parts) == 1 && r.Method == http.MethodGet:
		d.searchISAs(rec, r)
	case parts[0] == "identification_service_areas" && len(parts) == 2 && r.Method == http.MethodGet:
		d.getISA(rec, parts[1])
	case parts[0] == "identification_service_areas" && len(parts) == 2 && r.Method == http.MethodPut:
		d.putISA(rec, r, parts[1], nil)
	case parts[0] == "identification_service_areas" && len(parts) == 3 && r.Method == http.MethodPut:
		d.putISA(rec, r, parts[1], &parts[2])
	case parts[0] == "identification_service_areas" && len(parts) == 3 && r.Method == http.MethodDelete:
		d.deleteISA(rec, parts[1], parts[2])
	case parts[0] == "subscriptions" && len(parts) == 2 && r.Method == http.MethodPut:
		d.putSubscription(rec, r, parts[1])
	case parts[0] == "subscriptions" && len(parts) == 2 && r.Method == http.MethodGet:
		d.getSubscription(rec, parts[1])
	case parts[0] == "subscriptions" && len(parts) == 3 && r.Method == http.MethodPut:
		d.updateSubscription(rec, r, parts[1], parts[2])
	case parts[0] == "subscriptions" && len(parts) == 3 && r.Method == http.MethodDelete:
		d.deleteSubscription(rec, parts[1], parts[2])
	default:
		fail(rec, http.StatusNotFound, "not served by the fake DSS")
	}
}

func decode(r *http.Request, v any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil || len(b) > maxBodyBytes {
		return fmt.Errorf("body not read")
	}
	return json.Unmarshal(b, v)
}

// window checks an extent's box and times: an end is required, a start
// in the past beyond startTolerance is refused, a missing start is now.
func (d *DSS) window(ext f3411.Volume4D) (geodesy.BBox, time.Time, time.Time, error) {
	box, start, end, err := f3411.Volume4DToZonesEnvelope(ext)
	if err != nil {
		return box, start, end, err
	}
	now := d.now()
	switch {
	case end.IsZero():
		return box, start, end, core.Fieldf("extents.time_end", "required")
	case start.IsZero():
		start = now
	case start.Before(now.Add(-startTolerance)):
		return box, start, end, core.Fieldf("extents.time_start", "in the past")
	}
	if !end.After(start) {
		return box, start, end, core.Fieldf("extents.time_end", "not after time_start")
	}
	return box, start, end, nil
}

func overlaps(a, b geodesy.BBox) bool {
	return a.MinLat <= b.MaxLat && b.MinLat <= a.MaxLat && a.MinLon <= b.MaxLon && b.MinLon <= a.MaxLon
}

// subscribersLocked are the subscriptions touching box in [start, end],
// each with its notification index advanced, grouped by base URL.
func (d *DSS) subscribersLocked(box geodesy.BBox, start, end time.Time) []f3411.SubscriberToNotify {
	byURL := map[string]*f3411.SubscriberToNotify{}
	var order []string
	for _, s := range d.subs {
		if !overlaps(box, s.box) || end.Before(s.start) || start.After(s.end) {
			continue
		}
		n := int32(0)
		if s.s.NotificationIndex != nil {
			n = *s.s.NotificationIndex
		}
		n++
		s.s.NotificationIndex = &n
		idx := n
		u := s.s.UssBaseUrl
		if byURL[u] == nil {
			byURL[u] = &f3411.SubscriberToNotify{Url: u}
			order = append(order, u)
		}
		byURL[u].Subscriptions = append(byURL[u].Subscriptions, f3411.SubscriptionState{SubscriptionId: s.s.Id, NotificationIndex: &idx})
	}
	out := make([]f3411.SubscriberToNotify, 0, len(order))
	for _, u := range order {
		out = append(out, *byURL[u])
	}
	return out
}

func wireTime(t time.Time) f3411.Time { return f3411.Time{Format: f3411.RFC3339, Value: t.UTC()} }

func (d *DSS) putISA(w http.ResponseWriter, r *http.Request, id string, version *string) {
	var p f3411.CreateIdentificationServiceAreaParameters
	if err := decode(r, &p); err != nil || p.UssBaseUrl == "" {
		fail(w, http.StatusBadRequest, "not CreateIdentificationServiceAreaParameters")
		return
	}
	box, start, end, err := d.window(p.Extents)
	if err != nil {
		fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	cur, exists := d.isas[id]
	switch {
	case version == nil && exists:
		fail(w, http.StatusConflict, "ISA %s already exists", id)
		return
	case version != nil && !exists:
		fail(w, http.StatusNotFound, "ISA %s not found", id)
		return
	case version != nil && cur.area.Version != *version:
		fail(w, http.StatusConflict, "version %s is not the current one", *version)
		return
	case !exists && len(d.isas) >= MaxEntries:
		fail(w, http.StatusTooManyRequests, "the fake DSS is full")
		return
	}
	a := f3411.IdentificationServiceArea{Id: id, Owner: d.Owner, TimeStart: wireTime(start), TimeEnd: wireTime(end),
		UssBaseUrl: p.UssBaseUrl, Version: d.version()}
	d.isas[id] = &isa{area: a, ext: p.Extents, box: box, start: start, end: end}
	subs := d.subscribersLocked(box, start, end)
	write(w, http.StatusOK, f3411.PutIdentificationServiceAreaResponse{ServiceArea: a, Subscribers: &subs})
}

func (d *DSS) getISA(w http.ResponseWriter, id string) {
	i, ok := d.isas[id]
	if !ok {
		fail(w, http.StatusNotFound, "ISA %s not found", id)
		return
	}
	write(w, http.StatusOK, f3411.GetIdentificationServiceAreaResponse{ServiceArea: i.area})
}

func (d *DSS) deleteISA(w http.ResponseWriter, id, version string) {
	i, ok := d.isas[id]
	switch {
	case !ok:
		fail(w, http.StatusNotFound, "ISA %s not found", id)
		return
	case i.area.Version != version:
		fail(w, http.StatusConflict, "version %s is not the current one", version)
		return
	}
	delete(d.isas, id)
	subs := d.subscribersLocked(i.box, i.start, i.end)
	write(w, http.StatusOK, f3411.DeleteIdentificationServiceAreaResponse{ServiceArea: i.area, Subscribers: &subs})
}

// searchISAs is GET identification_service_areas?area=lat,lng,...: the
// ISAs whose box touches the area's box (and the time window when given).
func (d *DSS) searchISAs(w http.ResponseWriter, r *http.Request) {
	box, err := areaBox(r.URL.Query().Get("area"))
	if err != nil {
		fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	var from, to time.Time
	if v := r.URL.Query().Get("earliest_time"); v != "" {
		from, _ = time.Parse(time.RFC3339Nano, v)
	}
	if v := r.URL.Query().Get("latest_time"); v != "" {
		to, _ = time.Parse(time.RFC3339Nano, v)
	}
	out := []f3411.IdentificationServiceArea{}
	for _, i := range d.isas {
		if !overlaps(box, i.box) || (!from.IsZero() && i.end.Before(from)) || (!to.IsZero() && i.start.After(to)) {
			continue
		}
		out = append(out, i.area)
	}
	write(w, http.StatusOK, f3411.SearchIdentificationServiceAreasResponse{ServiceAreas: &out})
}

// areaBox reads a GeoPolygonString (lat,lng pairs) as its box.
func areaBox(s string) (geodesy.BBox, error) {
	parts := strings.Split(s, ",")
	if len(parts) < 6 || len(parts)%2 != 0 || len(parts) > 2*1000 {
		return geodesy.BBox{}, fmt.Errorf("area must be at least three lat,lng pairs")
	}
	var ring geodesy.Ring
	for i := 0; i < len(parts); i += 2 {
		lat, err1 := strconv.ParseFloat(parts[i], 64)
		lng, err2 := strconv.ParseFloat(parts[i+1], 64)
		p := core.LatLon{LatDeg: lat, LonDeg: lng}
		if err1 != nil || err2 != nil || !p.Valid() {
			return geodesy.BBox{}, fmt.Errorf("area vertex %d is not a position", i/2)
		}
		ring = append(ring, p)
	}
	ring = append(ring, ring[0])
	return geodesy.Polygon{Rings: []geodesy.Ring{ring}}.BBox(), nil
}

func (d *DSS) putSubscription(w http.ResponseWriter, r *http.Request, id string) {
	var p f3411.CreateSubscriptionParameters
	if err := decode(r, &p); err != nil || p.UssBaseUrl == "" {
		fail(w, http.StatusBadRequest, "not CreateSubscriptionParameters")
		return
	}
	box, start, end, err := d.window(p.Extents)
	if err != nil {
		fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	if _, exists := d.subs[id]; exists {
		fail(w, http.StatusConflict, "subscription %s already exists", id)
		return
	}
	if len(d.subs) >= MaxEntries {
		fail(w, http.StatusTooManyRequests, "the fake DSS is full")
		return
	}
	zero := int32(0)
	ts, te := wireTime(start), wireTime(end)
	s := f3411.Subscription{Id: id, Owner: d.Owner, UssBaseUrl: p.UssBaseUrl, Version: d.version(), NotificationIndex: &zero,
		TimeStart: &ts, TimeEnd: &te}
	d.subs[id] = &sub{s: s, box: box, start: start, end: end}
	var areas []f3411.IdentificationServiceArea
	for _, i := range d.isas {
		if overlaps(box, i.box) {
			areas = append(areas, i.area)
		}
	}
	write(w, http.StatusOK, f3411.PutSubscriptionResponse{Subscription: s, ServiceAreas: &areas})
}

func (d *DSS) getSubscription(w http.ResponseWriter, id string) {
	s, ok := d.subs[id]
	if !ok {
		fail(w, http.StatusNotFound, "subscription %s not found", id)
		return
	}
	write(w, http.StatusOK, f3411.GetSubscriptionResponse{Subscription: s.s})
}

// updateSubscription is PUT subscriptions/{id}/{version} (WP-14: a
// Display Provider renews its subscription before its 24 h end).
func (d *DSS) updateSubscription(w http.ResponseWriter, r *http.Request, id, version string) {
	var p f3411.UpdateSubscriptionParameters
	if err := decode(r, &p); err != nil || p.UssBaseUrl == "" {
		fail(w, http.StatusBadRequest, "not UpdateSubscriptionParameters")
		return
	}
	box, start, end, err := d.window(p.Extents)
	if err != nil {
		fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	cur, ok := d.subs[id]
	switch {
	case !ok:
		fail(w, http.StatusNotFound, "subscription %s not found", id)
		return
	case cur.s.Version != version:
		fail(w, http.StatusConflict, "version %s is not the current one", version)
		return
	}
	ts, te := wireTime(start), wireTime(end)
	cur.s.UssBaseUrl, cur.s.Version, cur.s.TimeStart, cur.s.TimeEnd = p.UssBaseUrl, d.version(), &ts, &te
	cur.box, cur.start, cur.end = box, start, end
	var areas []f3411.IdentificationServiceArea
	for _, i := range d.isas {
		if overlaps(box, i.box) {
			areas = append(areas, i.area)
		}
	}
	write(w, http.StatusOK, f3411.PutSubscriptionResponse{Subscription: cur.s, ServiceAreas: &areas})
}

// Subscriptions returns the F3411 subscriptions held, by id.
func (d *DSS) Subscriptions() map[string]f3411.Subscription {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]f3411.Subscription, len(d.subs))
	for id, s := range d.subs {
		out[id] = s.s
	}
	return out
}

func (d *DSS) deleteSubscription(w http.ResponseWriter, id, version string) {
	s, ok := d.subs[id]
	switch {
	case !ok:
		fail(w, http.StatusNotFound, "subscription %s not found", id)
		return
	case s.s.Version != version:
		fail(w, http.StatusConflict, "version %s is not the current one", version)
		return
	}
	delete(d.subs, id)
	write(w, http.StatusOK, f3411.DeleteSubscriptionResponse{Subscription: s.s})
}
