//go:build integration

package integration

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/geodesy"

	"github.com/rootxkit/uspace-ussp/internal/accounts"
	"github.com/rootxkit/uspace-ussp/internal/intent"
	"github.com/rootxkit/uspace-ussp/internal/national"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/store"
	fakewx "github.com/rootxkit/uspace-ussp/internal/testfakes/weather"
	"github.com/rootxkit/uspace-ussp/internal/weather"
	weatherstore "github.com/rootxkit/uspace-ussp/internal/weather/pgstore"
)

// weatherRig is the weather service on the real database against the
// fake source, with a METAR observed a minute ago and a TAF valid from
// the last hour for a day at the station UGTB placed at o.
type weatherRig struct {
	t     *testing.T
	fake  *fakewx.Fake
	svc   *weather.Service
	pol   atomic.Pointer[policy.Record]
	o     core.LatLon
	obsAt time.Time
}

func newWeatherRig(t *testing.T, o core.LatLon, metarBody string) *weatherRig {
	t.Helper()
	ensureSchemas(t)
	w := &weatherRig{t: t, fake: fakewx.New(), o: o, obsAt: time.Now().UTC().Add(-time.Minute).Truncate(time.Minute)}
	t.Cleanup(w.fake.Close)
	issued := time.Now().UTC().Add(-time.Hour).Truncate(time.Hour)
	w.fake.Set([]fakewx.METAR{fakewx.NewMETAR("UGTB", w.obsAt, o.LatDeg, o.LonDeg, metarBody)},
		[]fakewx.TAF{fakewx.NewTAF("UGTB", issued, issued, issued.Add(24*time.Hour), o.LatDeg, o.LonDeg, "32016KT CAVOK")})
	r := policy.Record{Version: 1, Values: policy.Defaults()}
	r.Values.WeatherStationIDs = []string{"UGTB"}
	r.Values.WeatherAreaRadiusM = policy.MaxWeatherAreaRadiusM
	w.pol.Store(&r)
	src, err := weather.NewSource("awc:"+w.fake.URL(), nil)
	if err != nil {
		t.Fatal(err)
	}
	w.svc = w.service(src)
	name := src.Name()
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = relOwner(t).Exec(ctx, "DELETE FROM weather_products WHERE source = $1", name)
		_, _ = relOwner(t).Exec(ctx, "DELETE FROM weather_source_status WHERE source = $1", name)
	})
	return w
}

// service is a new service on the same database and source: what the
// api process is after a restart.
func (w *weatherRig) service(src weather.Source) *weather.Service {
	return &weather.Service{Source: src, Store: weatherstore.Store{S: appStore(w.t)}, Counters: &core.Counters{}, Logger: quiet(),
		Policy: func() policy.Record { return *w.pol.Load() }}
}

func (w *weatherRig) box() geodesy.BBox {
	return geodesy.BBox{MinLat: w.o.LatDeg - 0.05, MinLon: w.o.LonDeg - 0.05, MaxLat: w.o.LatDeg + 0.05, MaxLon: w.o.LonDeg + 0.05}
}

// weatherAdapter is the api process's adapter (internal/app/api) as the
// decision sees it.
type weatherAdapter struct{ s *weather.Service }

func (a weatherAdapter) Check(ctx context.Context, boxes []geodesy.BBox, from, to time.Time) intent.WeatherCheck {
	c := a.s.Check(ctx, boxes, from, to)
	return intent.WeatherCheck{Ref: c.Ref(), Unavailable: c.Unavailable, Stale: c.Stale, Advisories: c.Advisories}
}

// The done-when E-02 triple on the real database: configured and never
// fetched is stale with nothing; up is fresh, stored once however often
// it is polled; the fake source down answers the last products stale
// with the failure time, and so does a new service after a restart (the
// state is the database's); back up is fresh.
func TestIntegrationWeatherFreshStaleAndAcrossARestart(t *testing.T) {
	ctx := context.Background()
	w := newWeatherRig(t, origin(t), "31013G25KT 9999 FEW030 BKN045 17/05 Q1026")
	a, err := w.svc.Answer(ctx, w.box(), time.Time{})
	if err != nil || !a.Stale || a.Source.State != weather.StateNeverFetched || len(a.Products) != 0 {
		t.Fatalf("never fetched: %+v %v", a, err)
	}
	for range 2 {
		if err := w.svc.Poll(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := count(t, relOwner(t), "SELECT count(*) FROM weather_products WHERE source = $1", w.svc.Source.Name()); n != 2 {
		t.Fatalf("%d rows after two polls of two reports", n)
	}
	if n := count(t, relOwner(t), "SELECT count(*) FROM events WHERE entity_id = $1 AND event_type = $2", w.svc.Source.Name(), weatherstore.EventFetched); n != 1 {
		t.Fatalf("%d events rows (the second poll stored nothing)", n)
	}
	a, err = w.svc.Answer(ctx, w.box(), time.Time{})
	if err != nil || a.Stale || a.Source.State != weather.StateUp || len(a.Products) != 2 {
		t.Fatalf("up: %+v %v", a, err)
	}
	m := a.Products[0]
	if m.Kind != weather.KindMETAR || !m.ObservedAt.Equal(w.obsAt) || !m.InForce || m.AgeS < 60 || m.AgeS > 600 ||
		*m.Fields.GustMS != 12.9 || *m.Fields.CloudBaseFtAGL != 4500 || m.QNHArea != "UGTB" || m.Area.RadiusM != policy.MaxWeatherAreaRadiusM {
		t.Fatalf("METAR %+v", m)
	}
	// The area is a circle of radius_m on the ellipsoid: a box 2 degrees
	// away (over 200 km) meets none of it.
	far := geodesy.BBox{MinLat: w.o.LatDeg + 2, MinLon: w.o.LonDeg, MaxLat: w.o.LatDeg + 2.1, MaxLon: w.o.LonDeg + 0.1}
	if a, _ := w.svc.Answer(ctx, far, time.Time{}); len(a.Products) != 0 {
		t.Fatalf("far away: %+v", a.Products)
	}

	w.fake.Down()
	if err := w.svc.Poll(ctx); err == nil {
		t.Fatal("a failed fetch reported success")
	}
	restarted := w.service(w.svc.Source)
	// The failure time is the database clock's: it is compared with that
	// clock, never with this process's.
	dbNow, err := w.svc.Store.Now(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for name, svc := range map[string]*weather.Service{"running": w.svc, "restarted": restarted} {
		a, err := svc.Answer(ctx, w.box(), time.Time{})
		if err != nil || !a.Stale || a.Source.State != weather.StateFailing || a.Source.LastFailureAt == nil ||
			a.Source.LastFailureAt.After(dbNow) || dbNow.Sub(*a.Source.LastFailureAt) > time.Minute ||
			!strings.Contains(*a.Source.Failure, "503") || len(a.Products) != 2 || !a.Products[0].Stale {
			t.Fatalf("%s, the source down: %+v %v", name, a, err)
		}
	}
	if st, d := restarted.Probe()(ctx); st != "degraded" || !strings.Contains(d, "failed since") {
		t.Fatal(st, d)
	}

	w.fake.Up()
	if err := restarted.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if a, err := w.svc.Answer(ctx, w.box(), time.Time{}); err != nil || a.Stale || a.Source.State != weather.StateUp || a.Source.Failure != nil {
		t.Fatalf("back: %+v %v", a, err)
	}
	if st, _ := w.svc.Probe()(ctx); st != "up" {
		t.Fatal(st)
	}
}

// GET /v1/weather through the national API with a ussp.geo token: the
// configured service answers 200; no service is 503 weather_unavailable
// with reason not_configured, never an empty answer (E-02 both).
func TestIntegrationWeatherRoute(t *testing.T) {
	ctx := context.Background()
	w := newWeatherRig(t, origin(t), "31013KT 9999 FEW030 17/05 Q1026")
	if err := w.svc.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	bbox := func() string {
		b := w.box()
		f := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
		return strings.Join([]string{f(b.MinLon), f(b.MinLat), f(b.MaxLon), f(b.MaxLat)}, ",")
	}
	for name, c := range map[string]struct {
		svc    national.WeatherAnswerer
		status int
	}{
		"configured": {w.svc, 200},
		"no service": {nil, 503},
		"no source":  {&weather.Service{Store: weatherstore.Store{S: appStore(t)}}, 503},
		"restarted":  {w.service(w.svc.Source), 200},
	} {
		t.Run(name, func(t *testing.T) {
			s := newStackWith(t, newClock(), &logBuffer{}, nil, func(ns *national.Server) {
				if c.svc != nil {
					ns.Weather = c.svc
				}
			})
			token := geoToken(t, s)
			r := s.call("GET", "/v1/weather?bbox="+bbox(), nil, bearer(token))
			if r.status != c.status {
				t.Fatalf("%d %s", r.status, r.raw)
			}
			if c.status == 503 {
				if r.str("type") != "https://schemas.uspace.ge/problems/weather_unavailable" || !strings.Contains(r.raw, `"reason":"not_configured"`) ||
					!strings.Contains(r.raw, `"field":"weather_source"`) {
					t.Fatalf("%s", r.raw)
				}
				return
			}
			ps, _ := r.body["products"].([]any)
			if len(ps) != 2 || r.body["stale"] != false {
				t.Fatalf("%s", r.raw)
			}
			// Without the geo scope the route is refused (fail closed).
			if r := s.call("GET", "/v1/weather?bbox="+bbox(), nil, nil); r.status != 401 {
				t.Fatalf("no token: %d", r.status)
			}
		})
	}
}

// geoToken registers an operator and returns a token of a client with
// ussp.geo.
func geoToken(t *testing.T, s *stack) string {
	t.Helper()
	s.registry.set(accounts.RegistryValid)
	u := unique()
	user, pass := "weather."+u, "weather-password-"+u
	r := s.call("POST", "/v1/accounts/operators", map[string]any{
		"registration_number": "GEO-TEST-" + u, "display_name": "Weather operator " + u, "contact_email": "wx" + u + "@example.test",
		"admin_username": user, "admin_password": pass,
	}, nil)
	if r.status != 201 {
		t.Fatalf("register: %d %s", r.status, r.raw)
	}
	l := s.login("portal", user, pass, "")
	id, secret := s.client(r.str("id"), l.str("token"), "ussp.geo")
	tok := s.token(id, secret)
	if tok.status != 200 {
		t.Fatalf("token: %d %s", tok.status, tok.raw)
	}
	return tok.str("access_token")
}

// The done-when decision pair on the real database: an intent decided
// with a product in force carries its id in weather_checked_ref, the
// column and the stored decision alike; without a source it is null
// with the condition weather_unavailable, and still authorised.
func TestIntegrationIntentWeatherCheckedRef(t *testing.T) {
	g := newIntentRig(t)
	g.publishAll(nil, nil, nil)
	number, serial, token := g.operatorClient()
	r := g.file(token, g.request(number, serial, "wx-in-force", g.box(0, 0, 0.01)))
	ref, _ := r.body["weather_checked_ref"].(string)
	if r.status != 201 || r.str("decision") != "authorised" || ref == "" || strings.Contains(r.raw, intent.CondWeatherUnavailable) {
		t.Fatalf("%d %s", r.status, r.raw)
	}
	ids := strings.Split(ref, ",")
	if len(ids) != 1 {
		// The METAR (observed a minute ago) is no longer in force an
		// hour ahead; the TAF is.
		t.Fatalf("consulted %v", ids)
	}
	if n := count(t, relOwner(t), "SELECT count(*) FROM weather_products WHERE id = $1::uuid AND kind = 'taf'", ids[0]); n != 1 {
		t.Fatalf("the ref %s is not the TAF in force", ids[0])
	}
	if n := count(t, relOwner(t), "SELECT count(*) FROM operational_intents WHERE id = $1::uuid AND weather_checked_ref = $2", r.str("intent_id"), ref); n != 1 {
		t.Fatal("weather_checked_ref is not stored on the intent")
	}

	g.svc.Decider.Weather = weatherAdapter{&weather.Service{Store: weatherstore.Store{S: appStore(t)}}}
	r = g.file(token, g.request(number, serial, "wx-none", g.box(0.1, 0, 0.01)))
	if r.status != 201 || r.str("decision") != "authorised" || r.body["weather_checked_ref"] != nil || !strings.Contains(r.raw, `"code":"weather_unavailable"`) {
		t.Fatalf("%d %s", r.status, r.raw)
	}
	if n := count(t, relOwner(t), "SELECT count(*) FROM operational_intents WHERE id = $1::uuid AND weather_checked_ref IS NULL", r.str("intent_id")); n != 1 {
		t.Fatal("a null weather_checked_ref is not stored as null")
	}
}

// 00024's Down keeps the products and its Up reads their station and
// kind back from what each row holds, so a rollback and a redeploy keep
// the products the decisions name; a row that says neither is removed
// (E-01 pair).
func TestIntegrationWeatherMigrationDownAndUpKeepsProducts(t *testing.T) {
	ctx := context.Background()
	w := newWeatherRig(t, origin(t), "31013KT 9999 FEW030 17/05 Q1026")
	if err := w.svc.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	name := w.svc.Source.Name()
	owner := relOwner(t)
	if _, err := owner.MigrateDown(ctx, store.TreeRelational, 23); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := relOwner(t).Migrate(context.Background(), store.TreeRelational); err != nil {
			t.Errorf("restore the schema: %v", err)
		}
	})
	if _, err := owner.Exec(ctx, `INSERT INTO weather_products (area, observed_at, valid_from, valid_to, source, product, fetched_at)
		VALUES (ST_Buffer(ST_MakePoint(0, 0)::geography, 10), now(), now(), now() + interval '1 hour', $1, '{"raw":"garbled"}', now())`, name); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Migrate(ctx, store.TreeRelational); err != nil {
		t.Fatal(err)
	}
	if n := count(t, owner, "SELECT count(*) FROM weather_products WHERE source = $1", name); n != 2 {
		t.Fatalf("%d products after down and up, want the 2 stored", n)
	}
	if n := count(t, owner, "SELECT count(*) FROM weather_products WHERE source = $1 AND station = 'UGTB' AND kind IN ('metar', 'taf')", name); n != 2 {
		t.Fatalf("%d products read back", n)
	}
	if a, err := w.svc.Answer(ctx, w.box(), time.Time{}); err != nil || len(a.Products) != 2 {
		t.Fatalf("%+v %v", a, err)
	}
}
