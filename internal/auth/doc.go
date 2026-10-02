// Package auth is the token side of uspace-ussp, wired on uspace-core's
// auth package (CLAUDE.md rule 3: JWT verification lives once, in core).
//
//   - Verifier: core's Verifier for every token this system accepts:
//     ecosystem tokens of the allow-listed issuers (USSP_TOKEN_ISSUERS:
//     the authority's token service, the lab issuer in the lab) and the
//     tokens of this USSP's own issuer, with aud in USSP_AUDIENCES (M18;
//     USSP_SYSTEM_ID is the USSP code and never an audience) and
//     StrictSessionClaims. The ecosystem JWKS is fetched by core; a
//     process that starts while the token service is down keeps
//     starting and tries again (B-08), and /readyz reports jwks with the
//     age of the cached keys during an outage (T5, E-02).
//   - Guard: the httpx.Guard of the national router. Every operation has
//     an httpx.Access entry (GuardedMux fails closed); the guard admits
//     a machine token granting one of the entry's scopes (operator scopes
//     only from this issuer, ecosystem scopes only from the ecosystem)
//     or a session of the entry's realm and roles, after the session row
//     says it is live. Refusals are RFC 9457 problems typed by core's
//     TokenError counter or the guard's slug, counted, and audited with
//     the token's sub as actor when it parses, "unknown" otherwise.
//   - Issuer: this USSP's own issuer from USSP_ISSUER_KEY_FILE (kid = the
//     RFC 7638 thumbprint; USSP_ISSUER_PREVIOUS_KEY_FILE published during
//     a rotation): operator machine tokens through core's Issuer
//     (aud = this host, at most one hour), and the portal and console
//     session tokens of M20 with the same key.
//   - WSAuth: the WebSocket upgrade of M22: the uspace_session cookie
//     plus an Origin on USSP_WS_ALLOWED_ORIGINS, or a bearer token; a
//     refusal is closed with 4401.
//   - Outgoing: the client-credentials client for the calls this USSP
//     makes, one token per (audience, scope set) until 60 s before exp.
//   - Bindings: the client_bindings projection (client_id -> serial
//     folds) through a projector interface; MemoryBindings until WP-6.
//   - Hasher: argon2id for passwords and client secrets, in constant
//     time, with a dummy verification for unknown accounts.
//
// Nothing here logs or returns a token, a secret or a cookie value:
// errors name the claim or the field, never the credential.
package auth
