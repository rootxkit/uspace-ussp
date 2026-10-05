package proc

import (
	"context"

	coreauth "github.com/rootxkit/uspace-core/auth"
	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// RefuseAll is the verifier of a process with no audience configured:
// it can verify nothing, and says so.
type RefuseAll struct{}

// Verify refuses every token as rejected_audience.
func (RefuseAll) Verify(context.Context, string) (coreauth.Claims, error) {
	return coreauth.Claims{}, &coreauth.TokenError{Counter: coreauth.CounterRejectedAudience, Claim: "aud", Reason: "USSP_AUDIENCES is empty: no token is accepted"}
}

// Publish adds counter set c under name to the status line and /metrics.
func Publish(rt *Runtime, name string, c *core.Counters) {
	rt.Status.Add(name, c)
	rt.Registry.MustRegister(obs.NewCountersCollector(name, c))
}

// TokenVerifier builds the verifier of every token a process accepts
// (the ecosystem issuers of USSP_TOKEN_ISSUERS and, for api, this
// USSP's own issuer; nil for a process without one) and registers its
// readiness (jwks); without an audience the verifier refuses every
// token and jwks says why. It returns the own issuer's iss ("" without
// one).
func TokenVerifier(ctx context.Context, rt *Runtime, issuer *auth.Issuer) (auth.TokenVerifier, string, error) {
	cfg := rt.Config
	if len(cfg.Audiences) == 0 {
		rt.Health.Register(auth.DepJWKS, false, func(context.Context) (obs.State, string) {
			return obs.StateDown, "USSP_AUDIENCES is empty: no token is accepted"
		})
		return RefuseAll{}, "", nil
	}
	eco := coreauth.Config{Audiences: cfg.Audiences, StrictSessionClaims: true}
	if len(cfg.TokenIssuers) > 0 {
		var err error
		if eco, err = cfg.VerifierConfig(); err != nil {
			return nil, "", err
		}
	}
	// Each ecosystem issuer's counter set exists once its verifier is
	// built, which Run does issuer by issuer; it is published then.
	v, err := auth.NewVerifier(ctx, auth.VerifierConfig{Ecosystem: eco, Own: issuer,
		OnBuilt: func(set string, c *core.Counters) { Publish(rt, set, c) }})
	if err != nil {
		return nil, "", err
	}
	for name, c := range v.CounterSets() {
		Publish(rt, name, c)
	}
	rt.Health.Register(auth.DepJWKS, false, v.Probe)
	rt.Go(ctx, v.Run)
	own := v.OwnIssuer()
	return v, own, nil
}
