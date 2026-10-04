package api

import (
	"context"
	"strconv"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/app/proc"
	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/cis"
	"github.com/rootxkit/uspace-ussp/internal/coordination"
	coordstore "github.com/rootxkit/uspace-ussp/internal/coordination/pgstore"
	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/policy"
)

// startCoordination runs the Annex V coordination with the ANSP (WP-15,
// internal/coordination): the Notifier queues the notices the database
// shows are due, the Sender posts them to USSP_ANSP_BASE_URL (mTLS per
// USSP_MTLS_MODE, a token of scope ansp.coordination whose aud is the
// ANSP's host) and reads back the acknowledgements; ansp_coordination on
// /readyz says what is waiting. Without USSP_ANSP_BASE_URL, or without
// an outgoing token client, the notices are queued and not sent, and
// /readyz says why. A required mTLS whose files do not load refuses the
// start, naming the variable.
func startCoordination(ctx context.Context, rt *proc.Runtime, current func() policy.Values, tokens *auth.Outgoing, cisState *CIS) (coordination.Store, error) {
	cfg := rt.Config
	st := coordstore.Store{S: rt.Store}
	counters := &core.Counters{}
	proc.Publish(rt, "coordination", counters)
	logger := rt.Logger.With("component", "coordination")
	var ansp coordination.ANSP
	unconfigured := ""
	switch {
	case cfg.ANSPBaseURL == "":
		unconfigured = "USSP_ANSP_BASE_URL is not set"
	case tokens == nil:
		unconfigured = "no outgoing token client (USSP_TOKEN_ISSUERS, USSP_TOKEN_CLIENT_SECRET_FILE)"
	default:
		hc, err := httpx.MTLSClient(httpx.MTLSConfig{Mode: cfg.MTLSMode, CertFile: cfg.MTLSCertFile, KeyFile: cfg.MTLSKeyFile, CAFile: cfg.MTLSCAFile},
			coordination.DefaultCallTimeout)
		if err != nil {
			return nil, err
		}
		if cfg.MTLSMode == httpx.MTLSOff {
			logger.Error("USSP_MTLS_MODE=off: Annex V notices go to the ANSP without a client certificate (the lab and staging only)")
		}
		client, err := coordination.NewClient(cfg.ANSPBaseURL, tokens, hc)
		if err != nil {
			return nil, err
		}
		ansp = client
	}
	if unconfigured != "" {
		logger.Warn("Annex V notices are queued and not sent", "reason", unconfigured)
	}
	rt.Health.Register(coordination.DepANSP, false, coordination.Probe(st, unconfigured))
	n := &coordination.Notifier{Store: st, Airspaces: airspacesOf(cisState.Evaluator), SystemID: cfg.SystemID,
		Lookback: time.Duration(cfg.ConfStreamMaxAgeS) * time.Second, Counters: counters, Logger: logger}
	s := &coordination.Sender{Store: st, ANSP: ansp, Policy: current, Counters: counters, Logger: logger}
	rt.Go(ctx, func(ctx context.Context) { n.Run(ctx, coordination.DefaultSweepEvery) })
	rt.Go(ctx, func(ctx context.Context) { s.Run(ctx, coordination.DefaultSweepEvery) })
	return st, nil
}

// airspacesOf reads the installed U-space airspace version of the CIS
// cache: each airspace by identifier with in_controlled_airspace as its
// requirement block publishes it (nil: not said, or a block that does
// not read). False while no version is installed.
func airspacesOf(e *cis.Evaluator) coordination.AirspaceSource {
	return func() (coordination.Airspaces, bool) {
		if e == nil {
			return coordination.Airspaces{}, false
		}
		snap := e.Snapshot()
		if snap == nil {
			return coordination.Airspaces{}, false
		}
		v := snap.Version(cis.USpaceAirspace)
		if v == nil {
			return coordination.Airspaces{}, false
		}
		out := coordination.Airspaces{Version: strconv.FormatInt(v.Number, 10), Controlled: map[string]*bool{}}
		for _, en := range snap.Entries(cis.USpaceAirspace) {
			var c *bool
			if en.Requirements != nil {
				c = en.Requirements.InControlledAirspace
			}
			out.Controlled[en.Identifier] = c
		}
		return out, true
	}
}
