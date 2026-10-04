package weather

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/rootxkit/uspace-core/core"
)

// Observed is one report the source gave, with the station's position
// as the source gives it (the centre of the product's area).
type Observed struct {
	Report Report
	LatDeg float64
	LonDeg float64
}

// Batch is one fetch: the reports that parsed whole, and why each other
// one was refused (at most MaxRefusals reasons are kept; Refused counts
// them all).
type Batch struct {
	Observed []Observed
	Refused  int
	Reasons  []string
}

// MaxRefusals bounds Batch.Reasons.
const MaxRefusals = 16

func (b *Batch) refuse(station, why string) {
	b.Refused++
	if len(b.Reasons) < MaxRefusals {
		b.Reasons = append(b.Reasons, clip(station)+": "+why)
	}
}

// Source is a weather source adapter: the reports of the stations.
type Source interface {
	// Name is the label stored with every product of this source.
	Name() string
	Fetch(ctx context.Context, stations []string) (Batch, error)
}

// AdapterAWC is the adapter of the NOAA Aviation Weather Center data API
// format: GET <base>/metar and <base>/taf with ids=<stations> and
// format=json.
const AdapterAWC = "awc"

// Bounds of one AWC call (E-10).
const (
	// AWCTimeout is the deadline of one call.
	AWCTimeout = 10 * time.Second
	// AWCMaxBodyBytes bounds one answer.
	AWCMaxBodyBytes = 2 << 20
	// AWCMaxEntries bounds the reports of one answer.
	AWCMaxEntries = 1000
)

// ConfigError is a USSP_WEATHER_SOURCE that cannot be used: the process
// refuses to start (fail closed), never runs without saying so.
func configError(format string, a ...any) error {
	return core.Fieldf("USSP_WEATHER_SOURCE", format, a...)
}

// NewSource builds the source of USSP_WEATHER_SOURCE, "<adapter>:<base
// URL>"; "" is no source (nil, nil). The only adapter is awc.
func NewSource(spec string, client *http.Client) (Source, error) {
	if spec == "" {
		return nil, nil
	}
	adapter, base, ok := strings.Cut(spec, ":")
	if !ok || adapter != AdapterAWC {
		return nil, configError("must be %s:<base URL>", AdapterAWC)
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, configError("the base URL is not an absolute http(s) URL without credentials, query or fragment")
	}
	if client == nil {
		client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return &AWC{base: strings.TrimRight(u.String(), "/"), name: AdapterAWC + ":" + u.Host, client: client}, nil
}

// AWC reads METAR and TAF in the NOAA Aviation Weather Center data API
// format. The envelope gives the station, its position and the report's
// times; the content is parsed from the raw report's groups, and a
// report whose station or times disagree with its envelope is refused.
type AWC struct {
	base   string
	name   string
	client *http.Client
}

// Name implements Source: awc:<host>.
func (a *AWC) Name() string { return a.name }

// awcMETAR is the part of a METAR entry this adapter reads.
type awcMETAR struct {
	ICAOID  string   `json:"icaoId"`
	ObsTime *int64   `json:"obsTime"`
	RawOb   string   `json:"rawOb"`
	Lat     *float64 `json:"lat"`
	Lon     *float64 `json:"lon"`
}

// awcTAF is the part of a TAF entry this adapter reads.
type awcTAF struct {
	ICAOID        string   `json:"icaoId"`
	IssueTime     string   `json:"issueTime"`
	ValidTimeFrom *int64   `json:"validTimeFrom"`
	ValidTimeTo   *int64   `json:"validTimeTo"`
	RawTAF        string   `json:"rawTAF"`
	Lat           *float64 `json:"lat"`
	Lon           *float64 `json:"lon"`
}

// Fetch implements Source: the METARs, then the TAFs. Either call
// failing fails the fetch (nothing of it is stored).
func (a *AWC) Fetch(ctx context.Context, stations []string) (Batch, error) {
	var b Batch
	if len(stations) == 0 {
		return b, nil
	}
	want := map[string]bool{}
	for _, s := range stations {
		want[s] = true
	}
	var metars []awcMETAR
	if err := a.get(ctx, "metar", stations, &metars); err != nil {
		return Batch{}, fmt.Errorf("metar: %w", err)
	}
	var tafs []awcTAF
	if err := a.get(ctx, "taf", stations, &tafs); err != nil {
		return Batch{}, fmt.Errorf("taf: %w", err)
	}
	for _, m := range metars {
		if !want[m.ICAOID] {
			b.refuse(m.ICAOID, "a station that was not asked for")
			continue
		}
		if m.ObsTime == nil {
			b.refuse(m.ICAOID, "no obsTime")
			continue
		}
		ref := time.Unix(*m.ObsTime, 0).UTC()
		r, err := Parse(m.RawOb, ref)
		switch {
		case err != nil:
			b.refuse(m.ICAOID, err.Error())
			continue
		case r.Kind == KindTAF:
			b.refuse(m.ICAOID, "a TAF in the METAR answer")
			continue
		case !r.IssuedAt.Equal(ref):
			b.refuse(m.ICAOID, "the report's time is not its obsTime")
			continue
		}
		a.add(&b, r, m.ICAOID, m.Lat, m.Lon)
	}
	for _, t := range tafs {
		if !want[t.ICAOID] {
			b.refuse(t.ICAOID, "a station that was not asked for")
			continue
		}
		issued, err := time.Parse(time.RFC3339, t.IssueTime)
		if err != nil || t.ValidTimeFrom == nil || t.ValidTimeTo == nil {
			b.refuse(t.ICAOID, "no issueTime, validTimeFrom or validTimeTo")
			continue
		}
		r, err := Parse(t.RawTAF, issued)
		switch {
		case err != nil:
			b.refuse(t.ICAOID, err.Error())
			continue
		case r.Kind != KindTAF:
			b.refuse(t.ICAOID, "not a TAF in the TAF answer")
			continue
		case !r.IssuedAt.Equal(issued.UTC()) || r.ValidFrom.Unix() != *t.ValidTimeFrom || r.ValidTo.Unix() != *t.ValidTimeTo:
			b.refuse(t.ICAOID, "the report's issue or validity is not its envelope's")
			continue
		}
		a.add(&b, r, t.ICAOID, t.Lat, t.Lon)
	}
	return b, nil
}

func (a *AWC) add(b *Batch, r Report, id string, lat, lon *float64) {
	switch {
	case r.Station != id:
		b.refuse(id, "the report is of station "+clip(r.Station))
	case lat == nil || lon == nil || !core.IsFinite(*lat) || !core.IsFinite(*lon) || math.Abs(*lat) > 90 || math.Abs(*lon) > 180:
		b.refuse(id, "no position, or one outside WGS84 bounds")
	default:
		b.Observed = append(b.Observed, Observed{Report: r, LatDeg: *lat, LonDeg: *lon})
	}
}

// get calls <base>/<product>?ids=&format=json into out: 200 with a JSON
// array of at most AWCMaxEntries, or 204 (no report). Anything else, a
// redirect included, is an error; the answer is read up to
// AWCMaxBodyBytes and a longer one is refused whole.
func (a *AWC) get(ctx context.Context, product string, stations []string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, AWCTimeout)
	defer cancel()
	q := url.Values{"ids": {strings.Join(stations, ",")}, "format": {"json"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.base+"/"+product+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNoContent:
		return json.Unmarshal([]byte("[]"), out)
	default:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8<<10))
		return fmt.Errorf("the source answered %d", resp.StatusCode)
	}
	if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		return errors.New("the answer is not application/json")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, AWCMaxBodyBytes+1))
	if err != nil {
		return fmt.Errorf("reading the answer: %w", err)
	}
	if len(body) > AWCMaxBodyBytes {
		return fmt.Errorf("the answer is longer than %d bytes", AWCMaxBodyBytes)
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return errors.New("the answer is not a JSON array")
	}
	if len(raw) > AWCMaxEntries {
		return fmt.Errorf("the answer has more than %d reports", AWCMaxEntries)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return errors.New("an entry of the answer is not a report")
	}
	return nil
}

// Stations sorts and deduplicates ids (the order of a fetch's ids= is
// stable).
func Stations(ids []string) []string {
	out := slices.Clone(ids)
	slices.Sort(out)
	return slices.Compact(out)
}
