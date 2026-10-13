# The rules of deploy/compose/staging.yaml's header, read against the
# model `docker compose config --format json` renders (scripts/
# check-deploy.sh). Prints one "<service>: <finding>" line per broken
# rule and nothing when every rule holds.
#
#   jq -r --argjson bounds '["USSP_..._MAX_BYTES", ...]' -f scripts/deploy-shape.jq model.json

def go: ["api", "telemetry-ingest", "rid-sp", "monitor", "traffic-ws", "dss-sync", "tsdb-writer"];
def oneshots: ["migrate", "migrate-timeseries"];
def present($s): .services[$s] != null;
def set: (. // "") | tostring | length > 0;
def mib: if test("^[0-9]+MiB$") then (.[:-3] | tonumber) * 1048576 else null end;
def say($s; $m): "\($s): \($m)";

. as $m
| ($m.services // {}) as $svc
| [
    # Every service.
    ($svc | to_entries[] | .key as $s | .value as $v
      | (if ($v.ports // [] | length) > 0 then say($s; "publishes a port") else empty end),
        (if ($v.deploy.resources.limits.memory // null) == null then say($s; "no memory limit") else empty end),
        (if ($v.deploy.resources.limits.cpus // null) == null then say($s; "no CPU limit") else empty end),
        (if ($v.logging.options["max-size"] // null) == null then say($s; "unbounded logs") else empty end),
        (if ($v.image // "" | test("@sha256:[0-9a-f]{64}$")) | not then say($s; "image \($v.image // "none") is not pinned by digest") else empty end)),

    # Hardening of what this repository builds and of NATS.
    ((go + oneshots + ["web", "nats"])[] as $s
      | if ($m | present($s)) | not then say($s; "missing")
        elif ($svc[$s].read_only // false) | not then say($s; "root filesystem writable")
        else empty end),
    ((go + oneshots + ["web"])[] as $s | select($m | present($s))
      | (if ($svc[$s].cap_drop // [] | index("ALL")) == null then say($s; "capabilities not dropped") else empty end),
        (if ($svc[$s].security_opt // [] | index("no-new-privileges:true")) == null then say($s; "no-new-privileges unset") else empty end)),

    # The long-running Go processes.
    (go[] as $s | select($m | present($s)) | $svc[$s] as $v | ($v.environment // {}) as $e
      | (oneshots[] as $o | if ($v.depends_on[$o].condition // "") != "service_completed_successfully" then say($s; "does not wait for \($o) to complete") else empty end),
        (if ($v.healthcheck.test // [] | join(" ")) != "CMD /usr/bin/ussp-\($s) healthcheck /readyz" then say($s; "health check is not /readyz") else empty end),
        (($v.deploy.resources.limits.memory // 0 | tonumber) as $lim | ($e.GOMEMLIMIT // "" | mib) as $g
          | if $g == null then say($s; "GOMEMLIMIT unset")
            elif $lim > 0 and $g * 100 / $lim > 85 then say($s; "GOMEMLIMIT \($e.GOMEMLIMIT) above 85 % of the limit")
            else empty end),
        (["USSP_PG_URL", "USSP_TS_URL"][] as $d
          | ((["api:USSP_PG_URL", "api:USSP_TS_URL", "tsdb-writer:USSP_TS_URL"] | index("\($s):\($d)")) != null) as $want
          | ($e[$d] | set) as $has
          | if $has != $want then say($s; "\($d) is \(if $has then "set" else "unset" end), want \(if $want then "set" else "unset" end)") else empty end),
        (($e.USSP_NATS_URL // "" | capture("^nats://(?<u>[^:@/]+)[:@]").u // "") as $u
          | if $u != $s then say($s; "connects to NATS as '\(if $u == "" then "nobody" else $u end)', not its own user") else empty end),
        (($e.USSP_TRUSTED_PROXIES // "") | select(length > 0) | split(",")[]
          | select(test("^[0-9]+\\.[0-9]+\\.[0-9]+\\.[0-9]+/32$") | not) | say($s; "USSP_TRUSTED_PROXIES names \(.), not one host"))),

    # Every JWKS a process fetches is https (the verifier refuses plain
    # http but for localhost); this USSP's own host resolves to the edge
    # Caddy for the two processes that read its JWKS there.
    (go[] as $s | select($m | present($s)) | ($svc[$s].environment.USSP_TOKEN_ISSUERS // "") | select(length > 0) | split(",")[]
      | (split("=") | .[1] // "") as $j | select($j | startswith("https://") | not)
      | say($s; "fetches the JWKS \($j) over plain http")),
    (["telemetry-ingest", "traffic-ws"][] as $s | select($m | present($s))
      | ($svc[$s].environment.USSP_TOKEN_ISSUERS // "" | split(",") | map(split("=") | .[1] // "" | capture("^https://(?<h>[^/:]+)").h) | last) as $own
      | if ($svc[$s].extra_hosts // [] | map(tostring) | any(startswith($own)) | not) then say($s; "this USSP's own host \($own) is not mapped to the edge Caddy") else empty end),

    # The processes that verify tokens each read their own jwks_cache key
    # (USSP_JWKS_CACHE_KEY_FILE), mounted read-only from
    # keys/jwks-cache-<process>.key at that path, and no other process's;
    # a process that verifies nothing reads none.
    (go[] as $s | select($m | present($s)) | $svc[$s] as $v
      | ((["api", "telemetry-ingest", "rid-sp", "traffic-ws"] | index($s)) != null) as $want
      | ($v.environment.USSP_JWKS_CACHE_KEY_FILE // "") as $path
      | [$v.volumes // [] | .[] | select((.source // "") | test("/keys/jwks-cache-[a-z-]+\\.key$"))] as $keys
      | (if $want and $path == "" then say($s; "USSP_JWKS_CACHE_KEY_FILE unset: it stores and uses no JWKS")
         elif ($want | not) and $path != "" then say($s; "USSP_JWKS_CACHE_KEY_FILE set on a process that verifies no token")
         else empty end),
        (if $want and ($keys | map(select((.source | endswith("/keys/jwks-cache-\($s).key")) and .target == $path and (.read_only // false))) | length) != 1
         then say($s; "its jwks-cache key is not mounted read-only at \($path)") else empty end),
        (if ($keys | map(select(.source | endswith("/keys/jwks-cache-\($s).key") | not)) | length) > 0 then say($s; "mounts another process's jwks-cache key") else empty end)),

    # The one-shots: each opens only the tree it migrates, and no NATS.
    ([["migrate", "USSP_PG_URL"], ["migrate-timeseries", "USSP_TS_URL"]][] as [$s, $d] | select($m | present($s))
      | ["USSP_PG_URL", "USSP_TS_URL", "USSP_NATS_URL"][] as $v
      | ($svc[$s].environment[$v] | set) as $has
      | if $has != ($v == $d) then say($s; "\($v) is \(if $has then "set" else "unset" end)") else empty end),

    # Networks.
    (if ($m.networks.ussp.internal // false) | not then say("ussp"; "network not internal") else empty end),
    (["timescaledb", "nats", "tsdb-writer", "migrate", "migrate-timeseries"][] as $s | select($m | present($s))
      | if ($svc[$s].networks // {} | keys) != ["ussp"] then say($s; "on a network other than ussp") else empty end),

    # Every JetStream bound set on every Go service, their sum within the store.
    ((go + oneshots)[] as $s | select($m | present($s)) | first($bounds[] as $b
      | select(($svc[$s].environment[$b] // null) == null) | say($s; "\($b) unset (its default would be reserved)")) // empty),
    (([$bounds[] as $b | $svc.api.environment[$b] // "0" | tonumber] | add // 0) as $sum
      | ($svc.nats.environment.NATS_MAX_FILE_STORE // "0" | tonumber) as $store
      | if $store <= 0 or $sum > $store then say("nats"; "streams and buckets reserve \($sum) bytes, the store holds \($store)") else empty end)
  ]
| .[]
