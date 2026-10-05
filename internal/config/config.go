package config

import (
	"errors"
	"os"
	"regexp"
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

	PGURL                          string `env:"USSP_PG_URL" by:"api" need:"api" kind:"url" secret:"url" help:"relational database (PostgreSQL + PostGIS); only api opens it"`
	TSURL                          string `env:"USSP_TS_URL" by:"api,tsdb-writer" need:"api,tsdb-writer" kind:"url" secret:"url" help:"time-series database (TimescaleDB); tsdb-writer writes, api reads"`
	SchemaWaitS                    int    `env:"USSP_SCHEMA_WAIT_S" default:"60" by:"api,tsdb-writer" min:"0" max:"3600" unit:"s" help:"how long a process waits at start for the migrate subcommand to bring its schema to the version it needs; then it refuses to start, naming both versions"`
	WriterQueueS                   int    `env:"USSP_WRITER_QUEUE_S" default:"10" by:"tsdb-writer" min:"1" max:"300" unit:"s" help:"how long rows may wait in tsdb-writer's memory while writes succeed; beyond it the writer stops pulling and the streams hold the rest"`
	WriterHoldRows                 int    `env:"USSP_WRITER_HOLD_ROWS" default:"50000" by:"tsdb-writer" min:"1000" max:"1000000" unit:"rows" help:"rows tsdb-writer holds in memory per stream, also while TimescaleDB is down (B-07); at the bound it stops pulling and the streams hold the rest"`
	NATSURL                        string `env:"USSP_NATS_URL" by:"all" need:"all" kind:"url" secret:"url" help:"NATS JetStream; the process reconnects forever and starts degraded when it is down"`
	NATSCreds                      string `env:"USSP_NATS_CREDS" by:"all" help:"path of the NATS credentials file of this process; empty uses the URL's userinfo"`
	ConfStreamMaxAgeS              int    `env:"USSP_CONF_STREAM_MAX_AGE_S" default:"172800" by:"all" min:"3600" max:"2592000" unit:"s" help:"how long the CONF stream keeps a conformance state when every process ensures the topology; the record is TimescaleDB's conformance_samples"`
	ConfStreamMaxBytes             int    `env:"USSP_CONF_STREAM_MAX_BYTES" default:"4294967296" by:"all" min:"67108864" max:"1099511627776" unit:"bytes" help:"size bound of the CONF stream, the oldest message discarded beyond it"`
	TRKStreamMaxAgeS               int    `env:"USSP_TRK_STREAM_MAX_AGE_S" default:"3600" by:"all" min:"600" max:"7776000" unit:"s" help:"how long the TRK (the hot path's tracks: restart replay, rid-sp, tsdb-writer) stream keeps a message, the oldest discarded beyond it"`
	TRKStreamMaxBytes              int    `env:"USSP_TRK_STREAM_MAX_BYTES" default:"2147483648" by:"all" min:"8388608" max:"1099511627776" unit:"bytes" help:"size bound of the TRK stream, reserved in the JetStream file store"`
	MANStreamMaxAgeS               int    `env:"USSP_MAN_STREAM_MAX_AGE_S" default:"3600" by:"all" min:"600" max:"7776000" unit:"s" help:"how long the MAN (manned tracks for tsdb-writer) stream keeps a message, the oldest discarded beyond it"`
	MANStreamMaxBytes              int    `env:"USSP_MAN_STREAM_MAX_BYTES" default:"536870912" by:"all" min:"8388608" max:"1099511627776" unit:"bytes" help:"size bound of the MAN stream, reserved in the JetStream file store"`
	PEERStreamMaxAgeS              int    `env:"USSP_PEER_STREAM_MAX_AGE_S" default:"3600" by:"all" min:"600" max:"7776000" unit:"s" help:"how long the PEER (peer flights for tsdb-writer) stream keeps a message, the oldest discarded beyond it"`
	PEERStreamMaxBytes             int    `env:"USSP_PEER_STREAM_MAX_BYTES" default:"536870912" by:"all" min:"8388608" max:"1099511627776" unit:"bytes" help:"size bound of the PEER stream, reserved in the JetStream file store"`
	ALRTStreamMaxAgeS              int    `env:"USSP_ALRT_STREAM_MAX_AGE_S" default:"604800" by:"all" min:"600" max:"7776000" unit:"s" help:"how long the ALRT (alerts: the hand-over to api and traffic-ws; the record is api's alerts table) stream keeps a message, the oldest discarded beyond it"`
	ALRTStreamMaxBytes             int    `env:"USSP_ALRT_STREAM_MAX_BYTES" default:"1073741824" by:"all" min:"8388608" max:"1099511627776" unit:"bytes" help:"size bound of the ALRT stream, reserved in the JetStream file store"`
	IDENTStreamMaxAgeS             int    `env:"USSP_IDENT_STREAM_MAX_AGE_S" default:"86400" by:"all" min:"600" max:"7776000" unit:"s" help:"how long the IDENT (identification changes) stream keeps a message, the oldest discarded beyond it"`
	IDENTStreamMaxBytes            int    `env:"USSP_IDENT_STREAM_MAX_BYTES" default:"268435456" by:"all" min:"8388608" max:"1099511627776" unit:"bytes" help:"size bound of the IDENT stream, reserved in the JetStream file store"`
	INTENTStreamMaxAgeS            int    `env:"USSP_INTENT_STREAM_MAX_AGE_S" default:"2592000" by:"all" min:"600" max:"7776000" unit:"s" help:"how long the INTENT (intent states) stream keeps a message, the oldest discarded beyond it"`
	INTENTStreamMaxBytes           int    `env:"USSP_INTENT_STREAM_MAX_BYTES" default:"536870912" by:"all" min:"8388608" max:"1099511627776" unit:"bytes" help:"size bound of the INTENT stream, reserved in the JetStream file store"`
	CISStreamMaxAgeS               int    `env:"USSP_CIS_STREAM_MAX_AGE_S" default:"2592000" by:"all" min:"600" max:"7776000" unit:"s" help:"how long the CIS (CIS changes) stream keeps a message, the oldest discarded beyond it"`
	CISStreamMaxBytes              int    `env:"USSP_CIS_STREAM_MAX_BYTES" default:"268435456" by:"all" min:"8388608" max:"1099511627776" unit:"bytes" help:"size bound of the CIS stream, reserved in the JetStream file store"`
	TRAFFICStreamMaxAgeS           int    `env:"USSP_TRAFFIC_STREAM_MAX_AGE_S" default:"86400" by:"all" min:"600" max:"7776000" unit:"s" help:"how long the TRAFFIC (traffic products for tsdb-writer) stream keeps a message, the oldest discarded beyond it"`
	TRAFFICStreamMaxBytes          int    `env:"USSP_TRAFFIC_STREAM_MAX_BYTES" default:"536870912" by:"all" min:"8388608" max:"1099511627776" unit:"bytes" help:"size bound of the TRAFFIC stream, reserved in the JetStream file store"`
	INGESTStreamMaxAgeS            int    `env:"USSP_INGEST_STREAM_MAX_AGE_S" default:"600" by:"all" min:"60" max:"7776000" unit:"s" help:"how long the INGEST (telemetry-ingest's work queue; full refuses new messages) stream keeps a message, the oldest discarded beyond it"`
	INGESTStreamMaxBytes           int    `env:"USSP_INGEST_STREAM_MAX_BYTES" default:"268435456" by:"all" min:"8388608" max:"1099511627776" unit:"bytes" help:"size bound of the INGEST stream, reserved in the JetStream file store"`
	FLIGHTStreamMaxAgeS            int    `env:"USSP_FLIGHT_STREAM_MAX_AGE_S" default:"2592000" by:"all" min:"600" max:"7776000" unit:"s" help:"how long the FLIGHT (flight facts from telemetry-ingest to api) stream keeps a message, the oldest discarded beyond it"`
	FLIGHTStreamMaxBytes           int    `env:"USSP_FLIGHT_STREAM_MAX_BYTES" default:"268435456" by:"all" min:"8388608" max:"1099511627776" unit:"bytes" help:"size bound of the FLIGHT stream, reserved in the JetStream file store"`
	CISCurrentBucketMaxBytes       int    `env:"USSP_CIS_CURRENT_BUCKET_MAX_BYTES" default:"268435456" by:"all" min:"1048576" max:"1099511627776" unit:"bytes" help:"size bound of the cis_current bucket, reserved in the JetStream file store; a full bucket refuses puts"`
	PolicyBucketMaxBytes           int    `env:"USSP_POLICY_BUCKET_MAX_BYTES" default:"8388608" by:"all" min:"1048576" max:"1099511627776" unit:"bytes" help:"size bound of the policy bucket, reserved in the JetStream file store; a full bucket refuses puts"`
	SourceControlBucketMaxBytes    int    `env:"USSP_SOURCE_CONTROL_BUCKET_MAX_BYTES" default:"16777216" by:"all" min:"1048576" max:"1099511627776" unit:"bytes" help:"size bound of the source_control bucket, reserved in the JetStream file store; a full bucket refuses puts"`
	RegistryValidityBucketMaxBytes int    `env:"USSP_REGISTRY_VALIDITY_BUCKET_MAX_BYTES" default:"268435456" by:"all" min:"1048576" max:"1099511627776" unit:"bytes" help:"size bound of the registry_validity bucket, reserved in the JetStream file store; a full bucket refuses puts"`
	ClientBindingsBucketMaxBytes   int    `env:"USSP_CLIENT_BINDINGS_BUCKET_MAX_BYTES" default:"67108864" by:"all" min:"1048576" max:"1099511627776" unit:"bytes" help:"size bound of the client_bindings bucket, reserved in the JetStream file store; a full bucket refuses puts"`
	IntentActiveBucketMaxBytes     int    `env:"USSP_INTENT_ACTIVE_BUCKET_MAX_BYTES" default:"268435456" by:"all" min:"1048576" max:"1099511627776" unit:"bytes" help:"size bound of the intent_active bucket, reserved in the JetStream file store; a full bucket refuses puts"`
	TelemetrySeenBucketMaxBytes    int    `env:"USSP_TELEMETRY_SEEN_BUCKET_MAX_BYTES" default:"1073741824" by:"all" min:"1048576" max:"1099511627776" unit:"bytes" help:"size bound of the telemetry_seen bucket, reserved in the JetStream file store; a full bucket refuses puts"`
	ISANotificationsBucketMaxBytes int    `env:"USSP_RID_ISA_NOTIFICATIONS_BUCKET_MAX_BYTES" default:"134217728" by:"all" min:"1048576" max:"1099511627776" unit:"bytes" help:"size bound of the rid_isa_notifications bucket, reserved in the JetStream file store; a full bucket refuses puts"`
	ConformanceStateBucketMaxBytes int    `env:"USSP_CONFORMANCE_STATE_BUCKET_MAX_BYTES" default:"536870912" by:"all" min:"1048576" max:"1099511627776" unit:"bytes" help:"size bound of the conformance_state bucket, reserved in the JetStream file store; a full bucket refuses puts"`
	ProximityStateBucketMaxBytes   int    `env:"USSP_PROXIMITY_STATE_BUCKET_MAX_BYTES" default:"67108864" by:"all" min:"1048576" max:"1099511627776" unit:"bytes" help:"size bound of the proximity_state bucket, reserved in the JetStream file store; a full bucket refuses puts"`
	SessionsLiveBucketMaxBytes     int    `env:"USSP_SESSIONS_LIVE_BUCKET_MAX_BYTES" default:"67108864" by:"all" min:"1048576" max:"1099511627776" unit:"bytes" help:"size bound of the sessions_live bucket, reserved in the JetStream file store; a full bucket refuses puts"`
	RecordHoldsBucketMaxBytes      int    `env:"USSP_RECORD_HOLDS_BUCKET_MAX_BYTES" default:"16777216" by:"all" min:"1048576" max:"1099511627776" unit:"bytes" help:"size bound of the record_holds bucket, reserved in the JetStream file store; a full bucket refuses puts"`
	RIDSubscriptionsBucketMaxBytes int    `env:"USSP_RID_DP_SUBSCRIPTIONS_BUCKET_MAX_BYTES" default:"4194304" by:"all" min:"1048576" max:"1099511627776" unit:"bytes" help:"size bound of the rid_dp_subscriptions bucket, reserved in the JetStream file store; a full bucket refuses puts"`
	MonitorStatusBucketMaxBytes    int    `env:"USSP_MONITOR_STATUS_BUCKET_MAX_BYTES" default:"4194304" by:"all" min:"1048576" max:"1099511627776" unit:"bytes" help:"size bound of the monitor_status bucket, reserved in the JetStream file store; a full bucket refuses puts"`
	JWKSCacheBucketMaxBytes        int    `env:"USSP_JWKS_CACHE_BUCKET_MAX_BYTES" default:"1048576" by:"all" min:"1048576" max:"1099511627776" unit:"bytes" help:"size bound of the jwks_cache bucket, reserved in the JetStream file store; a full bucket refuses puts"`

	SystemID                string   `env:"USSP_SYSTEM_ID" default:"USSP-DEV" by:"api,rid-sp,monitor,dss-sync" help:"the USSP code from the authority's certificate (M8); never an audience"`
	Audiences               []string `env:"USSP_AUDIENCES" by:"api,telemetry-ingest,rid-sp,traffic-ws" help:"hosts accepted as JWT aud, comma-separated: the public host and a lab alias (M18)"`
	JWKSCacheMaxAgeS        int      `env:"USSP_JWKS_CACHE_MAX_AGE_S" default:"86400" by:"api,telemetry-ingest,rid-sp,monitor,traffic-ws" min:"0" max:"86400" unit:"s" help:"how long an ecosystem issuer's JWKS stored in jwks_cache verifies its tokens after its fetch, when a process starts while that issuer cannot be fetched; 0 uses no stored JWKS (spec 05 section 6 default 24 h, pending GCAA)"`
	TokenIssuers            []string `env:"USSP_TOKEN_ISSUERS" by:"api,telemetry-ingest,rid-sp,monitor,traffic-ws" kind:"issuers" help:"allow-listed token issuers as iss=jwks_url, comma-separated; the first is the token service for outgoing calls"`
	CISNotifyIssuers        []string `env:"USSP_CIS_NOTIFY_ISSUERS" by:"api" kind:"issuers" help:"issuers of CIS change notifications (the CISP, the ANSP) as iss=jwks_url, comma-separated"`
	USSBaseURL              string   `env:"USSP_USS_BASE_URL" by:"api,rid-sp,monitor,dss-sync" kind:"url" help:"this USSP's published base URL (uss_base_url in the DSS); monitor's Display Provider subscribes with it and never polls an ISA that names it"`
	DSSBaseURL              string   `env:"USSP_DSS_BASE_URL" by:"api,rid-sp,monitor,dss-sync" kind:"url" help:"InterUSS DSS base URL; its host is the outgoing aud; monitor discovers the peers' ISAs there (F3411 Display Provider)"`
	DSSForAll               string   `env:"USSP_DSS_FOR_ALL" default:"off" by:"api" enum:"on|off" help:"on: every intent that needs an authorisation is deconflicted and written through the DSS, outside U-space airspace too; off: only intents inside U-space airspace (02 F5)"`
	CISPBaseURL             string   `env:"USSP_CISP_BASE_URL" by:"api" kind:"url" help:"CISP base URL (F3 pull)"`
	CISBBox                 string   `env:"USSP_CIS_BBOX" by:"api" help:"box of the CIS change subscription as min_lng,min_lat,max_lng,max_lat in WGS84 degrees; empty is everywhere"`
	CISPublisherKeys        []string `env:"USSP_CIS_PUBLISHER_KEYS" by:"api" kind:"issuers" help:"JWKS of the CIS publishers as authority=jwks_url,ansp=jwks_url: a dataset version is used only when its X-Publisher-Signature verifies with its publisher's key (the authority for zones, uspace_airspace and ussp_list, the ANSP for restrictions); otherwise it is held"`
	CISPublisherSigMaxAgeS  int      `env:"USSP_CIS_PUBLISHER_SIG_MAX_AGE_S" default:"31622400" by:"api" min:"300" max:"315360000" unit:"s" help:"how old the iat of a publisher signature may be when this USSP first reads its version; the CISP forwards the signature made at publication, so it is as old as the version (default 366 days)"`
	CISReconcileS           int      `env:"USSP_CIS_RECONCILE_S" default:"60" by:"api" min:"5" max:"60" unit:"s" help:"period of the conditional pull of every CIS dataset that bounds what a missed change notification costs (spec 02 F3: at most 60 s)"`
	CertificateID           string   `env:"USSP_CERTIFICATE_ID" by:"api" help:"the id of this USSP's certificate at the authority (32 hex characters), named by the Art. 7(6) operating-status notices; unset, no notice can be sent and /readyz says so"`
	AuthorityBaseURL        string   `env:"USSP_AUTHORITY_BASE_URL" by:"api" kind:"url" help:"authority base URL (F8 registry, occurrences, status)"`
	OccurrenceMaxAttempts   int      `env:"USSP_OCCURRENCE_MAX_ATTEMPTS" default:"50" by:"api" min:"1" max:"1000" help:"tries of one occurrence report before it fails and is an alarm on the console; a timeout, 408, 429 or 5xx is tried again, a 409 or another 4xx never (spec 02 section 1; the bound is the spec's default, pending GCAA)"`
	OccurrenceBackoffMaxS   int      `env:"USSP_OCCURRENCE_BACKOFF_MAX_S" default:"600" by:"api" min:"5" max:"86400" unit:"s" help:"the longest wait between two tries of an occurrence report, which doubles from 5 s (the spec's bounded backoff; the default is pending GCAA)"`
	ANSPBaseURL             string   `env:"USSP_ANSP_BASE_URL" by:"api" kind:"url" help:"ANSP base URL (Annex V coordination notices)"`
	ANSPStreamURL           string   `env:"USSP_ANSP_STREAM_URL" by:"monitor" kind:"url" help:"ANSP manned-traffic stream (F4, wss://<ansp>/v1/manned-traffic/stream): monitor reads it with a token of scope ansp.traffic and mTLS per USSP_MTLS_MODE, bootstraps from /v1/manned-traffic/snapshot, publishes its tracks on man.v1 (an echo of one of this USSP's own flights left out, PLAN §15 Q23) and reports it as ansp_feed on /readyz; unset means no manned traffic, shown as unavailable"`
	MTLSMode                string   `env:"USSP_MTLS_MODE" default:"required" by:"api,monitor" enum:"required|off" help:"mTLS towards the ANSP (M25): Annex V notices (api) and the manned-traffic stream (monitor); off only in the lab and on staging, and logged at error level"`
	MTLSCertFile            string   `env:"USSP_MTLS_CERT_FILE" by:"api,monitor" help:"client certificate (PEM) for USSP_MTLS_MODE=required"`
	MTLSKeyFile             string   `env:"USSP_MTLS_KEY_FILE" by:"api,monitor" help:"client key (PEM) for USSP_MTLS_MODE=required"`
	MTLSCAFile              string   `env:"USSP_MTLS_CA_FILE" by:"api,monitor" help:"CA bundle (PEM) the ANSP's certificate is checked against"`
	IssuerKeyFile           string   `env:"USSP_ISSUER_KEY_FILE" by:"api" help:"RSA key (PEM, at least 2048 bits) of this USSP's own token issuer (scripts/gen-issuer-key.sh); unset, api issues no token and starts no session, and says so on /readyz"`
	IssuerPreviousKeyFile   string   `env:"USSP_ISSUER_PREVIOUS_KEY_FILE" by:"api" help:"the previous issuer key (PEM) during a rotation: published in the JWKS and accepted, never used to sign"`
	IssuerURL               string   `env:"USSP_ISSUER_URL" by:"api,telemetry-ingest" kind:"url" help:"iss of this USSP's own tokens; default https:// followed by the first USSP_AUDIENCES entry; telemetry-ingest honours operator scopes only on tokens of this iss (list it in USSP_TOKEN_ISSUERS with api's JWKS)"`
	MFAKeyFile              string   `env:"USSP_MFA_KEY_FILE" by:"api" help:"file holding the base64 of a 32-byte AES-256-GCM key that seals staff TOTP secrets; unset, a staff admin cannot sign in"`
	SessionTTLS             int      `env:"USSP_SESSION_TTL_S" default:"43200" by:"api" min:"300" max:"43200" unit:"s" help:"lifetime of a portal or console session (exp; at most 12 h, M20)"`
	SessionIdleS            int      `env:"USSP_SESSION_IDLE_S" default:"1800" by:"api" min:"60" max:"43200" unit:"s" help:"a session unused this long ends"`
	LoginLockoutAfter       int      `env:"USSP_LOGIN_LOCKOUT_AFTER" default:"10" by:"api" min:"3" max:"100" help:"consecutive failed sign-ins of one username that lock it (kept in the database, across replicas)"`
	LoginLockoutS           int      `env:"USSP_LOGIN_LOCKOUT_S" default:"900" by:"api" min:"60" max:"86400" unit:"s" help:"how long a locked username stays locked"`
	LoginRatePerMin         int      `env:"USSP_LOGIN_RATE_PER_MIN" default:"30" by:"api" min:"1" max:"10000" unit:"1/min" help:"sign-in and self-registration attempts per client address per minute (per process)"`
	TokenRatePerMin         int      `env:"USSP_TOKEN_RATE_PER_MIN" default:"60" by:"api" min:"1" max:"10000" unit:"1/min" help:"POST /oauth/token requests per client id and per client address per minute (per process)"`
	RegistryRatePerMin      int      `env:"USSP_REGISTRY_RATE_PER_MIN" default:"60" by:"api" min:"1" max:"10000" unit:"1/min" help:"GET /v1/registry/validate lookups per operator client per minute (per process)"`
	TrustedProxies          []string `env:"USSP_TRUSTED_PROXIES" by:"api,telemetry-ingest,rid-sp,traffic-ws" kind:"cidrs" help:"CIDRs or addresses of the reverse proxies whose X-Forwarded-For is believed, comma-separated; the client is the rightmost hop that is not one of them; empty: the peer is the client"`
	TokenClientSecretFile   string   `env:"USSP_TOKEN_CLIENT_SECRET_FILE" by:"api,rid-sp,monitor,dss-sync" secret:"true" help:"file holding the client secret of this USSP's client ussp-<code>-01 at the first USSP_TOKEN_ISSUERS entry, for outgoing calls"`
	GeoidFile               string   `env:"USSP_GEOID_FILE" by:"api,telemetry-ingest,monitor,traffic-ws" help:"geoid grid file for AMSL"`
	TerrainDir              string   `env:"USSP_TERRAIN_DIR" by:"monitor" help:"directory of terrain tiles"`
	CellOwnership           string   `env:"USSP_CELL_OWNERSHIP" default:"all" by:"monitor" help:"cells this monitor instance owns: all, or a comma list of c3 cells"`
	AuthorityPush           string   `env:"USSP_AUTHORITY_PUSH" default:"off" by:"rid-sp" enum:"on|off" help:"the optional WS /v1/authority/flights extension (D12)"`
	RecordsDir              string   `env:"USSP_RECORDS_DIR" by:"api" help:"directory (a local volume) the daily record bundles are written to and served from (GET /v1/records/daily/{date}); unset, no bundle is built and /readyz says so"`
	WeatherSource           string   `env:"USSP_WEATHER_SOURCE" by:"api" help:"weather source as awc:<base URL> (the NOAA Aviation Weather Center data API format: <base>/metar and <base>/taf with ids= and format=json, for the policy's weather_station_ids); unset means GET /v1/weather answers 503 weather_unavailable (reason not_configured) and every decision carries weather_unavailable; a malformed value refuses the start"`
	ADSBSource              string   `env:"USSP_ADSB_SOURCE" by:"monitor" help:"e-conspicuity receiver feed: http(s)://<host>/data/aircraft.json (readsb/dump1090, polled at 1 Hz), sbs://<host>:<port> (BaseStation lines) or file://<path>.jsonl (a recorded aircraft.json replay, timestamps re-based); unset means no receiver, shown as such"`
	ADSBReceiverID          string   `env:"USSP_ADSB_RECEIVER_ID" default:"adsb-rx-1" by:"monitor" help:"the e-conspicuity receiver's id: source_instance of its tracks and the instance of its adsb_rx source switch"`
	TrafficInputBBox        string   `env:"USSP_TRAFFIC_INPUT_BBOX" by:"monitor" help:"box of the manned and peer inputs as min_lng,min_lat,max_lng,max_lat in WGS84 degrees: the ANSP stream's bbox and the peer Display Provider's area; empty is every U-space airspace of cis_current, padded"`
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
	if c.CertificateID != "" && !certificateIDRe.MatchString(c.CertificateID) {
		errs = append(errs, core.Fieldf("USSP_CERTIFICATE_ID", "must be the 32 hexadecimal characters of the authority's certificate id"))
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

// certificateIDRe is the authority's CertificateID (its OpenAPI).
var certificateIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

func addrVar(process string) string {
	return "USSP_" + strings.ToUpper(strings.ReplaceAll(process, "-", "_")) + "_ADDR"
}
