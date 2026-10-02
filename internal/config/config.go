package config

import (
	"errors"
	"os"
	"slices"
	"strings"

	"github.com/rootxkit/uspace-core/core"
)

// The seven long-running processes (docs/PLAN.md D1, spec 00 §6.1).
const (
	ProcessAPI             = "api"
	ProcessTelemetryIngest = "telemetry-ingest"
	ProcessRIDSP           = "rid-sp"
	ProcessMonitor         = "monitor"
	ProcessTrafficWS       = "traffic-ws"
	ProcessDSSSync         = "dss-sync"
	ProcessTSDBWriter      = "tsdb-writer"
)

// Processes lists the seven processes in the order of the plan.
var Processes = []string{
	ProcessAPI, ProcessTelemetryIngest, ProcessRIDSP, ProcessMonitor,
	ProcessTrafficWS, ProcessDSSSync, ProcessTSDBWriter,
}

// Config is the whole configuration. Struct tags (read by Load, Help
// and Redacted; see load.go):
//
//	env       the variable
//	default   used when the variable is unset or empty
//	by        the processes that read it ("all" or a comma list)
//	need      the processes for which it is required
//	secret    "url": the userinfo of the URL is redacted; "true": the whole value
//	enum      the allowed values, "|"-separated
//	kind      "url" (absolute URL), "issuers" (iss=jwks_url pairs) or
//	          "cidrs" (CIDRs or addresses)
//	min, max  bounds of an integer
//	unit      the unit, for deploy/ENV.md
//	help      one line
type Config struct {
	LogLevel           string `env:"USSP_LOG_LEVEL" default:"info" by:"all" enum:"debug|info|warn|error" help:"minimum level of the JSON log on stdout"`
	OTLPURL            string `env:"USSP_OTLP_URL" by:"all" kind:"url" help:"OTLP/HTTP trace endpoint; tracing is a no-op when unset"`
	StatusIntervalS    int    `env:"USSP_STATUS_INTERVAL_S" default:"60" by:"all" min:"1" max:"3600" unit:"s" help:"seconds between status lines carrying every counter and dependency state (E-09)"`
	ShutdownTimeoutS   int    `env:"USSP_SHUTDOWN_TIMEOUT_S" default:"15" by:"all" min:"1" max:"300" unit:"s" help:"bound on the drain after SIGTERM; exceeding it exits 1"`
	MaxBodyBytes       int    `env:"USSP_HTTP_MAX_BODY_BYTES" default:"1048576" by:"all" min:"1024" max:"67108864" unit:"bytes" help:"default request body cap; a route may override it"`
	ReadHeaderTimeoutS int    `env:"USSP_HTTP_READ_HEADER_TIMEOUT_S" default:"5" by:"all" min:"1" max:"60" unit:"s" help:"time allowed to read the request headers"`

	APIAddr             string `env:"USSP_API_ADDR" default:":8080" by:"api" help:"listen address of api (national API, /healthz, /readyz, /metrics)"`
	TelemetryIngestAddr string `env:"USSP_TELEMETRY_INGEST_ADDR" default:":8081" by:"telemetry-ingest" help:"listen address of telemetry-ingest"`
	RIDSPAddr           string `env:"USSP_RID_SP_ADDR" default:":8082" by:"rid-sp" help:"listen address of rid-sp"`
	MonitorAddr         string `env:"USSP_MONITOR_ADDR" default:":8083" by:"monitor" help:"listen address of monitor (health and metrics only)"`
	TrafficWSAddr       string `env:"USSP_TRAFFIC_WS_ADDR" default:":8084" by:"traffic-ws" help:"listen address of traffic-ws"`
	DSSSyncAddr         string `env:"USSP_DSS_SYNC_ADDR" default:":8085" by:"dss-sync" help:"listen address of dss-sync (health and metrics only)"`
	TSDBWriterAddr      string `env:"USSP_TSDB_WRITER_ADDR" default:":8086" by:"tsdb-writer" help:"listen address of tsdb-writer (health and metrics only)"`

	PGURL       string `env:"USSP_PG_URL" by:"api" need:"api" kind:"url" secret:"url" help:"relational database (PostgreSQL + PostGIS); only api opens it"`
	TSURL       string `env:"USSP_TS_URL" by:"api,tsdb-writer" need:"api,tsdb-writer" kind:"url" secret:"url" help:"time-series database (TimescaleDB); tsdb-writer writes, api reads"`
	SchemaWaitS int    `env:"USSP_SCHEMA_WAIT_S" default:"60" by:"api,tsdb-writer" min:"0" max:"3600" unit:"s" help:"how long a process waits at start for the migrate subcommand to bring its schema to the version it needs; then it refuses to start, naming both versions"`
	NATSURL     string `env:"USSP_NATS_URL" by:"all" need:"all" kind:"url" secret:"url" help:"NATS JetStream; the process reconnects forever and starts degraded when it is down"`
	NATSCreds   string `env:"USSP_NATS_CREDS" by:"all" help:"path of the NATS credentials file of this process; empty uses the URL's userinfo"`

	SystemID                string   `env:"USSP_SYSTEM_ID" default:"USSP-DEV" by:"api,rid-sp,dss-sync" help:"the USSP code from the authority's certificate (M8); never an audience"`
	Audiences               []string `env:"USSP_AUDIENCES" by:"api,telemetry-ingest,rid-sp,traffic-ws" help:"hosts accepted as JWT aud, comma-separated: the public host and a lab alias (M18)"`
	TokenIssuers            []string `env:"USSP_TOKEN_ISSUERS" by:"api,telemetry-ingest,rid-sp,traffic-ws" kind:"issuers" help:"allow-listed token issuers as iss=jwks_url, comma-separated; the first is the token service for outgoing calls"`
	CISNotifyIssuers        []string `env:"USSP_CIS_NOTIFY_ISSUERS" by:"api" kind:"issuers" help:"issuers of CIS change notifications (the CISP, the ANSP) as iss=jwks_url, comma-separated"`
	USSBaseURL              string   `env:"USSP_USS_BASE_URL" by:"api,rid-sp,dss-sync" kind:"url" help:"this USSP's published base URL (uss_base_url in the DSS)"`
	DSSBaseURL              string   `env:"USSP_DSS_BASE_URL" by:"api,rid-sp,dss-sync" kind:"url" help:"InterUSS DSS base URL; its host is the outgoing aud"`
	CISPBaseURL             string   `env:"USSP_CISP_BASE_URL" by:"api" kind:"url" help:"CISP base URL (F3 pull)"`
	CISBBox                 string   `env:"USSP_CIS_BBOX" by:"api" help:"box of the CIS change subscription as min_lng,min_lat,max_lng,max_lat in WGS84 degrees; empty is everywhere"`
	CISPublisherKeys        []string `env:"USSP_CIS_PUBLISHER_KEYS" by:"api" kind:"issuers" help:"JWKS of the CIS publishers as authority=jwks_url,ansp=jwks_url: a dataset version is used only when its X-Publisher-Signature verifies with its publisher's key (the authority for zones, uspace_airspace and ussp_list, the ANSP for restrictions); otherwise it is held"`
	CISPublisherSigMaxAgeS  int      `env:"USSP_CIS_PUBLISHER_SIG_MAX_AGE_S" default:"31622400" by:"api" min:"300" max:"315360000" unit:"s" help:"how old the iat of a publisher signature may be when this USSP first reads its version; the CISP forwards the signature made at publication, so it is as old as the version (default 366 days)"`
	CISReconcileS           int      `env:"USSP_CIS_RECONCILE_S" default:"60" by:"api" min:"5" max:"60" unit:"s" help:"period of the conditional pull of every CIS dataset that bounds what a missed change notification costs (spec 02 F3: at most 60 s)"`
	AuthorityBaseURL        string   `env:"USSP_AUTHORITY_BASE_URL" by:"api" kind:"url" help:"authority base URL (F8 registry, occurrences, status)"`
	ANSPBaseURL             string   `env:"USSP_ANSP_BASE_URL" by:"api" kind:"url" help:"ANSP base URL (Annex V coordination notices)"`
	ANSPStreamURL           string   `env:"USSP_ANSP_STREAM_URL" by:"monitor" kind:"url" help:"ANSP manned-traffic stream (F4)"`
	MTLSMode                string   `env:"USSP_MTLS_MODE" default:"required" by:"monitor" enum:"required|off" help:"mTLS towards the ANSP (M25); off only in the lab and on staging, and logged at error level"`
	MTLSCertFile            string   `env:"USSP_MTLS_CERT_FILE" by:"monitor" help:"client certificate (PEM) for USSP_MTLS_MODE=required"`
	MTLSKeyFile             string   `env:"USSP_MTLS_KEY_FILE" by:"monitor" help:"client key (PEM) for USSP_MTLS_MODE=required"`
	MTLSCAFile              string   `env:"USSP_MTLS_CA_FILE" by:"monitor" help:"CA bundle (PEM) the ANSP's certificate is checked against"`
	IssuerKeyFile           string   `env:"USSP_ISSUER_KEY_FILE" by:"api" help:"RSA key (PEM, at least 2048 bits) of this USSP's own token issuer (scripts/gen-issuer-key.sh); unset, api issues no token and starts no session, and says so on /readyz"`
	IssuerPreviousKeyFile   string   `env:"USSP_ISSUER_PREVIOUS_KEY_FILE" by:"api" help:"the previous issuer key (PEM) during a rotation: published in the JWKS and accepted, never used to sign"`
	IssuerURL               string   `env:"USSP_ISSUER_URL" by:"api" kind:"url" help:"iss of this USSP's own tokens; default https:// followed by the first USSP_AUDIENCES entry"`
	MFAKeyFile              string   `env:"USSP_MFA_KEY_FILE" by:"api" help:"file holding the base64 of a 32-byte AES-256-GCM key that seals staff TOTP secrets; unset, a staff admin cannot sign in"`
	SessionTTLS             int      `env:"USSP_SESSION_TTL_S" default:"43200" by:"api" min:"300" max:"43200" unit:"s" help:"lifetime of a portal or console session (exp; at most 12 h, M20)"`
	SessionIdleS            int      `env:"USSP_SESSION_IDLE_S" default:"1800" by:"api" min:"60" max:"43200" unit:"s" help:"a session unused this long ends"`
	LoginLockoutAfter       int      `env:"USSP_LOGIN_LOCKOUT_AFTER" default:"10" by:"api" min:"3" max:"100" help:"consecutive failed sign-ins of one username that lock it (kept in the database, across replicas)"`
	LoginLockoutS           int      `env:"USSP_LOGIN_LOCKOUT_S" default:"900" by:"api" min:"60" max:"86400" unit:"s" help:"how long a locked username stays locked"`
	LoginRatePerMin         int      `env:"USSP_LOGIN_RATE_PER_MIN" default:"30" by:"api" min:"1" max:"10000" unit:"1/min" help:"sign-in and self-registration attempts per client address per minute (per process)"`
	TokenRatePerMin         int      `env:"USSP_TOKEN_RATE_PER_MIN" default:"60" by:"api" min:"1" max:"10000" unit:"1/min" help:"POST /oauth/token requests per client id and per client address per minute (per process)"`
	TrustedProxies          []string `env:"USSP_TRUSTED_PROXIES" by:"api,telemetry-ingest,rid-sp,traffic-ws" kind:"cidrs" help:"CIDRs or addresses of the reverse proxies whose X-Forwarded-For is believed, comma-separated; the client is the rightmost hop that is not one of them; empty: the peer is the client"`
	TokenClientSecretFile   string   `env:"USSP_TOKEN_CLIENT_SECRET_FILE" by:"api,rid-sp,monitor,dss-sync" secret:"true" help:"file holding the client secret of this USSP's client ussp-<code>-01 at the first USSP_TOKEN_ISSUERS entry, for outgoing calls"`
	GeoidFile               string   `env:"USSP_GEOID_FILE" by:"telemetry-ingest,monitor" help:"geoid grid file for AMSL"`
	TerrainDir              string   `env:"USSP_TERRAIN_DIR" by:"monitor" help:"directory of terrain tiles"`
	CellOwnership           string   `env:"USSP_CELL_OWNERSHIP" default:"all" by:"monitor" help:"cells this monitor instance owns: all, or a comma list of c3 cells"`
	AuthorityPush           string   `env:"USSP_AUTHORITY_PUSH" default:"off" by:"rid-sp" enum:"on|off" help:"the optional WS /v1/authority/flights extension (D12)"`
	WeatherSource           string   `env:"USSP_WEATHER_SOURCE" by:"api" help:"weather source adapter and URL; unset means weather answers 503 weather_unavailable"`
	ADSBSource              string   `env:"USSP_ADSB_SOURCE" by:"monitor" help:"e-conspicuity receiver feed; unset means no receiver, shown as such"`
	WSAllowedOrigins        []string `env:"USSP_WS_ALLOWED_ORIGINS" by:"telemetry-ingest,traffic-ws" help:"Origin allow-list of browser WebSocket upgrades (M22), comma-separated"`
	ReadinessCheckTimeoutMS int      `env:"USSP_READINESS_CHECK_TIMEOUT_MS" default:"2000" by:"all" min:"100" max:"10000" unit:"ms" help:"bound on one dependency check of /readyz"`
}

// Load reads the configuration from the process environment.
func Load() (Config, error) { return LoadFrom(os.LookupEnv) }

// LoadFrom reads the configuration through lookup (a map in tests). Every
// problem is a *core.FieldError naming the variable; all of them are
// returned, joined, so a deployment sees every mistake in one run.
// Requirements that depend on the process are checked by Require.
func LoadFrom(lookup LookupFunc) (Config, error) {
	var c Config
	if err := load(&c, lookup); err != nil {
		return Config{}, err
	}
	if err := c.validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Require returns a *core.FieldError for every variable one of
// processes needs and does not have.
func (c Config) Require(processes ...string) error {
	var errs []error
	each(&c, func(f field) {
		if f.empty() && slices.ContainsFunc(processes, f.neededBy) {
			errs = append(errs, &core.FieldError{Field: f.name, Reason: "required by " + strings.Join(f.neededFor(processes), ", ")})
		}
	})
	return errors.Join(errs...)
}

// Addr returns the listen address of process.
func (c Config) Addr(process string) string {
	switch process {
	case ProcessAPI:
		return c.APIAddr
	case ProcessTelemetryIngest:
		return c.TelemetryIngestAddr
	case ProcessRIDSP:
		return c.RIDSPAddr
	case ProcessMonitor:
		return c.MonitorAddr
	case ProcessTrafficWS:
		return c.TrafficWSAddr
	case ProcessDSSSync:
		return c.DSSSyncAddr
	case ProcessTSDBWriter:
		return c.TSDBWriterAddr
	default:
		return ""
	}
}

// Redacted renders the configuration as NAME=value pairs with the
// credentials of every URL and every secret removed, for the start line.
func (c Config) Redacted() string { return describe(&c) }

// Issuer is one allow-listed token issuer and its JWKS URL.
type Issuer struct {
	Issuer  string
	JWKSURL string
}

// ParseIssuers splits iss=jwks_url entries at the first "=".
func ParseIssuers(entries []string) ([]Issuer, error) {
	out := make([]Issuer, 0, len(entries))
	for _, e := range entries {
		iss, jwks, ok := strings.Cut(e, "=")
		iss, jwks = strings.TrimSpace(iss), strings.TrimSpace(jwks)
		if !ok || iss == "" || jwks == "" {
			return nil, errors.New("each entry must be iss=jwks_url")
		}
		if err := checkURL(jwks); err != nil {
			return nil, errors.New("the JWKS URL of " + iss + " " + err.Error())
		}
		out = append(out, Issuer{Issuer: iss, JWKSURL: jwks})
	}
	return out, nil
}

func (c Config) validate() error {
	var errs []error
	if c.PGURL != "" && c.PGURL == c.TSURL {
		// CLAUDE.md rule 10: two trees, two databases, never one.
		errs = append(errs, &core.FieldError{Field: "USSP_TS_URL", Reason: "must name a different database from USSP_PG_URL"})
	}
	addrs := map[string]string{}
	for _, p := range Processes {
		a := c.Addr(p)
		if other, dup := addrs[a]; dup && !strings.HasSuffix(a, ":0") {
			errs = append(errs, core.Fieldf(addrVar(p), "same listen address as %s", other))
		}
		addrs[a] = p
	}
	return errors.Join(errs...)
}

func addrVar(process string) string {
	return "USSP_" + strings.ToUpper(strings.ReplaceAll(process, "-", "_")) + "_ADDR"
}
