package weather

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Kind is the kind of a report.
type Kind string

// The report kinds (WMO FM 15 METAR, FM 16 SPECI, FM 51 TAF).
const (
	KindMETAR Kind = "metar"
	KindSPECI Kind = "speci"
	KindTAF   Kind = "taf"
)

// Ceiling says what the cloud groups tell of the lowest broken or
// overcast layer (Art. 12(2)(b)).
const (
	// CeilingLayer: the lowest BKN or OVC layer is cloud_base_ft_agl.
	CeilingLayer = "layer"
	// CeilingVerticalVisibility: the sky is obscured; cloud_base_ft_agl is
	// the vertical visibility.
	CeilingVerticalVisibility = "vertical_visibility"
	// CeilingNone: no broken or overcast layer (FEW or SCT only, NSC, SKC,
	// CLR, NCD or CAVOK).
	CeilingNone = "none"
	// CeilingNotReported: no cloud group, or a broken or overcast layer
	// whose height is not given; never read as "none".
	CeilingNotReported = "not_reported"
)

// Bounds of one report (E-10): a longer report, or one with more groups
// or change groups, is refused whole.
const (
	MaxRawBytes = 2048
	MaxGroups   = 200
	MaxChanges  = 32
	// MaxTAFSpan is the longest TAF validity accepted (WMO: up to 30 h).
	MaxTAFSpan = 36 * time.Hour
	// MaxClockSkew is how far a report's day-hour-minute may resolve from
	// the time it is resolved against.
	MaxClockSkew = 20 * 24 * time.Hour
	// TrendValidity is the validity of a METAR's trend (WMO: 2 h).
	TrendValidity = 2 * time.Hour
)

// Unit conversions (exact definitions: the knot is 1852 m per hour, the
// statute mile 1609.344 m, the inch of mercury 33.8639 hPa).
const (
	msPerKnot    = 1852.0 / 3600.0
	msPerKMH     = 1000.0 / 3600.0
	mPerSM       = 1609.344
	hPaPerInHg   = 33.8639
	ftPerHundred = 100
	// visibilityAtLeastM is 9999 and CAVOK: 10 km or more.
	visibilityAtLeastM = 10_000
)

// Fields are the Art. 12(2) minimum content of one report or of one of
// its change groups. A nil value was not reported; in a change group it
// is not changed.
type Fields struct {
	// WindDirDeg is the mean direction, degrees true, the wind comes from;
	// nil when variable (WindVariable) or not reported.
	WindDirDeg   *int `json:"wind_dir_deg"`
	WindVariable bool `json:"wind_variable"`
	// WindVarFromDeg and WindVarToDeg are the extremes of a varying
	// direction (dddVddd).
	WindVarFromDeg *int     `json:"wind_var_from_deg"`
	WindVarToDeg   *int     `json:"wind_var_to_deg"`
	WindSpeedMS    *float64 `json:"wind_speed_ms"`
	GustMS         *float64 `json:"gust_ms"`
	// VisibilityM is the prevailing visibility; VisibilityAtLeast says it
	// is a lower bound (9999, P6SM, CAVOK: 10 km or more).
	VisibilityM       *float64 `json:"visibility_m"`
	VisibilityAtLeast bool     `json:"visibility_at_least"`
	// Ceiling is one of the Ceiling constants; CloudBaseFtAGL the lowest
	// broken or overcast layer (or the vertical visibility) in feet above
	// the aerodrome, reported in hundreds of feet as the regulation says.
	Ceiling        string `json:"ceiling"`
	CloudBaseFtAGL *int   `json:"cloud_base_ft_agl"`
	CAVOK          bool   `json:"cavok"`
	TempC          *int   `json:"temp_c"`
	DewPointC      *int   `json:"dew_point_c"`
	// QNHHPa is the QNH; its area is the station (Product.QNHArea).
	QNHHPa *float64 `json:"qnh_hpa"`
	// Weather are the present weather groups verbatim; Convective the
	// convective indicators (TS from the weather, CB and TCU from the
	// clouds); Precipitation the precipitation phenomena (DZ, RA, SN, SG,
	// IC, PL, GR, GS, UP).
	Weather       []string `json:"weather"`
	Convective    []string `json:"convective"`
	Precipitation []string `json:"precipitation"`
}

// Change is one change group: a TAF's FM, BECMG, TEMPO or PROB, or a
// METAR's trend.
type Change struct {
	Kind      string     `json:"kind"`
	ValidFrom time.Time  `json:"valid_from"`
	ValidTo   *time.Time `json:"valid_to"`
	Fields    Fields     `json:"fields"`
}

// Report is one parsed report.
type Report struct {
	Kind    Kind
	Station string
	// IssuedAt is the observation time of a METAR or SPECI and the issue
	// time of a TAF.
	IssuedAt time.Time
	// ValidFrom and ValidTo are a TAF's validity (zero for a METAR).
	ValidFrom, ValidTo time.Time
	Fields             Fields
	Changes            []Change
	Raw                string
}

func newFields() Fields {
	return Fields{Ceiling: CeilingNotReported, Weather: []string{}, Convective: []string{}, Precipitation: []string{}}
}

var (
	reStation   = regexp.MustCompile(`^[A-Z][A-Z0-9]{3}$`)
	reTime      = regexp.MustCompile(`^(\d{2})(\d{2})(\d{2})Z$`)
	rePeriod    = regexp.MustCompile(`^(\d{2})(\d{2})/(\d{2})(\d{2})$`)
	reFM        = regexp.MustCompile(`^FM(\d{2})(\d{2})(\d{2})$`)
	reTrendTime = regexp.MustCompile(`^(FM|TL|AT)(\d{2})(\d{2})$`)
	reProb      = regexp.MustCompile(`^PROB(30|40)$`)
	reWind      = regexp.MustCompile(`^(\d{3}|VRB|///)(\d{2,3}|//)(?:G(\d{2,3}))?(KT|MPS|KMH)$`)
	reWindVar   = regexp.MustCompile(`^(\d{3})V(\d{3})$`)
	reVisM      = regexp.MustCompile(`^(\d{4})(NDV)?$`)
	reVisDir    = regexp.MustCompile(`^\d{4}(N|NE|E|SE|S|SW|W|NW)$`)
	reVisSM     = regexp.MustCompile(`^(P|M)?(\d{1,2}|\d{1,2}/\d{1,2})SM$`)
	reWhole     = regexp.MustCompile(`^\d{1,2}$`)
	reFracSM    = regexp.MustCompile(`^(\d)/(\d{1,2})SM$`)
	reRunway    = regexp.MustCompile(`^R\d{2}[LRC]?/`)
	reRunwayID  = regexp.MustCompile(`^R\d{2}[LRC]?$`)
	reWeather   = regexp.MustCompile(`^(\+|-|VC)?(MI|PR|BC|DR|BL|SH|TS|FZ)?((?:DZ|RA|SN|SG|IC|PL|GR|GS|UP|BR|FG|FU|VA|DU|SA|HZ|PY|PO|SQ|FC|SS|DS)*)$`)
	reCloud     = regexp.MustCompile(`^(FEW|SCT|BKN|OVC|///)(\d{3}|///)(CB|TCU|///)?$`)
	reVV        = regexp.MustCompile(`^VV(\d{3}|///)$`)
	reTemp      = regexp.MustCompile(`^(M?\d{2}|//)/(M?\d{2}|//)?$`)
	reQNH       = regexp.MustCompile(`^([QA])(\d{4}|////)$`)
	reRecent    = regexp.MustCompile(`^RE[A-Z]{2,8}$`)
	reShear     = regexp.MustCompile(`^WS\d{3}/\d{5}KT$`)
	reSea       = regexp.MustCompile(`^W(M?\d{2}|//)/(S\d|S/|H\d{1,3}|H///)$`)
	reTempFcst  = regexp.MustCompile(`^T[XN]M?\d{2}/\d{4}Z$`)
)

var precipitation = map[string]bool{"DZ": true, "RA": true, "SN": true, "SG": true, "IC": true, "PL": true, "GR": true, "GS": true, "UP": true}

// ErrNil is a NIL or cancelled (CNL) report: there is nothing to store.
var ErrNil = errors.New("the report is NIL or cancelled")

func round1(v float64) float64 { return math.Round(v*10) / 10 }

func ptr[T any](v T) *T { return &v }

// Parse reads one METAR, SPECI or TAF. Its day-hour-minute groups are
// resolved to the instants nearest ref (the time the source says the
// report is of). A report that does not parse whole is an error naming
// the group: nothing of it is used (fail closed).
func Parse(raw string, ref time.Time) (Report, error) {
	if len(raw) > MaxRawBytes {
		return Report{}, fmt.Errorf("the report is longer than %d bytes", MaxRawBytes)
	}
	toks := strings.Fields(strings.TrimSpace(raw))
	if n := len(toks); n > 0 {
		toks[n-1] = strings.TrimSuffix(toks[n-1], "=")
		if toks[n-1] == "" {
			toks = toks[:n-1]
		}
	}
	if len(toks) > MaxGroups {
		return Report{}, fmt.Errorf("the report has more than %d groups", MaxGroups)
	}
	p := &parser{toks: toks, ref: ref.UTC()}
	r, err := p.report()
	if err != nil {
		return Report{}, err
	}
	r.Raw = strings.Join(toks, " ")
	return r, nil
}

type parser struct {
	toks []string
	i    int
	ref  time.Time
}

func (p *parser) peek() string {
	if p.i < len(p.toks) {
		return p.toks[p.i]
	}
	return ""
}

func (p *parser) next() string {
	t := p.peek()
	p.i++
	return t
}

func (p *parser) done() bool { return p.i >= len(p.toks) }

func (p *parser) report() (Report, error) {
	var r Report
	switch p.next() {
	case "METAR":
		r.Kind = KindMETAR
	case "SPECI":
		r.Kind = KindSPECI
	case "TAF":
		r.Kind = KindTAF
	default:
		return Report{}, errors.New("the report does not start with METAR, SPECI or TAF")
	}
	for p.peek() == "COR" || p.peek() == "AMD" {
		if p.peek() == "AMD" && r.Kind != KindTAF {
			return Report{}, errors.New("AMD outside a TAF")
		}
		p.next()
	}
	r.Station = p.next()
	if !reStation.MatchString(r.Station) {
		return Report{}, fmt.Errorf("%q is not a location indicator", clip(r.Station))
	}
	m := reTime.FindStringSubmatch(p.next())
	if m == nil {
		return Report{}, errors.New("no issue or observation time (DDHHMMZ)")
	}
	at, ok := resolve(p.ref, atoi(m[1]), atoi(m[2]), atoi(m[3]))
	if !ok {
		return Report{}, fmt.Errorf("the time %sZ does not resolve near %s", m[0][:6], p.ref.Format(time.RFC3339))
	}
	r.IssuedAt = at
	if p.peek() == "NIL" {
		return Report{}, ErrNil
	}
	if r.Kind == KindTAF {
		return p.taf(r)
	}
	return p.metar(r)
}

func (p *parser) metar(r Report) (Report, error) {
	if p.peek() == "AUTO" {
		p.next()
	}
	r.Fields = newFields()
	if err := p.groups(&r.Fields, true); err != nil {
		return Report{}, err
	}
	for !p.done() {
		switch t := p.next(); t {
		case "RMK":
			p.i = len(p.toks)
		case "NOSIG":
		case "BECMG", "TEMPO":
			c, err := p.trend(t, r.IssuedAt)
			if err != nil {
				return Report{}, err
			}
			if len(r.Changes) == MaxChanges {
				return Report{}, fmt.Errorf("more than %d change groups", MaxChanges)
			}
			r.Changes = append(r.Changes, c)
		default:
			return Report{}, fmt.Errorf("unknown group %q", clip(t))
		}
	}
	if r.Changes == nil {
		r.Changes = []Change{}
	}
	return r, nil
}

// trend reads a METAR trend after its BECMG or TEMPO: optional FM, TL
// and AT times (hhmm on the observation's day or the next) and its
// groups; it is valid for TrendValidity from the observation.
func (p *parser) trend(kind string, obs time.Time) (Change, error) {
	c := Change{Kind: kind, ValidFrom: obs, ValidTo: ptr(obs.Add(TrendValidity)), Fields: newFields()}
	for {
		m := reTrendTime.FindStringSubmatch(p.peek())
		if m == nil {
			break
		}
		p.next()
		h, mi := atoi(m[2]), atoi(m[3])
		if h > 24 || mi > 59 || (h == 24 && mi != 0) {
			return Change{}, fmt.Errorf("trend time %q", m[0])
		}
		t := time.Date(obs.Year(), obs.Month(), obs.Day(), h, mi, 0, 0, time.UTC)
		if t.Before(obs) {
			t = t.Add(24 * time.Hour)
		}
		switch m[1] {
		case "FM":
			c.ValidFrom = t
		case "TL":
			c.ValidTo = ptr(t)
		case "AT":
			c.ValidFrom, c.ValidTo = t, ptr(t)
		}
	}
	if err := p.groups(&c.Fields, false); err != nil {
		return Change{}, err
	}
	return c, nil
}

func (p *parser) taf(r Report) (Report, error) {
	if p.peek() == "CNL" {
		return Report{}, ErrNil
	}
	from, to, err := p.period(r.IssuedAt)
	if err != nil {
		return Report{}, err
	}
	if p.peek() == "CNL" {
		return Report{}, ErrNil
	}
	r.ValidFrom, r.ValidTo = from, to
	r.Fields = newFields()
	if err := p.groups(&r.Fields, false); err != nil {
		return Report{}, err
	}
	r.Changes = []Change{}
	for !p.done() {
		t := p.next()
		var c Change
		switch {
		case t == "RMK":
			p.i = len(p.toks)
			continue
		case reFM.MatchString(t):
			m := reFM.FindStringSubmatch(t)
			at, ok := resolve(from, atoi(m[1]), atoi(m[2]), atoi(m[3]))
			if !ok || at.Before(from) || at.After(to) {
				return Report{}, fmt.Errorf("%s is outside the TAF's validity", t)
			}
			c = Change{Kind: "FM", ValidFrom: at}
		case t == "BECMG" || t == "TEMPO" || reProb.MatchString(t):
			kind := t
			if reProb.MatchString(t) && p.peek() == "TEMPO" {
				kind += " " + p.next()
			}
			cf, ct, err := p.period(from)
			if err != nil {
				return Report{}, fmt.Errorf("%s: %w", kind, err)
			}
			if cf.Before(from) || ct.After(to) {
				return Report{}, fmt.Errorf("%s %s is outside the TAF's validity", kind, cf.Format(time.RFC3339))
			}
			c = Change{Kind: kind, ValidFrom: cf, ValidTo: ptr(ct)}
		default:
			return Report{}, fmt.Errorf("unknown group %q", clip(t))
		}
		c.Fields = newFields()
		if err := p.groups(&c.Fields, false); err != nil {
			return Report{}, err
		}
		if len(r.Changes) == MaxChanges {
			return Report{}, fmt.Errorf("more than %d change groups", MaxChanges)
		}
		r.Changes = append(r.Changes, c)
	}
	// An FM group holds until the next FM group or the TAF's end.
	var last *Change
	for i := range r.Changes {
		if r.Changes[i].Kind != "FM" {
			continue
		}
		if last != nil {
			last.ValidTo = ptr(r.Changes[i].ValidFrom)
		}
		last = &r.Changes[i]
	}
	if last != nil {
		last.ValidTo = ptr(to)
	}
	return r, nil
}

// period reads DDHH/DDHH resolved near ref: from before to, at most
// MaxTAFSpan apart.
func (p *parser) period(ref time.Time) (time.Time, time.Time, error) {
	t := p.next()
	m := rePeriod.FindStringSubmatch(t)
	if m == nil {
		return time.Time{}, time.Time{}, fmt.Errorf("no validity period (DDHH/DDHH), got %q", clip(t))
	}
	from, ok1 := resolve(ref, atoi(m[1]), atoi(m[2]), 0)
	to, ok2 := resolve(from, atoi(m[3]), atoi(m[4]), 0)
	if !ok1 || !ok2 || !to.After(from) || to.Sub(from) > MaxTAFSpan {
		return time.Time{}, time.Time{}, fmt.Errorf("the validity period %s does not resolve", t)
	}
	return from, to, nil
}

// stop reports whether t ends a group sequence: a change group, NOSIG
// or remarks.
func stop(t string) bool {
	switch t {
	case "BECMG", "TEMPO", "NOSIG", "RMK":
		return true
	}
	return reFM.MatchString(t) || reProb.MatchString(t)
}

// groups reads the groups of one section into f until a change group,
// NOSIG, remarks or the end; observed is a METAR's or SPECI's own body,
// the only section with a temperature and recent weather. A group the
// parser does not know refuses the report.
func (p *parser) groups(f *Fields, observed bool) error {
	sawCloud, unknownCeiling := false, false
	lowest := -1
	vv := false
	for !p.done() && !stop(p.peek()) {
		t := p.next()
		switch {
		case reWind.MatchString(t):
			if err := wind(f, reWind.FindStringSubmatch(t)); err != nil {
				return err
			}
		case reWindVar.MatchString(t):
			m := reWindVar.FindStringSubmatch(t)
			a, b := atoi(m[1]), atoi(m[2])
			if a > 360 || b > 360 {
				return fmt.Errorf("wind variation %q", t)
			}
			f.WindVarFromDeg, f.WindVarToDeg = ptr(a), ptr(b)
		case t == "CAVOK":
			f.CAVOK, f.VisibilityAtLeast = true, true
			f.VisibilityM = ptr(float64(visibilityAtLeastM))
			sawCloud = true
		case reVisM.MatchString(t):
			if f.VisibilityM != nil {
				return fmt.Errorf("a second visibility %q", t)
			}
			v := atoi(reVisM.FindStringSubmatch(t)[1])
			if v == 9999 {
				f.VisibilityM, f.VisibilityAtLeast = ptr(float64(visibilityAtLeastM)), true
			} else {
				f.VisibilityM = ptr(float64(v))
			}
		case reVisDir.MatchString(t), t == "////":
			// The minimum visibility in a direction; //// a visibility not
			// reported.
		case reWhole.MatchString(t) && reFracSM.MatchString(p.peek()):
			m := reFracSM.FindStringSubmatch(p.next())
			d := atoi(m[2])
			if d == 0 || f.VisibilityM != nil {
				return fmt.Errorf("visibility %q %q", t, m[0])
			}
			f.VisibilityM = ptr(math.Round((float64(atoi(t)) + float64(atoi(m[1]))/float64(d)) * mPerSM))
		case reVisSM.MatchString(t):
			if f.VisibilityM != nil {
				return fmt.Errorf("a second visibility %q", t)
			}
			m := reVisSM.FindStringSubmatch(t)
			v, err := miles(m[2])
			if err != nil {
				return fmt.Errorf("visibility %q: %w", t, err)
			}
			f.VisibilityM = ptr(math.Round(v * mPerSM))
			f.VisibilityAtLeast = m[1] == "P"
		case reRunway.MatchString(t), reRecent.MatchString(t) && observed, reShear.MatchString(t), reSea.MatchString(t), reTempFcst.MatchString(t):
			// Runway visual range and state, recent weather, wind shear,
			// sea state and forecast temperatures: not Art. 12(2) content.
		case t == "WS":
			for p.peek() == "ALL" || p.peek() == "RWY" || reRunwayID.MatchString(p.peek()) {
				p.next()
			}
		case t == "NSW", t == "//":
			// No significant weather; present weather not reported.
		case reCloud.MatchString(t):
			m := reCloud.FindStringSubmatch(t)
			sawCloud = true
			switch m[3] {
			case "CB", "TCU":
				addOnce(&f.Convective, m[3])
			}
			switch {
			case m[1] == "///" || ((m[1] == "BKN" || m[1] == "OVC") && m[2] == "///"):
				// A cover or a height not reported: this layer may be the
				// lowest broken one.
				unknownCeiling = true
			case m[1] == "BKN" || m[1] == "OVC":
				if h := atoi(m[2]) * ftPerHundred; lowest < 0 || h < lowest {
					lowest, vv = h, false
				}
			}
		case reVV.MatchString(t):
			sawCloud = true
			m := reVV.FindStringSubmatch(t)
			if m[1] == "///" {
				unknownCeiling = true
				continue
			}
			if h := atoi(m[1]) * ftPerHundred; lowest < 0 || h < lowest {
				lowest, vv = h, true
			}
		case t == "NSC", t == "SKC", t == "CLR", t == "NCD":
			sawCloud = true
		case reTemp.MatchString(t) && observed:
			m := reTemp.FindStringSubmatch(t)
			if f.TempC != nil {
				return fmt.Errorf("a second temperature group %q", t)
			}
			f.TempC, f.DewPointC = celsius(m[1]), celsius(m[2])
		case reQNH.MatchString(t):
			m := reQNH.FindStringSubmatch(t)
			if m[2] == "////" {
				continue
			}
			v := float64(atoi(m[2]))
			if m[1] == "A" {
				v = round1(v / 100 * hPaPerInHg)
			}
			f.QNHHPa = ptr(v)
		case weatherGroup(t):
			if len(f.Weather) == MaxChanges {
				return fmt.Errorf("more than %d weather groups", MaxChanges)
			}
			f.Weather = append(f.Weather, t)
			m := reWeather.FindStringSubmatch(t)
			if m[2] == "TS" {
				addOnce(&f.Convective, "TS")
			}
			for i := 0; i+2 <= len(m[3]); i += 2 {
				if ph := m[3][i : i+2]; precipitation[ph] {
					addOnce(&f.Precipitation, ph)
				}
			}
		default:
			return fmt.Errorf("unknown group %q", clip(t))
		}
	}
	switch {
	case unknownCeiling:
		// A broken or overcast layer of unknown height: the lowest is not
		// known, whatever else is reported.
		f.Ceiling = CeilingNotReported
	case lowest >= 0 && vv:
		f.Ceiling, f.CloudBaseFtAGL = CeilingVerticalVisibility, ptr(lowest)
	case lowest >= 0:
		f.Ceiling, f.CloudBaseFtAGL = CeilingLayer, ptr(lowest)
	case sawCloud:
		f.Ceiling = CeilingNone
	}
	return nil
}

func weatherGroup(t string) bool {
	m := reWeather.FindStringSubmatch(t)
	return len(m) == 4 && (m[2] != "" || m[3] != "")
}

func addOnce(s *[]string, v string) {
	if !slices.Contains(*s, v) {
		*s = append(*s, v)
	}
}

func wind(f *Fields, m []string) error {
	switch m[1] {
	case "VRB":
		f.WindVariable = true
	case "///":
	default:
		d := atoi(m[1])
		if d > 360 {
			return fmt.Errorf("wind direction %s", m[1])
		}
		f.WindDirDeg = ptr(d % 360)
	}
	k := msPerKnot
	switch m[4] {
	case "MPS":
		k = 1
	case "KMH":
		k = msPerKMH
	}
	if m[2] != "//" {
		f.WindSpeedMS = ptr(round1(float64(atoi(m[2])) * k))
	}
	if m[3] != "" {
		f.GustMS = ptr(round1(float64(atoi(m[3])) * k))
	}
	return nil
}

func miles(s string) (float64, error) {
	if a, b, ok := strings.Cut(s, "/"); ok {
		d := atoi(b)
		if d == 0 {
			return 0, errors.New("a zero denominator")
		}
		return float64(atoi(a)) / float64(d), nil
	}
	return float64(atoi(s)), nil
}

func celsius(s string) *int {
	if s == "" || s == "//" {
		return nil
	}
	if v, ok := strings.CutPrefix(s, "M"); ok {
		return ptr(-atoi(v))
	}
	return ptr(atoi(s))
}

// atoi reads digits the regular expressions already matched.
func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func clip(s string) string {
	if len(s) > 24 {
		return s[:24] + "..."
	}
	return s
}

// resolve places day, hour and minute (hour 24 only at minute 0) in the
// month of ref or the one before or after, at the instant nearest ref
// and within MaxClockSkew of it.
func resolve(ref time.Time, day, hour, minute int) (time.Time, bool) {
	if day < 1 || day > 31 || hour > 24 || minute > 59 || (hour == 24 && minute != 0) {
		return time.Time{}, false
	}
	var best time.Time
	found := false
	for dm := -1; dm <= 1; dm++ {
		first := time.Date(ref.Year(), ref.Month()+time.Month(dm), 1, 0, 0, 0, 0, time.UTC)
		t := time.Date(first.Year(), first.Month(), day, 0, 0, 0, 0, time.UTC)
		if t.Month() != first.Month() {
			continue
		}
		t = t.Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute)
		if !found || absDur(t.Sub(ref)) < absDur(best.Sub(ref)) {
			best, found = t, true
		}
	}
	if !found || absDur(best.Sub(ref)) > MaxClockSkew {
		return time.Time{}, false
	}
	return best, true
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
