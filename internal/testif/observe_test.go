package testif

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"

	obsv "github.com/rootxkit/uspace-ussp/internal/stdapi/interuss/ridobservation"
)

func productExample(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "schemas", "traffic", "product", "v1", "examples", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The members the observation reads are the product schema's: its
// examples read through productTrack carry every one.
func TestObservationReadsTheProductSchema(t *testing.T) {
	for _, name := range []string{"console-bbox-stale-and-switched-off-degraded.json", "operator-head-on-with-manned-and-e-conspicuity.json"} {
		var p product
		if err := json.Unmarshal(productExample(t, name), &p); err != nil || p.Schema != "traffic/product/v1" || len(p.Body.Tracks) == 0 {
			t.Fatalf("%s: %v %+v", name, err, p)
		}
		for _, tr := range p.Body.Tracks {
			if tr.TrackID == "" || tr.Trust == "" || tr.TimeOfReport.IsZero() || (tr.Position.Lat == 0 && tr.Position.Lng == 0) {
				t.Errorf("%s: a track read without its members: %+v", name, tr)
			}
		}
	}
}

// A view is the staff snapshot of its box, its provider tracks the
// flights (the own and manned ones are not the Display Provider's);
// details answer a flight of a view and 404 another; without a staff
// session the view is 503 saying why; a malformed view 400.
func TestObservation(t *testing.T) {
	f := newFakeUSSP(t)
	product := map[string]any{}
	if err := json.Unmarshal(productExample(t, "console-bbox-stale-and-switched-off-degraded.json"), &product); err != nil {
		t.Fatal(err)
	}
	body := product["body"].(map[string]any)
	tracks := body["tracks"].([]any)
	peer := map[string]any{}
	for k, v := range tracks[0].(map[string]any) {
		peer[k] = v
	}
	peer["track_id"], peer["trust"], peer["source"] = "peer-flight-1", "provider", "network_rid"
	body["tracks"] = append(tracks, peer)
	f.product, _ = json.Marshal(product)
	ts := newTestServer(t, f, func(c *Config) { c.ConsoleSessionFile = writeFile(t, "console", "console-session") })
	ts.USSP.Console = ts.cfg.ConsoleSessionFile

	w := ts.do(t, http.MethodGet, BaseObservation+"/display_data?view=41.80,44.90,41.60,44.70", ScopeObserve, nil)
	var got obsv.GetDisplayDataResponse
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Flights == nil {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(*got.Flights) != 1 || (*got.Flights)[0].Id != "peer-flight-1" || (*got.Flights)[0].MostRecentPosition.Lat != 41.7204 {
		t.Errorf("flights %+v", *got.Flights)
	}
	f.mu.Lock()
	bboxes := slices.Clone(f.bboxes)
	f.mu.Unlock()
	if !slices.Equal(bboxes, []string{"44.7,41.6,44.9,41.8"}) {
		t.Errorf("bbox %v (west,south,east,north of the view's corners)", bboxes)
	}
	if w := ts.do(t, http.MethodGet, BaseObservation+"/display_data/peer-flight-1", ScopeObserve, nil); w.Code != http.StatusOK {
		t.Errorf("details of a flight of the view: %d", w.Code)
	}
	if w := ts.do(t, http.MethodGet, BaseObservation+"/display_data/33333333-3333-4333-8333-333333333333", ScopeObserve, nil); w.Code != http.StatusNotFound {
		t.Errorf("details of a track the view did not show: %d", w.Code)
	}
	for _, view := range []string{"", "41.8,44.9,41.6", "91,44.9,41.6,44.7", "a,b,c,d"} {
		if w := ts.do(t, http.MethodGet, BaseObservation+"/display_data?view="+view, ScopeObserve, nil); w.Code != http.StatusBadRequest {
			t.Errorf("view %q: %d", view, w.Code)
		}
	}
	if w := ts.do(t, http.MethodGet, BaseObservation+"/display_data?view=41.8,44.9,41.6,44.7", ScopeInjectTestData, nil); w.Code != http.StatusForbidden {
		t.Errorf("the injection scope: %d", w.Code)
	}

	none := newTestServer(t, f, nil)
	if w := none.do(t, http.MethodGet, BaseObservation+"/display_data?view=41.8,44.9,41.6,44.7", ScopeObserve, nil); w.Code != http.StatusServiceUnavailable {
		t.Errorf("without a staff session: %d %s", w.Code, w.Body)
	}
}
