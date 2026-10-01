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
| `USSP_NATS_URL` | `all` | `all` |  |  | NATS JetStream; the process reconnects forever and starts degraded when it is down |
| `USSP_NATS_CREDS` | `all` |  |  |  | path of the NATS credentials file of this process; empty uses the URL's userinfo |
| `USSP_SYSTEM_ID` | `api,rid-sp,dss-sync` |  | `USSP-DEV` |  | the USSP code from the authority's certificate (M8); never an audience |
| `USSP_AUDIENCES` | `api,telemetry-ingest,rid-sp,traffic-ws` |  |  |  | hosts accepted as JWT aud, comma-separated: the public host and a lab alias (M18) |
| `USSP_TOKEN_ISSUERS` | `api,telemetry-ingest,rid-sp,traffic-ws` |  |  |  | allow-listed token issuers as iss=jwks_url, comma-separated; the first is the token service for outgoing calls |
| `USSP_CIS_NOTIFY_ISSUERS` | `api` |  |  |  | issuers of CIS change notifications (the CISP, the ANSP) as iss=jwks_url, comma-separated |
| `USSP_USS_BASE_URL` | `api,rid-sp,dss-sync` |  |  |  | this USSP's published base URL (uss_base_url in the DSS) |
| `USSP_DSS_BASE_URL` | `api,rid-sp,dss-sync` |  |  |  | InterUSS DSS base URL; its host is the outgoing aud |
| `USSP_CISP_BASE_URL` | `api` |  |  |  | CISP base URL (F3 pull) |
| `USSP_AUTHORITY_BASE_URL` | `api` |  |  |  | authority base URL (F8 registry, occurrences, status) |
| `USSP_ANSP_BASE_URL` | `api` |  |  |  | ANSP base URL (Annex V coordination notices) |
| `USSP_ANSP_STREAM_URL` | `monitor` |  |  |  | ANSP manned-traffic stream (F4) |
| `USSP_MTLS_MODE` | `monitor` |  | `required` |  | mTLS towards the ANSP (M25); off only in the lab and on staging, and logged at error level |
| `USSP_MTLS_CERT_FILE` | `monitor` |  |  |  | client certificate (PEM) for USSP_MTLS_MODE=required |
| `USSP_MTLS_KEY_FILE` | `monitor` |  |  |  | client key (PEM) for USSP_MTLS_MODE=required |
| `USSP_MTLS_CA_FILE` | `monitor` |  |  |  | CA bundle (PEM) the ANSP's certificate is checked against |
| `USSP_ISSUER_KEY_FILE` | `api` |  |  |  | RSA key (PEM) of this USSP's own token issuer |
| `USSP_GEOID_FILE` | `telemetry-ingest,monitor` |  |  |  | geoid grid file for AMSL |
| `USSP_TERRAIN_DIR` | `monitor` |  |  |  | directory of terrain tiles |
| `USSP_CELL_OWNERSHIP` | `monitor` |  | `all` |  | cells this monitor instance owns: all, or a comma list of c3 cells |
| `USSP_AUTHORITY_PUSH` | `rid-sp` |  | `off` |  | the optional WS /v1/authority/flights extension (D12) |
| `USSP_WEATHER_SOURCE` | `api` |  |  |  | weather source adapter and URL; unset means weather answers 503 weather_unavailable |
| `USSP_ADSB_SOURCE` | `monitor` |  |  |  | e-conspicuity receiver feed; unset means no receiver, shown as such |
| `USSP_WS_ALLOWED_ORIGINS` | `telemetry-ingest,traffic-ws` |  |  |  | Origin allow-list of browser WebSocket upgrades (M22), comma-separated |
| `USSP_READINESS_CHECK_TIMEOUT_MS` | `all` |  | `2000` | ms | bound on one dependency check of /readyz |
