# Backlog

Ranked table of open items. Source is not recorded in the list this was built from.

| # | Severity (high/medium/low) | Item | Where (file:line if given) | Source (the review that found it) |
|---|---|---|---|---|
| 1 | medium | A restart load can skip in-force direct rows if a store delete fails. | - | not recorded |
| 2 | medium | Held telemetry is unrecoverable from backup after telemetry_retention_days + 14 days; TimescaleDB drops held telemetry after 90 days. | - | not recorded |
| 3 | medium | Flaky test TestANSPStreamAlarmsOnAnOversizeFrame. | - | not recorded |
| 4 | medium | uss_qualifier: F3411-SP fails 105 of 1555 checks and F3548-SCD 10 of 75, R6–R8 pending. Spec gaps: telemetry/v1 has no UA type or speed accuracy; the registration id needs an intent; there are no operator notifications. The F3411-DP role belongs to the authority (spec 02). | - | not recorded |
| 5 | low | deploy/backup.sh:100: an exact-match Destination refuses a USSP_RECORDS_DIR that is a subdirectory of the mounted volume. | deploy/backup.sh:100 (moved: the Destination match is now at deploy/backup.sh:128-132) | not recorded |
| 6 | low | .gitleaksignore:17: the fingerprint is keyed on a commit, so a squash merge would resurrect it. | location changed (.gitleaksignore:17 is now a comment; commit-keyed fingerprints are at lines 7, 12, 15, 20) | not recorded |
| 7 | low | internal/app/monitor/inputs.go:479: the "down" branch is unreachable in practice. | location changed (internal/app/monitor/inputs.go exists but has 455 lines; line 479 is gone) | not recorded |
| 8 | low | internal/testif/annex.go:150: document the identification mapping. | internal/testif/annex.go:150 | not recorded |
| 9 | low | Operations: no terrain or ADS-B source is configured on the demo; "start of operations" needs a supervisor's confirmation. | - | not recorded |
