package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3548"
	"github.com/rootxkit/uspace-core/geodesy"

	"github.com/rootxkit/uspace-ussp/internal/app/proc"
	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/cis"
	"github.com/rootxkit/uspace-ussp/internal/dss"
	dsspg "github.com/rootxkit/uspace-ussp/internal/dss/pgstore"
	"github.com/rootxkit/uspace-ussp/internal/geo"
	"github.com/rootxkit/uspace-ussp/internal/intent"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
)

// strategic is F3548 strategic coordination in api (brief WP-13): the
// DSS client, this USSP's availability and the gate the decisions read,
// built before the intent service; the writer, the subscriptions, the
// purge and the USS endpoints, started once the intent service exists.
// As WP-9's ISA worker, it runs in api, the only relational writer (D5):
// dss-sync keeps no database connection.
type strategic struct {
	counters *core.Counters
	store    dsspg.Store
	exlog    *dss.ExchangeLog
	client   *dss.Client
	avail    *dss.Availability
	gate     *dss.Gate
	// manager is the configured client id; the manager the DSS records
	// for us is the sub of our tokens (dss.Client.Self).
	manager string
	// why is set when no DSS write can be made (no base URL, no token
	// client): the decisions wait pending_dss and /readyz says so.
	why string
}

// newStrategic builds the client, the availability and the gate.
func newStrategic(rt *proc.Runtime, tokens *auth.Outgoing) *strategic {
	cfg := rt.Config
	s := &strategic{counters: &core.Counters{}, store: dsspg.Store{S: rt.Store}, manager: auth.ClientIDFor(cfg.SystemID)}
	proc.Publish(rt, "dss", s.counters)
	s.exlog = dss.NewExchangeLog(s.store, s.counters, rt.Logger.With("component", "dss_exchanges"))
	switch {
	case cfg.DSSBaseURL == "":
		s.why = "USSP_DSS_BASE_URL is not set"
	case cfg.USSBaseURL == "":
		s.why = "USSP_USS_BASE_URL is not set"
	case tokens == nil:
		s.why = "no outgoing token client (USSP_TOKEN_ISSUERS, USSP_TOKEN_CLIENT_SECRET_FILE)"
	}
	httpc := &http.Client{Timeout: dss.DefaultCallTimeout, Transport: &dss.Transport{Log: s.exlog}}
	s.client = &dss.Client{DSSBaseURL: cfg.DSSBaseURL, HTTP: httpc}
	if tokens != nil {
		s.client.Tokens = tokens
	}
	pub := bus.NewPublisher(rt.Bus, s.counters)
	s.avail = &dss.Availability{Client: s.client, Store: s.store, USSID: s.manager, Counters: s.counters,
		Logger: rt.Logger.With("component", "dss_availability"),
		OnChange: func(state f3548.UssAvailabilityState) {
			_ = pub.PublishControl(bus.CtlDSSState, map[string]any{"uss_availability": state, "at": time.Now().UTC()})
		}}
	s.gate = &dss.Gate{Client: s.client, Availability: s.avail, Unconfigured: s.why}
	return s
}

// areasOf are the U-space airspaces of the CIS cache as areas of
// interest; false while the dataset is not installed.
func areasOf(e *cis.Evaluator) func() ([]dss.Area, bool) {
	return func() ([]dss.Area, bool) {
		snap := e.Snapshot()
		if snap == nil || snap.Version(cis.USpaceAirspace) == nil {
			return nil, false
		}
		var out []dss.Area
		for _, en := range snap.Entries(cis.USpaceAirspace) {
			if len(en.Parts) == 0 {
				continue
			}
			box := en.Parts[0].BBox
			for _, p := range en.Parts[1:] {
				box = geodesy.BBox{MinLat: min(box.MinLat, p.BBox.MinLat), MinLon: min(box.MinLon, p.BBox.MinLon),
					MaxLat: max(box.MaxLat, p.BBox.MaxLat), MaxLon: max(box.MaxLon, p.BBox.MaxLon)}
			}
			out = append(out, dss.Area{ID: en.Identifier, Box: box})
		}
		return out, true
	}
}

// peerBus publishes peer.intent.v1 (core NATS).
type peerBus struct{ pub *bus.Publisher }

type peerIntentMessage struct {
	bus.Envelope
	Body dss.PeerIntentBody `json:"body"`
}

// PublishPeerIntent implements dss.PeerPublisher.
func (p peerBus) PublishPeerIntent(ctx context.Context, b dss.PeerIntentBody) error {
	subject, err := bus.PeerIntent(b.EntityID)
	if err != nil {
		return err
	}
	return p.pub.Publish(ctx, subject, &peerIntentMessage{Envelope: bus.SystemEnvelope(dss.SchemaPeerIntent, intent.Producer, time.Now()), Body: b})
}

// start runs the writer, the availability poll, the subscriptions and
// the purge (the last two need no DSS write and run even without one:
// the purge always, the subscriptions only with a DSS), registers dss on
// /readyz with the ISA worker's entry merged in, and returns the USS
// endpoints' server.
func (s *strategic) start(ctx context.Context, rt *proc.Runtime, current func() policy.Values, intents *intent.Service,
	re *geo.Rechecker, cisState *CIS, isaProbe obs.Probe) *dss.Server {
	cfg := rt.Config
	logger := rt.Logger.With("component", "dss")
	rt.Go(ctx, s.exlog.Run)
	purger := &dss.Purger{Store: s.store, Policy: current, Counters: s.counters, Logger: logger}
	rt.Go(ctx, purger.Run)
	server := &dss.Server{Intents: intents, Store: s.store, Telemetry: dsspg.Telemetry{S: rt.Store}, Constraints: re,
		Publisher: peerBus{pub: bus.NewPublisher(rt.Bus, s.counters)}, Manager: s.manager, USSBaseURL: cfg.USSBaseURL,
		Client: s.client, Counters: s.counters, Logger: logger}
	probes := map[string]obs.Probe{}
	if isaProbe != nil {
		probes["f3411"] = isaProbe
	}
	if s.why != "" {
		probes["f3548"] = func(context.Context) (obs.State, string) {
			return obs.StateDown, s.why + ": no operational intent is written to the DSS, so an intent inside U-space airspace waits pending_dss"
		}
		rt.Health.Register(DepDSS, false, dss.Merge(probes))
		logger.Warn("no F3548 DSS write is made: " + s.why)
		return server
	}
	w := &dss.Writer{Client: s.client, Store: s.store, Intents: intents, USSBaseURL: cfg.USSBaseURL, Manager: s.manager,
		ForAll: cfg.DSSForAll == "on", Availability: s.avail, Counters: s.counters, Logger: logger}
	intents.Writer = w
	subs := &dss.Subscriptions{Client: s.client, Store: s.store, USSBaseURL: cfg.USSBaseURL, Areas: areasOf(cisState.Evaluator),
		Policy: current, Counters: s.counters, Logger: logger}
	probes["f3548"] = dss.Probe(s.client, s.avail, s.store)
	rt.Health.Register(DepDSS, false, dss.Merge(probes))
	rt.Go(ctx, w.Run)
	rt.Go(ctx, s.avail.Run)
	rt.Go(ctx, subs.Run)
	manager, merr := s.client.Manager(ctx)
	if merr != nil {
		manager = s.manager + " (configured; no DSS token yet: " + merr.Error() + ")"
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "F3548 strategic coordination through the DSS", slog.String("manager", manager),
		slog.Bool("for_all", w.ForAll))
	return server
}
