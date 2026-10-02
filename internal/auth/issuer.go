package auth

import (
	"errors"
	"fmt"
	"time"

	coreauth "github.com/rootxkit/uspace-core/auth"
	"github.com/rootxkit/uspace-core/core"
)

// MaxOperatorTokenTTL bounds an operator machine token (cross-plan
// Appendix A: at most 1 h); policy.OperatorTokenTTLS is clipped to it.
const MaxOperatorTokenTTL = time.Hour

// MaxSessionTTL bounds a session token (M20: exp at most 12 h).
const MaxSessionTTL = 12 * time.Hour

// Issuer is this USSP's own token issuer (D9): operator machine tokens
// through core's Issuer, and portal and console sessions with the same
// key. iss is URL, aud is always Audience, this USSP's host
// (USSP_AUDIENCES[0], M18).
type Issuer struct {
	URL      string
	Audience string
	keys     *IssuerKeys
	core     *coreauth.Issuer
}

// NewIssuer builds the issuer of url for audience with keys.
func NewIssuer(url, audience string, keys *IssuerKeys) (*Issuer, error) {
	switch {
	case url == "":
		return nil, core.Fieldf("USSP_ISSUER_URL", "empty")
	case audience == "":
		return nil, core.Fieldf("USSP_AUDIENCES", "empty: an issuer needs this USSP's host as aud")
	case keys == nil:
		return nil, core.Fieldf("USSP_ISSUER_KEY_FILE", "no key")
	}
	ci, err := keys.Ring().Issuer(url)
	if err != nil {
		return nil, err
	}
	return &Issuer{URL: url, Audience: audience, keys: keys, core: ci}, nil
}

// Keys are the issuer's keys.
func (i *Issuer) Keys() *IssuerKeys { return i.keys }

// Issued is a signed operator token with what the audit records.
type Issued struct {
	Token     string
	JTI       string
	KID       string
	Scopes    []string
	ExpiresAt time.Time
}

// IssueOperator signs an operator machine token for clientID granting
// scopes (operator scopes only), valid for ttl (clipped to
// MaxOperatorTokenTTL) from now. jti is core's: 128 random bits, hex.
func (i *Issuer) IssueOperator(clientID string, scopes []string, ttl time.Duration, now time.Time) (Issued, error) {
	for _, s := range scopes {
		if !IsOperatorScope(s) {
			return Issued{}, fmt.Errorf("scope %s is not an operator scope", quote(s))
		}
	}
	if len(scopes) == 0 {
		return Issued{}, errors.New("an operator token grants at least one scope")
	}
	ttl = min(ttl, MaxOperatorTokenTTL)
	tok, err := i.core.Issue(clientID, i.Audience, scopes, ttl, now)
	if err != nil {
		return Issued{}, err
	}
	now = now.Truncate(time.Second)
	return Issued{Token: tok, JTI: peek(tok).JTI, KID: i.keys.Current.KID, Scopes: scopes, ExpiresAt: now.Add(ttl)}, nil
}

// IssueSession signs a session token (M20): sub is the account id, jti
// the session id, exp at most MaxSessionTTL after now.
func (i *Issuer) IssueSession(sub, realm, jti string, roles []string, ttl time.Duration, now time.Time) (string, time.Time, error) {
	if _, ok := RealmRoles[realm]; !ok {
		return "", time.Time{}, fmt.Errorf("realm %s does not exist", quote(realm))
	}
	now = now.Truncate(time.Second)
	exp := now.Add(min(ttl, MaxSessionTTL))
	tok, err := signSession(i.keys.Current, SessionClaims{
		Issuer: i.URL, Audience: i.Audience, Subject: sub, Roles: roles, Realm: realm, JTI: jti, IssuedAt: now, ExpiresAt: exp,
	})
	return tok, exp, err
}

// VerifierIssuer is the allow-list entry of this issuer for core's
// Verifier: the ring's JWKS as static keys (no fetch of our own JWKS).
func (i *Issuer) VerifierIssuer() coreauth.IssuerConfig {
	return coreauth.IssuerConfig{Keys: i.keys.Ring().JWKS()}
}
