package weather

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	fake "github.com/rootxkit/uspace-ussp/internal/testfakes/weather"
)

// serveFiles answers /metar and /taf with the recorded AWC answers.
func serveFiles(t *testing.T) *httptest.Server {
	t.Helper()
	files := map[string]string{"/metar": "testdata/awc-metar.json", "/taf": "testdata/awc-taf.json"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "json" || r.URL.Query().Get("ids") != "UGKO,UGSB,UGTB" {
			t.Errorf("query %s", r.URL.RawQuery)
		}
		b, err := os.ReadFile(files[r.URL.Path])
		if err != nil {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newAWC(t *testing.T, base string) Source {
	t.Helper()
	s, err := NewSource("awc:"+base, nil)
	if err != nil || s == nil {
		t.Fatal(s, err)
	}
	return s
}

// The recorded answers of 2026-10-04 parse whole: three METARs and
// three TAFs, each with its station's position from the envelope.
func TestAWCRecordedAnswers(t *testing.T) {
	srv := serveFiles(t)
	src := newAWC(t, srv.URL+"/")
	if !strings.HasPrefix(src.Name(), "awc:127.0.0.1:") {
		t.Fatal(src.Name())
	}
	b, err := src.Fetch(context.Background(), Stations([]string{"UGTB", "UGKO", "UGSB", "UGTB"}))
	if err != nil || b.Refused != 0 || len(b.Observed) != 6 {
		t.Fatalf("%+v %v", b, err)
	}
	byKey := map[string]Observed{}
	for _, o := range b.Observed {
		byKey[o.Report.Station+string(o.Report.Kind)] = o
	}
	m := byKey["UGTBmetar"]
	if m.LatDeg != 41.669 || m.LonDeg != 44.955 || !m.Report.IssuedAt.Equal(time.Date(2026, 10, 4, 13, 0, 0, 0, time.UTC)) ||
		*m.Report.Fields.WindSpeedMS != 6.7 || *m.Report.Fields.QNHHPa != 1026 || m.Report.Fields.Ceiling != CeilingNone {
		t.Errorf("UGTB METAR %+v", m)
	}
	taf := byKey["UGKOtaf"]
	if !taf.Report.ValidFrom.Equal(time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)) || len(taf.Report.Changes) != 3 ||
		taf.Report.Changes[1].Fields.Ceiling != CeilingVerticalVisibility {
		t.Errorf("UGKO TAF %+v", taf)
	}
}

func TestAWCRefusesAReportThatDisagreesWithItsEnvelope(t *testing.T) {
	f := fake.New()
	t.Cleanup(f.Close)
	obs := time.Date(2026, 10, 4, 13, 0, 0, 0, time.UTC)
	good := fake.NewMETAR("UGTB", obs, 41.669, 44.955, "31013KT 9999 FEW030 17/05 Q1026")
	otherTime := good
	otherTime.ObsTime = obs.Add(30 * time.Minute).Unix()
	otherStation := good
	otherStation.RawOb = strings.Replace(good.RawOb, "UGTB", "UGKO", 1)
	notAsked := fake.NewMETAR("UGSB", obs, 41.6, 41.6, "31013KT 9999 Q1026")
	badPos := fake.NewMETAR("UGKO", obs, 91, 42, "31013KT 9999 Q1026")
	garbled := fake.NewMETAR("UGTB", obs.Add(-time.Hour), 41.669, 44.955, "31013KT 9999 GARBLE Q1026")
	tafInMETAR := good
	tafInMETAR.RawOb = "TAF UGTB 041300Z 0412/0512 32016KT CAVOK"
	issued := time.Date(2026, 10, 4, 11, 0, 0, 0, time.UTC)
	tf := fake.NewTAF("UGTB", issued, issued.Add(time.Hour), issued.Add(25*time.Hour), 41.669, 44.955, "32016KT CAVOK")
	tfBadValidity := tf
	tfBadValidity.ValidTimeTo += 3600
	tfMETAR := tf
	tfMETAR.RawTAF = "METAR UGTB 041100Z 31013KT 9999"
	tfNoTime := tf
	tfNoTime.IssueTime = "yesterday"
	f.Set([]fake.METAR{good, otherTime, otherStation, notAsked, badPos, garbled, tafInMETAR}, []fake.TAF{tf, tfBadValidity, tfMETAR, tfNoTime})
	b, err := newAWC(t, f.URL()).Fetch(context.Background(), []string{"UGKO", "UGTB"})
	if err != nil {
		t.Fatal(err)
	}
	// notAsked is filtered by the fake itself; the other eight are refused.
	if len(b.Observed) != 2 || b.Refused != 8 || len(b.Reasons) != 8 {
		t.Fatalf("%d observed, %d refused: %v", len(b.Observed), b.Refused, b.Reasons)
	}
	for _, want := range []string{"not its obsTime", "of station UGKO", "outside WGS84", "GARBLE", "TAF in the METAR", "envelope's", "not a TAF", "no issueTime"} {
		if !strings.Contains(strings.Join(b.Reasons, "|"), want) {
			t.Errorf("no reason %q in %v", want, b.Reasons)
		}
	}
}

// A station the source answers but was not asked for is refused
// (presence twin of the fake's filter).
func TestAWCRefusesAStationNotAskedFor(t *testing.T) {
	obs := time.Date(2026, 10, 4, 13, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if r.URL.Path == "/taf" {
			_ = json.NewEncoder(w).Encode([]any{fake.NewTAF("UGSB", obs, obs, obs.Add(24*time.Hour), 41, 41, "32016KT CAVOK"), map[string]any{"icaoId": "UGTB"}})
			return
		}
		_ = json.NewEncoder(w).Encode([]any{fake.NewMETAR("UGSB", obs, 41, 41, "31013KT 9999"), map[string]any{"icaoId": "UGTB"}})
	}))
	t.Cleanup(srv.Close)
	b, err := newAWC(t, srv.URL).Fetch(context.Background(), []string{"UGTB"})
	if err != nil || len(b.Observed) != 0 || b.Refused != 4 || !strings.Contains(b.Reasons[0], "not asked for") || !strings.Contains(b.Reasons[1], "no obsTime") {
		t.Fatalf("%+v %v", b, err)
	}
}

// Every way an answer fails, fails the whole fetch: down, a redirect,
// not JSON, too long, too many entries, an entry that is not a report.
func TestAWCFailsClosed(t *testing.T) {
	cases := map[string]struct {
		h    http.HandlerFunc
		want string
	}{
		"down": {func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }, "answered 503"},
		"redirect": {func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://elsewhere.test/metar", http.StatusFound)
		}, "answered 302"},
		"not json": {func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("METAR UGTB 041300Z 31013KT"))
		}, "not application/json"},
		"too long": {func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("[" + strings.Repeat(" ", AWCMaxBodyBytes) + "]"))
		}, "longer than"},
		"too many": {func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("[" + strings.Repeat("{},", AWCMaxEntries) + "{}]"))
		}, "more than 1000 reports"},
		"not an array": {func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"error":"x"}`))
		}, "not a JSON array"},
		"not a report": {func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"icaoId": 7}]`))
		}, "not a report"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(c.h)
			t.Cleanup(srv.Close)
			b, err := newAWC(t, srv.URL).Fetch(context.Background(), []string{"UGTB"})
			if err == nil || !strings.Contains(err.Error(), c.want) || len(b.Observed) != 0 {
				t.Fatalf("%+v %v, want %q", b, err, c.want)
			}
		})
	}
	// At the entry bound the answer is read (E-10 twin).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[" + strings.Repeat(`{"icaoId":"ZZZZ"},`, AWCMaxEntries-1) + `{"icaoId":"ZZZZ"}]`))
	}))
	t.Cleanup(srv.Close)
	b, err := newAWC(t, srv.URL).Fetch(context.Background(), []string{"UGTB"})
	if err != nil || b.Refused != 2*AWCMaxEntries || len(b.Reasons) != MaxRefusals {
		t.Fatalf("at the bound: %d refused, %d reasons, %v", b.Refused, len(b.Reasons), err)
	}
}

func TestAWCNoContentAndNoStations(t *testing.T) {
	f := fake.New()
	t.Cleanup(f.Close)
	src := newAWC(t, f.URL())
	b, err := src.Fetch(context.Background(), []string{"UGTB"})
	if err != nil || len(b.Observed) != 0 || b.Refused != 0 || f.Calls() != 2 {
		t.Fatalf("204: %+v %v %d", b, err, f.Calls())
	}
	if b, err := src.Fetch(context.Background(), nil); err != nil || len(b.Observed) != 0 || f.Calls() != 2 {
		t.Fatalf("no station asked the source: %v %d", err, f.Calls())
	}
	f.Down()
	if _, err := src.Fetch(context.Background(), []string{"UGTB"}); err == nil {
		t.Fatal("down answered")
	}
}

func TestAWCDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		cancel()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	if _, err := newAWC(t, srv.URL).Fetch(ctx, []string{"UGTB"}); err == nil {
		t.Fatal("a cancelled call answered")
	}
}

func TestNewSource(t *testing.T) {
	if s, err := NewSource("", nil); s != nil || err != nil {
		t.Fatal("unset is no source", s, err)
	}
	for _, bad := range []string{"awc", "metar:https://x.test", "awc:ftp://x.test", "awc:https://", "awc:https://u:p@x.test",
		"awc:https://x.test/?a=b", "awc:https://x.test/#f", "awc:%zz"} {
		if _, err := NewSource(bad, nil); err == nil || !strings.Contains(err.Error(), "USSP_WEATHER_SOURCE") {
			t.Errorf("%s: %v", bad, err)
		}
	}
	s, err := NewSource("awc:https://wx.example.test/api/data/", nil)
	if err != nil || s.Name() != "awc:wx.example.test" || s.(*AWC).base != "https://wx.example.test/api/data" {
		t.Fatal(s, err)
	}
}
