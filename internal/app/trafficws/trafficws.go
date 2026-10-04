// Package trafficws is the traffic-ws process (docs/PLAN.md §3.1; brief
// WP-11): traffic information and the alerts to operators and the
// console, over WebSockets in the console frame (M29).
//
// WS /v1/traffic (an operator's intent, or a staff bbox and
// console/subscribe/v1), GET /v1/traffic/snapshot and WS /v1/alerts (an
// operator's intent) are served from one picture (Hub): the latest
// sample of every track on trk.v1, peer.v1 and man.v1 (core NATS), every
// alert on alrt.v1 (replayed from ALRT so a restart holds the active
// alerts at once), the source statuses of src.v1, and the projections
// intent_active, client_bindings, policy, source_control and
// cis_current (D6: it never opens a database). Every frame carries the
// common envelope; a product says which inputs are degraded and since
// when, and a track whose source is silent or switched off stays with
// its age and state (B-11, SC-16, SC-22).
//
// Nothing here has a send path towards an aircraft (CLAUDE.md rule 1):
// the sockets carry information to people, and no frame carries
// resolution advice (rule 2).
package trafficws

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/geoid"

	"github.com/rootxkit/uspace-ussp/internal/app/proc"
	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/cis"
	"github.com/rootxkit/uspace-ussp/internal/config"
	"github.com/rootxkit/uspace-ussp/internal/geo"
	"github.com/rootxkit/uspace-ussp/internal/intent"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/sources"
	"github.com/rootxkit/uspace-ussp/internal/telemetry"
	"github.com/rootxkit/uspace-ussp/internal/traffic"
)

// Readiness dependencies beside nats.
const (
	DepTracks         = "tracks"
	DepAlerts         = "alerts"
	DepIntentActive   = "intent_active"
	DepClientBindings = "client_bindings"
	DepPolicy         = "policy"
	DepSourceControl  = "source_control"
	DepCIS            = "cis_current"
	DepSessionsLive   = "sessions_live"
	DepGeoid          = "geoid"
)

// AlertReplay is how far back the alert feed starts: every active alert
// is republished every second (C-08), so a start holds them all at once.
const AlertReplay = 30 * time.Second

// Options are what a test replaces.
type Options struct {
	// Server, when set, receives the server once it exists.
	Server func(*Server)
	// ProductEvery, StatusEvery, RecordEvery and RepeatEvery override
	// the periods (zero keeps them).
	ProductEvery, StatusEvery, RecordEvery, RepeatEvery time.Duration
}

// Spec declares the process and the dependencies it reads.
var Spec = SpecWith(Options{})

// SpecWith is the process with o.
func SpecWith(o Options) proc.Spec {
	return proc.Spec{
		Process: config.ProcessTrafficWS,
		NATS:    proc.Required,
		Routes:  func(ctx context.Context, mux *http.ServeMux, rt *proc.Runtime) error { return routes(ctx, mux, rt, o) },
	}
}

// Run runs traffic-ws with cfg until ctx ends.
func Run(ctx context.Context, cfg config.Config) error {
	return proc.Run(ctx, cfg, Spec, proc.Options{})
}

// ownIssuer is the iss of this USSP's own tokens.
func ownIssuer(cfg config.Config) string {
	if cfg.IssuerURL != "" {
		return cfg.IssuerURL
	}
	if len(cfg.Audiences) > 0 {
		return "https://" + cfg.Audiences[0]
	}
	return ""
}

func mirrorProbe[T any](m *bus.Mirror[T], what string) obs.Probe {
	return func(context.Context) (obs.State, string) {
		_, age, loaded := m.Snapshot()
		if !loaded {
			return obs.StateUnknown, what + " not read yet"
		}
		if age > 0 {
			return obs.StateDegraded, fmt.Sprintf("%s: the watch is down; last read %.0f s ago", what, age)
		}
		return obs.StateUp, ""
	}
}

func routes(ctx context.Context, mux *http.ServeMux, rt *proc.Runtime, o Options) error {
	cfg := rt.Config
	logger := rt.Logger
	js := rt.Bus.JetStream()
	verifier, _, err := proc.TokenVerifier(ctx, rt, nil)
	if err != nil {
		return err
	}
	// The projections (D6).
	followCounters := &core.Counters{}
	proc.Publish(rt, "projections", followCounters)
	// A console session is live while api's sessions_live holds it
	// (audit B2): signed out, ended or idle in api, it is refused here
	// at the upgrade and its open sockets close with 4401. Until the
	// bucket was read once, a session is neither admitted nor refused
	// (503).
	sessionsLive := &bus.Mirror[auth.LiveSession]{JS: js, Bucket: bus.BucketSessionsLive, Counters: followCounters, Logger: logger}
	sessions := auth.LiveSessions{Get: func(jti string) (auth.LiveSession, bool, bool) {
		v, found, _, loaded := sessionsLive.Get(bus.KeyToken(jti))
		return v, found, loaded
	}}
	guardCounters := &core.Counters{}
	proc.Publish(rt, "guard", guardCounters)
	guard := &auth.Guard{Verifier: verifier, OwnIssuer: ownIssuer(cfg), Sessions: sessions, Counters: guardCounters, Logger: logger}
	rt.Health.Register(DepSessionsLive, false, mirrorProbe(sessionsLive, "sessions_live"))
	pol := &bus.Follower[policy.Record]{JS: js, Bucket: bus.BucketPolicy, Key: bus.KeyPolicy, Decode: telemetry.DecodePolicy,
		Core: rt.Bus.Conn, Push: bus.CtlPolicy, Counters: followCounters, Logger: logger}
	current := func() policy.Record {
		if r, _, ok := pol.Value(); ok {
			return r
		}
		return policy.Record{Values: policy.Defaults()}
	}
	src := sources.Follow(rt.Bus, logger)
	intents := &bus.Mirror[intent.StateBody]{JS: js, Bucket: bus.BucketIntentActive, Counters: followCounters, Logger: logger}
	bindings := &bus.Mirror[[]string]{JS: js, Bucket: bus.BucketClientBindings, Counters: followCounters, Logger: logger}
	basis := &bus.Follower[cis.BasisValue]{JS: js, Bucket: bus.BucketCISCurrent, Key: cis.KeyBasis, Counters: followCounters, Logger: logger,
		Decode: func(data []byte) (cis.BasisValue, error) {
			v, err := telemetry.DecodeCIS(cis.KeyBasis, data)
			if err != nil || v.Basis == nil {
				return cis.BasisValue{}, core.Fieldf("basis", "not a CIS basis")
			}
			return *v.Basis, nil
		}}
	rt.Health.Register(DepIntentActive, false, mirrorProbe(intents, "intent_active"))
	rt.Health.Register(DepClientBindings, false, mirrorProbe(bindings, "client_bindings"))
	rt.Health.Register(DepPolicy, false, func(context.Context) (obs.State, string) {
		if _, _, ok := pol.Value(); ok {
			return obs.StateUp, ""
		}
		return obs.StateUnknown, "not read yet: the defaults apply"
	})
	rt.Health.Register(DepSourceControl, false, func(context.Context) (obs.State, string) {
		if _, ok := src.AgeS(); ok {
			return obs.StateUp, ""
		}
		return obs.StateUnknown, "not read yet: every source is enabled (B-09)"
	})
	rt.Health.Register(DepCIS, false, func(context.Context) (obs.State, string) {
		if _, _, ok := basis.Value(); ok {
			return obs.StateUp, ""
		}
		return obs.StateUnknown, "cis_current not read: the CIS version is unknown"
	})

	hubCounters := &core.Counters{}
	proc.Publish(rt, "traffic_ws", hubCounters)
	// The geoid shows a manned track's geometric altitude as AMSL in the
	// product (brief WP-14: alt_source as the data allows).
	var und geoid.Undulator
	geoidWhy := "USSP_GEOID_FILE is not set: a manned track's geometric altitude is not shown as AMSL (alt_source none, or pressure when it has one)"
	if gf := rt.Config.GeoidFile; gf != "" {
		if g, err := geoid.LoadMapped(gf); err == nil {
			und, geoidWhy = g, ""
		} else {
			geoidWhy = "the geoid grid of USSP_GEOID_FILE does not load (" + err.Error() + "): a manned track's geometric altitude is not shown as AMSL"
		}
	}
	rt.Health.Register(DepGeoid, false, func(context.Context) (obs.State, string) {
		if und == nil {
			return obs.StateDown, geoidWhy
		}
		return obs.StateUp, ""
	})
	hub := &Hub{
		Geoid:   und,
		Picture: &traffic.Picture{Counters: hubCounters}, Book: &traffic.Book{Counters: hubCounters},
		Policy: current, Gate: src, Intents: intents, Bindings: bindings,
		CIS: func() (cis.BasisValue, bool) {
			v, _, ok := basis.Value()
			return v, ok
		},
		GateLoaded:   func() bool { _, ok := src.AgeS(); return ok },
		PolicyLoaded: func() bool { _, _, ok := pol.Value(); return ok },
		Pub:          bus.NewPublisher(rt.Bus, hubCounters), Counters: hubCounters, Logger: logger,
	}
	hub.Started()
	hub.AlertFeed(false, "not opened yet")

	geoChanges := &geo.Changes{Counters: hubCounters}
	srv := &Server{
		Health: proc.HealthHandlers{Health: rt.Health}, Hub: hub, Geo: geoChanges,
		WS: &auth.WSAuth{Guard: guard, AllowedOrigins: cfg.WSAllowedOrigins}, Sessions: sessions,
		Ctx: ctx, ProductEvery: o.ProductEvery, StatusEvery: o.StatusEvery, RecordEvery: o.RecordEvery, RepeatEvery: o.RepeatEvery,
		Degraded: func() []string { return rt.Health.Snapshot().Degraded }, Counters: hubCounters, Logger: logger,
	}
	if o.Server != nil {
		o.Server(srv)
	}
	if err := Register(mux, srv, guard.Require); err != nil {
		return fmt.Errorf("access table: %w", err)
	}

	// The tracks and source statuses (core NATS, live only: a picture
	// is the latest sample).
	subs := &listenSet{}
	rt.Health.Register(DepTracks, true, subs.probe)
	for subject, take := range map[string]func(string, []byte){
		bus.SubjectTrkAll:  func(_ string, d []byte) { hub.TakeTrack(traffic.NSTrack, d) },
		bus.SubjectPeerAll: func(_ string, d []byte) { hub.TakeTrack(traffic.NSPeer, d) },
		bus.SubjectManAll:  func(_ string, d []byte) { hub.TakeManned(d) },
		bus.SubjectSrcAll:  func(_ string, d []byte) { hub.TakeSourceStatus(d) },
		// The installed CIS versions (geo/changed/v1, brief WP-12): each
		// is forwarded to every traffic subscription.
		bus.SubjectCISAll: func(_ string, d []byte) { geoChanges.Take(d) },
	} {
		rt.Go(ctx, func(ctx context.Context) { subs.listen(ctx, rt.Bus, subject, take, logger) })
	}
	// The alerts (ALRT, replayed AlertReplay back).
	rt.Health.Register(DepAlerts, true, hub.alertProbe)
	replay := bus.Replay{JS: js, Stream: bus.StreamALRT, Subject: bus.SubjectAlrtAll}
	rt.Go(ctx, func(ctx context.Context) { alertFeed(ctx, replay.OpenSubjects, hub, logger) })
	// The picture is swept of tracks silent past traffic_drop_after_s
	// (shown stale with their age all that time first).
	rt.Go(ctx, func(ctx context.Context) {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				hub.Picture.Sweep(time.Now().Add(-time.Duration(current().Values.TrafficDropAfterS * float64(time.Second))))
			}
		}
	})
	for _, run := range []func(context.Context){pol.Run, src.Run, intents.Run, bindings.Run, basis.Run, sessionsLive.Run} {
		rt.Go(ctx, run)
	}
	logger.Info("traffic-ws serving", slog.Any("allowed_origins", cfg.WSAllowedOrigins))
	return nil
}

// alertProbe is the readiness of the alert feed.
func (h *Hub) alertProbe(context.Context) (obs.State, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.alertFeed {
		return obs.StateUp, ""
	}
	return obs.StateDown, "alert feed not open since " + h.alertSince.Format(time.RFC3339) + ": " + h.alertReason
}

// alertFeed keeps the ALRT replay open, retried every 2 s, and says on
// the hub whether it is.
func alertFeed(ctx context.Context, open func(context.Context, time.Time, func(string, []byte)) (func(), error), hub *Hub, logger *slog.Logger) {
	for ctx.Err() == nil {
		stop, err := open(ctx, time.Now().Add(-AlertReplay), hub.TakeAlert)
		if err != nil {
			hub.AlertFeed(false, err.Error())
			logger.LogAttrs(ctx, slog.LevelWarn, "alert feed not open; retried", obs.Err(err))
			wait(ctx, 2*time.Second)
			continue
		}
		hub.AlertFeed(true, "")
		<-ctx.Done()
		stop()
	}
}

func wait(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// listenSet keeps core subscriptions open and reports them on /readyz.
type listenSet struct {
	mu   sync.Mutex
	down map[string]string
}

func (l *listenSet) set(subject, why string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.down == nil {
		l.down = map[string]string{}
	}
	if why == "" {
		delete(l.down, subject)
	} else {
		l.down[subject] = why
	}
}

func (l *listenSet) probe(context.Context) (obs.State, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for s, why := range l.down {
		return obs.StateDown, s + ": " + why
	}
	return obs.StateUp, ""
}

func (l *listenSet) listen(ctx context.Context, nc *bus.Conn, subject string, take func(string, []byte), logger *slog.Logger) {
	l.set(subject, "not subscribed yet")
	for ctx.Err() == nil {
		stop, err := nc.Listen(subject, take)
		if err != nil {
			l.set(subject, err.Error())
			logger.LogAttrs(ctx, slog.LevelWarn, "subscription not open; retried", slog.String("subject", subject), obs.Err(err))
			wait(ctx, 2*time.Second)
			continue
		}
		l.set(subject, "")
		<-ctx.Done()
		stop()
	}
}
