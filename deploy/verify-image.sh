#!/usr/bin/env bash
# Verifies the cosign signature and the SBOM attestation of uspace-ussp
# images before they are pulled (spec 06 §4, WP-20). deploy/deploy.sh
# runs it first; by hand:
#
#   deploy/verify-image.sh ghcr.io/rootxkit/uspace-ussp@sha256:<digest> \
#                          ghcr.io/rootxkit/uspace-ussp-web@sha256:<digest>
#
# Every reference must be by digest: a tag can move after it is checked.
# The signature must come from this repository's .github/workflows/ci.yml
# on main or on a v* tag, issued by GitHub's OIDC issuer (keyless, the
# `image` job); the SPDX SBOM attestation from the same identity (the
# `attest` job). cosign runs from the host when installed, else from the
# pinned image, so the host needs nothing but Docker. Exits non-zero on
# the first reference that does not verify, and says which check failed;
# nothing is pulled or changed here.
set -euo pipefail

COSIGN_IMAGE="${COSIGN_IMAGE:-gcr.io/projectsigstore/cosign:v2.4.1@sha256:b03690aa52bfe94054187142fba24dc54137650682810633901767d8a3e15b31}"
REPO="${USSP_SIGNER_REPO:-rootxkit/uspace-ussp}"
identity="^https://github\.com/${REPO//./\.}/\.github/workflows/ci\.yml@refs/(heads/main|tags/v[0-9][^ ]*)\$"
issuer=https://token.actions.githubusercontent.com

if [ "$#" -eq 0 ]; then
  echo "usage: deploy/verify-image.sh <image@sha256:digest>..." >&2
  exit 2
fi

# Every reference is checked before cosign runs for any.
for ref in "$@"; do
  if [[ ! "$ref" =~ ^[a-z0-9./-]+@sha256:[0-9a-f]{64}$ ]]; then
    echo "verify-image: $ref is not a reference by digest (repo@sha256:<64 hex>)" >&2
    exit 1
  fi
  case "$ref" in
    "ghcr.io/$REPO@"* | "ghcr.io/$REPO-web@"*) ;;
    *) echo "verify-image: $ref is not an image of ghcr.io/$REPO" >&2; exit 1 ;;
  esac
done

# type -P: the binary on PATH, never this function.
cosign() {
  if type -P cosign >/dev/null 2>&1; then
    command cosign "$@"
  else
    MSYS_NO_PATHCONV=1 docker run --rm "$COSIGN_IMAGE" "$@"
  fi
}

for ref in "$@"; do
  if ! cosign verify "$ref" --certificate-identity-regexp "$identity" \
      --certificate-oidc-issuer "$issuer" >/dev/null; then
    echo "verify-image: FAIL signature of $ref" >&2
    exit 1
  fi
  echo "verify-image: ok signature   $ref"
  if ! cosign verify-attestation --type spdxjson "$ref" \
      --certificate-identity-regexp "$identity" \
      --certificate-oidc-issuer "$issuer" >/dev/null; then
    echo "verify-image: FAIL SBOM attestation of $ref" >&2
    exit 1
  fi
  echo "verify-image: ok SBOM (spdx) $ref"
done
echo "verify-image: $# image(s) verified; pull them by these digests"
