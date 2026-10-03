package api

import (
	"context"
	"math"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/app/proc"
	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/cis"
	"github.com/rootxkit/uspace-ussp/internal/cis/pgstore"
	"github.com/rootxkit/uspace-ussp/internal/config"
	"github.com/rootxkit/uspace-ussp/internal/policy"
)

// Readiness dependencies of the CIS cache.
const (
	DepCIS              = "cis"
	DepCISNotifyKeys    = "cis_notify_keys"
	DepCISPublisherKeys = "cis_publisher_keys"
)

// CIS is what the api process holds of the CIS cache: the evaluator
// intent and geo call, and the receiver of POST /v1/cis/notifications
// (nil without USSP_CIS_NOTIFY_ISSUERS: the route answers 503).
type CIS struct {
	Evaluator *cis.Evaluator
	Cache     *cis.Cache
	Receiver  http.Handler

	mu    sync.Mutex
	hooks []cis.ChangeHook
}

// OnChange adds a hook told every installed version (the cis.v1
// publish, the standing re-check). Hooks are added before Start.
func (c *CIS) OnChange(h cis.ChangeHook) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hooks = append(c.hooks, h)
}

func (c *CIS) changed(ctx context.Context, ch cis.Change) {
	c.mu.Lock()
	hooks := slices.Clone(c.hooks)
	c.mu.Unlock()
	for _, h := range hooks {
		h(ctx, ch)
	}
}

// Start runs the cache's workers on rt (after the hooks are added, so
// the warm start's change reaches them).
func (c *CIS) Start(ctx context.Context, rt *proc.Runtime) { rt.Go(ctx, c.Cache.Run) }

// startCIS builds the CIS cache from the configuration (tokens is the
// process's outgoing token client, nil without one), registers its
// readiness and starts its workers on rt. Nothing here refuses the start
// because a dependency is down; a configuration that cannot be right
// (an issuer on neither the CISP's nor the ANSP's host, a malformed box)
// does. The workers start with Start.
func startCIS(ctx context.Context, rt *proc.Runtime, current func() policy.Values, tokens *auth.Outgoing, kv cis.KVWriter) (*CIS, error) {
	cfg := rt.Config
	counters := &core.Counters{}
	proc.Publish(rt, "cis", counters)
	eval := cis.NewEvaluator(cis.EvaluatorConfig{
		StaleS:   func() float64 { return current().CISStaleS },
		Counters: counters,
	})
	bbox, err := parseBBox(cfg.CISBBox)
	if err != nil {
		return nil, err
	}
	var client *cis.Client
	if cfg.CISPBaseURL != "" {
		cc := cis.ClientConfig{BaseURL: cfg.CISPBaseURL}
		if tokens != nil {
			cc.Tokens = tokens
		}
		if client, err = cis.NewClient(cc); err != nil {
			return nil, err
		}
	}
	callback := ""
	if cfg.USSBaseURL != "" && client != nil {
		callback = strings.TrimRight(cfg.USSBaseURL, "/") + cis.NotificationsPath
	}
	var st cis.Store
	if rt.Store != nil && rt.Store.Rel != nil {
		st = pgstore.Store{S: rt.Store}
	}
	// Without USSP_CIS_PUBLISHER_KEYS the cache holds every new version
	// and says so on /readyz.
	var publishers cis.PublisherVerifier
	if len(cfg.CISPublisherKeys) > 0 {
		pc, err := cfg.PublisherConfig()
		if err != nil {
			return nil, err
		}
		keys := cis.NewLazyPublisherVerifier(pc, 0)
		rt.Health.Register(DepCISPublisherKeys, false, keys.Probe)
		rt.Go(ctx, func(ctx context.Context) {
			keys.Run(ctx)
			if c := keys.Counters(); c != nil {
				proc.Publish(rt, "cis_publisher_jws", c)
			}
		})
		publishers = keys
	}
	out := &CIS{Evaluator: eval}
	cache := cis.NewCache(cis.CacheConfig{
		Client: client, Publishers: publishers, Store: st, Evaluator: eval, Counters: counters, Logger: rt.Logger.With("component", "cis"),
		CallbackURL: callback, BBox: bbox, ReconcileInterval: time.Duration(cfg.CISReconcileS) * time.Second,
		RetentionDays: func() int { return current().RecordRetentionDays },
		Projector:     &cis.BusProjector{KV: kv},
		OnChange:      out.changed,
	})
	rt.Health.Register(DepCIS, false, cache.Probe)
	out.Cache = cache
	if len(cfg.CISNotifyIssuers) > 0 && st != nil {
		cc, err := cfg.CompactConfig()
		if err != nil {
			return nil, err
		}
		senders, err := notifySenders(cfg)
		if err != nil {
			return nil, err
		}
		keys := cis.NewLazyVerifier(cc, 0, counters)
		rt.Health.Register(DepCISNotifyKeys, false, keys.Probe)
		rt.Go(ctx, func(ctx context.Context) {
			keys.Run(ctx)
			if c := keys.Counters(); c != nil {
				proc.Publish(rt, "cis_notify_jws", c)
			}
		})
		out.Receiver = cis.NewReceiver(cis.ReceiverConfig{
			Verifier: keys, Senders: senders, Store: st, Trigger: cache.Trigger, Counters: counters,
			Logger: rt.Logger.With("component", "cis_receiver"),
		})
	}
	return out, nil
}

// notifySenders says who each USSP_CIS_NOTIFY_ISSUERS entry is, from the
// host of its JWKS URL: the CISP's (USSP_CISP_BASE_URL) or the ANSP's
// (USSP_ANSP_BASE_URL). A pull_url is honoured only on that host (M5).
// An issuer served from neither refuses the start: its pull_url could
// not be checked against anything.
func notifySenders(cfg config.Config) (map[string]cis.Sender, error) {
	issuers, err := config.ParseIssuers(cfg.CISNotifyIssuers)
	if err != nil {
		return nil, core.Fieldf("USSP_CIS_NOTIFY_ISSUERS", "%v", err)
	}
	cispHost, anspHost := hostOf(cfg.CISPBaseURL), hostOf(cfg.ANSPBaseURL)
	out := make(map[string]cis.Sender, len(issuers))
	for _, iss := range issuers {
		h := hostOf(iss.JWKSURL)
		switch {
		case h != "" && h == cispHost:
			out[iss.Issuer] = cis.Sender{BaseHost: cispHost}
		case h != "" && h == anspHost:
			out[iss.Issuer] = cis.Sender{ANSP: true, BaseHost: anspHost}
		default:
			return nil, core.Fieldf("USSP_CIS_NOTIFY_ISSUERS", "the JWKS of %s is on neither the host of USSP_CISP_BASE_URL nor that of USSP_ANSP_BASE_URL", iss.Issuer)
		}
	}
	return out, nil
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// parseBBox reads USSP_CIS_BBOX: four finite numbers, longitudes in
// [-180, 180], latitudes in [-90, 90], min not above max.
func parseBBox(s string) ([]float64, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	if len(parts) != 4 {
		return nil, core.Fieldf("USSP_CIS_BBOX", "four numbers min_lng,min_lat,max_lng,max_lat")
	}
	out := make([]float64, 4)
	for i, p := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil || math.IsNaN(v) || v > 180 || v < -180 {
			return nil, core.Fieldf("USSP_CIS_BBOX", "%q is not a coordinate", strings.TrimSpace(p))
		}
		out[i] = v
	}
	if out[1] < -90 || out[3] > 90 || out[0] > out[2] || out[1] > out[3] {
		return nil, core.Fieldf("USSP_CIS_BBOX", "not a box (min above max, or a latitude beyond 90)")
	}
	return out, nil
}

// outgoingTokens is the token client of the calls this process makes
// (POST /oauth/token on the origin that serves the first
// USSP_TOKEN_ISSUERS entry's JWKS, the token service; client
// ussp-<code>-01), or nil when no client secret is configured: the CIS
// cache then says on /readyz that it cannot authenticate.
func outgoingTokens(cfg config.Config) (*auth.Outgoing, error) {
	if len(cfg.TokenIssuers) == 0 || cfg.TokenClientSecretFile == "" {
		return nil, nil
	}
	issuers, err := config.ParseIssuers(cfg.TokenIssuers)
	if err != nil {
		return nil, core.Fieldf("USSP_TOKEN_ISSUERS", "%v", err)
	}
	b, err := os.ReadFile(cfg.TokenClientSecretFile)
	if err != nil {
		return nil, core.Fieldf("USSP_TOKEN_CLIENT_SECRET_FILE", "cannot be read")
	}
	secret := strings.TrimSpace(string(b))
	if secret == "" {
		return nil, core.Fieldf("USSP_TOKEN_CLIENT_SECRET_FILE", "is empty")
	}
	tokenURL, err := url.Parse(issuers[0].JWKSURL)
	if err != nil {
		return nil, core.Fieldf("USSP_TOKEN_ISSUERS", "the JWKS URL does not parse")
	}
	tokenURL.Path, tokenURL.RawQuery, tokenURL.Fragment = "/oauth/token", "", ""
	return auth.NewOutgoing(auth.OutgoingConfig{
		TokenURL: tokenURL.String(),
		ClientID: auth.ClientIDFor(cfg.SystemID), ClientSecret: secret,
	})
}
