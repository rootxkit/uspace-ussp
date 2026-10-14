# Deploying uspace-ussp to staging (WP-20)

**The deploy is the owner's act.** An agent writes these files and this
runbook and never runs a deploy, a backup or a teardown against a server.

The staging droplet runs this USSP as compose project `uspace-ussp`,
composed by the private deployment repository `uspace-deploy` (deploy
WP-D1) behind the one shared Caddy. This repository holds the reference
shape that deployment derives from and the scripts that check it:

| File | What |
|---|---|
| `deploy/compose/staging.yaml` | the staging and production shape (header: every rule it keeps) |
| `deploy/compose/deploy.env.example` | what a deployment gives the compose file: images by digest, hosts, directories, the edge network and its two trusted addresses |
| `deploy/compose/ussp.env.example` | the operator's settings: every `USSP_*` variable of `deploy/ENV.md`, set or commented out with its default |
| `deploy/compose/nats.staging.conf` | NATS: one user per process, a read-only `backup` user, store bounds from the environment |
| `deploy/staging/hosts.env`, `peers.yaml` | the staging hosts (the only file naming the domain) and the overlay that resolves every peer to the edge Caddy |
| `deploy/caddy/ussp.caddy`, `proof.sh` | the Caddy site uspace-deploy imports, and its proof against the pinned Caddy |
| `deploy/gen-secrets.sh` | the secrets directory, never printed, never overwritten |
| `deploy/verify-image.sh` | cosign signature and SPDX attestation of both images, by digest |
| `deploy/deploy.sh` | preflight, verify, pull, migrate, rolling start, verification |
| `deploy/teardown.sh` | removes a project and proves nothing is left |
| `deploy/backup.sh`, `restore-check.sh` | nightly backup and its restore check ([backup.md](backup.md)) |
| `deploy/smoke/` | the staging smoke CI runs on every change (below) |

Rollback: [rollback.md](rollback.md). Backups: [backup.md](backup.md).

## 1. Images

CI's `image` job pushes both images on `main` and on `v*` tags, tagged by
short SHA (and the tag), signs each digest with cosign keyless and reads
the signature back; `sbom` and `attest` attach the SPDX SBOM of each as a
signed attestation and verify it. The digests are in the job summary.
Deploy by digest only:

```
deploy/verify-image.sh ghcr.io/rootxkit/uspace-ussp@sha256:<go> ghcr.io/rootxkit/uspace-ussp-web@sha256:<web>
```

`verify-image.sh` refuses a tag, an image of another repository, a
signature from another workflow or ref, and an image without an SPDX
attestation. `deploy.sh` runs it before it pulls anything.

## 2. Secrets

On the host, once:

```
deploy/gen-secrets.sh --owner 65532 /srv/uspace-ussp/secrets
```

It writes `db.env`, `nats.env`, one `<process>.env` per process (its own
NATS user and only the DSNs it opens), `migrate.env`,
`migrate-timeseries.env`, `backup.env`, `web.env` and `keys/issuer.pem`,
`keys/mfa.key`, and `keys/jwks-cache-<process>.key` for api,
telemetry-ingest, rid-sp and traffic-ws (each process's own HMAC key for
the JWKS it stores in `jwks_cache`, mounted into it alone); `--check`
lists what is missing, and `deploy.sh` refuses to start without it. The
layout is uspace-deploy's, so either tool's directory serves the compose
file, once uspace-deploy's `scripts/gen-secrets.sh` writes the four
`jwks-cache` keys too (until then, run this one on its directory: it
writes only what is missing).
Every NATS password starts with a letter: NATS reads `$VARIABLE` as a
value, and the smoke's first start failed on a hex password beginning
with a digit ("variable reference for 'USSP_NATS_TSDB_WRITER_PASSWORD'
... could not be parsed"). uspace-deploy's `gen-secrets.sh` also writes
plain hex NATS passwords; whether its droplet has met this was not
checked here.

## 3. The authority's certificate

The authority registers this USSP's client `ussp-<code>-01` when it
issues the USSP's certificate (`POST /v1/certificates`, holder `ussp`,
code = `USSP_SYSTEM_ID`) and shows the client secret once, in its answer.
uspace-deploy's `scripts/ussp-certificate.sh ANSWER.json` copies the
answer into the secrets directory:

- `certificate.env`: `USSP_CERTIFICATE_ID=<the authority's 32 hex>`, read by
  api (Art. 7(6) operating-status notices);
- `keys/client.secret`: the client secret (client_secret_post only).

Then uncomment `USSP_TOKEN_CLIENT_SECRET_FILE=/run/secrets/ussp/client.secret`
in the settings. `deploy.sh` checks the wiring before anything starts:

| State | deploy.sh |
|---|---|
| `certificate.env` with one line `USSP_CERTIFICATE_ID=<32 hex>` | names the id api gets |
| `certificate.env` in another form | refused |
| no `certificate.env` | refused, naming the script; `--without-certificate` deploys anyway and says no notice can be sent |
| `keys/client.secret` and the settings line | names `ussp-<code>-01` and the file |
| the file without the line | refused: no outgoing call would use it |
| the line without the file | refused: the four callers would not start |

## 4. Settings (pending GCAA)

Copy `deploy/compose/ussp.env.example` outside the checkout (mode 600)
and set `USSP_SYSTEM_ID` to the code the authority assigned. GCAA has not
answered its policy questions (PLAN §15.2): every value marked
"pending GCAA" is the spec's default, kept in configuration so an answer
is a change of this file or of the policy row, never of code
(`USSP_OCCURRENCE_MAX_ATTEMPTS` 50, `USSP_OCCURRENCE_BACKOFF_MAX_S` 600,
`USSP_BACKUP_KEEP_DAYS` 14, `USSP_BACKUP_TS_DAYS` 7, the weather source).
`USSP_MTLS_MODE=off` on staging: each process logs it at error level every
status period (M25). The settings may not hold a DSN, password, secret,
`USSP_CERTIFICATE_ID` or `USSP_TRUSTED_PROXIES`: `deploy.sh` refuses them.

## 5. Deploy

```
deploy/deploy.sh --env-file /srv/uspace-ussp/deploy.env --env-file deploy/staging/hosts.env \
  -f deploy/staging/peers.yaml
```

In order, each step bounded and each failure named: the preflight
(render, digests, one Go image, complete secrets, settings without
secrets, the code's form, the geoid grid, the certificate wiring);
signatures and attestations; pull by digest; the database and NATS
healthy; `migrate` and `migrate-timeseries`, whose exit status
(`docker wait`) and `migrate done` line are read, and which must leave
each tree at the latest version the image knows; the processes one at a
time (tsdb-writer, api, telemetry-ingest, rid-sp, monitor, traffic-ws,
dss-sync, web), each healthy before the next; then every process's
`/healthz`, `/readyz` with every dependency and its detail, and the
version of its `started` line (`USSP_EXPECT_VERSION` to require one). A
required dependency that is not up fails the deploy. An optional one is
printed `DEGRADED` and fails only with `--strict`; `--accept
<process>:<dependency>` takes a named one out and prints it as
`ACCEPTED`. `--verify-only` changes nothing.

Reading the migrate step: `migrate exited 0: version 26 -> 27 (latest 27)`
is an upgrade; `27 -> 27` changed nothing. A process started against a
tree older than it needs refuses to start after `USSP_SCHEMA_WAIT_S`
(60 s) and says which, for example `relational schema is at version 26,
this build needs 27: run ussp-api migrate` (the smoke makes this happen
on both trees).

### First deploy: what is not up yet

A fresh stack reports these until the owner acts, and nothing else (the
smoke accepts exactly these by name and fails without them):

| Dependency | Why | What makes it up |
|---|---|---|
| `api:operating_status` | no Art. 7(6) start notice sent yet | a staff admin confirms the start of operations on the console (`/v1/admin/status`), which sends the `start` notice to the authority |
| `<process>:policy` | no policy version stored; the defaults apply (pending GCAA) | an admin publishes the first policy version from the console (`PUT /v1/admin/policy`) |
| `monitor:adsb_rx` | no e-conspicuity receiver configured (`USSP_ADSB_SOURCE`, §15.2 Q11) | a receiver feed |
| `monitor:terrain` | no pinned terrain source yet (`USSP_TERRAIN_DIR`) | a terrain release |

Then the smoke checks of the brief: the verification above, and one
simulated operator from the lab (`sim-operator`) or the smoke's driver
filing an intent through the public host.

## 6. The staging smoke (CI, and a laptop)

```
make staging-smoke        # both images from this checkout, then deploy/smoke/run.sh
```

`deploy/smoke/run.sh` deploys `staging.yaml` from scratch with
`deploy.sh --unsigned-local-images --strict` behind a local Caddy that
serves `deploy/caddy/ussp.caddy`, with fakes of the authority, CISP,
ANSP and DSS over https from a throwaway root, and the certificate files
in `ussp-certificate.sh`'s format. It proves, in order: the deploy with
only the four dependencies above accepted, and its failure without them;
a simulated operator through the public host (registration, sign-in,
client, serial, token, an intent authorised and activated, telemetry
over `WS /v1/telemetry` until every sample is acknowledged); a backup and
its restore check; the stack removed (volumes kept), redeployed and the
intent read back; one migration of each tree rolled back and refused by
name; memory per container; the teardown verified.

Run on 2026-10-13 (Windows 11, Docker Desktop 28.5.1, images built from
this branch): every step passed. Memory after the smoke (`docker stats`):

| container | MiB | limit |
|---|---|---|
| timescaledb | 71 | 512 |
| web | 43 | 224 |
| api | 35 | 256 |
| nats | 17 | 128 |
| caddy (smoke only) | 13 | 128 |
| monitor | 12 | 256 |
| telemetry-ingest | 10 | 224 |
| tsdb-writer | 10 | 128 |
| traffic-ws | 8 | 96 |
| rid-sp | 7 | 96 |
| dss-sync | 4 | 64 |
| **total** | **232** | **2112 with the one-shots** |

Not measured: a 2 vCPU / 3.8 GB VM at idle and with 100 simulated
drones (the lab's load profile). The figures above are one idle sample
on a laptop after a single operator's flight.

## 7. Teardown

```
deploy/teardown.sh --project uspace-ussp            # containers and networks; volumes kept and listed
deploy/teardown.sh --project uspace-ussp --volumes  # also the databases, JetStream and the records: take a backup first
```

It exits 0 only after it has listed what is left of the project and
found nothing (without `--volumes`, the volumes are expected and named).
