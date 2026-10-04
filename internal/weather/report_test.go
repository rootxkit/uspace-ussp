package weather

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

var ref = time.Date(2026, 10, 4, 13, 0, 0, 0, time.UTC)

func at(day, hour, minute int) time.Time {
	return time.Date(2026, 10, day, hour, minute, 0, 0, time.UTC)
}

// fields is the expected content: what a test does not set is
// not reported.
func fields(set func(*Fields)) Fields {
	f := newFields()
	set(&f)
	return f
}

// Every group kind the parser reads, from the recorded AWC answers of
// 2026-10-04 (testdata) and from report shapes of WMO No. 306 and FMH-1.
func TestParseFixtureMETARs(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		ref  time.Time
		kind Kind
		at   time.Time
		want Fields
		ch   []Change
	}{
		{
			name: "CAVOK and a varying wind (recorded UGKO)",
			raw:  "METAR UGKO 041300Z 29005KT 260V320 CAVOK 19/09 Q1026 NOSIG",
			ref:  ref, kind: KindMETAR, at: ref,
			want: fields(func(f *Fields) {
				f.WindDirDeg, f.WindVarFromDeg, f.WindVarToDeg, f.WindSpeedMS = ptr(290), ptr(260), ptr(320), ptr(2.6)
				f.VisibilityM, f.VisibilityAtLeast, f.CAVOK, f.Ceiling = ptr(10000.0), true, true, CeilingNone
				f.TempC, f.DewPointC, f.QNHHPa = ptr(19), ptr(9), ptr(1026.0)
			}),
		},
		{
			name: "9999, FEW and SCT only: no ceiling (recorded UGTB)",
			raw:  "METAR UGTB 041300Z 31013KT 9999 FEW030 SCT061 17/05 Q1026 NOSIG",
			ref:  ref, kind: KindMETAR, at: ref,
			want: fields(func(f *Fields) {
				f.WindDirDeg, f.WindSpeedMS = ptr(310), ptr(6.7)
				f.VisibilityM, f.VisibilityAtLeast, f.Ceiling = ptr(10000.0), true, CeilingNone
				f.TempC, f.DewPointC, f.QNHHPa = ptr(17), ptr(5), ptr(1026.0)
			}),
		},
		{
			name: "a gust and the lowest broken layer",
			raw:  "METAR UGTB 041300Z 31013G25KT 8000 SCT020 OVC035 BKN030 17/05 Q1026=",
			ref:  ref, kind: KindMETAR, at: ref,
			want: fields(func(f *Fields) {
				f.WindDirDeg, f.WindSpeedMS, f.GustMS = ptr(310), ptr(6.7), ptr(12.9)
				f.VisibilityM, f.Ceiling, f.CloudBaseFtAGL = ptr(8000.0), CeilingLayer, ptr(3000)
				f.TempC, f.DewPointC, f.QNHHPa = ptr(17), ptr(5), ptr(1026.0)
			}),
		},
		{
			name: "a missing dew point, MPS, showers and CB",
			raw:  "METAR UGTB 041330Z 24008MPS 4000 -SHRA FEW020CB OVC040 12/// Q1008",
			ref:  at(4, 13, 30), kind: KindMETAR, at: at(4, 13, 30),
			want: fields(func(f *Fields) {
				f.WindDirDeg, f.WindSpeedMS = ptr(240), ptr(8.0)
				f.VisibilityM, f.Ceiling, f.CloudBaseFtAGL = ptr(4000.0), CeilingLayer, ptr(4000)
				f.Weather, f.Convective, f.Precipitation = []string{"-SHRA"}, []string{"CB"}, []string{"RA"}
				f.TempC, f.QNHHPa = ptr(12), ptr(1008.0)
			}),
		},
		{
			name: "SPECI, statute miles, a thunderstorm, an obscured sky, inches of mercury",
			raw:  "SPECI KJFK 041351Z VRB03KT 1 1/2SM +TSRA BR VV008 22/21 A2992 RMK AO2 TSB45",
			ref:  at(4, 13, 51), kind: KindSPECI, at: at(4, 13, 51),
			want: fields(func(f *Fields) {
				f.WindVariable, f.WindSpeedMS = true, ptr(1.5)
				f.VisibilityM, f.Ceiling, f.CloudBaseFtAGL = ptr(2414.0), CeilingVerticalVisibility, ptr(800)
				f.Weather, f.Convective, f.Precipitation = []string{"+TSRA", "BR"}, []string{"TS"}, []string{"RA"}
				f.TempC, f.DewPointC, f.QNHHPa = ptr(22), ptr(21), ptr(1013.2)
			}),
		},
		{
			name: "AUTO with everything missing",
			raw:  "METAR UGTB 041300Z AUTO /////KT //// // ////// ///// Q////",
			ref:  ref, kind: KindMETAR, at: ref,
			want: newFields(),
		},
		{
			name: "COR, KMH, P6SM, NSC, negative temperatures and the groups not used",
			raw:  "METAR COR UGSB 041300Z 18036KMH P6SM 4000NE R13/P2000 NSC M02/M05 Q0998 RERA WS R13 W15/S3",
			ref:  ref, kind: KindMETAR, at: ref,
			want: fields(func(f *Fields) {
				f.WindDirDeg, f.WindSpeedMS = ptr(180), ptr(10.0)
				f.VisibilityM, f.VisibilityAtLeast, f.Ceiling = ptr(9656.0), true, CeilingNone
				f.TempC, f.DewPointC, f.QNHHPa = ptr(-2), ptr(-5), ptr(998.0)
			}),
		},
		{
			name: "a broken layer of unknown height: the ceiling is not reported",
			raw:  "METAR UGTB 041300Z 00000KT 1/4SM FG BKN/// OVC010 05/05 Q1020",
			ref:  ref, kind: KindMETAR, at: ref,
			want: fields(func(f *Fields) {
				f.WindDirDeg, f.WindSpeedMS = ptr(0), ptr(0.0)
				f.VisibilityM, f.Weather = ptr(402.0), []string{"FG"}
				f.TempC, f.DewPointC, f.QNHHPa = ptr(5), ptr(5), ptr(1020.0)
			}),
		},
		{
			name: "a trend with its times",
			raw:  "METAR UGTB 041300Z 31013KT 9999 FEW030 17/05 Q1026 TEMPO FM1400 TL1530 TSRA BKN015CB",
			ref:  ref, kind: KindMETAR, at: ref,
			want: fields(func(f *Fields) {
				f.WindDirDeg, f.WindSpeedMS = ptr(310), ptr(6.7)
				f.VisibilityM, f.VisibilityAtLeast, f.Ceiling = ptr(10000.0), true, CeilingNone
				f.TempC, f.DewPointC, f.QNHHPa = ptr(17), ptr(5), ptr(1026.0)
			}),
			ch: []Change{{Kind: "TEMPO", ValidFrom: at(4, 14, 0), ValidTo: ptr(at(4, 15, 30)), Fields: fields(func(f *Fields) {
				f.Weather, f.Convective, f.Precipitation = []string{"TSRA"}, []string{"TS", "CB"}, []string{"RA"}
				f.Ceiling, f.CloudBaseFtAGL = CeilingLayer, ptr(1500)
			})}},
		},
		{
			name: "across the end of a month",
			raw:  "METAR UGTB 302350Z 31005KT CAVOK 10/02 Q1030",
			ref:  time.Date(2026, 9, 30, 23, 50, 0, 0, time.UTC), kind: KindMETAR, at: time.Date(2026, 9, 30, 23, 50, 0, 0, time.UTC),
			want: fields(func(f *Fields) {
				f.WindDirDeg, f.WindSpeedMS = ptr(310), ptr(2.6)
				f.VisibilityM, f.VisibilityAtLeast, f.CAVOK, f.Ceiling = ptr(10000.0), true, true, CeilingNone
				f.TempC, f.DewPointC, f.QNHHPa = ptr(10), ptr(2), ptr(1030.0)
			}),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := Parse(c.raw, c.ref)
			if err != nil {
				t.Fatal(err)
			}
			if r.Kind != c.kind || !r.IssuedAt.Equal(c.at) || !reflect.DeepEqual(r.Fields, c.want) {
				t.Errorf("got %s %s\n%+v\nwant %s %s\n%+v", r.Kind, r.IssuedAt, show(r.Fields), c.kind, c.at, show(c.want))
			}
			want := c.ch
			if want == nil {
				want = []Change{}
			}
			if !reflect.DeepEqual(r.Changes, want) {
				t.Errorf("changes %+v, want %+v", r.Changes, want)
			}
			if r.Raw != strings.TrimSuffix(c.raw, "=") {
				t.Errorf("raw %q", r.Raw)
			}
		})
	}
}

func show(f Fields) string {
	b, _ := json.Marshal(f)
	return string(b)
}

func TestParseTAF(t *testing.T) {
	issued := at(4, 11, 0)
	r, err := Parse("TAF UGKO 041100Z 0412/0512 VRB02KT 9999 SCT050 BKN100 TX22/0511Z TN07/0502Z BECMG 0413/0416 CAVOK "+
		"PROB40 TEMPO 0422/0506 0300 FG VV002 TEMPO 0508/0512 26010KT BKN080", issued)
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind != KindTAF || r.Station != "UGKO" || !r.IssuedAt.Equal(issued) || !r.ValidFrom.Equal(at(4, 12, 0)) || !r.ValidTo.Equal(at(5, 12, 0)) {
		t.Fatalf("header %+v", r)
	}
	want := fields(func(f *Fields) {
		f.WindVariable, f.WindSpeedMS = true, ptr(1.0)
		f.VisibilityM, f.VisibilityAtLeast, f.Ceiling, f.CloudBaseFtAGL = ptr(10000.0), true, CeilingLayer, ptr(10000)
	})
	if !reflect.DeepEqual(r.Fields, want) {
		t.Errorf("base %s", show(r.Fields))
	}
	wantCh := []Change{
		{Kind: "BECMG", ValidFrom: at(4, 13, 0), ValidTo: ptr(at(4, 16, 0)), Fields: fields(func(f *Fields) {
			f.CAVOK, f.VisibilityAtLeast, f.VisibilityM, f.Ceiling = true, true, ptr(10000.0), CeilingNone
		})},
		{Kind: "PROB40 TEMPO", ValidFrom: at(4, 22, 0), ValidTo: ptr(at(5, 6, 0)), Fields: fields(func(f *Fields) {
			f.VisibilityM, f.Weather, f.Ceiling, f.CloudBaseFtAGL = ptr(300.0), []string{"FG"}, CeilingVerticalVisibility, ptr(200)
		})},
		{Kind: "TEMPO", ValidFrom: at(5, 8, 0), ValidTo: ptr(at(5, 12, 0)), Fields: fields(func(f *Fields) {
			f.WindDirDeg, f.WindSpeedMS, f.Ceiling, f.CloudBaseFtAGL = ptr(260), ptr(5.1), CeilingLayer, ptr(8000)
		})},
	}
	if !reflect.DeepEqual(r.Changes, wantCh) {
		for i := range r.Changes {
			t.Logf("%d %s %s %v %s", i, r.Changes[i].Kind, r.Changes[i].ValidFrom, r.Changes[i].ValidTo, show(r.Changes[i].Fields))
		}
		t.Error("changes differ")
	}
}

func TestParseTAFFromGroupsHoldUntilTheNext(t *testing.T) {
	r, err := Parse("TAF AMD UGTB 041100Z 0412/0512 32016KT CAVOK FM041800 20005KT 6000 BKN012 FM050600 VRB02KT 9999 NSC RMK NXT FCST BY 17Z", at(4, 11, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Changes) != 2 || r.Changes[0].Kind != "FM" || !r.Changes[0].ValidFrom.Equal(at(4, 18, 0)) || !r.Changes[0].ValidTo.Equal(at(5, 6, 0)) ||
		!r.Changes[1].ValidTo.Equal(at(5, 12, 0)) || r.Changes[0].Fields.Ceiling != CeilingLayer || *r.Changes[0].Fields.CloudBaseFtAGL != 1200 ||
		r.Changes[1].Fields.Ceiling != CeilingNone {
		t.Fatalf("%+v", r.Changes)
	}
}

// Every refusal names what is wrong; nothing of a refused report is
// used (the presence twin is every case above).
func TestParseRefuses(t *testing.T) {
	cases := map[string]struct {
		raw  string
		want string
	}{
		"not a report":       {"ZZZZ UGTB 041300Z", "does not start"},
		"bad station":        {"METAR ug1 041300Z 31013KT", "location indicator"},
		"no time":            {"METAR UGTB 31013KT", "DDHHMMZ"},
		"time not a time":    {"METAR UGTB 041360Z 31013KT", "does not resolve"},
		"unknown group":      {"METAR UGTB 041300Z 31013KT 9999 XYZZY 17/05 Q1026", `unknown group "XYZZY"`},
		"unknown after body": {"METAR UGTB 041300Z 31013KT 9999 Q1026 NOSIG FOO", `unknown group "FOO"`},
		"two visibilities":   {"METAR UGTB 041300Z 31013KT 9999 8000 Q1026", "second visibility"},
		"two temperatures":   {"METAR UGTB 041300Z 31013KT 9999 17/05 18/05", "second temperature"},
		"wind direction":     {"METAR UGTB 041300Z 37013KT 9999", "wind direction"},
		"wind variation":     {"METAR UGTB 041300Z 31013KT 370V010 9999", "wind variation"},
		"AMD on a METAR":     {"METAR AMD UGTB 041300Z 31013KT", "AMD outside"},
		"zero denominator":   {"METAR UGTB 041300Z 31013KT 1/0SM", "zero denominator"},
		"mixed zero":         {"METAR UGTB 041300Z 31013KT 1 1/0SM", "visibility"},
		"trend time":         {"METAR UGTB 041300Z 31013KT 9999 TEMPO FM2460 TSRA", "trend time"},
		"TAF no period":      {"TAF UGTB 041100Z 32016KT CAVOK", "validity period"},
		"TAF bad period":     {"TAF UGTB 041100Z 0412/0412 32016KT", "does not resolve"},
		"TAF FM outside":     {"TAF UGTB 041100Z 0412/0512 32016KT FM060000 20005KT", "outside the TAF"},
		"TAF TEMPO outside":  {"TAF UGTB 041100Z 0412/0512 32016KT TEMPO 0510/0514 BKN010", "outside the TAF"},
		"TAF TEMPO period":   {"TAF UGTB 041100Z 0412/0512 32016KT TEMPO 32010KT", "validity period"},
		"TAF unknown":        {"TAF UGTB 041100Z 0412/0512 32016KT CAVOK 620304", `unknown group "620304"`},
		"TAF temperature":    {"TAF UGTB 041100Z 0412/0512 32016KT 17/05", "unknown group"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(c.raw, ref)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%v, want %q", err, c.want)
			}
		})
	}
}

func TestParseNIL(t *testing.T) {
	for _, raw := range []string{"METAR UGTB 041300Z NIL=", "TAF UGTB 041100Z NIL", "TAF UGTB 041100Z 0412/0512 CNL", "TAF AMD UGTB 041100Z CNL"} {
		if _, err := Parse(raw, ref); !errors.Is(err, ErrNil) {
			t.Errorf("%s: %v", raw, err)
		}
	}
}

// E-10: each bound of a report is exceeded and refused, and met and
// accepted.
func TestParseBounds(t *testing.T) {
	head := "METAR UGTB 041300Z 31013KT 9999 "
	at := head + strings.Repeat("SCT050 ", MaxGroups-6) + "Q1026"
	if len(strings.Fields(at)) != MaxGroups || len(at) > MaxRawBytes {
		t.Fatal(len(strings.Fields(at)), len(at))
	}
	if _, err := Parse(at, ref); err != nil {
		t.Fatalf("at the bound: %v", err)
	}
	if _, err := Parse(at+" Q1026", ref); err == nil || !strings.Contains(err.Error(), "more than 200 groups") {
		t.Fatalf("over the group bound: %v", err)
	}
	if _, err := Parse(head+strings.Repeat("X", MaxRawBytes), ref); err == nil || !strings.Contains(err.Error(), "bytes") {
		t.Fatalf("over the byte bound: %v", err)
	}
	wx := head + strings.Repeat("RA ", MaxChanges)
	if _, err := Parse(wx, ref); err != nil {
		t.Fatalf("weather groups at the bound: %v", err)
	}
	if _, err := Parse(wx+"RA", ref); err == nil || !strings.Contains(err.Error(), "weather groups") {
		t.Fatalf("weather groups over the bound: %v", err)
	}
	tr := head + strings.Repeat("BECMG 32010KT ", MaxChanges)
	if r, err := Parse(tr, ref); err != nil || len(r.Changes) != MaxChanges {
		t.Fatalf("changes at the bound: %v", err)
	}
	if _, err := Parse(tr+"BECMG 32010KT", ref); err == nil || !strings.Contains(err.Error(), "change groups") {
		t.Fatalf("changes over the bound: %v", err)
	}
	taf := "TAF UGTB 041100Z 0412/0512 32016KT " + strings.Repeat("TEMPO 0414/0416 BKN010 ", MaxChanges)
	if r, err := Parse(taf, at4(11)); err != nil || len(r.Changes) != MaxChanges {
		t.Fatalf("TAF changes at the bound: %v", err)
	}
	if _, err := Parse(taf+"TEMPO 0414/0416 BKN010", at4(11)); err == nil || !strings.Contains(err.Error(), "change groups") {
		t.Fatalf("TAF changes over the bound: %v", err)
	}
	if _, err := Parse("TAF UGTB 041100Z 0412/0612 32016KT", at4(11)); err == nil {
		t.Fatal("a TAF longer than MaxTAFSpan")
	}
}

func at4(hour int) time.Time { return time.Date(2026, 10, 4, hour, 0, 0, 0, time.UTC) }

func TestResolve(t *testing.T) {
	if _, ok := resolve(ref, 31, 0, 0); ok {
		t.Error("31 resolved near 4 October (no 31 September, 31 October is far)")
	}
	if got, ok := resolve(ref, 4, 24, 0); !ok || !got.Equal(at(5, 0, 0)) {
		t.Errorf("hour 24: %v %v", got, ok)
	}
	for _, bad := range [][3]int{{0, 1, 0}, {32, 1, 0}, {4, 25, 0}, {4, 24, 30}, {4, 1, 60}} {
		if _, ok := resolve(ref, bad[0], bad[1], bad[2]); ok {
			t.Errorf("%v resolved", bad)
		}
	}
	jan := time.Date(2027, 1, 1, 0, 30, 0, 0, time.UTC)
	if got, ok := resolve(jan, 31, 23, 50); !ok || !got.Equal(time.Date(2026, 12, 31, 23, 50, 0, 0, time.UTC)) {
		t.Errorf("across the year: %v", got)
	}
}

// FuzzParse: no input panics; a report that parses keeps every bound
// and says a ceiling height only with a ceiling.
func FuzzParse(f *testing.F) {
	for _, s := range []string{
		"METAR UGKO 041300Z 29005KT 260V320 CAVOK 19/09 Q1026 NOSIG",
		"SPECI KJFK 041351Z VRB03KT 1 1/2SM +TSRA BR VV008 22/21 A2992 RMK AO2",
		"METAR UGTB 041300Z 31013KT 9999 FEW030 17/05 Q1026 TEMPO FM1400 TL1530 TSRA BKN015CB",
		"TAF UGKO 041100Z 0412/0512 VRB02KT 9999 SCT050 BKN100 TX22/0511Z TN07/0502Z BECMG 0413/0416 CAVOK PROB40 TEMPO 0422/0506 0300 FG VV002",
		"TAF AMD UGTB 041100Z 0412/0512 32016KT CAVOK FM041800 20005KT 6000 BKN012",
		"METAR UGTB 041300Z AUTO /////KT //// // ////// ///// Q////",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		r, err := Parse(raw, ref)
		if err != nil {
			return
		}
		if len(r.Raw) > MaxRawBytes || len(r.Changes) > MaxChanges || len(strings.Fields(r.Raw)) > MaxGroups {
			t.Fatalf("a bound broken: %q", raw)
		}
		all := append([]Fields{r.Fields}, fieldsOf(r.Changes)...)
		for i := range all {
			fs := &all[i]
			switch fs.Ceiling {
			case CeilingLayer, CeilingVerticalVisibility:
				if fs.CloudBaseFtAGL == nil {
					t.Fatalf("a ceiling without a height: %q", raw)
				}
			case CeilingNone, CeilingNotReported:
				if fs.CloudBaseFtAGL != nil {
					t.Fatalf("a height without a ceiling: %q", raw)
				}
			default:
				t.Fatalf("ceiling %q: %q", fs.Ceiling, raw)
			}
			if fs.Weather == nil || fs.Convective == nil || fs.Precipitation == nil {
				t.Fatalf("a nil list: %q", raw)
			}
		}
		if r.Kind == KindTAF && (!r.ValidTo.After(r.ValidFrom) || r.ValidTo.Sub(r.ValidFrom) > MaxTAFSpan) {
			t.Fatalf("a TAF validity out of bounds: %q", raw)
		}
	})
}
