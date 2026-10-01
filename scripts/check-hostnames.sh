#!/usr/bin/env bash
# No production hostname in code or configuration (CLAUDE.md engineering
# rules, spec 00 §7): `chikox.net` may appear only under deploy/staging/.
# Markdown is exempt: the plan and the rules name the staging hosts to
# state the audience rule (M18), which is documentation, not
# configuration.
set -euo pipefail
cd "$(dirname "$0")/.."

if hits="$(git grep --untracked -n -I -i 'chikox\.net' -- . ':!deploy/staging/**' ':!*.md' ':!scripts/check-hostnames.sh')"; then
  echo "check-hostnames: a staging hostname outside deploy/staging/:" >&2
  echo "$hits" >&2
  exit 1
fi
echo "check-hostnames: no staging hostname outside deploy/staging/ in $(git ls-files -co --exclude-standard | wc -l | tr -d ' ') files"
