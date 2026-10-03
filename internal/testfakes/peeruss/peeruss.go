// Package peeruss is a fake peer USSP for tests (brief WP-13): another
// USS of the F3548 ecosystem. It files its operational intents in a DSS
// with the key protocol (it reads the ovns of the intents it meets from
// their managers, as a real peer would), serves their details at GET
// /uss/v1/operational_intents/{id}, records every notification POSTed to
// /uss/v1/operational_intents with the time it arrived, and can manage a
// constraint (GET /uss/v1/constraints/{id}). Down makes its server answer
// 503; Slow delays every answer.
package peeruss

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/f3548"
)

// TokenFunc gives a bearer token for a call to base with scope (the
// fake signs its own; aud is the host of base).
type TokenFunc func(base string, scope f3548.Scope) string

// Notification is one notification the peer received.
type Notification struct {
	At   time.Time
	Body f3548.PutOperationalIntentDetailsParameters
	Auth string
}

// Fake is a running fake peer USSP.
type Fake struct {
	// Manager is its client id at the DSS (the sub of its tokens).
	Manager string
	DSSURL  string
	Token   TokenFunc
	HTTP    *http.Client

	srv *httptest.Server

	mu      sync.Mutex
	down    bool
	slow    time.Duration
	intents map[string]f3548.OperationalIntent
	cstrs   map[string]f3548.Constraint
	notes   []Notification
	cnotes  []f3548.PutConstraintDetailsParameters
	served  int
}

// New starts a fake peer filing in the DSS at dssURL.
func New(manager, dssURL string, tok TokenFunc) *Fake {
	f := &Fake{Manager: manager, DSSURL: strings.TrimRight(dssURL, "/"), Token: tok, HTTP: &http.Client{Timeout: 5 * time.Second},
		intents: map[string]f3548.OperationalIntent{}, cstrs: map[string]f3548.Constraint{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

// URL is its uss_base_url.
func (f *Fake) URL() string { return f.srv.URL }

// Close stops it.
func (f *Fake) Close() { f.srv.Close() }

// Down makes it answer 503 (down true) or serve again.
func (f *Fake) Down(down bool) { f.mu.Lock(); f.down = down; f.mu.Unlock() }

// Slow delays every answer by d.
func (f *Fake) Slow(d time.Duration) { f.mu.Lock(); f.slow = d; f.mu.Unlock() }

// Notifications are the notifications received so far.
func (f *Fake) Notifications() []Notification {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Notification(nil), f.notes...)
}

// Served is how many details requests it answered 200.
func (f *Fake) Served() int { f.mu.Lock(); defer f.mu.Unlock(); return f.served }

func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	down, slow := f.down, f.slow
	f.mu.Unlock()
	if slow > 0 {
		t := time.NewTimer(slow)
		select {
		case <-t.C:
		case <-r.Context().Done():
			t.Stop()
			return
		}
	}
	if down {
		write(w, http.StatusServiceUnavailable, f3548.ErrorResponse{})
		return
	}
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		write(w, http.StatusUnauthorized, f3548.ErrorResponse{})
		return
	}
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/uss/v1/operational_intents/"):
		id := strings.TrimPrefix(r.URL.Path, "/uss/v1/operational_intents/")
		f.mu.Lock()
		oi, ok := f.intents[id]
		if ok {
			f.served++
		}
		f.mu.Unlock()
		if !ok {
			write(w, http.StatusNotFound, f3548.ErrorResponse{})
			return
		}
		write(w, http.StatusOK, f3548.GetOperationalIntentDetailsResponse{OperationalIntent: oi})
	case r.Method == http.MethodPost && r.URL.Path == "/uss/v1/operational_intents":
		var p f3548.PutOperationalIntentDetailsParameters
		b, _ := io.ReadAll(io.LimitReader(r.Body, f3548.MaxMessageBytes))
		if json.Unmarshal(b, &p) != nil {
			write(w, http.StatusBadRequest, f3548.ErrorResponse{})
			return
		}
		f.mu.Lock()
		f.notes = append(f.notes, Notification{At: time.Now(), Body: p, Auth: r.Header.Get("Authorization")})
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/uss/v1/constraints/"):
		id := strings.TrimPrefix(r.URL.Path, "/uss/v1/constraints/")
		f.mu.Lock()
		c, ok := f.cstrs[id]
		f.mu.Unlock()
		if !ok {
			write(w, http.StatusNotFound, f3548.ErrorResponse{})
			return
		}
		write(w, http.StatusOK, f3548.GetConstraintDetailsResponse{Constraint: c})
	case r.Method == http.MethodPost && r.URL.Path == "/uss/v1/constraints":
		var p f3548.PutConstraintDetailsParameters
		if json.NewDecoder(io.LimitReader(r.Body, f3548.MaxMessageBytes)).Decode(&p) != nil {
			write(w, http.StatusBadRequest, f3548.ErrorResponse{})
			return
		}
		f.mu.Lock()
		f.cnotes = append(f.cnotes, p)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		write(w, http.StatusNotFound, f3548.ErrorResponse{})
	}
}

func (f *Fake) call(ctx context.Context, method, url string, scope f3548.Scope, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	base := url
	if i := strings.Index(url, "/dss/v1"); i > 0 {
		base = url[:i]
	} else if i := strings.Index(url, "/uss/v1"); i > 0 {
		base = url[:i]
	}
	req.Header.Set("Authorization", "Bearer "+f.Token(base, scope))
	res, err := f.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(res.Body, f3548.MaxMessageBytes))
	return res.StatusCode, b, err
}

// Spec is one intent the peer files.
type Spec struct {
	ID       string
	Volumes  []f3548.Volume4D
	Priority int
	State    f3548.OperationalIntentState
}

// File writes the intent to the DSS as a real peer would: the
// references its extents meet queried, the ovn of each other manager's
// read from that manager (GET details), its own known, the PUT made with
// the key; it returns the reference the DSS answered and the subscribers
// it listed, which it notifies (POST /uss/v1/operational_intents) as the
// standard asks.
func (f *Fake) File(ctx context.Context, s Spec) (f3548.OperationalIntentReference, error) {
	var zero f3548.OperationalIntentReference
	aoi, err := areaOf(s.Volumes)
	if err != nil {
		return zero, err
	}
	st, b, err := f.call(ctx, http.MethodPost, f.DSSURL+"/dss/v1/operational_intent_references/query", f3548.ScopeStrategicCoordination,
		f3548.QueryOperationalIntentReferenceParameters{AreaOfInterest: &aoi})
	if err != nil || st != http.StatusOK {
		return zero, fmt.Errorf("query: %d %s %w", st, b, err)
	}
	var q f3548.QueryOperationalIntentReferenceResponse
	if err := json.Unmarshal(b, &q); err != nil {
		return zero, err
	}
	key := []string{}
	var prevOVN string
	f.mu.Lock()
	if cur, ok := f.intents[s.ID]; ok && cur.Reference.Ovn != nil {
		prevOVN = *cur.Reference.Ovn
	}
	f.mu.Unlock()
	for i := range q.OperationalIntentReferences {
		ref := &q.OperationalIntentReferences[i]
		if ref.Id == s.ID {
			continue
		}
		if ref.Manager == f.Manager && ref.Ovn != nil {
			key = append(key, *ref.Ovn)
			continue
		}
		st, b, err := f.call(ctx, http.MethodGet, strings.TrimRight(ref.UssBaseUrl, "/")+"/uss/v1/operational_intents/"+ref.Id, f3548.ScopeStrategicCoordination, nil)
		if err != nil || st != http.StatusOK {
			return zero, fmt.Errorf("details of %s: %d %s %w", ref.Id, st, b, err)
		}
		var d f3548.GetOperationalIntentDetailsResponse
		if err := json.Unmarshal(b, &d); err != nil || d.OperationalIntent.Reference.Ovn == nil {
			return zero, fmt.Errorf("details of %s are not usable", ref.Id)
		}
		key = append(key, *d.OperationalIntent.Reference.Ovn)
	}
	state := s.State
	if state == "" {
		state = f3548.Accepted
	}
	yes := true
	p := f3548.PutOperationalIntentReferenceParameters{Extents: s.Volumes, Key: &key, State: state, UssBaseUrl: f.URL(),
		NewSubscription: &f3548.ImplicitSubscriptionParameters{UssBaseUrl: f.URL(), NotifyForConstraints: &yes}}
	url := f.DSSURL + "/dss/v1/operational_intent_references/" + s.ID
	if prevOVN != "" {
		url += "/" + prevOVN
		p.NewSubscription = nil
		f.mu.Lock()
		sub := f.intents[s.ID].Reference.SubscriptionId
		f.mu.Unlock()
		p.SubscriptionId = &sub
	}
	st, b, err = f.call(ctx, http.MethodPut, url, f3548.ScopeStrategicCoordination, p)
	if err != nil || (st != http.StatusOK && st != http.StatusCreated) {
		return zero, fmt.Errorf("put: %d %s %w", st, b, err)
	}
	var res f3548.ChangeOperationalIntentReferenceResponse
	if err := json.Unmarshal(b, &res); err != nil {
		return zero, err
	}
	pri := s.Priority
	vols := s.Volumes
	off := []f3548.Volume4D{}
	oi := f3548.OperationalIntent{Reference: res.OperationalIntentReference, Details: f3548.OperationalIntentDetails{Volumes: &vols, OffNominalVolumes: &off, Priority: &pri}}
	f.mu.Lock()
	f.intents[s.ID] = oi
	f.mu.Unlock()
	for _, sub := range res.Subscribers {
		if strings.TrimRight(sub.UssBaseUrl, "/") == f.URL() {
			continue
		}
		body := f3548.PutOperationalIntentDetailsParameters{OperationalIntentId: s.ID, OperationalIntent: &oi, Subscriptions: sub.Subscriptions}
		if st, b, err := f.call(ctx, http.MethodPost, strings.TrimRight(sub.UssBaseUrl, "/")+"/uss/v1/operational_intents", f3548.ScopeStrategicCoordination, body); err != nil || st/100 != 2 {
			return res.OperationalIntentReference, fmt.Errorf("notify %s: %d %s %w", sub.UssBaseUrl, st, b, err)
		}
	}
	return res.OperationalIntentReference, nil
}

// Delete removes the intent from the DSS and notifies its subscribers.
func (f *Fake) Delete(ctx context.Context, id string) error {
	f.mu.Lock()
	cur, ok := f.intents[id]
	f.mu.Unlock()
	if !ok || cur.Reference.Ovn == nil {
		return fmt.Errorf("intent %s not filed", id)
	}
	st, b, err := f.call(ctx, http.MethodDelete, f.DSSURL+"/dss/v1/operational_intent_references/"+id+"/"+*cur.Reference.Ovn, f3548.ScopeStrategicCoordination, nil)
	if err != nil || st != http.StatusOK {
		return fmt.Errorf("delete: %d %s %w", st, b, err)
	}
	var res f3548.ChangeOperationalIntentReferenceResponse
	if err := json.Unmarshal(b, &res); err != nil {
		return err
	}
	f.mu.Lock()
	delete(f.intents, id)
	f.mu.Unlock()
	for _, sub := range res.Subscribers {
		if strings.TrimRight(sub.UssBaseUrl, "/") == f.URL() {
			continue
		}
		body := f3548.PutOperationalIntentDetailsParameters{OperationalIntentId: id, Subscriptions: sub.Subscriptions}
		if st, _, err := f.call(ctx, http.MethodPost, strings.TrimRight(sub.UssBaseUrl, "/")+"/uss/v1/operational_intents", f3548.ScopeStrategicCoordination, body); err != nil || st/100 != 2 {
			return fmt.Errorf("notify %s: %d %w", sub.UssBaseUrl, st, err)
		}
	}
	return nil
}

// Constrain files a constraint reference in the DSS as its manager and
// serves its details.
func (f *Fake) Constrain(ctx context.Context, id string, vols []f3548.Volume4D, geozoneID string) (f3548.ConstraintReference, error) {
	var zero f3548.ConstraintReference
	st, b, err := f.call(ctx, http.MethodPut, f.DSSURL+"/dss/v1/constraint_references/"+id, f3548.ScopeConstraintManagement,
		f3548.PutConstraintReferenceParameters{Extents: vols, UssBaseUrl: f.URL()})
	if err != nil || (st != http.StatusOK && st != http.StatusCreated) {
		return zero, fmt.Errorf("put constraint: %d %s %w", st, b, err)
	}
	var res f3548.ChangeConstraintReferenceResponse
	if err := json.Unmarshal(b, &res); err != nil {
		return zero, err
	}
	d := f3548.ConstraintDetails{Volumes: vols}
	if geozoneID != "" {
		d.Geozone = &f3548.GeoZone{Identifier: geozoneID, Country: "GEO", Type: "COMMON", Restriction: "PROHIBITED", ZoneAuthority: []f3548.Authority{}}
	}
	c := f3548.Constraint{Reference: res.ConstraintReference, Details: d}
	f.mu.Lock()
	f.cstrs[id] = c
	f.mu.Unlock()
	return res.ConstraintReference, nil
}

// Constraint is the constraint it manages; false when none.
func (f *Fake) Constraint(id string) (f3548.Constraint, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.cstrs[id]
	return c, ok
}

// Intent is the intent it filed; false when none.
func (f *Fake) Intent(id string) (f3548.OperationalIntent, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	oi, ok := f.intents[id]
	return oi, ok
}

// NotifyTo POSTs a notification of one of its intents to a USS (as the
// DSS's subscriber list would ask), with the given subscriptions.
func (f *Fake) NotifyTo(ctx context.Context, base, id string, subs []f3548.SubscriptionState) (int, error) {
	f.mu.Lock()
	oi, ok := f.intents[id]
	f.mu.Unlock()
	body := f3548.PutOperationalIntentDetailsParameters{OperationalIntentId: id, Subscriptions: subs}
	if ok {
		body.OperationalIntent = &oi
	}
	st, _, err := f.call(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/uss/v1/operational_intents", f3548.ScopeStrategicCoordination, body)
	return st, err
}

// areaOf is a box around the volumes with their window.
func areaOf(vols []f3548.Volume4D) (f3548.Volume4D, error) {
	var out f3548.Volume4D
	if len(vols) == 0 {
		return out, fmt.Errorf("no volumes")
	}
	minLat, minLng, maxLat, maxLng := 90.0, 180.0, -90.0, -180.0
	var from, to time.Time
	for i, v := range vols {
		box, s, e, err := f3548.Volume4DToZonesEnvelope(v)
		if err != nil {
			return out, err
		}
		minLat, minLng = min(minLat, box.MinLat), min(minLng, box.MinLon)
		maxLat, maxLng = max(maxLat, box.MaxLat), max(maxLng, box.MaxLon)
		if i == 0 || s.Before(from) {
			from = s
		}
		if i == 0 || e.After(to) {
			to = e
		}
	}
	out.Volume = f3548.Volume3D{OutlinePolygon: &f3548.Polygon{Vertices: []f3548.LatLngPoint{
		{Lat: minLat, Lng: minLng}, {Lat: minLat, Lng: maxLng}, {Lat: maxLat, Lng: maxLng}, {Lat: maxLat, Lng: minLng}}}}
	out.TimeStart = &f3548.Time{Format: f3548.RFC3339, Value: from}
	out.TimeEnd = &f3548.Time{Format: f3548.RFC3339, Value: to}
	return out, nil
}

// Subscribe puts a subscription of the area (a box with the window
// from now for d) in the DSS, telling the peer of operational intents
// and constraints there, as a USS watching an area does.
func (f *Fake) Subscribe(ctx context.Context, id string, minLat, minLng, maxLat, maxLng float64, d time.Duration) error {
	now := time.Now().UTC()
	yes := true
	p := f3548.PutSubscriptionParameters{
		Extents: f3548.Volume4D{Volume: f3548.Volume3D{OutlinePolygon: &f3548.Polygon{Vertices: []f3548.LatLngPoint{
			{Lat: minLat, Lng: minLng}, {Lat: minLat, Lng: maxLng}, {Lat: maxLat, Lng: maxLng}, {Lat: maxLat, Lng: minLng}}}},
			TimeStart: &f3548.Time{Format: f3548.RFC3339, Value: now}, TimeEnd: &f3548.Time{Format: f3548.RFC3339, Value: now.Add(d)}},
		NotifyForOperationalIntents: &yes, NotifyForConstraints: &yes, UssBaseUrl: f.URL(),
	}
	st, b, err := f.call(ctx, http.MethodPut, f.DSSURL+"/dss/v1/subscriptions/"+id, f3548.ScopeStrategicCoordination, p)
	if err != nil || st != http.StatusOK {
		return fmt.Errorf("subscribe: %d %s %w", st, b, err)
	}
	return nil
}
