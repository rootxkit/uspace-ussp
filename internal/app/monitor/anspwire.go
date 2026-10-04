package monitor

import (
	"context"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/app/proc"
	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// startANSPFeed reads the ANSP's manned-traffic stream when
// USSP_ANSP_STREAM_URL is set (ANSPFeed): a token of scope ansp.traffic
// from the outgoing token client (USSP_TOKEN_ISSUERS,
// USSP_TOKEN_CLIENT_SECRET_FILE), mTLS per USSP_MTLS_MODE (required
// without its files refuses the start, naming them; off is logged at
// error level). ansp_feed on /readyz says what the stream does, and
// that no stream is read when the variable is unset.
func startANSPFeed(ctx context.Context, rt *proc.Runtime) error {
	cfg := rt.Config
	if cfg.ANSPStreamURL == "" {
		rt.Health.Register(DepANSPFeed, false, func(context.Context) (obs.State, string) {
			return obs.StateUp, "USSP_ANSP_STREAM_URL is not set: no manned traffic is read from the ANSP"
		})
		return nil
	}
	hc, err := httpx.MTLSClient(httpx.MTLSConfig{Mode: cfg.MTLSMode, CertFile: cfg.MTLSCertFile, KeyFile: cfg.MTLSKeyFile, CAFile: cfg.MTLSCAFile}, 0)
	if err != nil {
		return err
	}
	logger := rt.Logger.With("component", "ansp_feed")
	if cfg.MTLSMode == httpx.MTLSOff {
		logger.Error("USSP_MTLS_MODE=off: the ANSP's manned-traffic stream is read without a client certificate (the lab and staging only)")
	}
	tokens, err := proc.OutgoingTokens(cfg)
	if err != nil {
		return err
	}
	if tokens == nil {
		rt.Health.Register(DepANSPFeed, false, func(context.Context) (obs.State, string) {
			return obs.StateDown, "USSP_ANSP_STREAM_URL is set but no outgoing token client (USSP_TOKEN_ISSUERS, USSP_TOKEN_CLIENT_SECRET_FILE): the stream is not read"
		})
		return nil
	}
	counters := &core.Counters{}
	proc.Publish(rt, "ansp_feed", counters)
	f := &ANSPFeed{URL: cfg.ANSPStreamURL, Tokens: tokens, HTTP: hc, Counters: counters, Logger: logger}
	rt.Health.Register(DepANSPFeed, false, f.Probe)
	rt.Go(ctx, f.Run)
	return nil
}
