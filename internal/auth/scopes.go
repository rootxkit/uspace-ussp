package auth

import (
	"fmt"
	"slices"
	"strings"

	"github.com/rootxkit/uspace-ussp/internal/httpx"
)

// The scopes of this USSP's own issuer (cross-plan Appendix B: "at the
// USSP issuer only"). Operator clients hold a subset of them.
const (
	ScopeIntents   = "ussp.intents"
	ScopeTelemetry = "ussp.telemetry"
	ScopeTraffic   = "ussp.traffic"
	ScopeGeo       = "ussp.geo"
)

// SessionScope is the scope claim of every session token (M20); it is
// not a catalogue scope and no route requires it as one.
const SessionScope = "session"

// OperatorScopes are the scopes an operator client may hold, in the
// order the catalogue lists them.
var OperatorScopes = []string{ScopeIntents, ScopeTelemetry, ScopeTraffic, ScopeGeo}

// EcosystemScopes is the reconciled catalogue of spec 06 §3 (cross-plan
// M23, Appendix B, held by authority WP-2) that tokens of the ecosystem
// issuers carry. rid.observe is not a JWT scope; console roles are not
// scopes.
var EcosystemScopes = []string{
	"cis.read", "cis.publish:zones", "cis.publish:uspace", "cis.publish:ussp_list",
	"cis.publish:restrictions", "cis.publish:ats_data",
	"registry.validate", "ussp.records", "ansp.traffic", "ansp.coordination", "ansp.requests",
	"occurrences.write", "certificates.status", "police.query", "dp.observe",
	"utm.strategic_coordination", "utm.constraint_processing", "utm.constraint_management",
	"utm.conformance_monitoring_sa", "utm.availability_arbitration",
	"rid.service_provider", "rid.display_provider",
}

// The session realms (M20) and the roles each one carries.
const (
	RealmPortal  = "portal"
	RealmConsole = "console"
)

// Portal roles (operator users) and console roles (staff).
const (
	RoleOperatorAdmin = "operator_admin"
	RoleRemotePilot   = "remote_pilot"
	RoleViewer        = "viewer"
	RoleSupervisor    = "supervisor"
	RoleSupport       = "support"
	RoleAdmin         = "admin"
)

// RealmRoles lists the roles of each realm.
var RealmRoles = map[string][]string{
	RealmPortal:  {RoleOperatorAdmin, RoleRemotePilot, RoleViewer},
	RealmConsole: {RoleSupervisor, RoleSupport, RoleAdmin},
}

// IsOperatorScope reports whether s is one of this issuer's scopes.
func IsOperatorScope(s string) bool { return slices.Contains(OperatorScopes, s) }

// KnownScope reports whether s is in the catalogue (operator or
// ecosystem).
func KnownScope(s string) bool { return IsOperatorScope(s) || slices.Contains(EcosystemScopes, s) }

// ParseOperatorScopes splits a space-separated scope parameter and
// refuses an unknown or a non-operator scope, naming it. Duplicates are
// dropped; the order is the catalogue's.
func ParseOperatorScopes(raw string) ([]string, error) {
	var out []string
	for s := range strings.FieldsSeq(raw) {
		if !IsOperatorScope(s) {
			return nil, fmt.Errorf("scope %s is not an operator scope of this issuer", quote(s))
		}
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b string) int {
		return slices.Index(OperatorScopes, a) - slices.Index(OperatorScopes, b)
	})
	return out, nil
}

// ValidateAccess refuses an access-table entry that names a scope
// outside the catalogue, a realm that does not exist or a role the realm
// does not have (brief WP-2: an unknown scope is refused). The
// GuardedMux calls it for every route at start.
func ValidateAccess(a httpx.Access) error {
	for _, s := range a.Scopes {
		if !KnownScope(s) {
			return fmt.Errorf("scope %s is not in the catalogue (cross-plan Appendix B)", quote(s))
		}
	}
	for _, sa := range a.Sessions {
		roles, ok := RealmRoles[sa.Realm]
		if !ok {
			return fmt.Errorf("realm %s does not exist", quote(sa.Realm))
		}
		for _, r := range sa.Roles {
			if !slices.Contains(roles, r) {
				return fmt.Errorf("role %s is not a role of realm %s", quote(r), sa.Realm)
			}
		}
	}
	return nil
}

// quote renders an untrusted value for an error text: quoted and cut
// at 64 bytes.
func quote(s string) string {
	if len(s) > 64 {
		s = s[:64] + "..."
	}
	return fmt.Sprintf("%q", s)
}
