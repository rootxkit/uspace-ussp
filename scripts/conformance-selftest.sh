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
# unchanged, 0, 1 and 3 alike; the reviewed baseline is handed to the
# suite and refused when it accepts a requirement not applicable without
# saying why, and a gate requirement it accepts is named as not a pass
# while a run that checked everything names none; a declared InterUSS
# interface that answers 401 without a credential runs the suite and one
# that does not stops it, and uss_qualifier reports made elsewhere are
# handed to the suite (WP-21).
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
echo 'USSP_SYSTEM_ID=DEV01' >"$lab/deploy/demo.env.example"

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
    if [ "${FAKE_NO_REPORT:-}" != 1 ]; then
      d="$a/run-$(date +%s%N)"; mkdir -p "$d"
      cp "${FAKE_REPORT:-$FAKE_DEFAULT_REPORT}" "$d/report.json"
    fi
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
        if self.path == "/v1/authority/flights":
            code = int(open(code_file).read().strip())
        else:
            # The InterUSS test interfaces refuse a call without a
            # credential; anything else is not served.
            code = 401 if self.path.startswith("/test/") else 404
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

# What the fake suite reports unless told otherwise: every requirement
# of the repository's baseline judged, and passed with many checks.
jq '{requirements: [.requirements | keys[] | {id: ., role: "gate", status: "pass", outcomes: {pass: 1000, fail: 0, not_applicable: 0}}]}' \
  "$here/deploy/conformance/baseline.json" >"$work/report-default.json"

# hook <expected exit> <description> [VAR=value ...]: run the hook with a
# clean configuration plus the given variables.
hook_rc() {
  local want="$1" what="$2"
  shift 2
  local rc=0
  env -u USSP_CONFORMANCE_BASE_URL -u USSP_CONFORMANCE_AUTHORITY_PUSH -u USSP_CONFORMANCE_SYSTEM_ID \
    -u CONFORMANCE_ENV -u CONFORMANCE_ALLOW_INCOMPLETE -u CONFORMANCE_KEEP -u CONFORMANCE_BASELINE -u CONFORMANCE_EXERCISED \
    -u CONFORMANCE_QUALIFIER -u CONFORMANCE_QUALIFIER_REPORTS -u USSP_CONFORMANCE_RID_INJECTION_URL \
    -u USSP_CONFORMANCE_RID_OBSERVATION_URL -u USSP_CONFORMANCE_FLIGHT_PLANNING_URL \
    LAB_DIR="$lab" CONFORMANCE_SUITE_BIN="$suite" CONFORMANCE_OUT="$work/out" \
    FAKE_ARGS="$work/args" FAKE_ENV="$work/env" FAKE_DEFAULT_REPORT="$work/report-default.json" "$@" "$hook" run >"$work/log" 2>&1 || rc=$?
  if [ "$rc" = "$want" ]; then pass "$what (exit $rc)"; else
    fail "$what: exit $rc, want $want"
    sed 's/^/       /' "$work/log"
  fi
}
# What the fake suite was handed (empty when it did not run).
handed_overrides() { if [ -f "$work/env" ]; then sed -n 's/^USSP_CONFORMANCE_OVERRIDES=//p' "$work/env"; fi; }
handed_args() { if [ -f "$work/args" ]; then cat "$work/args"; fi; }
ext=(USSP_CONFORMANCE_BASE_URL="$target" USSP_CONFORMANCE_SYSTEM_ID=TEST01)

# ---- configuration errors: exit 2, nothing run ---------------------------------
rm -f "$work/args"
hook_rc 2 "LAB_DIR that does not exist is refused" LAB_DIR="$work/nowhere"
hook_rc 2 "an external target without its push declared is refused" "${ext[@]}"
hook_rc 2 "a push value other than on or off is refused" "${ext[@]}" USSP_CONFORMANCE_AUTHORITY_PUSH=maybe
hook_rc 2 "a production-shaped system id is refused" USSP_CONFORMANCE_BASE_URL="$target" USSP_CONFORMANCE_SYSTEM_ID=GE01 USSP_CONFORMANCE_AUTHORITY_PUSH=off
hook_rc 2 "an empty system id is refused" USSP_CONFORMANCE_BASE_URL="$target" USSP_CONFORMANCE_SYSTEM_ID= USSP_CONFORMANCE_AUTHORITY_PUSH=off
hook_rc 2 "a base URL that is not http(s) is refused" USSP_CONFORMANCE_BASE_URL=ftp://x USSP_CONFORMANCE_SYSTEM_ID=DEV01 USSP_CONFORMANCE_AUTHORITY_PUSH=off
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

# ---- the InterUSS interfaces (WP-21) --------------------------------------------
echo 404 >"$work/code"
hook_rc 0 "a declared interface that answers 401 without a credential: the suite runs" "${ext[@]}" USSP_CONFORMANCE_AUTHORITY_PUSH=off \
  USSP_CONFORMANCE_FLIGHT_PLANNING_URL="$target/test/flight_planning/v1" USSP_CONFORMANCE_RID_INJECTION_URL="$target/test/rid/v1/injection"
if grep -q "interface flight_planning declared and served" "$work/log" && grep -q "interface rid_injection declared and served" "$work/log"; then
  pass "both declarations were checked"
else
  fail "the declarations were not checked: $(cat "$work/log")"
fi
rm -f "$work/args"
hook_rc 1 "a declared interface that is not served (404): stopped" "${ext[@]}" USSP_CONFORMANCE_AUTHORITY_PUSH=off \
  USSP_CONFORMANCE_RID_INJECTION_URL="$target/not/served"
if [ -e "$work/args" ]; then fail "the suite ran on a false interface declaration"; else pass "the suite never ran on a false interface declaration"; fi
echo '{"report": {}}' >"$work/qualifier-report.json"
hook_rc 0 "a uss_qualifier report made elsewhere is folded in" "${ext[@]}" USSP_CONFORMANCE_AUTHORITY_PUSH=off \
  CONFORMANCE_QUALIFIER_REPORTS="F3411-SP=$work/qualifier-report.json"
if handed_args | grep -q '^--qualifier-report$' && handed_args | grep -q 'F3411-SP=.*qualifier-report.json$'; then
  pass "--qualifier-report F3411-SP handed to the suite"
else
  fail "--qualifier-report missing: $(handed_args | tr '\n' ' ')"
fi
hook_rc 0 "without reports, none is handed" "${ext[@]}" USSP_CONFORMANCE_AUTHORITY_PUSH=off
if handed_args | grep -q '^--qualifier-report$'; then fail "--qualifier-report handed unasked"; else pass "--qualifier-report absent"; fi
if grep -q "not run in this invocation" "$work/log"; then pass "the run says uss_qualifier was not run in this invocation"; else fail "no 'not run in this invocation': $(cat "$work/log")"; fi
hook_rc 2 "a report that cannot be read is refused" "${ext[@]}" USSP_CONFORMANCE_AUTHORITY_PUSH=off CONFORMANCE_QUALIFIER_REPORTS="F3411-SP=$work/none.json"
hook_rc 2 "a report without its requirement is refused" "${ext[@]}" USSP_CONFORMANCE_AUTHORITY_PUSH=off CONFORMANCE_QUALIFIER_REPORTS="$work/qualifier-report.json"
# On the stack (not run here: it needs Docker) the interfaces are
# checked at the URLs the target declares, as uss_qualifier reaches them
# on the lab network, not through the lab Caddy's /test/* route: the
# declaration is what is checked. Read from the hook itself.
declared="$(sed -n 's/^  iface_\(injection\|observation\|planning\)=\(.*\)$/\2/p' "$hook" | tr '\n' ' ')"
stack_call="$(sed -n '/^  stack_up$/,/^else$/p' "$hook" | grep '^  check_interfaces ' || true)"
if [ "$stack_call" = '  check_interfaces "$iface_injection" "$iface_observation" "$iface_planning"' ] &&
  [ "$declared" = "http://ussp:8087/test/rid/v1/injection http://ussp:8087/test/rid/v1/observation http://ussp:8087/test/flight_planning/v1 " ] &&
  grep -qF 'code="$(lab_status "$url$path")"' "$hook"; then
  pass "on the stack the declared interfaces are checked where they are declared, from the lab network"
else
  fail "on the stack the interfaces are not checked at their declared URLs: ${stack_call:-no call} (declared: $declared)"
fi
hook_rc 2 "CONFORMANCE_QUALIFIER other than on or off is refused" "${ext[@]}" USSP_CONFORMANCE_AUTHORITY_PUSH=off CONFORMANCE_QUALIFIER=sometimes

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
# Without the InterUSS suites and with an incomplete run accepted, the
# hook would exit 0 having judged none of their requirements: refused
# before anything runs. Each switch alone still runs the suite.
rm -f "$work/args"
hook_rc 2 "CONFORMANCE_QUALIFIER=off with CONFORMANCE_ALLOW_INCOMPLETE=1 is refused" "${ext[@]}" USSP_CONFORMANCE_AUTHORITY_PUSH=off CONFORMANCE_QUALIFIER=off CONFORMANCE_ALLOW_INCOMPLETE=1
if [ -e "$work/args" ]; then fail "the suite ran with the InterUSS suites off and an incomplete run accepted"; else pass "the suite never ran with the InterUSS suites off and an incomplete run accepted"; fi
if grep -qF 'CONFORMANCE_QUALIFIER=off with CONFORMANCE_ALLOW_INCOMPLETE=1' "$work/log"; then pass "the refusal names both switches"; else fail "the refusal does not name both switches: $(tail -n 1 "$work/log")"; fi
hook_rc 0 "CONFORMANCE_QUALIFIER=off alone runs the suite" "${ext[@]}" USSP_CONFORMANCE_AUTHORITY_PUSH=off CONFORMANCE_QUALIFIER=off
if [ -e "$work/args" ]; then pass "the suite ran with the InterUSS suites off"; else fail "the suite did not run with CONFORMANCE_QUALIFIER=off alone"; fi

# ---- the workflow's guard of the same combination -------------------------------
# The conformance workflow refuses qualifier off with allow_incomplete in
# its first step, before the lab and the geoid grid are fetched: the
# step's script (between its inputs-guard markers) is run here with each
# combination, and its inputs are the ones `make conformance` is given.
wf="$here/.github/workflows/conformance.yml"
guard="$work/inputs-guard.sh"
awk '/# inputs-guard:begin/ { on = 1; next } /# inputs-guard:end/ { on = 0 } on { sub(/^ +/, ""); print }' "$wf" >"$guard"
if [ ! -s "$guard" ]; then
  fail "the conformance workflow has no inputs guard (# inputs-guard:begin ... end)"
else
  guard_rc() { # <want> <label> <qualifier> <allow_incomplete>
    local rc=0
    env CONFORMANCE_QUALIFIER="$3" CONFORMANCE_ALLOW_INCOMPLETE="$4" bash "$guard" >"$work/guard.log" 2>&1 || rc=$?
    if [ "$rc" = "$1" ]; then pass "the workflow's guard: $2 (exit $rc)"; else fail "the workflow's guard: $2: exit $rc, want $1: $(tail -n 1 "$work/guard.log")"; fi
  }
  guard_rc 1 "qualifier off with allow_incomplete is refused" off 1
  if grep -qF 'qualifier off with allow_incomplete' "$work/guard.log"; then pass "the guard's refusal names both inputs"; else fail "the guard's refusal does not name both inputs: $(tail -n 1 "$work/guard.log")"; fi
  guard_rc 0 "qualifier off alone runs" off ""
  guard_rc 0 "allow_incomplete alone runs" on 1
  guard_rc 0 "the defaults run" on ""
fi
for v in "CONFORMANCE_ALLOW_INCOMPLETE: \${{ inputs.allow_incomplete && '1' || '' }}" "CONFORMANCE_QUALIFIER: \${{ inputs.qualifier || 'on' }}"; do
  n="$(grep -cF -- "$v" "$wf" || true)"
  if [ "$n" -eq 2 ]; then pass "the guard and make conformance read $v"; else fail "$v is in the workflow $n times, want 2 (the guard and make conformance)"; fi
done

# ---- the reviewed baseline ------------------------------------------------------
bl="$work/baseline.json"
cat >"$bl" <<'JSON'
{"format": "conformance-baseline/v1", "target": "ussp-conformance", "from_run": "r", "from_digest": "sha256:0",
 "requirements": {"NAT-UNAUTH": {"status": "pass"},
  "F3411-SP": {"status": "not_applicable", "note": "blocked by owner decision Q34 (selftest)"}}}
JSON
cat >"$work/unnoted.json" <<'JSON'
{"format": "conformance-baseline/v1", "target": "ussp-conformance", "requirements": {"F3411-SP": {"status": "not_applicable"}}}
JSON
cat >"$work/report-na.json" <<'JSON'
{"requirements": [{"id": "NAT-UNAUTH", "role": "gate", "status": "pass"},
  {"id": "F3411-SP", "role": "gate", "status": "not_applicable"},
  {"id": "A11Y-PUBLIC", "role": "informative", "status": "not_applicable"}]}
JSON
cat >"$work/report-pass.json" <<'JSON'
{"requirements": [{"id": "NAT-UNAUTH", "role": "gate", "status": "pass"}, {"id": "F3411-SP", "role": "gate", "status": "pass"}]}
JSON
rm -f "$work/args"
hook_rc 2 "a baseline accepting not applicable without a note is refused" "${ext[@]}" USSP_CONFORMANCE_AUTHORITY_PUSH=off CONFORMANCE_BASELINE="$work/unnoted.json"
hook_rc 2 "a baseline that does not exist is refused" "${ext[@]}" USSP_CONFORMANCE_AUTHORITY_PUSH=off CONFORMANCE_BASELINE="$work/nowhere.json"
if [ -e "$work/args" ]; then fail "the suite ran with a refused baseline"; else pass "the suite never ran with a refused baseline"; fi
touch "$work/mark"
hook_rc 0 "a noted baseline is accepted" "${ext[@]}" USSP_CONFORMANCE_AUTHORITY_PUSH=off CONFORMANCE_BASELINE="$bl" FAKE_REPORT="$work/report-na.json"
if handed_args | grep -q '^--baseline$' && handed_args | grep -q 'baseline.json$'; then pass "--baseline handed to the suite"; else fail "--baseline missing: $(handed_args | tr '\n' ' ')"; fi
if grep -q 'NOT CHECKED, NOT A PASS: F3411-SP: blocked by owner decision Q34 (selftest)' "$work/log" &&
  grep -q 'the verdict is incomplete, not a pass' "$work/log" &&
  ! grep -q 'the suite passed' "$work/log" && ! grep -q 'A11Y-PUBLIC' "$work/log"; then
  pass "a gate requirement the baseline accepts is named as not a pass, with its note"
else
  fail "the accepted requirement was not named"
  sed 's/^/       /' "$work/log"
fi
info="$(find "$work/out" -name run-info.txt -newer "$work/mark" | sort | tail -n 1)"
if [ -n "$info" ] && grep -q '^not checked F3411-SP: blocked by owner decision Q34' "$info"; then pass "run-info.txt names it"; else fail "run-info.txt: ${info:-none}"; fi
hook_rc 0 "a run that checked every gate requirement" "${ext[@]}" USSP_CONFORMANCE_AUTHORITY_PUSH=off CONFORMANCE_BASELINE="$bl" FAKE_REPORT="$work/report-pass.json"
if grep -q 'NOT CHECKED' "$work/log" || ! grep -q 'the suite passed' "$work/log"; then
  fail "a complete run was reported as not checked"
  sed 's/^/       /' "$work/log"
else
  pass "a complete run names nothing as not checked and passes"
fi
# A known failure (WP-21 R6 to R8): accepted by the suite's gate while it
# fails only the recorded checks, never a pass. The pair: the same
# failure the baseline does not record fails the run, and names nothing
# as known failing. A known failure without its note or its failing
# checks is refused before anything runs.
cat >"$work/known.json" <<'JSON'
{"format": "conformance-baseline/v1", "target": "ussp-conformance", "from_run": "r", "from_digest": "sha256:0",
 "requirements": {"NAT-UNAUTH": {"status": "pass"},
  "F3411-SP": {"status": "fail", "note": "WP-21 R6 (selftest)", "failing": ["uss_qualifier astm.f3411.v22a.NET0260,Table1,1a"]}}}
JSON
cat >"$work/report-fail.json" <<'JSON'
{"requirements": [{"id": "NAT-UNAUTH", "role": "gate", "status": "pass"},
  {"id": "F3411-SP", "role": "gate", "status": "fail", "outcomes": {"pass": 21, "fail": 1, "not_applicable": 0}}]}
JSON
touch "$work/mark"
hook_rc 0 "a known failure the suite's gate accepts" "${ext[@]}" USSP_CONFORMANCE_AUTHORITY_PUSH=off CONFORMANCE_BASELINE="$work/known.json" FAKE_REPORT="$work/report-fail.json"
if grep -q 'KNOWN FAILING, NOT A PASS: F3411-SP: WP-21 R6 (selftest)' "$work/log" && grep -q 'the verdict is not a pass' "$work/log" &&
  ! grep -q 'the suite passed' "$work/log"; then
  pass "a known failure is named as not a pass, with its note"
else
  fail "the known failure was not named"
  sed 's/^/       /' "$work/log"
fi
info="$(find "$work/out" -name run-info.txt -newer "$work/mark" | sort | tail -n 1)"
if [ -n "$info" ] && grep -q '^known failing F3411-SP: WP-21 R6 (selftest)' "$info"; then pass "run-info.txt names the known failure"; else fail "run-info.txt: ${info:-none}"; fi
hook_rc 1 "a failure the baseline does not record fails the run" "${ext[@]}" USSP_CONFORMANCE_AUTHORITY_PUSH=off CONFORMANCE_BASELINE="$bl" FAKE_REPORT="$work/report-fail.json" FAKE_RC=1
if grep -q 'KNOWN FAILING' "$work/log"; then fail "an unrecorded failure was named known failing"; else pass "an unrecorded failure is not named known failing"; fi
jq '.requirements["F3411-SP"].note = ""' "$work/known.json" >"$work/known-unnoted.json"
jq '.requirements["F3411-SP"].failing = []' "$work/known.json" >"$work/known-unlisted.json"
rm -f "$work/args"
hook_rc 2 "a known failure without a note is refused" "${ext[@]}" USSP_CONFORMANCE_AUTHORITY_PUSH=off CONFORMANCE_BASELINE="$work/known-unnoted.json"
hook_rc 2 "a known failure without its failing checks is refused" "${ext[@]}" USSP_CONFORMANCE_AUTHORITY_PUSH=off CONFORMANCE_BASELINE="$work/known-unlisted.json"
if [ -e "$work/args" ]; then fail "the suite ran with a refused known failure"; else pass "the suite never ran with a refused known failure"; fi
# A run that exercises nothing (no injection reaches the USSP) fails only
# listed checks, so the suite's gate accepts it: the hook requires the
# known failure's minimum passed checks (deploy/conformance/exercised.json)
# and fails the run below it. The pair: the same failure with the
# minimum passed is accepted above.
cat >"$work/exercised.json" <<'JSON'
{"format": "ussp-conformance-exercised/v1", "requirements": {"F3411-SP": {"min_passed": 21}}}
JSON
cat >"$work/report-nothing-injected.json" <<'JSON'
{"requirements": [{"id": "NAT-UNAUTH", "role": "gate", "status": "pass"},
  {"id": "F3411-SP", "role": "gate", "status": "fail", "outcomes": {"pass": 2, "fail": 1, "not_applicable": 0}}]}
JSON
known_env=(CONFORMANCE_BASELINE="$work/known.json" CONFORMANCE_EXERCISED="$work/exercised.json")
hook_rc 0 "a known failure with its minimum passed checks is accepted" "${ext[@]}" USSP_CONFORMANCE_AUTHORITY_PUSH=off "${known_env[@]}" FAKE_REPORT="$work/report-fail.json"
if grep -q 'EXERCISED TOO LITTLE' "$work/log"; then fail "a known failure with its minimum was named exercised too little"; else pass "a known failure with its minimum is not named exercised too little"; fi
touch "$work/mark"
hook_rc 1 "a run where nothing was injected, the gate green: fails" "${ext[@]}" USSP_CONFORMANCE_AUTHORITY_PUSH=off "${known_env[@]}" FAKE_REPORT="$work/report-nothing-injected.json"
if grep -q 'EXERCISED TOO LITTLE: F3411-SP: 2 passed checks, the minimum is 21' "$work/log" && ! grep -q 'the suite passed\|the suite exited 0' "$work/log"; then
  pass "the under-exercised known failure is named with its count and minimum"
else
  fail "the under-exercised known failure was not named"
  sed 's/^/       /' "$work/log"
fi
info="$(find "$work/out" -name run-info.txt -newer "$work/mark" | sort | tail -n 1)"
if [ -n "$info" ] && grep -q '^exercised too little F3411-SP: 2 passed checks, minimum 21' "$info"; then pass "run-info.txt names it"; else fail "run-info.txt: ${info:-none}"; fi
hook_rc 1 "a suite exit 3 with a known failure exercised too little: 1" "${ext[@]}" USSP_CONFORMANCE_AUTHORITY_PUSH=off "${known_env[@]}" FAKE_REPORT="$work/report-nothing-injected.json" FAKE_RC=3
echo '{"requirements": [{"id": "NAT-UNAUTH", "role": "gate", "status": "pass"}]}' >"$work/report-unnamed.json"
hook_rc 1 "a report that does not name the known failure: fails" "${ext[@]}" USSP_CONFORMANCE_AUTHORITY_PUSH=off "${known_env[@]}" FAKE_REPORT="$work/report-unnamed.json"
hook_rc 1 "no report with known failures in the baseline: fails" "${ext[@]}" USSP_CONFORMANCE_AUTHORITY_PUSH=off "${known_env[@]}" FAKE_NO_REPORT=1
hook_rc 0 "no report and no baseline: the suite's status" "${ext[@]}" USSP_CONFORMANCE_AUTHORITY_PUSH=off CONFORMANCE_BASELINE=none FAKE_NO_REPORT=1
rm -f "$work/args"
echo '{"format": "ussp-conformance-exercised/v1", "requirements": {}}' >"$work/exercised-none.json"
jq '.requirements["F3411-SP"].min_passed = 0' "$work/exercised.json" >"$work/exercised-zero.json"
jq '.requirements["F3411-SP"].min_passed = "21"' "$work/exercised.json" >"$work/exercised-string.json"
hook_rc 2 "a known failure without a minimum is refused" "${ext[@]}" USSP_CONFORMANCE_AUTHORITY_PUSH=off CONFORMANCE_BASELINE="$work/known.json" CONFORMANCE_EXERCISED="$work/exercised-none.json"
hook_rc 2 "a minimum of 0 is refused" "${ext[@]}" USSP_CONFORMANCE_AUTHORITY_PUSH=off CONFORMANCE_BASELINE="$work/known.json" CONFORMANCE_EXERCISED="$work/exercised-zero.json"
hook_rc 2 "a minimum that is not a number is refused" "${ext[@]}" USSP_CONFORMANCE_AUTHORITY_PUSH=off CONFORMANCE_BASELINE="$work/known.json" CONFORMANCE_EXERCISED="$work/exercised-string.json"
hook_rc 2 "an exercised file that does not exist is refused" "${ext[@]}" USSP_CONFORMANCE_AUTHORITY_PUSH=off CONFORMANCE_BASELINE="$work/known.json" CONFORMANCE_EXERCISED="$work/nowhere.json"
if [ -e "$work/args" ]; then fail "the suite ran with a known failure not bounded"; else pass "the suite never ran with a known failure not bounded"; fi
hook_rc 3 "the suite's exit 3 with a baseline comes back unchanged" "${ext[@]}" USSP_CONFORMANCE_AUTHORITY_PUSH=off CONFORMANCE_BASELINE="$bl" FAKE_REPORT="$work/report-na.json" FAKE_RC=3
hook_rc 0 "CONFORMANCE_BASELINE=none runs without one" "${ext[@]}" USSP_CONFORMANCE_AUTHORITY_PUSH=off CONFORMANCE_BASELINE=none
if handed_args | grep -q '^--baseline$'; then fail "--baseline given with none"; else pass "--baseline absent with none"; fi

# The repository's own baseline: the default, valid, and F3411-SP and
# F3548-SCD recorded as known failures tracked by WP-21's findings (R6,
# R7; R8) with their failing checks, never as a pass.
repo_bl="$here/deploy/conformance/baseline.json"
for pair in "F3411-SP|R6" "F3411-SP|R7" "F3548-SCD|R8"; do
  id="${pair%%|*}"; r="${pair#*|}"
  if jq -e --arg id "$id" --arg r "$r" '.requirements[$id] | .status == "fail" and (.note | test("WP-21")) and (.note | contains($r + " ")) and (.note | test("not a pass")) and ((.failing // []) | length > 0)' "$repo_bl" >/dev/null; then
    pass "deploy/conformance/baseline.json records $id known failing, tracked by WP-21 $r, with its failing checks"
  else
    fail "deploy/conformance/baseline.json: $id is not recorded known failing with WP-21 $r and its failing checks"
  fi
done
# An injection the USSP refuses is never a known failure: with the
# injection checks accepted, a run where nothing reaches the USSP fails
# only listed checks and the gate stays green. The reviewed run
# 20261013T163501Z is such a run (every injection refused): at least one
# of its failing checks must be outside the baseline's list.
nothing_injected="$here/docs/RUNBOOKS/WP-21/20261013T163501Z-ussp-conformance/report.json"
uncovered="$(jq -r --slurpfile bl "$repo_bl" '[.outcomes[] | select(.requirement == "F3411-SP" and .status == "fail") | "\(.check) \(.subject)"]
  - ($bl[0].requirements["F3411-SP"].failing // []) | length' "$nothing_injected")"
if [ "${uncovered:-0}" -gt 0 ]; then
  pass "a run where no injection reached the USSP fails a check deploy/conformance/baseline.json does not accept ($uncovered)"
else
  fail "deploy/conformance/baseline.json accepts every failing check of a run where nothing was injected"
fi
if jq -e '[.requirements[] | (.failing // [])[]] | any(endswith(".rid.injection.UpsertTestSuccess"))' "$repo_bl" >/dev/null; then
  fail "deploy/conformance/baseline.json accepts UpsertTestSuccess as known failing"
else
  pass "deploy/conformance/baseline.json does not accept UpsertTestSuccess as known failing"
fi
# The repository's minimums would fail the reviewed run where nothing
# was injected (20261013T163501Z) and accept the reviewed run they come
# from (20261013T224233Z).
repo_ex="$here/deploy/conformance/exercised.json"
below() { jq -r --slurpfile ex "$repo_ex" '[.requirements[] | select(.role == "gate" and .status != "not_applicable")
  | select(($ex[0].requirements[.id].min_passed // 0) > .outcomes.pass) | .id] | join(" ")' "$1"; }
if [ -n "$(below "$nothing_injected")" ]; then pass "deploy/conformance/exercised.json fails the run where nothing was injected ($(below "$nothing_injected"))"; else fail "deploy/conformance/exercised.json accepts the run where nothing was injected"; fi
reviewed="$here/docs/RUNBOOKS/WP-21/20261013T224233Z-ussp-conformance/report.json"
if [ -z "$(below "$reviewed")" ]; then pass "deploy/conformance/exercised.json accepts the reviewed run"; else fail "deploy/conformance/exercised.json fails the reviewed run: $(below "$reviewed")"; fi
hook_rc 0 "the repository's baseline is the default and valid" "${ext[@]}" USSP_CONFORMANCE_AUTHORITY_PUSH=off
if handed_args | grep -q 'deploy/conformance/baseline.json$'; then pass "the default baseline is deploy/conformance/baseline.json"; else fail "default baseline: $(handed_args | tr '\n' ' ')"; fi

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
