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

No copy is here yet: the first arrives with the work package that first
calls a sibling (WP-5, the authority's registry).
