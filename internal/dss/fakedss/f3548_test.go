package fakedss

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/f3548"
)

// as calls the fake as the USS sub (a JWT whose payload names it).
func as(t *testing.T, d *DSS, sub, method, path string, body any) (int, []byte) {
	t.Helper()
	var r *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = strings.NewReader(string(b))
	} else {
		r = strings.NewReader("")
	}
	req, _ := http.NewRequest(method, d.URL()+path, r)
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"` + sub + `"}`))
	req.Header.Set("Authorization", "Bearer h."+payload+".s")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var raw json.RawMessage
	_ = json.NewDecoder(res.Body).Decode(&raw)
	return res.StatusCode, raw
}

func vol(lat, lng float64) f3548.Volume4D {
	now := time.Now().UTC()
	lo := f3548.Altitude{Reference: f3548.W84, Units: f3548.AltitudeUnitsM, Value: 100}
	hi := f3548.Altitude{Reference: f3548.W84, Units: f3548.AltitudeUnitsM, Value: 200}
	return f3548.Volume4D{Volume: f3548.Volume3D{OutlinePolygon: &f3548.Polygon{Vertices: []f3548.LatLngPoint{
		{Lat: lat, Lng: lng}, {Lat: lat, Lng: lng + 0.01}, {Lat: lat + 0.01, Lng: lng + 0.01}, {Lat: lat + 0.01, Lng: lng}}},
		AltitudeLower: &lo, AltitudeUpper: &hi},
		TimeStart: &f3548.Time{Format: f3548.RFC3339, Value: now.Add(time.Minute)},
		TimeEnd:   &f3548.Time{Format: f3548.RFC3339, Value: now.Add(time.Hour)}}
}

const (
	idA = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	idB = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
)

func put(state f3548.OperationalIntentState, key []string, v f3548.Volume4D) f3548.PutOperationalIntentReferenceParameters {
	yes := true
	return f3548.PutOperationalIntentReferenceParameters{Extents: []f3548.Volume4D{v}, Key: &key, State: state, UssBaseUrl: "https://uss.example",
		NewSubscription: &f3548.ImplicitSubscriptionParameters{UssBaseUrl: "https://uss.example", NotifyForConstraints: &yes}}
}

// The key protocol both ways: a write that meets another USS's reference
// without its ovn is 409 naming it, ovn scrubbed; with the ovn it is
// created; an update at an old ovn is 409, by another manager 403; a
// Nonconforming write needs no key; the subscribers are listed with their
// indexes advanced; a delete answers them too.
func TestF3548KeyProtocol(t *testing.T) {
	d := New()
	defer d.Close()
	st, b := as(t, d, "uss-a", http.MethodPut, "/dss/v1/operational_intent_references/"+idA, put(f3548.Accepted, nil, vol(41.7, 44.8)))
	var a f3548.ChangeOperationalIntentReferenceResponse
	if st != http.StatusCreated || json.Unmarshal(b, &a) != nil || a.OperationalIntentReference.Ovn == nil || a.OperationalIntentReference.Version != 1 {
		t.Fatalf("%d %s", st, b)
	}
	st, b = as(t, d, "uss-b", http.MethodPut, "/dss/v1/operational_intent_references/"+idB, put(f3548.Accepted, nil, vol(41.705, 44.805)))
	var c f3548.AirspaceConflictResponse
	if st != http.StatusConflict || json.Unmarshal(b, &c) != nil || c.MissingOperationalIntents == nil ||
		(*c.MissingOperationalIntents)[0].Id != idA || *(*c.MissingOperationalIntents)[0].Ovn != NoOVN || d.Conflicts() != 1 {
		t.Fatalf("%d %s", st, b)
	}
	st, b = as(t, d, "uss-b", http.MethodPut, "/dss/v1/operational_intent_references/"+idB, put(f3548.Accepted, []string{*a.OperationalIntentReference.Ovn}, vol(41.705, 44.805)))
	var bb f3548.ChangeOperationalIntentReferenceResponse
	if st != http.StatusCreated || json.Unmarshal(b, &bb) != nil || len(bb.Subscribers) != 1 || len(bb.Subscribers[0].Subscriptions) != 2 {
		t.Fatalf("%d %s", st, b)
	}
	// An update at an old ovn; by another manager.
	p := put(f3548.Activated, []string{*bb.OperationalIntentReference.Ovn}, vol(41.7, 44.8))
	if st, _ := as(t, d, "uss-a", http.MethodPut, "/dss/v1/operational_intent_references/"+idA+"/old", p); st != http.StatusConflict {
		t.Fatalf("old ovn: %d", st)
	}
	if st, _ := as(t, d, "uss-b", http.MethodPut, "/dss/v1/operational_intent_references/"+idA+"/"+*a.OperationalIntentReference.Ovn, p); st != http.StatusForbidden {
		t.Fatalf("another manager: %d", st)
	}
	// Nonconforming needs no key.
	nc := put(f3548.Nonconforming, nil, vol(41.7, 44.8))
	nc.NewSubscription, nc.SubscriptionId = nil, &a.OperationalIntentReference.SubscriptionId
	st, b = as(t, d, "uss-a", http.MethodPut, "/dss/v1/operational_intent_references/"+idA+"/"+*a.OperationalIntentReference.Ovn, nc)
	var n f3548.ChangeOperationalIntentReferenceResponse
	if st != http.StatusOK || json.Unmarshal(b, &n) != nil || n.OperationalIntentReference.State != f3548.Nonconforming || n.OperationalIntentReference.Version != 2 {
		t.Fatalf("%d %s", st, b)
	}
	// The query shows the other's ovn scrubbed, the caller's own in full.
	q := f3548.QueryOperationalIntentReferenceParameters{AreaOfInterest: ptr(vol(41.7, 44.8))}
	st, b = as(t, d, "uss-b", http.MethodPost, "/dss/v1/operational_intent_references/query", q)
	var qr f3548.QueryOperationalIntentReferenceResponse
	if st != http.StatusOK || json.Unmarshal(b, &qr) != nil || len(qr.OperationalIntentReferences) != 2 {
		t.Fatalf("%d %s", st, b)
	}
	for _, r := range qr.OperationalIntentReferences {
		if (r.Manager == "uss-b") == (*r.Ovn == NoOVN) {
			t.Fatalf("ovn shown wrongly: %+v", r)
		}
	}
	// A create in a state other than Accepted, and of an existing id.
	if st, _ := as(t, d, "uss-c", http.MethodPut, "/dss/v1/operational_intent_references/"+newUUID(), put(f3548.Activated, nil, vol(10, 10))); st != http.StatusBadRequest {
		t.Fatalf("create Activated: %d", st)
	}
	if st, _ := as(t, d, "uss-a", http.MethodPut, "/dss/v1/operational_intent_references/"+idA, put(f3548.Accepted, nil, vol(10, 10))); st != http.StatusConflict {
		t.Fatalf("create twice: %d", st)
	}
	// Delete: by another manager 403, at an old ovn 409, then gone.
	if st, _ := as(t, d, "uss-b", http.MethodDelete, "/dss/v1/operational_intent_references/"+idA+"/"+*n.OperationalIntentReference.Ovn, nil); st != http.StatusForbidden {
		t.Fatalf("%d", st)
	}
	if st, _ := as(t, d, "uss-a", http.MethodDelete, "/dss/v1/operational_intent_references/"+idA+"/old", nil); st != http.StatusConflict {
		t.Fatalf("%d", st)
	}
	if st, _ := as(t, d, "uss-a", http.MethodDelete, "/dss/v1/operational_intent_references/"+idA+"/"+*n.OperationalIntentReference.Ovn, nil); st != http.StatusOK {
		t.Fatalf("%d", st)
	}
	if _, _, ok := d.OIR(idA); ok || len(d.OIRs()) != 1 {
		t.Fatal("not deleted")
	}
	// Down: every call 503.
	d.Down(true)
	if st, _ := as(t, d, "uss-a", http.MethodPost, "/dss/v1/operational_intent_references/query", q); st != http.StatusServiceUnavailable {
		t.Fatalf("down: %d", st)
	}
}

// Subscriptions by version, constraints with their own key entries when
// the subscription tells of constraints, and the availability.
func TestF3548SubscriptionsConstraintsAvailability(t *testing.T) {
	d := New()
	defer d.Close()
	yes := true
	sp := f3548.PutSubscriptionParameters{Extents: vol(41.7, 44.8), UssBaseUrl: "https://uss.example", NotifyForOperationalIntents: &yes, NotifyForConstraints: &yes}
	st, b := as(t, d, "uss-a", http.MethodPut, "/dss/v1/subscriptions/"+idA, sp)
	var s f3548.PutSubscriptionResponse
	if st != http.StatusOK || json.Unmarshal(b, &s) != nil || s.Subscription.Version == "" {
		t.Fatalf("%d %s", st, b)
	}
	if st, _ := as(t, d, "uss-a", http.MethodPut, "/dss/v1/subscriptions/"+idA, sp); st != http.StatusConflict {
		t.Fatalf("created twice: %d", st)
	}
	if st, _ := as(t, d, "uss-a", http.MethodPut, "/dss/v1/subscriptions/"+idA+"/"+s.Subscription.Version, sp); st != http.StatusOK {
		t.Fatalf("update: %d", st)
	}
	if st, _ := as(t, d, "uss-a", http.MethodGet, "/dss/v1/subscriptions/"+idA, nil); st != http.StatusOK {
		t.Fatalf("get: %d", st)
	}
	long := sp
	long.Extents.TimeEnd = &f3548.Time{Format: f3548.RFC3339, Value: time.Now().Add(48 * time.Hour)}
	if st, _ := as(t, d, "uss-a", http.MethodPut, "/dss/v1/subscriptions/"+newUUID(), long); st != http.StatusBadRequest {
		t.Fatalf("over 24 h: %d", st)
	}
	// A constraint: a write telling of constraints must carry its ovn.
	cp := f3548.PutConstraintReferenceParameters{Extents: []f3548.Volume4D{vol(41.7, 44.8)}, UssBaseUrl: "https://ansp.example"}
	st, b = as(t, d, "ansp", http.MethodPut, "/dss/v1/constraint_references/"+idB, cp)
	var cr f3548.ChangeConstraintReferenceResponse
	if st != http.StatusCreated || json.Unmarshal(b, &cr) != nil || len(cr.Subscribers) != 1 {
		t.Fatalf("%d %s", st, b)
	}
	st, b = as(t, d, "uss-a", http.MethodPut, "/dss/v1/operational_intent_references/"+newUUID(), put(f3548.Accepted, nil, vol(41.7, 44.8)))
	var c f3548.AirspaceConflictResponse
	if st != http.StatusConflict || json.Unmarshal(b, &c) != nil || c.MissingConstraints == nil {
		t.Fatalf("%d %s", st, b)
	}
	st, _ = as(t, d, "uss-a", http.MethodPut, "/dss/v1/operational_intent_references/"+newUUID(), put(f3548.Accepted, []string{*cr.ConstraintReference.Ovn}, vol(41.7, 44.8)))
	if st != http.StatusCreated {
		t.Fatalf("with the constraint's ovn: %d", st)
	}
	st, b = as(t, d, "uss-a", http.MethodPost, "/dss/v1/constraint_references/query", f3548.QueryConstraintReferenceParameters{AreaOfInterest: ptr(vol(41.7, 44.8))})
	var cq f3548.QueryConstraintReferencesResponse
	if st != http.StatusOK || json.Unmarshal(b, &cq) != nil || len(cq.ConstraintReferences) != 1 || *cq.ConstraintReferences[0].Ovn != NoOVN {
		t.Fatalf("%d %s", st, b)
	}
	if st, _ := as(t, d, "ansp", http.MethodDelete, "/dss/v1/constraint_references/"+idB+"/"+*cr.ConstraintReference.Ovn, nil); st != http.StatusOK {
		t.Fatalf("%d", st)
	}
	if st, _ := as(t, d, "uss-a", http.MethodDelete, "/dss/v1/subscriptions/"+idA+"/old", nil); st != http.StatusConflict {
		t.Fatalf("%d", st)
	}
	// Availability: Unknown until set.
	st, b = as(t, d, "uss-a", http.MethodGet, "/dss/v1/uss_availability/uss-a", nil)
	var av f3548.UssAvailabilityStatusResponse
	if st != http.StatusOK || json.Unmarshal(b, &av) != nil || av.Status.Availability != f3548.Unknown {
		t.Fatalf("%d %s", st, b)
	}
	d.SetAvailability("uss-a", f3548.Down)
	_, b = as(t, d, "uss-a", http.MethodGet, "/dss/v1/uss_availability/uss-a", nil)
	if json.Unmarshal(b, &av) != nil || av.Status.Availability != f3548.Down {
		t.Fatalf("%s", b)
	}
	if len(d.UTMSubscriptions()) == 0 {
		t.Fatal("no subscription held")
	}
}
