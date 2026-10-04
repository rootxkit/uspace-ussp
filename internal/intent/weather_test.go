package intent

import (
	"strings"
	"testing"
)

func codes(cs []Condition) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Code)
	}
	return out
}

func hasCode(cs []Condition, code string) bool {
	for _, c := range cs {
		if c.Code == code {
			return true
		}
	}
	return false
}

// A product in force: its id in weather_checked_ref, no weather
// condition, and the weather was asked about the intent's own window
// (brief WP-16 done-when; the absence twin is the next test).
func TestDecisionCarriesTheWeatherConsulted(t *testing.T) {
	g := newRig()
	w := &fakeWeather{}
	g.decider.Weather = w
	s, _, _ := newService(g)
	d, _, err := submit(t, s, baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	if d.Decision != DecisionAuthorised || d.WeatherCheckedRef == nil || *d.WeatherCheckedRef != testWeatherRef {
		t.Fatalf("%s %v", d.Decision, d.WeatherCheckedRef)
	}
	for _, c := range []string{CondWeatherUnavailable, CondWeatherStale, CondWeatherAdvisory} {
		if hasCode(d.Conditions, c) {
			t.Errorf("condition %s with a fresh product in force: %v", c, codes(d.Conditions))
		}
	}
	if len(w.asked) != 2 || !w.asked[0].Equal(t0) || !w.asked[1].Equal(t1) {
		t.Errorf("asked %v", w.asked)
	}
}

// Without a weather service, and with one that has no product in
// force: weather_checked_ref null and weather_unavailable, saying why;
// the intent is still authorised (weather never rejects).
func TestDecisionWithoutWeather(t *testing.T) {
	for name, w := range map[string]Weather{
		"no service":        nil,
		"none in force":     &fakeWeather{check: &WeatherCheck{Unavailable: "no weather product of the source is in force"}},
		"silent no product": &fakeWeather{check: &WeatherCheck{}},
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig()
			g.decider.Weather = w
			s, _, _ := newService(g)
			d, _, err := submit(t, s, baseRequest())
			if err != nil {
				t.Fatal(err)
			}
			if d.Decision != DecisionAuthorised || d.WeatherCheckedRef != nil || !hasCode(d.Conditions, CondWeatherUnavailable) {
				t.Fatalf("%s %v %v", d.Decision, d.WeatherCheckedRef, codes(d.Conditions))
			}
			if g.counters.Get("intent_"+CondWeatherUnavailable) != 1 {
				t.Fatal(g.counters.Snapshot())
			}
			for _, c := range d.Conditions {
				if c.Code == CondWeatherUnavailable && c.Detail == "" {
					t.Fatal("no detail")
				}
			}
		})
	}
}

// A stale source and an advisory are conditions the operator reads,
// never a conflict; the advisories are bounded (E-10).
func TestDecisionWeatherStaleAndAdvisory(t *testing.T) {
	g := newRig()
	advs := make([]string, MaxWeatherAdvisories+3)
	for i := range advs {
		advs[i] = "METAR UGTB " + strings.Repeat("x", i) + ": wind or gust 12.0 m/s at or above the policy's 10.0 m/s"
	}
	g.decider.Weather = &fakeWeather{check: &WeatherCheck{Ref: ptr(testWeatherRef), Stale: "the weather source has failed since T", Advisories: advs}}
	s, _, _ := newService(g)
	d, _, err := submit(t, s, baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	if d.Decision != DecisionAuthorised || len(d.Conflicts) != 0 || d.WeatherCheckedRef == nil {
		t.Fatalf("%s %+v", d.Decision, d.Conflicts)
	}
	n := 0
	for _, c := range d.Conditions {
		if c.Code == CondWeatherAdvisory {
			n++
		}
	}
	if n != MaxWeatherAdvisories || !hasCode(d.Conditions, CondWeatherStale) || hasCode(d.Conditions, CondWeatherUnavailable) {
		t.Fatalf("%d advisories, %v", n, codes(d.Conditions))
	}
}
