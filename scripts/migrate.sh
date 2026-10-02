#!/usr/bin/env bash
# Runs a migrate subcommand against one database URL:
#
#   scripts/migrate.sh relational up|down [version]|status <postgres-url>
#   scripts/migrate.sh timeseries up|down [version]|status <postgres-url>
#
# relational is `ussp-api migrate` (USSP_PG_URL, as the owner ussp_api);
# timeseries is `ussp-tsdb-writer migrate` (USSP_TS_URL, as ussp_tsdb).
# The subcommand is the only code path that migrates (docs/PLAN.md D5);
# this script only hands it the URL. The URL is passed through the
# environment, never on a command line another process can read.
set -euo pipefail

usage() { echo "usage: $0 relational|timeseries up|down [version]|status <postgres-url>" >&2; exit 2; }
[ $# -ge 3 ] || usage
tree=$1; shift
url=${!#}
args=("${@:1:$#-1}")

GO=${GO:-go}
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"

case "$tree" in
  relational) USSP_PG_URL="$url" "$GO" run ./cmd/api migrate "${args[@]}" ;;
  timeseries) USSP_TS_URL="$url" "$GO" run ./cmd/tsdb-writer migrate "${args[@]}" ;;
  *) usage ;;
esac
