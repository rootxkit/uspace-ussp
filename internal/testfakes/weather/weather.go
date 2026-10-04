// Package weather is a fake weather source for tests (WP-16): the NOAA
// Aviation Weather Center data API format, GET /metar and GET /taf with
// ids= and format=json, answering the reports a test set for the
// stations asked for (204 when none). Down makes it answer 503, Up
// brings it back.
package weather

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// METAR is one METAR entry of the fake.
type METAR struct {
	ICAOID  string  `json:"icaoId"`
	ObsTime int64   `json:"obsTime"`
	RawOb   string  `json:"rawOb"`
	Lat     float64 `json:"lat"`
	Lon     float64 `json:"lon"`
}

// TAF is one TAF entry of the fake.
type TAF struct {
	ICAOID        string  `json:"icaoId"`
	IssueTime     string  `json:"issueTime"`
	ValidTimeFrom int64   `json:"validTimeFrom"`
	ValidTimeTo   int64   `json:"validTimeTo"`
	RawTAF        string  `json:"rawTAF"`
	Lat           float64 `json:"lat"`
	Lon           float64 `json:"lon"`
}

// Fake is a running fake source.
type Fake struct {
	srv *httptest.Server

	mu     sync.Mutex
	down   bool
	metars []METAR
	tafs   []TAF
	calls  int
}

// New starts a fake source.
func New() *Fake {
	f := &Fake{}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

// URL is the base URL (USSP_WEATHER_SOURCE awc:<URL>).
func (f *Fake) URL() string { return f.srv.URL }

// Close stops the fake.
func (f *Fake) Close() { f.srv.Close() }

// Down makes every answer 503; Up undoes it.
func (f *Fake) Down() { f.mu.Lock(); f.down = true; f.mu.Unlock() }

// Up undoes Down.
func (f *Fake) Up() { f.mu.Lock(); f.down = false; f.mu.Unlock() }

// Calls is the number of requests served.
func (f *Fake) Calls() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

// Set replaces the reports.
func (f *Fake) Set(metars []METAR, tafs []TAF) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.metars, f.tafs = metars, tafs
}

// NewMETAR is a METAR of station at obs at lat, lon whose raw report is
// "METAR <station> <DDHHMM>Z " + body.
func NewMETAR(station string, obs time.Time, lat, lon float64, body string) METAR {
	obs = obs.UTC().Truncate(time.Minute)
	return METAR{ICAOID: station, ObsTime: obs.Unix(), Lat: lat, Lon: lon,
		RawOb: "METAR " + station + " " + obs.Format("021504") + "Z " + body}
}

// NewTAF is a TAF of station issued at issued, valid [from, to) (whole
// hours), whose raw report is "TAF <station> <DDHHMM>Z <DDHH>/<DDHH> " +
// body.
func NewTAF(station string, issued, from, to time.Time, lat, lon float64, body string) TAF {
	issued, from, to = issued.UTC().Truncate(time.Minute), from.UTC().Truncate(time.Hour), to.UTC().Truncate(time.Hour)
	return TAF{ICAOID: station, IssueTime: issued.Format(time.RFC3339), ValidTimeFrom: from.Unix(), ValidTimeTo: to.Unix(), Lat: lat, Lon: lon,
		RawTAF: "TAF " + station + " " + issued.Format("021504") + "Z " + from.Format("0215") + "/" + to.Format("0215") + " " + body}
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls++
	down := f.down
	metars, tafs := f.metars, f.tafs
	f.mu.Unlock()
	if down {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodGet || r.URL.Query().Get("format") != "json" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	want := map[string]bool{}
	for _, id := range strings.Split(r.URL.Query().Get("ids"), ",") {
		want[id] = true
	}
	var out []any
	switch r.URL.Path {
	case "/metar":
		for _, m := range metars {
			if want[m.ICAOID] {
				out = append(out, m)
			}
		}
	case "/taf":
		for _, t := range tafs {
			if want[t.ICAOID] {
				out = append(out, t)
			}
		}
	default:
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if len(out) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
