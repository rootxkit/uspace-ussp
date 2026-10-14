# Backups of uspace-ussp (WP-20)

**Backups, restores and their schedule are the owner's acts.** A backup
that reports success must have counted what it wrote, and a restore that
was never rehearsed is not a backup (E-02).

## Nightly

On the host, as the deploy user, the backup and then its check:

```
30 2 * * *  cd /srv/uspace-ussp && USSP_SECRETS_DIR=/srv/uspace-ussp/secrets \
              deploy/backup.sh --settings /srv/uspace-ussp/ussp.env /var/backups/uspace-ussp \
              && deploy/restore-check.sh /var/backups/uspace-ussp
```

`deploy/backup.sh` writes under one UTC stamp:

| File | What |
|---|---|
| `ussp_relational-<stamp>.dump` | the relational database whole, `pg_dump -Fc` |
| `ussp_timeseries-<stamp>.dump` | the time series with the rows of the chunks of the last `USSP_BACKUP_TS_DAYS` days (7); older chunks and their compressed relations keep their definitions only. "The last 7 days" is the database's clock (`now()` in the container), never the host's |
| `ussp_timeseries_held-<stamp>.sql` | the rows the dump leaves out that belong to a held flight, whatever their age: every flight an occurrence report names (`occurrence_reports.flight_ids`, the source of `record_holds`). Its telemetry and conformance samples, and the traffic products its client was shown while it flew, read from each chunk before the cutoff, one `COPY` per chunk. A hypertable with a `flight_id` that the export does not read stops the backup before it writes anything |
| `records-<stamp>.tar` | every daily record bundle `record_bundles` names, whatever its age, from the `records` volume (api's `USSP_RECORDS_DIR`), read back file by file against its recorded SHA-256 |
| `nats-kv-<stamp>.jsonl` | every JetStream KV bucket, one JSON line per key, read as the `backup` NATS user, which may read the `KV_<bucket>` streams only and may not write, read another stream or delete a consumer (`nats.staging.conf`) |
| `manifest-<stamp>.json` | the database clock, the cutoff, each tree's schema version, each file's bytes, SHA-256 and entries, the held flights and their rows per chunk, the bundles by name and SHA-256, the KV export's keys, buckets and buckets with keys; written last, so a stamp without it is incomplete |

Each file is written under a `.partial` name and read back before it is
kept (a dump must list with `pg_restore --list`; the KV export must hold
one valid line per key it listed and must have read at least one bucket:
a running USSP always has them). Then stamps older than
`USSP_BACKUP_KEEP_DAYS` (14) are removed. Both figures are national
choices (spec 05 §4), **pending GCAA**, and are settings.

Off the host: `USSP_BACKUP_REMOTE` (an rclone remote and path) copies
each file and compares the copy's size. Which account holds the copies
is the owner's open question; without it the run says the files stayed on
the host.

The JetStream streams are not backed up: each is bounded by age and what
they carry is in the databases.

## The restore check

`deploy/restore-check.sh <dir>` takes the newest stamp with a manifest,
checks every file's bytes and SHA-256 against it, restores both databases
into `ussp_relational_restore_check` and `ussp_timeseries_restore_check`
in the running container (the time series through
`timescaledb_pre_restore()` / `post_restore()`, then the held rows with
`psql`), and prints what came back: audit events, operators, intents,
telemetry rows (the held ones among them), hypertables, both schema
versions, the held flights and their rows, the record bundles, KV keys.
Before any database is touched it checks that the records archive holds
exactly the bundles the manifest names, each with its SHA-256; after the
restore, that every chunk holds the held rows the backup read from it
and that every bundle the restored `record_bundles` names is in the
archive. A manifest without the held rows and the bundles (from before
this check) is refused. It fails on a file that differs, a failed
restore, a schema version other than the manifest's, a KV export with
another number of keys or of buckets holding keys than the manifest's
(`kv.keys`, `kv.buckets_with_keys`; a manifest without the latter, from
before that check, is refused), a backup that exported another number
of buckets than the topology has (`kv.buckets` against
`deploy/nats-kv-buckets.txt`, which `TestKVBucketListIsTheTopologys` holds
to `internal/bus`) or a key of a bucket it does not name, a KV value that
does not decode (the KV checks run before any database is touched), or
counts other than `RESTORE_CHECK_EXPECT`. The scratch
databases are dropped afterwards, also on failure, and their absence is
checked. They take about the databases' size again, for the length of the
check.

## Rehearsed (2026-10-13, the staging smoke, Windows 11 / Docker Desktop)

After one operator's registration, intent and flight, with one telemetry
row written 10 days back by the database's clock, the writers paused:

```
smoke: live counts with the writers paused: events=16,operator_accounts=1,operational_intents=1,telemetry=5 (telemetry 6 rows in all, 5 in the window)
backup: ussp_relational-20261013T165046Z.dump (157570 bytes, 382 entries)
backup: ussp_timeseries-20261013T165046Z.dump (43487 bytes, 112 entries)
backup: time series since 2026-10-06T16:50:47Z (database clock 2026-10-13T16:50:46Z): 3 chunks, the rows of 1 relations before the cutoff left out
backup: nats-kv-20261013T165046Z.jsonl (17 keys in 15 buckets)
backup: manifest-20261013T165046Z.json (schema relational 27, timeseries 4)
smoke: the backup user reads the buckets and its put is refused (Permissions Violation)
restore-check: 3 files match the manifest (bytes, SHA-256)
restore-check: restored 20261013T165046Z: events=16 operator_accounts=1 operational_intents=1 telemetry=5 hypertables=7 schema relational=27 timeseries=4 nats_kv_keys=17 nats_kv_buckets=11
restore-check: the counts equal the live databases' at the dump (events=16,operator_accounts=1,operational_intents=1,telemetry=5)
smoke: restore-check refuses counts that differ (exit 1)
smoke: restore-check refuses a dump that differs from its manifest (exit 1)
```

The old row is the one the restore does not give back (6 live, 5
restored). The chunk exclusion was also checked by hand on the pinned
`timescaledb-ha:pg16` (TimescaleDB 2.30.2) with a compressed chunk 10 days
back: 151 rows live, 1 restored, all 3 chunk definitions present. CI runs
the same smoke on every change.

## Restoring for real

1. Stop the processes: `docker compose -p uspace-ussp ... stop api telemetry-ingest rid-sp monitor traffic-ws dss-sync tsdb-writer web`.
2. Restore the relational dump into a new database and swap names, or
   into `ussp_relational` after dropping it, with the server's own
   `pg_restore --exit-on-error`; the time series as `restore-check.sh`
   does (pre-restore, `pg_restore`, post-restore, then
   `ussp_timeseries_held-<stamp>.sql` with `psql -v ON_ERROR_STOP=1`:
   the held flights' rows before the window, which the dump does not
   hold). The roles (`ussp_api`, `ussp_tsdb`, `ussp_app`) exist from
   `initdb/10-ussp.sql`. The record bundles: extract `records-<stamp>.tar`
   into the `records` volume (api's `USSP_RECORDS_DIR`); api serves a
   bundle only when its bytes still match `record_bundles`.
3. KV: `record_holds` does not hold a flight's records: it is api's
   projection of the occurrence reports, and the rows it protects come
   back from the held file in step 2, not from the KV export. api writes
   it again from the restored `occurrence_reports` at its start and every
   10 minutes, but tsdb-writer's retention, which may run first, removes
   the remote pilot's position from the old telemetry of every flight it
   does not name. So put `record_holds` back from the export with
   `nats kv put` as a process user before any process starts, or start
   api first and wait until the bucket holds a key per held flight.
   Other buckets: put keys back only where the bucket has no other
   source. Which buckets the processes refill by themselves after a cold
   start has not been rehearsed here, so decide per bucket from its
   description in `internal/bus/topology.go`. A night-old copy of live
   alert state (`conformance_state`, `proximity_state`) would restart
   alerts as they were last night, and is not put back.
4. Deploy (`deploy/deploy.sh`): migrate brings the schema to the image's
   version, and the verification reads every process.
