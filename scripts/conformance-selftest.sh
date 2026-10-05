#!/usr/bin/env bash
# make conformance-selftest: scripts/conformance.sh in each of its
# states, against a fake uspace-lab checkout, a fake suite and a fake
# target (no Docker, no network beyond 127.0.0.1). CI runs it on every
# change to the hook (job build-vet-lint).
#
# Each refusal is paired with the case that is accepted (E-01): the
# safety check refuses a production-shaped system id and accepts a test
# one; the push declaration skips the authority stream when off and not
# when on, and a declaration the target contradicts stops the run while
# a true one runs the suite; the suite's exit status comes back
# unchanged, 0, 1 and 3 alike.
set -euo pipefail
here="$(cd "$(dirname "$0")/.." && pwd)"
hook="$here/scripts/conformance.sh"
py="$(command -v python3 || command -v python || true)"
[ -n "$py" ] || { echo "conformance-selftest: python3 is required (the fake target)" >&2; exit 2; }

work="$(mktemp -d)"
srv_pid=""
cleanup() {
  if [ -n "$srv_pid" ]; then kill "$srv_pid" 2>/dev/null || true; fi
  rm -rf "$work"
}
trap cleanup EXIT
fails=0
pass() { echo "ok     $*"; }
fail() { echo "FAIL   $*"; fails=$((fails + 1)); }

# ---- a fake uspace-lab checkout -------------------------------------------------
lab="$work/lab"
mkdir -p "$lab/cmd/conformance" "$lab/conformance/national/contracts" "$lab/deploy"
echo 'package main' >"$lab/cmd/conformance/main.go"
echo 'format: conformance-requirements/v1' >"$lab/conformance/requirements.yaml"
cat >"$lab/conformance/national/contracts/ussp.yaml" <<'EOF'
system: ussp
scopes:
  openAuthorityFlights: [rid.display_provider]
EOF
echo 'USSP_SYSTEM_ID=USSP-DEV' >"$lab/deploy/demo.env.example"

# ---- a fake suite: records its arguments and env file, exits FAKE_RC ------------
suite="$work/suite.sh"
cat >"$suite" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$@" >"$FAKE_ARGS"
prev=""
for a in "$@"; do
  if [ "$prev" = "--env-file" ]; then cp "$a" "$FAKE_ENV"; fi
  if [ "$prev" = "--out" ]; then
    # A report directory, as the suite writes one per run.
    d="$a/run-$(date +%s%N)"; mkdir -p "$d"; echo '{}' >"$d/report.json"
  fi
  prev="$a"
done
exit "${FAKE_RC:-0}"
EOF
chmod +x "$suite"

# ---- a fake target: /v1/authority/flights answers FAKE_PUSH_CODE --------------
cat >"$work/target.py" <<'EOF'
import http.server, os, sys
code_file = sys.argv[1]
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        code = int(open(code_file).read().strip()) if self.path == "/v1/authority/flights" else 404
        self.send_response(code)
        self.send_header("Content-Type", "application/problem+json")
        self.end_headers()
        self.wfile.write(b'{"type":"x","title":"x","status":%d,"errors":[]}' % code)
    def log_message(self, *a):
        pass
s = http.server.HTTPServer(("127.0.0.1", 0), H)
print(s.server_address[1], flush=True)
s.serve_forever()
EOF
echo 404 >"$work/code"
coproc SRV { "$py" "$(if command -v cygpath >/dev/null 2>&1; then cygpath -w "$work/target.py"; else echo "$work/target.py"; fi)" \
  "$(if command -v cygpath >/dev/null 2>&1; then cygpath -w "$work/code"; else echo "$work/code"; fi)"; }
srv_pid="$SRV_PID"
read -r port <&"${SRV[0]}"
port="${port//[!0-9]/}" # digits only: a Windows python ends the line with CRLF
[ -n "$port" ] || { echo "conformance-selftest: the fake target did not start" >&2; exit 1; }
target="http://127.0.0.1:$port"

# hook <expected exit> <description> [VAR=value ...]: run the hook with a
# clean configuration plus the given variables.
hook_rc() {
  local want="$1" what="$2"
  shift 2
  local rc=0
  env -u USSP_CONFORMANCE_BASE_URL -u USSP_CONFORMANCE_AUTHORITY_PUSH -u USSP_CONFORMANCE_SYSTEM_ID \
    -u CONFORMANCE_ENV -u CONFORMANCE_ALLOW_INCOMPLETE -u CONFORMANCE_KEEP \
    LAB_DIR="$lab" CONFORMANCE_SUITE_BIN="$suite" CONFORMANCE_OUT="$work/out" \
    FAKE_ARGS="$work/args" FAKE_ENV="$work/env" "$@" "$hook" run >"$work/log" 2>&1 || rc=$?
  if [ "$rc" = "$want" ]; then pass "$what (exit $rc)"; else
    fail "$what: exit $rc, want $want"
    sed 's/^/       /' "$work/log"
  fi
}
# What the fake suite was handed (empty when it did not run).
handed_overrides() { if [ -f "$work/env" ]; then sed -n 's/^USSP_CONFORMANCE_OVERRIDES=//p' "$work/env"; fi; }
handed_args() { if [ -f "$work/args" ]; then cat "$work/args"; fi; }
ext=(USSP_CONFORMANCE_BASE_URL="$target" USSP_CONFORMANCE_SYSTEM_ID=USSP-TEST-01)

# ---- configuration errors: exit 2, nothing run ---------------------------------
rm -f "$work/args"
hook_rc 2 "LAB_DIR that does not exist is refused" LAB_DIR="$work/nowhere"
hook_rc 2 "an external target without its push declared is refused" "${ext[@]}"
hook_rc 2 "a push value other than on or off is refused" "${ext[@]}" USSP_CONFORMANCE_AUTHORITY_PUSH=maybe
hook_rc 2 "a production-shaped system id is refused" USSP_CONFORMANCE_BASE_URL="$target" USSP_CONFORMANCE_SYSTEM_ID=USSP-GE-01 USSP_CONFORMANCE_AUTHORITY_PUSH=off
hook_rc 2 "an empty system id is refused" USSP_CONFORMANCE_BASE_URL="$target" USSP_CONFORMANCE_SYSTEM_ID= USSP_CONFORMANCE_AUTHORITY_PUSH=off
hook_rc 2 "a base URL that is not http(s) is refused" USSP_CONFORMANCE_BASE_URL=ftp://x USSP_CONFORMANCE_SYSTEM_ID=USSP-DEV USSP_CONFORMANCE_AUTHORITY_PUSH=off
if [ -e "$work/args" ]; then fail "the suite ran on a configuration error"; else pass "the suite never ran on a configuration error"; fi

# ---- the push declaration -------------------------------------------------------
echo 404 >"$work/code"
hook_rc 0 "declared off, the target answers 404: the suite runs" "${ext[@]}" USSP_CONFORMANCE_AUTHORITY_PUSH=off
if handed_args | grep -q '^--target$' && handed_args | grep -q 'deploy/conformance/target.yaml$'; then pass "the suite got deploy/conformance/target.yaml"; else fail "the suite's arguments: $(handed_args | tr '\n' ' ')"; fi
ov="$(handed_overrides)"
if [ -n "$ov" ] && grep -q '^skip:' "$ov" && grep -q '^  openAuthorityFlights:' "$ov" && grep -q '^scopes:' "$ov"; then
  pass "push off: the overrides are the lab's plus the skip of openAuthorityFlights"
else
  fail "push off: overrides $ov lack the lab's table or the skip"
fi
echo 401 >"$work/code"
hook_rc 0 "declared on, the target answers 401: the suite runs" "${ext[@]}" USSP_CONFORMANCE_AUTHORITY_PUSH=on
ov="$(handed_overrides)"
if [ -n "$ov" ] && ! grep -q '^skip:' "$ov" && grep -q '^scopes:' "$ov"; then pass "push on: the overrides are the lab's alone, nothing skipped"; else fail "push on: overrides $ov"; fi
rm -f "$work/args"
hook_rc 1 "declared off, the target answers 401 (the push is on): stopped" "${ext[@]}" USSP_CONFORMANCE_AUTHORITY_PUSH=off
echo 404 >"$work/code"
hook_rc 1 "declared on, the target answers 404 (the push is off): stopped" "${ext[@]}" USSP_CONFORMANCE_AUTHORITY_PUSH=on
if [ -e "$work/args" ]; then fail "the suite ran on a false declaration"; else pass "the suite never ran on a false declaration"; fi

# ---- the suite's exit status, unchanged -----------------------------------------
for rc in 1 3 0; do
  hook_rc "$rc" "the suite exits $rc: the hook exits $rc" "${ext[@]}" USSP_CONFORMANCE_AUTHORITY_PUSH=off FAKE_RC="$rc"
  info="$(find "$work/out" -name run-info.txt -newer "$work/code" | sort | tail -n 1)"
  if [ -n "$info" ] && grep -q "^suite exit $rc$" "$info" && grep -q "^USSP_AUTHORITY_PUSH off$" "$info"; then
    pass "run-info.txt beside the report records exit $rc"
  else
    fail "run-info.txt for exit $rc: ${info:-none}"
  fi
  touch "$work/code"
done
hook_rc 0 "CONFORMANCE_ALLOW_INCOMPLETE=1 is handed to the suite" "${ext[@]}" USSP_CONFORMANCE_AUTHORITY_PUSH=off CONFORMANCE_ALLOW_INCOMPLETE=1
if handed_args | grep -q '^--allow-incomplete$'; then pass "--allow-incomplete given"; else fail "--allow-incomplete missing: $(handed_args | tr '\n' ' ')"; fi
hook_rc 0 "without it, --allow-incomplete is not given" "${ext[@]}" USSP_CONFORMANCE_AUTHORITY_PUSH=off
if handed_args | grep -q '^--allow-incomplete$'; then fail "--allow-incomplete given unasked"; else pass "--allow-incomplete absent"; fi

# ---- the lab's overrides grow a skip table: refused, not merged blindly ---------
printf 'skip:\n  getGeo:\n    success: "x"\n' >>"$lab/conformance/national/contracts/ussp.yaml"
hook_rc 2 "lab overrides with their own skip table are refused" "${ext[@]}" USSP_CONFORMANCE_AUTHORITY_PUSH=off

# ---- the declaration names operations the contract has --------------------------
for op in $(sed -n 's/^  \([A-Za-z]*\):$/\1/p' "$here/deploy/conformance/authority-push-off.yaml"); do
  if grep -q "operationId: $op$" "$here/api/openapi.yaml"; then pass "authority-push-off.yaml names $op, an operation of api/openapi.yaml"; else fail "authority-push-off.yaml names $op, which api/openapi.yaml does not have"; fi
done

if [ "$fails" -ne 0 ]; then
  echo "conformance-selftest: $fails failed"
  exit 1
fi
echo "conformance-selftest: every case passed"
