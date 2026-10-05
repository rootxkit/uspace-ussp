# Sibling OpenAPI copies

The national APIs this system calls (the CISP, the authority, the ANSP)
are published by their own repositories. Each one is copied here as
`<system>.yaml`, unmodified, and pinned by one line in `SOURCE`
(reconciliation M11):

```
# <system> <owner/repo> <commit> <path in that repo>
cisp rootxkit/uspace-cisp 0123abc... api/openapi.yaml
```

Clients are generated from these copies, never written by hand.
`scripts/check-contracts.sh` (run by `make check-contracts` and in CI)
fetches every file at its pinned commit and fails on any difference, on
a copy without a `SOURCE` line and on a line without a copy. Updating a
copy is one commit that changes the file and its `SOURCE` line together.
When the lab's aggregate of the national APIs is published, it replaces
these copies.

Copies: `cisp.yaml` (WP-4, the CIS cache), generated into
`internal/cis/cispclient` with `api/oapi-codegen.cisp.yaml` (the
`datasets` and `subscriptions` tags only); `authority.yaml` (WP-5, the
registry validity cache), generated into `internal/registry/authclient`
with `api/oapi-codegen.authority.yaml` (the `registry-f8` tag only: the
F8 lookups and the change feed) and, with
`api/oapi-codegen.authority-status.yaml`, into `internal/status/authclient`
(the one operation `postCertificateStatus`: Art. 7(6) start, cease and
restart notices, WP-15) and, with
`api/oapi-codegen.authority-occurrence.yaml`, into
`internal/occurrence/authclient` (the one operation `createOccurrence`:
the occurrence reports of 376/2014 Art. 4, H-1); `ansp.yaml` (WP-15, the Annex V coordination
inbox), generated into `internal/coordination/anspclient` with
`api/oapi-codegen.ansp.yaml` (the `coordination` tag only). The ANSP's
file references its own body schema as
`../schemas/coordination/annex_v/v1.json`; `scripts/generate.sh` stages
the copy beside the pinned schema (`schemas/CONSUMED`) in the ANSP
repository's layout before generating.
