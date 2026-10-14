# Rolling uspace-ussp back (WP-20)

**The rollback is the owner's act**, like the deploy ([deploy.md](deploy.md)).

## The rule that makes a rollback possible

A release never drops or renames a column, table or KV key that the
previous release reads, and never makes a column the previous release
writes `NOT NULL` without a default. A change that needs it is split over
two releases: the first stops reading or writing, the second drops. Every
migration has a `down` that undoes it, and the staging smoke runs one
`down` of each tree against a deployed stack on every change, then
migrates forward again (`deploy/smoke/run.sh`, step 8).

## Steps

1. Find the previous digests: the earlier copy of the deployment's images
   file (uspace-deploy `compose/images.env`), or the `image` job summary of
   the commit to return to.
2. Back up first (`deploy/backup.sh`, [backup.md](backup.md)) and check it
   (`deploy/restore-check.sh`).
3. If the release being left applied migrations the previous one does not
   know, roll them back with the **newer** image, which holds their `down`:

   ```
   docker compose -p uspace-ussp ... run --rm --no-deps migrate down <version>
   docker compose -p uspace-ussp ... run --rm --no-deps migrate-timeseries down <version>
   ```

   `<version>` is the latest version the previous image knows (its
   `migrate done` line says `latest`). Each run logs `migration rolled back`
   per step and `migrate done` with `from` and `to`.
4. Deploy the previous digests with `deploy/deploy.sh`. Its migrate step
   must say `version N -> N (latest N)`; a `to` above `latest` means step 3
   was skipped, and the processes of the older image would run against a
   schema they do not know.
5. Read the verification: every process's version is the previous one
   (`USSP_EXPECT_VERSION`), every `/readyz` as before.

The data written by the newer release stays in the database as far as the
older schema holds it; nothing is restored from a backup in a rollback
unless the release corrupted data, which is a restore ([backup.md](backup.md)),
not a rollback.
