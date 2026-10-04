package api

import (
	"context"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/app/proc"
	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/status"
	statusstore "github.com/rootxkit/uspace-ussp/internal/status/pgstore"
)

// startStatus runs the operating-status notices (WP-15, internal/status):
// stored on the console's request, sent to the authority after their
// commit (USSP_AUTHORITY_BASE_URL, a token of scope certificates.status
// whose aud is the authority's host), and operating_status on /readyz.
// Without the certificate id or an authority client the notices are
// stored and not sent, and /readyz says why.
func startStatus(ctx context.Context, rt *proc.Runtime, tokens *auth.Outgoing) (*status.Service, error) {
	cfg := rt.Config
	counters := &core.Counters{}
	proc.Publish(rt, "operating_status", counters)
	svc := &status.Service{Store: statusstore.Store{S: rt.Store}, CertificateID: cfg.CertificateID, SystemID: cfg.SystemID,
		Counters: counters, Logger: rt.Logger.With("component", "status")}
	if cfg.AuthorityBaseURL != "" && tokens != nil {
		c, err := status.NewClient(cfg.AuthorityBaseURL, tokens, nil)
		if err != nil {
			return nil, err
		}
		svc.Authority = c
	}
	rt.Health.Register(status.DepStatus, false, svc.Probe())
	rt.Go(ctx, func(ctx context.Context) { svc.Run(ctx, status.DefaultEvery) })
	return svc, nil
}
