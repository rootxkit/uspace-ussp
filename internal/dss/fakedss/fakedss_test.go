package fakedss

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/f3411"
)

func call(t *testing.T, d *DSS, method, path string, body any, auth bool) (int, []byte) {
	t.Helper()
	var r *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = strings.NewReader(string(b))
	} else {
		r = strings.NewReader("")
	}
	req, _ := http.NewRequest(method, d.URL()+path, r)
	if auth {
		req.Header.Set("Authorization", "Bearer t")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var raw json.RawMessage
	_ = json.NewDecoder(res.Body).Decode(&raw)
	return res.StatusCode, raw
}

func extents(lat, lng float64, start, end time.Time) f3411.Volume4D {
	v := f3411.Volume4D{
		Volume: f3411.Volume3D{OutlinePolygon: &f3411.Polygon{Vertices: []f3411.LatLngPoint{
			{Lat: lat, Lng: lng}, {Lat: lat, Lng: lng + 0.01}, {Lat: lat + 0.01, Lng: lng + 0.01}, {Lat: lat + 0.01, Lng: lng},
		}}},
		TimeEnd: &f3411.Time{Format: f3411.RFC3339, Value: end},
	}
	if !start.IsZero() {
		v.TimeStart = &f3411.Time{Format: f3411.RFC3339, Value: start}
	}
	return v
}

// The fake's ISA semantics both ways: create, a second create 409, an
// update with the current version and a 409 with an old one, a search
// that finds it by area and not elsewhere, a delete with the version,
// 404 after; a subscriber over the area is listed with its index going
// up; no token 401; Down 503 and back; a start in the past 400.
func TestFakeDSSISASemantics(t *testing.T) {
	d := New()
	defer d.Close()
	now := time.Now()
	sub := f3411.CreateSubscriptionParameters{Extents: extents(41.7, 44.8, time.Time{}, now.Add(time.Hour)), UssBaseUrl: "https://dp.test/rid"}
	if code, b := call(t, d, http.MethodPut, "/rid/v2/dss/subscriptions/s1", sub, true); code != 200 {
		t.Fatalf("subscription %d %s", code, b)
	}
	isa := f3411.CreateIdentificationServiceAreaParameters{Extents: extents(41.7, 44.8, now, now.Add(time.Hour)), UssBaseUrl: "https://ussp.test"}
	code, b := call(t, d, http.MethodPut, "/rid/v2/dss/identification_service_areas/i1", isa, true)
	var put f3411.PutIdentificationServiceAreaResponse
	if code != 200 || json.Unmarshal(b, &put) != nil || put.Subscribers == nil || len(*put.Subscribers) != 1 ||
		*(*put.Subscribers)[0].Subscriptions[0].NotificationIndex != 1 || (*put.Subscribers)[0].Url != "https://dp.test/rid" {
		t.Fatalf("create %d %s", code, b)
	}
	if code, _ := call(t, d, http.MethodPut, "/rid/v2/dss/identification_service_areas/i1", isa, true); code != 409 {
		t.Errorf("second create %d", code)
	}
	v1 := put.ServiceArea.Version
	code, b = call(t, d, http.MethodPut, "/rid/v2/dss/identification_service_areas/i1/"+v1, isa, true)
	_ = json.Unmarshal(b, &put)
	if code != 200 || put.ServiceArea.Version == v1 || *(*put.Subscribers)[0].Subscriptions[0].NotificationIndex != 2 {
		t.Fatalf("update %d %s", code, b)
	}
	if code, _ := call(t, d, http.MethodPut, "/rid/v2/dss/identification_service_areas/i1/"+v1, isa, true); code != 409 {
		t.Errorf("update with an old version %d", code)
	}
	if code, _ := call(t, d, http.MethodPut, "/rid/v2/dss/identification_service_areas/i9/x", isa, true); code != 404 {
		t.Errorf("update of a missing ISA %d", code)
	}
	var found f3411.SearchIdentificationServiceAreasResponse
	_, b = call(t, d, http.MethodGet, "/rid/v2/dss/identification_service_areas?area=41.69,44.79,41.69,44.82,41.72,44.82", nil, true)
	if json.Unmarshal(b, &found) != nil || len(*found.ServiceAreas) != 1 {
		t.Fatalf("search here: %s", b)
	}
	_, b = call(t, d, http.MethodGet, "/rid/v2/dss/identification_service_areas?area=10,10,10,10.1,10.1,10.1", nil, true)
	if json.Unmarshal(b, &found) != nil || len(*found.ServiceAreas) != 0 {
		t.Fatalf("search elsewhere: %s", b)
	}
	if code, _ := call(t, d, http.MethodGet, "/rid/v2/dss/identification_service_areas?area=1,2", nil, true); code != 400 {
		t.Errorf("bad area %d", code)
	}
	if code, _ := call(t, d, http.MethodGet, "/rid/v2/dss/identification_service_areas/i1", nil, true); code != 200 {
		t.Errorf("get %d", code)
	}
	if code, _ := call(t, d, http.MethodGet, "/rid/v2/dss/identification_service_areas/i1", nil, false); code != 401 {
		t.Errorf("no token %d", code)
	}
	d.Down(true)
	if code, _ := call(t, d, http.MethodGet, "/rid/v2/dss/identification_service_areas/i1", nil, true); code != 503 {
		t.Errorf("down %d", code)
	}
	d.Down(false)
	if code, _ := call(t, d, http.MethodDelete, "/rid/v2/dss/identification_service_areas/i1/"+v1, nil, true); code != 409 {
		t.Errorf("delete with an old version %d", code)
	}
	if code, _ := call(t, d, http.MethodDelete, "/rid/v2/dss/identification_service_areas/i1/"+put.ServiceArea.Version, nil, true); code != 200 {
		t.Errorf("delete %d", code)
	}
	if code, _ := call(t, d, http.MethodGet, "/rid/v2/dss/identification_service_areas/i1", nil, true); code != 404 {
		t.Errorf("get after delete %d", code)
	}
	if code, _ := call(t, d, http.MethodDelete, "/rid/v2/dss/identification_service_areas/i1/x", nil, true); code != 404 {
		t.Errorf("delete of a missing ISA %d", code)
	}
	past := isa
	past.Extents = extents(41.7, 44.8, now.Add(-time.Hour), now.Add(time.Hour))
	if code, _ := call(t, d, http.MethodPut, "/rid/v2/dss/identification_service_areas/i2", past, true); code != 400 {
		t.Errorf("start in the past %d", code)
	}
	if len(d.ISAs()) != 0 || len(d.Calls()) == 0 {
		t.Error("state")
	}
	if code, _ := call(t, d, http.MethodPut, "/rid/v2/dss/subscriptions/s1", sub, true); code != 409 {
		t.Errorf("second subscription %d", code)
	}
	if code, _ := call(t, d, http.MethodDelete, "/rid/v2/dss/subscriptions/s1/x", nil, true); code != 409 {
		t.Errorf("subscription delete with a wrong version %d", code)
	}
	if code, _ := call(t, d, http.MethodGet, "/other", nil, true); code != 404 {
		t.Errorf("not a DSS path %d", code)
	}
}
