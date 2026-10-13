# Chaos runs: spec 05 §6 failure domains (WP-19, S-M6)

What happens to this USSP when one failure domain of spec `05 §6` goes
down for a while and comes back, observed on the conformance stack
with traffic flying. Each row is recorded with what was observed, not
what was expected (E-04); a behaviour that differs from the failure
columns of spec `02` and `05 §6` is a finding, listed at the end with
where it is fixed or tracked. The lab owns the cross-system chaos runs
(its WP-L9); this is the USSP's procedure and its record.

## Procedure

1. The stack with every system: `CONFORMANCE_SYSTEMS=all
   CONFORMANCE_KEEP=1 make conformance` (`deploy/conformance/README.md`;
   the ANSP image is the one the lab's `deploy/demo-up.sh` builds). It
   leaves compose project `uspace-ussp-conf` running.
2. The lab's seed against it, from a uspace-lab checkout, with the
   stack's env file (`local/conformance/state/stack.env`): `go run
   ./cmd/demo-seed --env <stack.env> --lab sim/sitl.env.example --steps
   registry,uspace,ussp --uspace-north-m 60000
   scenarios/ussp-wp10-conformance.yaml`, then `--steps sessions` with
   `--geoid`, `--targets`, `--secrets` (the lab's `docs/RUNBOOKS/demo.md`
   §2). The U-space airspace is moved 60 km north so the scenario's
   intents are authorised without the DSS deconfliction of the airspace.
3. Traffic: the lab's scenario runner with synthetic vehicles, `scenario
   run --targets <targets> --vehicles synthetic
   scenarios/ussp-wp10-conformance.yaml` (two operators, two intents,
   seven and a half minutes of hovering, excursions, a climb above the
   authorised upper and a link cut, which raise and clear
   `nonconformance`, `nonconformance_nearby`, `proximity` and
   `lost_link`). On Windows it runs as a Linux binary in a container
   that reaches the published lab Caddy (`--add-host <host>:host-gateway`,
   `SSL_CERT_FILE` the stack's CA). Between two runs, 140 s (the USSP
   ends a silent flight after `flight_end_after_s`).
4. One row per run, 60 s after the runner started:
   `deploy/conformance/chaos.sh <row>` takes the domain down
   (`CHAOS_DOWN_S`, 300 s, NATS 60 s), samples every USSP process's
   `/readyz` and its counters of interest 30 s in and at the end, brings
   the domain back (`--no-deps`, nothing else is recreated), waits for
   it to be healthy and samples again. `kill` is the restart row:
   every USSP process in turn gets SIGKILL and is started again, 30 s
   apart. The record is `local/conformance/chaos/<UTC>-<row>/`.
5. After each run, the duplicate query on the record: two alert rows of
   one kind, flight and peer whose active intervals overlap. It counts
   pairs, and `nonconformance_nearby` legitimately has several active
   per flight (one per source), so a new pair is read row by row.
6. `make conformance-down` removes the stack and checks nothing is
   left.

## Runs of 2026-10-05

Stack: uspace-lab `7fb2d45` (deploy/conformance/SOURCE), the lab's
images of `demo.env.example` for the authority, the CISP and the ANSP,
this branch's USSP image as named per pass, Windows 11, Docker Desktop
28.5.1 (7.7 GiB, shared with other projects). Policy: the defaults
(no policy row was written on the fresh stack; every process says
"not read yet: the defaults apply"). The baseline, nothing taken down:
`ussp-wp10-conformance` PASS, 0 missed, 0 false, both ledgers exact
(413 and 438 samples), 0 overlapping alert pairs.

Pass 1 ran on `aa8ef18` (image `uspace-ussp:conformance-aa8ef18309f6`,
`sha256:5bd4ebb5075c...`, with the verifier fix), pass 2 on `66c7346`
(`sha256:a4eb40b1d3af...`, the flight binding fix), pass 3 on `8f9dad7`
(`sha256:d0643ce3550c...`, both restart fixes); local image ids, built
from those commits with deploy/Dockerfile. The stack's memory, one `docker stats`
sample of its running containers after the runs: about 1.4 GiB.

| Row | Pass | Scenario | What the USSP said (sampled `/readyz`) | Matches `05 §6`? |
|---|---|---|---|---|
| restart (kill each process) | 1 | FAIL: 0 missed, 6 false | every process healthy again 8 to 9 s after SIGKILL. After telemetry-ingest's restart each aircraft got a **new flight id**; the old flights raised `lost_link` 15 s later and held it 5 min until ended; each new flight was told `nonconformance_nearby` about its own aircraft's old flight at 0 m; 8 overlapping alert pairs | **no**: finding F1 |
| restart | 2 | FAIL: 0 missed, 2 false | `running flights restored 2`: both aircraft kept their flight ids; **no new overlapping alert pair**. Each operator's ledger lost 100 accepted samples (sent 413, accepted 313) although every frame was acknowledged | **no**: finding F2 |
| restart | 3 | **PASS: 0 missed, 0 false**, both ledgers exact (413, 438) | every process healthy again 7 to 8 s after SIGKILL (the health check's own period included); telemetry-ingest restored both flights, the record holds two flights for the two aircraft and no new overlapping alert pair; the alerts raised and cleared inside their windows | yes |
| `api` down 300 s | 1 | PASS: 0 missed, 0 false | api not answering; telemetry-ingest and traffic-ws `jwks` degraded for this USSP's own issuer ("cached, age 331 s"); telemetry, conformance, alerts and traffic went on; operator ledgers exact but one dropped sample; back after 10 s | yes |
| NATS down 60 s | 1 | FAIL: 2 missed, 3 false | every process answered no `/readyz` body during the outage (the pass 1 sampler lost non-2xx bodies; pass 3 records the status); back after 6 s; ledgers exact (the operators' frames were acknowledged after the outage). The monitor **raised `lost_link` for both flights during the outage** (silence of its own input), published when NATS came back; the excursions flown during the outage raised nothing (their samples arrived as backlog, which is recorded and never alerted, T-04) | lost_link: **no**, finding F3; the missed alerts follow the backlog rule |
| NATS down 60 s | 3 | FAIL: 2 missed, 3 false (as pass 1) | 34 s in and at 60 s every one of the seven processes answered `/readyz` 503 (NATS is required everywhere); back after 5 s, ready 32 s later; ledgers exact. Again `lost_link` for both flights, raised during the outage and published when NATS came back, and the backlog of the excursions not alerted | lost_link: **no** (F3) |
| TimescaleDB down 300 s (both databases, M37) | 1 | PASS | api not ready (PostgreSQL is its required store); tsdb-writer `timescaledb` down ("failed to connect"); alerts raised and cleared on time. After the restore the record holds the four alerts raised during the outage and 624 telemetry rows captured during it (two aircraft, 5 min) | yes |
| CISP down 300 s | 1 | PASS | 33 s in: nothing degraded (the CIS is within its stale bound); at 339 s: api `cis` degraded "age 339 s > 300 s", monitor `cis_current` "stale: age 339 s"; flights went on | yes for in-flight services. A new U-space authorisation after 300 s was not filed: the `cis_stale` refusal is **not exercised** here (WP-7's integration test holds it) |
| authority down 300 s | 1 | PASS | api `registry` degraded with its age; `jwks` degraded per issuer on four processes: "https://authority.uspace.test: cached, age 312 s (unreachable)", tokens of the lab issuer still verified (the WP-19 verifier fix); flights went on | yes |
| DSS down 300 s | 2 | FAIL: 0 missed, 0 false | api `dss` "f3411: down since T: answers 502 \| f3548: F3548 DSS down since T", first said 53 s after the stop and still said 31 s after the restore; monitor `network_rid` stale; every flight and alert went on locally. The runner did not see one `lost_link` clear that the record holds (cleared `resolved` at 05:33:43, the first second of its window) | yes; the runner's miss is the lab's observation, cause not determined |
| ANSP down 300 s | 2 | PASS | monitor `ansp_feed` "down: unavailable since T" (2 s after the stop: the stream is not connected); flights and alerts went on; back after 2 s | yes |
| token service (lab issuer) down 300 s | 2 | PASS | `jwks` degraded on api, telemetry-ingest, rid-sp, traffic-ws: "https://lab.uspace.test: cached, age 311 s (unreachable)"; outgoing calls kept their cached tokens; flights and alerts went on | yes |

Pass 1's DSS, ANSP and token service rows are not counted: the DSS
row's restore let compose recreate the lab issuer (a dependency)
without the user it runs as, which then could not read its key, so the
issuer was down from the DSS row on. `chaos.sh` now restores with
`--no-deps` and the compose passthrough exports the issuer's user; the
three rows were run again in pass 2 on a repaired stack. What pass 1
showed while the issuer was down anyway: the USSP kept verifying from
its cached JWKS and named the issuer "cached" on `/readyz` for 15
minutes.

## Findings

| # | What was observed | Spec | Status |
|---|---|---|---|
| F1 | A telemetry-ingest restart started a new flight for every aircraft in the air; the old flights raised `lost_link` and kept it; a flight was told `nonconformance_nearby` about its own aircraft (record above, pass 1) | `05 §6` "each process killed in turn: no duplicate alerts" | fixed in this branch: `fix(flights): keep a running flight across an ingest restart` (the `flight_binding` bucket); pass 2 and 3 |
| F2 | The first telemetry connection after a restart carried the connection id of the first one before it (`telemetry-1`), and the lab's client lost the counters of the old connection (pass 2) | `02 F5` status frame, KT-4 ledger | fixed in this branch: `fix(telemetry): name a connection once across restarts`; pass 3 |
| F3 | During a NATS outage the monitor judged the silence of its own input as the aircraft's: `lost_link` for every flight, false. `05 §6` says consumers hold their last state | `05 §6` NATS row | **open**, not fixed here: a change to the lost-link judgement of `internal/conformance` (WP-10, safety-relevant) needs its own reviewed PR and lab run (PLAN §15.2 Q35) |
| F4 | A process restarted while a token service is down cannot verify that service's tokens until it answers: the JWKS cache is in memory. Observed after pass 3: lab issuer stopped at 06:16:10, rid-sp restarted, healthy at 06:16:21 with `jwks` "https://lab.uspace.test: never fetched (unreachable)" (its tokens, the peers' F3411 calls among them, answered 503); the issuer back at 06:16:47, rid-sp's `jwks` up at 06:16:45-06:17:00 (the 15 s retry) | `05 §6` token service "JWKS cached 24 h" | **open** (PLAN §15.2 Q35): a JWKS cache that survives a restart is a change to every process's verifier start |
| — | The lab runner's alert stream did not come back after traffic-ws was killed ("upgrade refused with 502" while it restarted), so its later observations of the restart run are partial | — | the lab's (runner reconnect); recorded, the record in the database is the evidence |

No finding was filed as an issue: the orchestration of this work package
posts nothing on GitHub; the findings are here and in the PR.
