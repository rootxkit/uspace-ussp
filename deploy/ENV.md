# Environment variables

Every process is configured by the environment only (`docs/PLAN.md`
§11). This table is the reference: `internal/config` reads exactly these
variables, and `TestEnvMarkdownDocumentsEveryVariable` fails when the
two differ (name, readers, requirement, default or unit). `ussp-<process>
--help` prints the subset one process reads.

- **Read by**: `all` or the processes that read the variable.
- **Required by**: the processes that exit with code 2, naming the
  variable, when it is unset or empty. Nothing else stops a process: a
  dependency that is unreachable is reported on `/readyz` and the
  process keeps running (LESSONS B-08).
- URLs that may carry credentials (`USSP_PG_URL`, `USSP_TS_URL`,
  `USSP_NATS_URL`) are logged with the userinfo replaced by `xxxxx`.
- No hostname is a default: every peer, issuer and base URL is
  deployment configuration (CLAUDE.md rule 9).

| Variable | Read by | Required by | Default | Unit | Meaning |
|---|---|---|---|---|---|
| `USSP_LOG_LEVEL` | `all` |  | `info` |  | minimum level of the JSON log on stdout |
| `USSP_OTLP_URL` | `all` |  |  |  | OTLP/HTTP trace endpoint; tracing is a no-op when unset |
| `USSP_STATUS_INTERVAL_S` | `all` |  | `60` | s | seconds between status lines carrying every counter and dependency state (E-09) |
| `USSP_SHUTDOWN_TIMEOUT_S` | `all` |  | `15` | s | bound on the drain after SIGTERM; exceeding it exits 1 |
| `USSP_HTTP_MAX_BODY_BYTES` | `all` |  | `1048576` | bytes | default request body cap; a route may override it |
| `USSP_HTTP_READ_HEADER_TIMEOUT_S` | `all` |  | `5` | s | time allowed to read the request headers |
| `USSP_API_ADDR` | `api` |  | `:8080` |  | listen address of api (national API, /healthz, /readyz, /metrics) |
| `USSP_TELEMETRY_INGEST_ADDR` | `telemetry-ingest` |  | `:8081` |  | listen address of telemetry-ingest |
| `USSP_RID_SP_ADDR` | `rid-sp` |  | `:8082` |  | listen address of rid-sp |
| `USSP_MONITOR_ADDR` | `monitor` |  | `:8083` |  | listen address of monitor (health and metrics only) |
| `USSP_TRAFFIC_WS_ADDR` | `traffic-ws` |  | `:8084` |  | listen address of traffic-ws |
| `USSP_DSS_SYNC_ADDR` | `dss-sync` |  | `:8085` |  | listen address of dss-sync (health and metrics only) |
| `USSP_TSDB_WRITER_ADDR` | `tsdb-writer` |  | `:8086` |  | listen address of tsdb-writer (health and metrics only) |
| `USSP_PG_URL` | `api` | `api` |  |  | relational database (PostgreSQL + PostGIS); only api opens it |
| `USSP_TS_URL` | `api,tsdb-writer` | `api,tsdb-writer` |  |  | time-series database (TimescaleDB); tsdb-writer writes, api reads |
| `USSP_SCHEMA_WAIT_S` | `api,tsdb-writer` |  | `60` | s | how long a process waits at start for the migrate subcommand to bring its schema to the version it needs; then it refuses to start, naming both versions |
| `USSP_WRITER_QUEUE_S` | `tsdb-writer` |  | `10` | s | how long rows may wait in tsdb-writer's memory while writes succeed; beyond it the writer stops pulling and the streams hold the rest |
| `USSP_WRITER_HOLD_ROWS` | `tsdb-writer` |  | `50000` | rows | rows tsdb-writer holds in memory per stream, also while TimescaleDB is down (B-07); at the bound it stops pulling and the streams hold the rest |
| `USSP_NATS_URL` | `all` | `all` |  |  | NATS JetStream; the process reconnects forever and starts degraded when it is down |
| `USSP_NATS_CREDS` | `all` |  |  |  | path of the NATS credentials file of this process; empty uses the URL's userinfo |
| `USSP_CONF_STREAM_MAX_AGE_S` | `all` |  | `172800` | s | how long the CONF stream keeps a conformance state when every process ensures the topology; the record is TimescaleDB's conformance_samples |
| `USSP_CONF_STREAM_MAX_BYTES` | `all` |  | `4294967296` | bytes | size bound of the CONF stream, the oldest message discarded beyond it |
| `USSP_TRK_STREAM_MAX_AGE_S` | `all` |  | `3600` | s | how long the TRK (the hot path's tracks: restart replay, rid-sp, tsdb-writer) stream keeps a message, the oldest discarded beyond it |
| `USSP_TRK_STREAM_MAX_BYTES` | `all` |  | `2147483648` | bytes | size bound of the TRK stream, reserved in the JetStream file store |
| `USSP_MAN_STREAM_MAX_AGE_S` | `all` |  | `3600` | s | how long the MAN (manned tracks for tsdb-writer) stream keeps a message, the oldest discarded beyond it |
| `USSP_MAN_STREAM_MAX_BYTES` | `all` |  | `536870912` | bytes | size bound of the MAN stream, reserved in the JetStream file store |
| `USSP_PEER_STREAM_MAX_AGE_S` | `all` |  | `3600` | s | how long the PEER (peer flights for tsdb-writer) stream keeps a message, the oldest discarded beyond it |
| `USSP_PEER_STREAM_MAX_BYTES` | `all` |  | `536870912` | bytes | size bound of the PEER stream, reserved in the JetStream file store |
| `USSP_ALRT_STREAM_MAX_AGE_S` | `all` |  | `604800` | s | how long the ALRT (alerts: the hand-over to api and traffic-ws; the record is api's alerts table) stream keeps a message, the oldest discarded beyond it |
| `USSP_ALRT_STREAM_MAX_BYTES` | `all` |  | `1073741824` | bytes | size bound of the ALRT stream, reserved in the JetStream file store |
| `USSP_IDENT_STREAM_MAX_AGE_S` | `all` |  | `86400` | s | how long the IDENT (identification changes) stream keeps a message, the oldest discarded beyond it |
| `USSP_IDENT_STREAM_MAX_BYTES` | `all` |  | `268435456` | bytes | size bound of the IDENT stream, reserved in the JetStream file store |
| `USSP_INTENT_STREAM_MAX_AGE_S` | `all` |  | `2592000` | s | how long the INTENT (intent states) stream keeps a message, the oldest discarded beyond it |
| `USSP_INTENT_STREAM_MAX_BYTES` | `all` |  | `536870912` | bytes | size bound of the INTENT stream, reserved in the JetStream file store |
| `USSP_CIS_STREAM_MAX_AGE_S` | `all` |  | `2592000` | s | how long the CIS (CIS changes) stream keeps a message, the oldest discarded beyond it |
| `USSP_CIS_STREAM_MAX_BYTES` | `all` |  | `268435456` | bytes | size bound of the CIS stream, reserved in the JetStream file store |
| `USSP_TRAFFIC_STREAM_MAX_AGE_S` | `all` |  | `86400` | s | how long the TRAFFIC (traffic products for tsdb-writer) stream keeps a message, the oldest discarded beyond it |
| `USSP_TRAFFIC_STREAM_MAX_BYTES` | `all` |  | `536870912` | bytes | size bound of the TRAFFIC stream, reserved in the JetStream file store |
| `USSP_INGEST_STREAM_MAX_AGE_S` | `all` |  | `600` | s | how long the INGEST (telemetry-ingest's work queue; full refuses new messages) stream keeps a message, the oldest discarded beyond it |
| `USSP_INGEST_STREAM_MAX_BYTES` | `all` |  | `268435456` | bytes | size bound of the INGEST stream, reserved in the JetStream file store |
| `USSP_FLIGHT_STREAM_MAX_AGE_S` | `all` |  | `2592000` | s | how long the FLIGHT (flight facts from telemetry-ingest to api) stream keeps a message, the oldest discarded beyond it |
| `USSP_FLIGHT_STREAM_MAX_BYTES` | `all` |  | `268435456` | bytes | size bound of the FLIGHT stream, reserved in the JetStream file store |
| `USSP_CIS_CURRENT_BUCKET_MAX_BYTES` | `all` |  | `268435456` | bytes | size bound of the cis_current bucket, reserved in the JetStream file store; a full bucket refuses puts |
| `USSP_POLICY_BUCKET_MAX_BYTES` | `all` |  | `8388608` | bytes | size bound of the policy bucket, reserved in the JetStream file store; a full bucket refuses puts |
| `USSP_SOURCE_CONTROL_BUCKET_MAX_BYTES` | `all` |  | `16777216` | bytes | size bound of the source_control bucket, reserved in the JetStream file store; a full bucket refuses puts |
| `USSP_REGISTRY_VALIDITY_BUCKET_MAX_BYTES` | `all` |  | `268435456` | bytes | size bound of the registry_validity bucket, reserved in the JetStream file store; a full bucket refuses puts |
| `USSP_CLIENT_BINDINGS_BUCKET_MAX_BYTES` | `all` |  | `67108864` | bytes | size bound of the client_bindings bucket, reserved in the JetStream file store; a full bucket refuses puts |
| `USSP_INTENT_ACTIVE_BUCKET_MAX_BYTES` | `all` |  | `268435456` | bytes | size bound of the intent_active bucket, reserved in the JetStream file store; a full bucket refuses puts |
| `USSP_TELEMETRY_SEEN_BUCKET_MAX_BYTES` | `all` |  | `1073741824` | bytes | size bound of the telemetry_seen bucket, reserved in the JetStream file store; a full bucket refuses puts |
| `USSP_RID_ISA_NOTIFICATIONS_BUCKET_MAX_BYTES` | `all` |  | `134217728` | bytes | size bound of the rid_isa_notifications bucket, reserved in the JetStream file store; a full bucket refuses puts |
| `USSP_CONFORMANCE_STATE_BUCKET_MAX_BYTES` | `all` |  | `536870912` | bytes | size bound of the conformance_state bucket, reserved in the JetStream file store; a full bucket refuses puts |
| `USSP_PROXIMITY_STATE_BUCKET_MAX_BYTES` | `all` |  | `67108864` | bytes | size bound of the proximity_state bucket, reserved in the JetStream file store; a full bucket refuses puts |
| `USSP_SESSIONS_LIVE_BUCKET_MAX_BYTES` | `all` |  | `67108864` | bytes | size bound of the sessions_live bucket, reserved in the JetStream file store; a full bucket refuses puts |
| `USSP_RECORD_HOLDS_BUCKET_MAX_BYTES` | `all` |  | `16777216` | bytes | size bound of the record_holds bucket, reserved in the JetStream file store; a full bucket refuses puts |
| `USSP_RID_DP_SUBSCRIPTIONS_BUCKET_MAX_BYTES` | `all` |  | `4194304` | bytes | size bound of the rid_dp_subscriptions bucket, reserved in the JetStream file store; a full bucket refuses puts |
| `USSP_MONITOR_STATUS_BUCKET_MAX_BYTES` | `all` |  | `4194304` | bytes | size bound of the monitor_status bucket, reserved in the JetStream file store; a full bucket refuses puts |
| `USSP_SYSTEM_ID` | `api,rid-sp,monitor,dss-sync` |  | `USSP-DEV` |  | the USSP code from the authority's certificate (M8); never an audience |
| `USSP_AUDIENCES` | `api,telemetry-ingest,rid-sp,traffic-ws` |  |  |  | hosts accepted as JWT aud, comma-separated: the public host and a lab alias (M18) |
| `USSP_TOKEN_ISSUERS` | `api,telemetry-ingest,rid-sp,monitor,traffic-ws` |  |  |  | allow-listed token issuers as iss=jwks_url, comma-separated; the first is the token service for outgoing calls |
| `USSP_CIS_NOTIFY_ISSUERS` | `api` |  |  |  | issuers of CIS change notifications (the CISP, the ANSP) as iss=jwks_url, comma-separated |
| `USSP_USS_BASE_URL` | `api,rid-sp,monitor,dss-sync` |  |  |  | this USSP's published base URL (uss_base_url in the DSS); monitor's Display Provider subscribes with it and never polls an ISA that names it |
| `USSP_DSS_BASE_URL` | `api,rid-sp,monitor,dss-sync` |  |  |  | InterUSS DSS base URL; its host is the outgoing aud; monitor discovers the peers' ISAs there (F3411 Display Provider) |
| `USSP_DSS_FOR_ALL` | `api` |  | `off` |  | on: every intent that needs an authorisation is deconflicted and written through the DSS, outside U-space airspace too; off: only intents inside U-space airspace (02 F5) |
| `USSP_CISP_BASE_URL` | `api` |  |  |  | CISP base URL (F3 pull) |
| `USSP_CIS_BBOX` | `api` |  |  |  | box of the CIS change subscription as min_lng,min_lat,max_lng,max_lat in WGS84 degrees; empty is everywhere |
| `USSP_CIS_PUBLISHER_KEYS` | `api` |  |  |  | JWKS of the CIS publishers as authority=jwks_url,ansp=jwks_url: a dataset version is used only when its X-Publisher-Signature verifies with its publisher's key (the authority for zones, uspace_airspace and ussp_list, the ANSP for restrictions); otherwise it is held |
| `USSP_CIS_PUBLISHER_SIG_MAX_AGE_S` | `api` |  | `31622400` | s | how old the iat of a publisher signature may be when this USSP first reads its version; the CISP forwards the signature made at publication, so it is as old as the version (default 366 days) |
| `USSP_CIS_RECONCILE_S` | `api` |  | `60` | s | period of the conditional pull of every CIS dataset that bounds what a missed change notification costs (spec 02 F3: at most 60 s) |
| `USSP_CERTIFICATE_ID` | `api` |  |  |  | the id of this USSP's certificate at the authority (32 hex characters), named by the Art. 7(6) operating-status notices; unset, no notice can be sent and /readyz says so |
| `USSP_AUTHORITY_BASE_URL` | `api` |  |  |  | authority base URL (F8 registry, occurrences, status) |
| `USSP_ANSP_BASE_URL` | `api` |  |  |  | ANSP base URL (Annex V coordination notices) |
| `USSP_ANSP_STREAM_URL` | `monitor` |  |  |  | ANSP manned-traffic stream (F4, wss://<ansp>/v1/manned-traffic/stream): monitor reads it with a token of scope ansp.traffic and mTLS per USSP_MTLS_MODE, bootstraps from /v1/manned-traffic/snapshot, publishes its tracks on man.v1 (an echo of one of this USSP's own flights left out, PLAN §15 Q23) and reports it as ansp_feed on /readyz; unset means no manned traffic, shown as unavailable |
| `USSP_MTLS_MODE` | `api,monitor` |  | `required` |  | mTLS towards the ANSP (M25): Annex V notices (api) and the manned-traffic stream (monitor); off only in the lab and on staging, and logged at error level |
| `USSP_MTLS_CERT_FILE` | `api,monitor` |  |  |  | client certificate (PEM) for USSP_MTLS_MODE=required |
| `USSP_MTLS_KEY_FILE` | `api,monitor` |  |  |  | client key (PEM) for USSP_MTLS_MODE=required |
| `USSP_MTLS_CA_FILE` | `api,monitor` |  |  |  | CA bundle (PEM) the ANSP's certificate is checked against |
| `USSP_ISSUER_KEY_FILE` | `api` |  |  |  | RSA key (PEM, at least 2048 bits) of this USSP's own token issuer (scripts/gen-issuer-key.sh); unset, api issues no token and starts no session, and says so on /readyz |
| `USSP_ISSUER_PREVIOUS_KEY_FILE` | `api` |  |  |  | the previous issuer key (PEM) during a rotation: published in the JWKS and accepted, never used to sign |
| `USSP_ISSUER_URL` | `api,telemetry-ingest` |  |  |  | iss of this USSP's own tokens; default https:// followed by the first USSP_AUDIENCES entry; telemetry-ingest honours operator scopes only on tokens of this iss (list it in USSP_TOKEN_ISSUERS with api's JWKS) |
| `USSP_MFA_KEY_FILE` | `api` |  |  |  | file holding the base64 of a 32-byte AES-256-GCM key that seals staff TOTP secrets; unset, a staff admin cannot sign in |
| `USSP_SESSION_TTL_S` | `api` |  | `43200` | s | lifetime of a portal or console session (exp; at most 12 h, M20) |
| `USSP_SESSION_IDLE_S` | `api` |  | `1800` | s | a session unused this long ends |
| `USSP_LOGIN_LOCKOUT_AFTER` | `api` |  | `10` |  | consecutive failed sign-ins of one username that lock it (kept in the database, across replicas) |
| `USSP_LOGIN_LOCKOUT_S` | `api` |  | `900` | s | how long a locked username stays locked |
| `USSP_LOGIN_RATE_PER_MIN` | `api` |  | `30` | 1/min | sign-in and self-registration attempts per client address per minute (per process) |
| `USSP_TOKEN_RATE_PER_MIN` | `api` |  | `60` | 1/min | POST /oauth/token requests per client id and per client address per minute (per process) |
| `USSP_REGISTRY_RATE_PER_MIN` | `api` |  | `60` | 1/min | GET /v1/registry/validate lookups per operator client per minute (per process) |
| `USSP_TRUSTED_PROXIES` | `api,telemetry-ingest,rid-sp,traffic-ws` |  |  |  | CIDRs or addresses of the reverse proxies whose X-Forwarded-For is believed, comma-separated; the client is the rightmost hop that is not one of them; empty: the peer is the client |
| `USSP_TOKEN_CLIENT_SECRET_FILE` | `api,rid-sp,monitor,dss-sync` |  |  |  | file holding the client secret of this USSP's client ussp-<code>-01 at the first USSP_TOKEN_ISSUERS entry, for outgoing calls |
| `USSP_GEOID_FILE` | `api,telemetry-ingest,monitor,traffic-ws` |  |  |  | geoid grid file for AMSL |
| `USSP_TERRAIN_DIR` | `monitor` |  |  |  | directory of terrain tiles |
| `USSP_CELL_OWNERSHIP` | `monitor` |  | `all` |  | cells this monitor instance owns: all, or a comma list of c3 cells |
| `USSP_AUTHORITY_PUSH` | `rid-sp` |  | `off` |  | the optional WS /v1/authority/flights extension (D12) |
| `USSP_RECORDS_DIR` | `api` |  |  |  | directory (a local volume) the daily record bundles are written to and served from (GET /v1/records/daily/{date}); unset, no bundle is built and /readyz says so |
| `USSP_WEATHER_SOURCE` | `api` |  |  |  | weather source as awc:<base URL> (the NOAA Aviation Weather Center data API format: <base>/metar and <base>/taf with ids= and format=json, for the policy's weather_station_ids); unset means GET /v1/weather answers 503 weather_unavailable (reason not_configured) and every decision carries weather_unavailable; a malformed value refuses the start |
| `USSP_ADSB_SOURCE` | `monitor` |  |  |  | e-conspicuity receiver feed: http(s)://<host>/data/aircraft.json (readsb/dump1090, polled at 1 Hz), sbs://<host>:<port> (BaseStation lines) or file://<path>.jsonl (a recorded aircraft.json replay, timestamps re-based); unset means no receiver, shown as such |
| `USSP_ADSB_RECEIVER_ID` | `monitor` |  | `adsb-rx-1` |  | the e-conspicuity receiver's id: source_instance of its tracks and the instance of its adsb_rx source switch |
| `USSP_TRAFFIC_INPUT_BBOX` | `monitor` |  |  |  | box of the manned and peer inputs as min_lng,min_lat,max_lng,max_lat in WGS84 degrees: the ANSP stream's bbox and the peer Display Provider's area; empty is every U-space airspace of cis_current, padded |
| `USSP_WS_ALLOWED_ORIGINS` | `telemetry-ingest,traffic-ws` |  |  |  | Origin allow-list of browser WebSocket upgrades (M22), comma-separated |
| `USSP_READINESS_CHECK_TIMEOUT_MS` | `all` |  | `2000` | ms | bound on one dependency check of /readyz |

## web (the portal's image)

The Next.js server of `web/` reads its own variables at request time;
they are documented with their defaults in `web/README.md`
(Configuration): `USSP_WEB_API_URL`, `USSP_WEB_SESSION_SECURE`,
`USSP_WEB_TRUSTED_PROXY_HOPS` (required with a secure session),
`USSP_WEB_BFF_TIMEOUT_MS`, `USSP_WEB_MAP_CENTER`, `USSP_WEB_MAP_ZOOM`,
`USSP_WEB_BFF_SECRET` (at least 32 bytes from the secret store: it seals
a staff admin's MFA challenge between the console's two sign-in steps;
without it an admin cannot sign in to the console) and the kit's
`UI_BRAND_*`. List the web container in the API's
`USSP_TRUSTED_PROXIES`, so the sign-in limits key on the client the BFF
names. The browser reaches traffic-ws's `WS /v1/traffic` and
`WS /v1/alerts` on the same origin, so its origin belongs in
`USSP_WS_ALLOWED_ORIGINS`, and traffic-ws lists this USSP's own issuer
(with api's JWKS) in `USSP_TOKEN_ISSUERS` to verify portal sessions.
