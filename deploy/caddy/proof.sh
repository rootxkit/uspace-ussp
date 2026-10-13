#!/usr/bin/env bash
# Proves deploy/caddy/ussp.caddy against a running Caddy (the pinned
# image), not against its text: every route class reaches the upstream
# it names, /metrics and /readyz reach none, the basemap is served with
# ranges and its cache headers, a forged certificate subject or client
# address never reaches an upstream, a WebSocket upgrade keeps its
# Origin, and the security headers are set (E-01: each absence is paired
# with the presence that makes it happen, and the proof fails on a
# snippet with a route removed).
#
#   deploy/caddy/proof.sh            (make check-deploy runs it)
#   PROOF_SNIPPET=<file> deploy/caddy/proof.sh   another snippet
#
# Stub upstreams inside the same Caddy answer with their own name and the
# headers they received. The container is removed on exit and its
# absence checked.
set -euo pipefail

CADDY_IMAGE="${CADDY_IMAGE:-caddy:2.10.2-alpine@sha256:4c6e91c6ed0e2fa03efd5b44747b625fec79bc9cd06ac5235a779726618e530d}"
port="${PROOF_PORT:-58454}"
host=ussp.proof.test
here="$(cd "$(dirname "$0")" && pwd)"
snippet="${PROOF_SNIPPET:-$here/ussp.caddy}"
work="$(mktemp -d)"
name="ussp-caddy-proof-$$"

# shellcheck disable=SC2317 # run by the EXIT trap
cleanup() {
  local rc=$?
  docker rm -f "$name" >/dev/null 2>&1 || true
  rm -rf "$work"
  if [ -n "$(docker ps -aq --filter "name=^${name}\$")" ]; then
    echo "proof: container $name is still present after cleanup" >&2
    rc=1
  fi
  exit "$rc"
}
trap cleanup EXIT
if type -P cygpath >/dev/null 2>&1; then work="$(cygpath -m "$work")"; fi

mkdir -p "$work/basemap/tiles"
printf '{"release":"proof"}\n' > "$work/basemap/SOURCE.json"
printf '0123456789abcdef' > "$work/basemap/tiles/proof.pmtiles"
chmod -R a+rX "$work/basemap"
cp "$snippet" "$work/ussp.caddy"

stub() { # <port> <name>
  printf ':%s {\n\trespond "upstream=%s subject=[{header.X-Client-Cert-Subject}] xff=[{header.X-Forwarded-For}] origin=[{header.Origin}] upgrade=[{header.Upgrade}]" 200\n}\n' "$1" "$2"
}
{
  cat <<'CADDY'
{
	admin off
	local_certs
	skip_install_trust
	auto_https disable_redirects
	https_port 8443
	http_port 8080
	log {
		output discard
	}
}

import /work/ussp.caddy

CADDY
  stub 9001 api
  stub 9002 telemetry
  stub 9003 ridsp
  stub 9004 traffic
  stub 9005 web
} > "$work/Caddyfile"

MSYS_NO_PATHCONV=1 docker run -d --name "$name" \
  -p "127.0.0.1:${port}:8443" \
  -e USSP_HOST="$host" \
  -e USSP_API_UPSTREAM=127.0.0.1:9001 -e USSP_TELEMETRY_UPSTREAM=127.0.0.1:9002 \
  -e USSP_RIDSP_UPSTREAM=127.0.0.1:9003 -e USSP_TRAFFIC_UPSTREAM=127.0.0.1:9004 \
  -e USSP_WEB_UPSTREAM=127.0.0.1:9005 -e USSP_BASEMAP_DIR=/work/basemap \
  -v "$work:/work:ro" \
  "$CADDY_IMAGE" caddy run --config /work/Caddyfile --adapter caddyfile >/dev/null

fail=0
pass=0
ok() { echo "ok     $*"; pass=$((pass + 1)); }
bad() { echo "FAIL   $*"; fail=1; }

# curl against the proof host. -k: the site certificate is Caddy's
# internal one. --ssl-no-revoke: Windows curl (Schannel) asks for a
# revocation list the internal CA does not publish; other backends
# ignore it.
pc() { # <path> [curl args...]
  local path="$1"; shift
  curl -sk --max-time 10 --ssl-no-revoke --resolve "$host:$port:127.0.0.1" \
    -w '\n%{http_code}' "$@" "https://$host:$port$path"
}

# Wait on the condition (Caddy serving), by curl's own bounded retries:
# at most 30 tries a second apart.
if ! curl -sk --max-time 5 --ssl-no-revoke --retry 30 --retry-delay 1 --retry-all-errors \
    --resolve "$host:$port:127.0.0.1" -o /dev/null "https://$host:$port/healthz"; then
  echo "proof: Caddy did not serve; its log:" >&2
  docker logs "$name" 2>&1 | tail -n 20 >&2
  exit 1
fi

expect() { # <label> <want code> <want body substring> <path> [curl args...]
  local label="$1" code="$2" want="$3" path="$4"; shift 4
  local out got body
  out="$(pc "$path" "$@" 2>&1 || true)"
  got="$(printf '%s' "$out" | tail -n 1)"
  body="$(printf '%s' "$out" | sed '$d')"
  if [ "$got" = "$code" ] && { [ -z "$want" ] || [[ "$body" == *"$want"* ]]; }; then
    ok "$label: $path -> $got"
  else
    bad "$label: $path -> ${got:-no answer} ($body), want $code with '$want'"
  fi
}

# Route classes.
expect telemetry 200 upstream=telemetry /v1/telemetry
expect telemetry 200 upstream=telemetry /v1/telemetry/batch -X POST
expect traffic   200 upstream=traffic   /v1/traffic
expect traffic   200 upstream=traffic   /v1/traffic/snapshot
expect traffic   200 upstream=traffic   /v1/alerts
expect api       200 upstream=api       /v1/alerts/01J0000000000000000000000/ack -X POST
expect ridsp     200 upstream=ridsp     /uss/flights
expect ridsp     200 upstream=ridsp     /uss/flights/x/details
expect ridsp     200 upstream=ridsp     /uss/identification_service_areas/x -X POST
expect ridsp     200 upstream=ridsp     /v1/authority/flights
expect api       200 upstream=api       /uss/v1/operational_intents/x
expect api       200 upstream=api       /v1/intents -X POST
expect api       200 upstream=api       /v1/telemetryx
expect api       200 upstream=api       /oauth/token -X POST
expect api       200 upstream=api       /.well-known/jwks.json
expect api       200 upstream=api       /healthz
expect web       200 upstream=web       /
expect web       200 upstream=web       /_bff/console/login -X POST
expect web       200 upstream=web       /ka/login

# Never routed: answered by Caddy itself, with no upstream body.
for p in /metrics /metrics/x /readyz; do
  out="$(pc "$p" || true)"
  if [ "$(printf '%s' "$out" | tail -n 1)" = 404 ] && [[ "$out" != *upstream=* ]]; then
    ok "internal: $p -> 404 from Caddy"
  else
    bad "internal: $p -> $(printf '%s' "$out" | tr '\n' ' ')"
  fi
done

# The basemap: a range answers 206 with exactly those bytes; the bundle
# is cached a day and SOURCE.json five minutes; a missing file is 404
# from the file server, not web.
hdr="$work/headers"
out="$(curl -sk --max-time 10 --ssl-no-revoke --resolve "$host:$port:127.0.0.1" -D "$hdr" \
  -H 'Range: bytes=2-5' -w '\n%{http_code}' "https://$host:$port/basemap/tiles/proof.pmtiles" || true)"
if [ "$(printf '%s' "$out" | tail -n 1)" = 206 ] && [ "$(printf '%s' "$out" | sed '$d')" = 2345 ] &&
   grep -qi '^cache-control: public, max-age=86400' "$hdr"; then
  ok "basemap: range bytes=2-5 -> 206 '2345', Cache-Control max-age=86400"
else
  bad "basemap: range -> $(printf '%s' "$out" | tr '\n' ' ')"
fi
out="$(curl -sk --max-time 10 --ssl-no-revoke --resolve "$host:$port:127.0.0.1" -D "$hdr" \
  -w '\n%{http_code}' "https://$host:$port/basemap/SOURCE.json" || true)"
if [ "$(printf '%s' "$out" | tail -n 1)" = 200 ] && grep -qi '^cache-control: public, max-age=300' "$hdr"; then
  ok "basemap: SOURCE.json -> 200, Cache-Control max-age=300"
else
  bad "basemap: SOURCE.json -> $(printf '%s' "$out" | tr '\n' ' ')"
fi
out="$(pc /basemap/absent.pmtiles || true)"
if [ "$(printf '%s' "$out" | tail -n 1)" = 404 ] && [[ "$out" != *upstream=* ]]; then
  ok "basemap: a missing file -> 404 from the file server"
else
  bad "basemap: a missing file -> $(printf '%s' "$out" | tr '\n' ' ')"
fi

# Headers on an upstream's answer.
curl -sk --max-time 10 --ssl-no-revoke --resolve "$host:$port:127.0.0.1" -D "$hdr" -o /dev/null "https://$host:$port/v1/intents" || true
if grep -qi '^strict-transport-security: max-age=31536000' "$hdr" && grep -qi '^x-content-type-options: nosniff' "$hdr" &&
   ! grep -qi '^server:' "$hdr"; then
  ok "headers: HSTS and nosniff set, Server removed"
else
  bad "headers: $(tr '\r\n' '  ' < "$hdr")"
fi

# A forged certificate subject never reaches an upstream; the presence
# of the header on the way in is what makes the absence mean something,
# and the stub shows the value it is given (the control below).
expect "forged subject" 200 "subject=[]" /v1/intents -H 'X-Client-Cert-Subject: CN=forged'
expect "forged subject" 200 "subject=[]" /v1/telemetry -H 'X-Client-Cert-Subject: CN=forged'
expect "forged subject" 200 "subject=[]" / -H 'X-Client-Cert-Subject: CN=forged'
# A client's own X-Forwarded-For is replaced by the peer's address: the
# processes take the rightmost hop that is not Caddy, which must be the
# real client, never a value the client wrote.
out="$(pc /v1/intents -H 'X-Forwarded-For: 203.0.113.7' || true)"
if [[ "$out" == *"xff=["* ]] && [[ "$out" != *203.0.113.7* ]] && [[ "$out" =~ xff=\[[0-9.]+\] ]]; then
  ok "client address: a forged X-Forwarded-For is replaced by the peer's (${BASH_REMATCH[0]})"
else
  bad "client address: $(printf '%s' "$out" | head -n 1)"
fi
# A WebSocket upgrade reaches telemetry-ingest and traffic-ws with its
# Origin and Upgrade headers intact (M22).
# A fresh nonce, as a client draws one (RFC 6455 4.1).
ws_key="$(openssl rand -base64 16)"
for p in /v1/telemetry /v1/traffic /v1/alerts; do
  expect "upgrade" 200 "origin=[https://$host] upgrade=[websocket]" "$p" \
    -H 'Connection: Upgrade' -H 'Upgrade: websocket' -H "Origin: https://$host" \
    -H 'Sec-WebSocket-Version: 13' -H "Sec-WebSocket-Key: $ws_key"
done

echo "proof: $pass checks passed$([ "$fail" -eq 0 ] || echo ', some FAILED')"
exit "$fail"
