// Package api is the api process: the control plane: the national API,
// accounts, intents, the CIS cache and the KV projections; the only
// writer of the relational database. WP-2 brings its routes: the token
// issuer, the JWKS and the accounts, every operation behind the
// fail-closed access table of internal/national; WP-3 mounts the F3548
// USS endpoints (internal/stdapi), 501 until WP-13; WP-4 runs the CIS
// cache (internal/cis) and its receiver POST /v1/cis/notifications;
// WP-5 the registry validity cache (internal/registry), its change feed
// and GET /v1/registry/validate, and checks operator accounts with it;
// WP-7 flight authorisation (internal/intent) and /v1/intents; WP-8 the
// flights table from telemetry-ingest's flight facts (internal/flights);
// WP-9 the F3411 ISA of every flight, planned with its facts and written
// to the DSS (internal/ridsp), with dss on /readyz; WP-11 the alerts
// record (internal/alerts): alrt.v1 recorded, POST
// /v1/alerts/{alert_id}/ack and the escalation of unacknowledged
// critical alerts; WP-12 geo-awareness (internal/geo): GET /v1/geo*,
// every installed CIS version on cis.v1 and the standing re-check of
// the active intents (Art. 10(10)).
package api

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"
	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/accounts"
	"github.com/rootxkit/uspace-ussp/internal/alerts"
	alertstore "github.com/rootxkit/uspace-ussp/internal/alerts/pgstore"
	"github.com/rootxkit/uspace-ussp/internal/app/proc"
	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/config"
	"github.com/rootxkit/uspace-ussp/internal/conformance"
	confstore "github.com/rootxkit/uspace-ussp/internal/conformance/pgstore"
	"github.com/rootxkit/uspace-ussp/internal/flights"
	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/national"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/registry"
	"github.com/rootxkit/uspace-ussp/internal/stdapi"
	"github.com/rootxkit/uspace-ussp/internal/store"
)

// Spec declares the process and the dependencies it reads.
var Spec = proc.Spec{
	Process:     config.ProcessAPI,
	Postgres:    proc.Required,
	TimescaleDB: proc.Optional,
	NATS:        proc.Required,
	Migrate:     true,
	Routes:      routes,
	Commands:    map[string]proc.Command{"staff-add": staffAdd},
}

// Run runs api with cfg until ctx ends.
func Run(ctx context.Context, cfg config.Config) error {
	return proc.Run(ctx, cfg, Spec, proc.Options{})
}

// DepIssuer is the readiness dependency of this USSP's own issuer key.
const DepIssuer = "issuer"

// sweepInterval is how often expired sessions and stale lockouts are
// deleted.
const sweepInterval = time.Hour

// IssuerFromConfig loads USSP_ISSUER_KEY_FILE (and the previous key)
// into the issuer; nil without a key file. A key without an audience,
// or a key that does not load, refuses the start.
func IssuerFromConfig(cfg config.Config) (*auth.Issuer, error) {
	if cfg.IssuerKeyFile == "" {
		if cfg.IssuerPreviousKeyFile != "" {
			return nil, core.Fieldf("USSP_ISSUER_PREVIOUS_KEY_FILE", "set without USSP_ISSUER_KEY_FILE")
		}
		return nil, nil
	}
	if len(cfg.Audiences) == 0 {
		return nil, core.Fieldf("USSP_AUDIENCES", "required with USSP_ISSUER_KEY_FILE: aud of this issuer's tokens is the first entry")
	}
	cur, err := auth.LoadKeyFile("USSP_ISSUER_KEY_FILE", cfg.IssuerKeyFile)
	if err != nil {
		return nil, err
	}
	var prev *coreauth.SigningKey
	if cfg.IssuerPreviousKeyFile != "" {
		k, err := auth.LoadKeyFile("USSP_ISSUER_PREVIOUS_KEY_FILE", cfg.IssuerPreviousKeyFile)
		if err != nil {
			return nil, err
		}
		prev = &k
	}
	keys, err := auth.NewIssuerKeys(cur, prev)
	if err != nil {
		return nil, err
	}
	url := cfg.IssuerURL
	if url == "" {
		url = "https://" + cfg.Audiences[0]
	}
	return auth.NewIssuer(url, cfg.Audiences[0], keys)
}

func routes(ctx context.Context, mux *http.ServeMux, rt *proc.Runtime) error {
	cfg := rt.Config
	issuer, err := IssuerFromConfig(cfg)
	if err != nil {
		return err
	}
	rt.Health.Register(DepIssuer, false, func(context.Context) (obs.State, string) {
		if issuer == nil {
			return obs.StateDown, "USSP_ISSUER_KEY_FILE is not set: no operator token is issued and no session starts"
		}
		return obs.StateUp, ""
	})
	verifier, ownIss, err := proc.TokenVerifier(ctx, rt, issuer)
	if err != nil {
		return err
	}
	hasher, err := auth.NewHasher()
	if err != nil {
		return err
	}
	var sealer *accounts.Sealer
	if cfg.MFAKeyFile != "" {
		if sealer, err = accounts.LoadSealer(cfg.MFAKeyFile); err != nil {
			return err
		}
	}
	// Every KV projection of this process (D6): written inside the
	// transaction that changes the rows, bounded, refused with 503 when
	// the KV cannot take it (B-09).
	kvCounters := &core.Counters{}
	proc.Publish(rt, "kv_projection", kvCounters)
	kv := bus.NewProjector(rt.Bus, kvCounters)
	kv.Topology = proc.TopologyOf(rt.Config)
	pol := policy.New(rt.Store, kv, nil)
	if _, err := pol.Load(ctx); err != nil {
		rt.Logger.Warn("policy not loaded; the defaults apply until it is", obs.Err(err))
	}
	current := func() policy.Values {
		if r, ok := pol.Current(); ok {
			return r.Values
		}
		return policy.Defaults()
	}

	// One outgoing token client for every call this process makes (the
	// CISP, the authority), with a token cached per audience and scope.
	tokens, err := outgoingTokens(cfg)
	if err != nil {
		return err
	}
	var registryTokens registry.TokenSource
	if tokens != nil {
		proc.Publish(rt, "token_client", tokens.Counters())
		registryTokens = tokens
	}
	cisState, err := startCIS(ctx, rt, current, tokens, kv)
	if err != nil {
		return err
	}
	reg, err := startRegistry(ctx, rt, current, registryTokens, kv)
	if err != nil {
		return err
	}

	counters := &core.Counters{}
	proc.Publish(rt, "accounts", counters)
	perMin := func(n int) float64 { return float64(n) / 60 }
	burst := func(n int) int { return max(1, n/6) }
	svc := &accounts.Service{
		Store: rt.Store, Hasher: hasher, Issuer: issuer, Registry: reg.Cache,
		Bindings: bindingsProjector{kv}, LiveSessions: sessionsProjector{kv}, Policy: current, MFA: sealer,
		LoginLimiter: httpx.NewRateLimiter(perMin(cfg.LoginRatePerMin), burst(cfg.LoginRatePerMin), 100_000, counters),
		Counters:     counters, Logger: rt.Logger,
		Config: accounts.Config{
			SessionTTL: time.Duration(cfg.SessionTTLS) * time.Second, SessionIdle: time.Duration(cfg.SessionIdleS) * time.Second,
			LockoutAfter: cfg.LoginLockoutAfter, LockoutFor: time.Duration(cfg.LoginLockoutS) * time.Second, TOTPIssuer: cfg.SystemID,
		},
	}
	guard := &auth.Guard{Verifier: verifier, OwnIssuer: ownIss, Sessions: svc, Audit: svc, Counters: counters, Logger: rt.Logger}
	token := &auth.TokenEndpoint{
		Issuer: issuer, Clients: svc, Hasher: hasher, Audit: svc,
		TTL:           func() time.Duration { return time.Duration(current().OperatorTokenTTLS) * time.Second },
		ClientLimiter: httpx.NewRateLimiter(perMin(cfg.TokenRatePerMin), burst(cfg.TokenRatePerMin), 100_000, counters),
		IPLimiter:     httpx.NewRateLimiter(perMin(cfg.TokenRatePerMin), burst(cfg.TokenRatePerMin), 100_000, counters),
		Counters:      counters, Logger: rt.Logger,
	}
	intents := startIntents(ctx, rt, pol, cisState, reg.Cache, kv)
	alertSvc := startAlerts(ctx, rt, current)
	geoState := startGeo(ctx, rt, cisState, intents)
	cisState.Start(ctx, rt)
	srv := &national.Server{Health: proc.HealthHandlers{Health: rt.Health}, Token: token, Issuer: issuer, Accounts: svc,
		CIS: cisState.Receiver, Registry: reg.Cache, Intents: intents, Alerts: alertSvc, Geo: geoState, Logger: rt.Logger,
		RegistryScope:   svc,
		RegistryLimiter: httpx.NewRateLimiter(perMin(cfg.RegistryRatePerMin), burst(cfg.RegistryRatePerMin), 100_000, counters)}
	if err := national.Register(mux, srv, guard.Require); err != nil {
		return fmt.Errorf("access table: %w", err)
	}
	// The F3548 USS endpoints (PLAN §6.2), 501 until WP-13, behind the
	// standard's scopes.
	std := &core.Counters{}
	proc.Publish(rt, "stdapi", std)
	if err := stdapi.MountF3548(mux, stdapi.NotImplementedF3548{}, stdapi.Options{Guard: guard.Require, Validate: auth.ValidateAccess, Counters: std}); err != nil {
		return fmt.Errorf("F3548 access table: %w", err)
	}
	rt.Go(ctx, func(ctx context.Context) { svc.RunSweep(ctx, sweepInterval) })
	// The flight facts of telemetry-ingest (WP-8, PLAN §3.2): recorded in
	// the flights table with their F3411 ISA plan (WP-9), then
	// acknowledged.
	planner := startISA(ctx, rt, current, tokens)
	flightCounters := &core.Counters{}
	proc.Publish(rt, "flight_records", flightCounters)
	rec := &flights.Recorder{
		Source: &bus.StreamSource{Open: bus.PullOpener(rt.Bus.JetStream(), proc.TopologyOf(rt.Config), bus.StreamFLIGHT, bus.PullSpec{
			Durable: FlightsConsumer, FilterSubject: bus.SubjectFlightAll, MaxAckPending: 256,
		})},
		Store: isaRecorder{S: rt.Store, Planner: planner}, Counters: flightCounters, Logger: rt.Logger,
	}
	rt.Go(ctx, rec.Run)
	// The conformance states of monitor (WP-10, PLAN §3.2): the
	// timeline in conformance_states and the intent moved
	// (nonconforming, contingent, back to activated), then acknowledged.
	confCounters := &core.Counters{}
	proc.Publish(rt, "conformance_records", confCounters)
	crec := &conformance.Recorder{
		Source: &bus.StreamSource{Open: bus.PullOpener(rt.Bus.JetStream(), proc.TopologyOf(rt.Config), bus.StreamCONF, bus.PullSpec{
			Durable: ConformanceConsumer, FilterSubject: bus.SubjectConfAll, MaxAckPending: 1024,
		})},
		Store: confstore.Store{S: rt.Store}, Intents: intents, Counters: confCounters, Logger: rt.Logger,
	}
	rt.Go(ctx, crec.Run)
	return nil
}

// AlertsConsumer is api's durable consumer of the ALRT stream.
const AlertsConsumer = "api-alerts"

// startAlerts runs the alerts record (WP-11, PLAN §3.2): every alert/v1
// of alrt.v1 recorded by its id, the deliveries traffic-ws reports, and
// the escalation of critical alerts left unacknowledged; it returns the
// acknowledgement service of POST /v1/alerts/{alert_id}/ack.
func startAlerts(ctx context.Context, rt *proc.Runtime, current func() policy.Values) *alerts.Service {
	counters := &core.Counters{}
	proc.Publish(rt, "alerts", counters)
	st := alertstore.Store{S: rt.Store}
	rec := &alerts.Recorder{
		Source: &bus.StreamSource{Open: bus.PullOpener(rt.Bus.JetStream(), proc.TopologyOf(rt.Config), bus.StreamALRT, bus.PullSpec{
			Durable: AlertsConsumer, FilterSubject: bus.SubjectAlrtAll, MaxAckPending: 1024,
		})},
		Store: st, Counters: counters, Logger: rt.Logger,
	}
	svc := &alerts.Service{Store: st, Bus: bus.NewPublisher(rt.Bus, counters), Policy: current, Counters: counters, Logger: rt.Logger}
	rt.Go(ctx, rec.Run)
	rt.Go(ctx, func(ctx context.Context) { svc.RunEscalation(ctx, alerts.DefaultEscalateEvery) })
	return svc
}

// FlightsConsumer is api's durable consumer of the FLIGHT stream.
const FlightsConsumer = "api-flights"

// ConformanceConsumer is api's durable consumer of the CONF stream.
const ConformanceConsumer = "api-conformance"

// staffAdd is `ussp-api staff-add <username> <role>`: it creates a
// console account with the password read from the first line of
// standard input, and prints the account id and, for an admin, the TOTP
// enrolment URI, once. The console's user management is WP-18's; this
// is the bootstrap.
func staffAdd(ctx context.Context, cfg config.Config, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	logger := obs.NewLogger(stderr, "info", "staff-add")
	if len(args) != 2 {
		logger.Error("usage: ussp-api staff-add <username> <supervisor|support|admin> < password")
		return proc.ExitConfig
	}
	if cfg.PGURL == "" {
		logger.Error("configuration invalid", obs.Err(core.Fieldf("USSP_PG_URL", "required")))
		return proc.ExitConfig
	}
	line, err := bufio.NewReader(io.LimitReader(stdin, auth.MaxSecretBytes+2)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		obs.Error(ctx, logger, "password not read", err)
		return proc.ExitFailed
	}
	pass := strings.TrimRight(line, "\r\n")
	st, err := store.Open(ctx, store.Config{RelURL: cfg.PGURL, RelRole: store.AppRole, MaxConns: 2, ApplicationName: "ussp-api-staff-add"})
	if err != nil {
		obs.Error(ctx, logger, "database", err)
		return proc.ExitFailed
	}
	defer st.Close()
	hasher, err := auth.NewHasher()
	if err != nil {
		obs.Error(ctx, logger, "hasher", err)
		return proc.ExitFailed
	}
	svc := &accounts.Service{Store: st, Hasher: hasher, Config: accounts.Config{TOTPIssuer: cfg.SystemID}}
	if cfg.MFAKeyFile != "" {
		if svc.MFA, err = accounts.LoadSealer(cfg.MFAKeyFile); err != nil {
			obs.Error(ctx, logger, "MFA key", err)
			return proc.ExitConfig
		}
	}
	out, err := svc.CreateStaff(ctx, args[0], pass, args[1], "staff-add:"+osUser())
	if err != nil {
		obs.Error(ctx, logger, "staff account not created", err)
		return proc.ExitFailed
	}
	_, _ = fmt.Fprintf(stdout, "id %s\n", out.ID)
	if out.TOTPURI != "" {
		_, _ = fmt.Fprintf(stdout, "totp %s\n", out.TOTPURI)
	}
	return proc.ExitOK
}

func osUser() string {
	for _, k := range []string{"USER", "USERNAME"} {
		if u := os.Getenv(k); u != "" {
			return u
		}
	}
	return "unknown"
}

// bindingsProjector writes one client's bindings to the KV bucket
// client_bindings (auth.BindingsProjector): the sorted fold keys under
// the client id as a bus.KeyToken, and a delete when none is left.
type bindingsProjector struct{ kv *bus.Projector }

// sessionsProjector writes the live sessions to the KV bucket
// sessions_live (auth.SessionsProjector), under the jti as a
// bus.KeyToken, for traffic-ws (audit B2).
type sessionsProjector struct{ kv *bus.Projector }

// ProjectSession implements auth.SessionsProjector.
func (p sessionsProjector) ProjectSession(ctx context.Context, jti string, s auth.LiveSession) error {
	return p.kv.PutJSON(ctx, bus.BucketSessionsLive, bus.KeyToken(jti), s)
}

// EndSession implements auth.SessionsProjector.
func (p sessionsProjector) EndSession(ctx context.Context, jti string) error {
	return p.kv.Delete(ctx, bus.BucketSessionsLive, bus.KeyToken(jti))
}

// ProjectClientBindings implements auth.BindingsProjector.
func (b bindingsProjector) ProjectClientBindings(ctx context.Context, clientID string, folds []string) error {
	key := bus.KeyToken(clientID)
	if len(folds) == 0 {
		return b.kv.Delete(ctx, bus.BucketClientBindings, key)
	}
	f := slices.Clone(folds)
	slices.Sort(f)
	return b.kv.PutJSON(ctx, bus.BucketClientBindings, key, slices.Compact(f))
}
