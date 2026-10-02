#!/usr/bin/env bash
# Generates the RSA key of this USSP's own token issuer
# (USSP_ISSUER_KEY_FILE) into local/ (git-ignored): 3072 bits, PKCS #8
# PEM, readable by its owner only. Its kid is the RFC 7638 thumbprint,
# computed by api at start. Refuses to overwrite a key: a rotation keeps
# the old one as USSP_ISSUER_PREVIOUS_KEY_FILE, so move it there first.
#
#   scripts/gen-issuer-key.sh                 # local/issuer-key.pem
#   scripts/gen-issuer-key.sh local/next.pem  # another path
#
# With --mfa it writes local/mfa-key instead: the base64 of 32 random
# bytes, the key that seals staff TOTP secrets (USSP_MFA_KEY_FILE).
set -euo pipefail
cd "$(dirname "$0")/.."
umask 077
mkdir -p local

if [ "${1:-}" = "--mfa" ]; then
  out=${2:-local/mfa-key}
  if [ -e "$out" ]; then echo "gen-issuer-key: $out exists; not overwritten" >&2; exit 1; fi
  openssl rand -base64 32 >"$out"
  echo "gen-issuer-key: wrote $out (USSP_MFA_KEY_FILE)"
  exit 0
fi

out=${1:-local/issuer-key.pem}
if [ -e "$out" ]; then echo "gen-issuer-key: $out exists; not overwritten" >&2; exit 1; fi
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:3072 -out "$out" 2>/dev/null
# Read it back: a key that does not parse must not be reported as written.
openssl pkey -in "$out" -noout
echo "gen-issuer-key: wrote $out (USSP_ISSUER_KEY_FILE)"
