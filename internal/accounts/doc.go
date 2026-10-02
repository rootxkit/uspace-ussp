// Package accounts is the USSP's own customer records and the people
// and machines that act for them (spec 01 §3 users table, 06 T3):
//
//   - operator self-registration with the authority registration number,
//     pending_validation until the registry (F8, WP-5's checker behind
//     RegistryChecker) says valid; "unknown" and an unreachable registry
//     keep it pending, any other answer refuses it;
//   - portal users of an operator (operator_admin, remote_pilot, viewer)
//     and staff accounts (supervisor, support, admin) with argon2id
//     passwords; a staff admin also proves a TOTP code (RFC 6238), its
//     secret sealed under USSP_MFA_KEY_FILE, each code accepted once;
//   - OAuth2 clients of an active operator with operator scopes, the
//     secret shown once, rotation with an overlap of
//     policy.client_secret_overlap_s for the previous secret;
//   - client-serial bindings validated by uspace-core serial and
//     projected to client_bindings in the same transaction (B-09);
//   - sign-in with a per-address limiter (per process) and a
//     per-username lockout in the database (across replicas), sessions
//     as rows (logout, idle end) checked on every request.
//
// Every change and every refusal is an events row (store.Audit). The
// service implements the interfaces internal/auth asks of a store:
// ClientStore and TokenAuditor for the token endpoint, SessionChecker
// and Auditor for the guard.
package accounts
