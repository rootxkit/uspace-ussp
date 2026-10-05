#!/usr/bin/env bash
# One row of the chaos procedure (docs/RUNBOOKS/chaos.md, spec 05 §6)
# against the stack `CONFORMANCE_SYSTEMS=all CONFORMANCE_KEEP=1 make
# conformance` left running. It takes one failure domain down, keeps it
# down, brings it back, and writes what the USSP said before, during and
# after: every process's /readyz and the counters of its /metrics whose
# names say something was dropped, refused, late, degraded, stale,
# spilled, queued or pending. The traffic is the lab's (its scenario
# runner with synthetic vehicles, started separately, as the runbook says).
#
#   deploy/conformance/chaos.sh <row>
#
#   cisp         the CISP's api and deliver          (CHAOS_DOWN_S, default 300)
#   authority    every authority process
#   dss          the InterUSS DSS
#   ansp         the ANSP's api, feed and adapter
#   issuer       the lab issuer (the token service)
#   nats         the USSP's NATS                      (default 60)
#   timescaledb  the USSP's database container (relational and time series, M37)
#   api          the USSP's api process
#   kill         every USSP process in turn: SIGKILL, start, healthy again
#                (the restart row; CHAOS_KILL_GAP_S between two, default 30)
#
# The record goes to local/conformance/chaos/<UTC>-<row>/ (git-ignored):
# before.txt, during.txt (CHAOS_SAMPLE_S after the outage began, default
# 30, and at its end), after.txt (once every stopped service is healthy
# again), and timeline.txt (what was done when). Nothing here is a
# verdict: the runbook compares what was recorded with the failure
# columns of spec 02 and 05 §6.
#
# Nothing in this stack reaches an aircraft; nothing here writes outside
# the stack and local/.
set -euo pipefail
here="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$here"

row="${1:?usage: deploy/conformance/chaos.sh <row> (cisp authority dss ansp issuer nats timescaledb api kill)}"
project="${CONFORMANCE_PROJECT:-uspace-ussp-conf}"
dc() { scripts/conformance.sh compose "$@"; }
die() { echo "chaos: $*" >&2; exit 1; }

down_default=300
case "$row" in
  cisp) svcs="cisp-api cisp-deliver" ;;
  authority) svcs="$(dc config --services | grep '^authority-' | grep -v -E 'postgres|nats|migrate' | tr '\n' ' ')" ;;
  dss) svcs="dss" ;;
  ansp) svcs="ansp-api ansp-manned-feed ansp-manned-adapter" ;;
  issuer) svcs="lab-issuer" ;;
  nats) svcs="ussp-nats"; down_default=60 ;;
  timescaledb) svcs="ussp-timescaledb" ;;
  api) svcs="ussp-api" ;;
  kill) svcs="ussp-api ussp-telemetry-ingest ussp-rid-sp ussp-monitor ussp-traffic-ws ussp-dss-sync ussp-tsdb-writer" ;;
  *) die "unknown row $row" ;;
esac
down_s="${CHAOS_DOWN_S:-$down_default}"
sample_s="${CHAOS_SAMPLE_S:-30}"
gap_s="${CHAOS_KILL_GAP_S:-30}"
for v in "$down_s" "$sample_s" "$gap_s"; do
  case "$v" in *[!0-9]* | "") die "CHAOS_DOWN_S, CHAOS_SAMPLE_S and CHAOS_KILL_GAP_S are whole seconds" ;; esac
done
[ "$sample_s" -lt "$down_s" ] || [ "$row" = kill ] || die "CHAOS_SAMPLE_S ($sample_s) must be shorter than the outage ($down_s)"
dc ps --services --status running | grep -q '^ussp-api$' || die "the stack of $project is not running (CONFORMANCE_SYSTEMS=all CONFORMANCE_KEEP=1 make conformance)"

dir="local/conformance/chaos/$(date -u +%Y%m%dT%H%M%SZ)-$row"
mkdir -p "$dir"
note() { echo "$(date -u +%H:%M:%S) $*" | tee -a "$dir/timeline.txt"; }

# sample <file> <label>: every USSP process's /readyz and its counters of
# interest, read from inside the stack's network (the processes publish
# no port; /metrics is never routed by Caddy).
sample() {
  {
    echo "# $2 at $(date -u +%Y-%m-%dT%H:%M:%SZ)"
    MSYS_NO_PATHCONV=1 docker run --rm --network "${project}_lab" alpine:latest sh -c '
      for p in api:8080 telemetry-ingest:8081 rid-sp:8082 monitor:8083 traffic-ws:8084 dss-sync:8085 tsdb-writer:8086; do
        echo "== ussp-$p /readyz"
        # A not-ready process answers 503, which wget reports as an
        # error without the body: the status line is kept apart.
        wget -S -T 5 -O /tmp/body "http://ussp-$p/readyz" 2>/tmp/hdr; rc=$?
        if [ -s /tmp/body ]; then cat /tmp/body; else echo "(no body, wget exit $rc: $(grep -m1 "HTTP/" /tmp/hdr | tr -s " " || echo no answer))"; fi
        rm -f /tmp/body /tmp/hdr
        echo
        echo "== ussp-$p /metrics (selected)"
        wget -T 5 -qO- "http://ussp-$p/metrics" 2>/dev/null |
          grep -E "^ussp_[a-z0-9_]*(drop|refus|late|degrad|stale|spill|queue|pending|gap|unavailable|lost|backlog|jwks|failed|dependency_up)" |
          grep -v -E "^ussp_[a-z0-9_]*_bucket" || echo "(no answer)"
      done'
  } >"$dir/$1" 2>&1
}

sample before.txt "before"
note "before sampled"
if [ "$row" = kill ]; then
  for s in $svcs; do
    note "SIGKILL $s"
    docker kill -s KILL "${project}-${s}-1" >/dev/null
    t0="$(date +%s)"
    dc up -d --no-deps --wait --wait-timeout 120 "$s" >/dev/null 2>&1 || die "$s did not become healthy again within 120 s"
    note "$s healthy again after $(($(date +%s) - t0)) s"
    sample "after-$s.txt" "after $s restarted"
    # The gap lets the restarted process take up its work before the next
    # one goes: the measured quantity, not a wait for something.
    sleep "$gap_s"
  done
else
  note "stop $svcs (for ${down_s} s)"
  # shellcheck disable=SC2086 # one argument per service
  dc stop $svcs >/dev/null 2>&1 || die "could not stop $svcs"
  sleep "$sample_s"
  sample during.txt "during, ${sample_s} s into the outage"
  note "during sampled"
  sleep "$((down_s - sample_s))"
  sample during-end.txt "during, at the end of the ${down_s} s outage"
  note "end of outage sampled; starting $svcs"
  t0="$(date +%s)"
  # shellcheck disable=SC2086 # one argument per service
  # --no-deps: bring back what was stopped, nothing else (a dependency
  # recreated here would be a second failure of the row).
  dc up -d --no-deps --wait --wait-timeout 300 $svcs >/dev/null 2>&1 || die "$svcs did not become healthy again within 300 s"
  note "$svcs back after $(($(date +%s) - t0)) s"
  sleep "$sample_s"
  sample after.txt "after, ${sample_s} s after the restore"
  note "after sampled"
fi
echo "chaos: record in $dir"
