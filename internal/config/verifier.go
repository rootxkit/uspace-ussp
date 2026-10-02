package config

import (
	"github.com/rootxkit/uspace-core/auth"
	"github.com/rootxkit/uspace-core/core"
)

// VerifierConfig is the core/auth.Config of every token verifier in this
// system: the allow-listed USSP_TOKEN_ISSUERS with their JWKS URLs, the
// accepted audiences USSP_AUDIENCES (M18; USSP_SYSTEM_ID is never an
// audience) and StrictSessionClaims on, so a session token whose roles
// is not an array of strings or whose realm is not a string is refused
// as rejected_claims instead of being read as having no role (M20,
// uspace-core v1.1.0). It is the one place an auth.Config is built;
// internal/auth (WP-2) passes it to auth.NewVerifier unchanged.
func (c Config) VerifierConfig() (auth.Config, error) {
	if len(c.TokenIssuers) == 0 {
		return auth.Config{}, &core.FieldError{Field: "USSP_TOKEN_ISSUERS", Reason: "required to verify tokens"}
	}
	if len(c.Audiences) == 0 {
		return auth.Config{}, &core.FieldError{Field: "USSP_AUDIENCES", Reason: "required to verify tokens"}
	}
	issuers, err := ParseIssuers(c.TokenIssuers)
	if err != nil {
		return auth.Config{}, &core.FieldError{Field: "USSP_TOKEN_ISSUERS", Reason: err.Error()}
	}
	allow := make(map[string]auth.IssuerConfig, len(issuers))
	for _, iss := range issuers {
		allow[iss.Issuer] = auth.IssuerConfig{JWKSURL: iss.JWKSURL}
	}
	return auth.Config{
		Issuers:             allow,
		Audiences:           append([]string(nil), c.Audiences...),
		StrictSessionClaims: true,
	}, nil
}

// CompactConfig is the core/auth.CompactConfig of the CIS change
// notification receiver (POST /v1/cis/notifications): the issuers of
// USSP_CIS_NOTIFY_ISSUERS (the CISP and, on its degraded direct path,
// the ANSP; M5) with their JWKS URLs, and the audiences USSP_AUDIENCES
// (aud is this host, M19).
func (c Config) CompactConfig() (auth.CompactConfig, error) {
	if len(c.CISNotifyIssuers) == 0 {
		return auth.CompactConfig{}, &core.FieldError{Field: "USSP_CIS_NOTIFY_ISSUERS", Reason: "required to receive CIS notifications"}
	}
	if len(c.Audiences) == 0 {
		return auth.CompactConfig{}, &core.FieldError{Field: "USSP_AUDIENCES", Reason: "required to receive CIS notifications"}
	}
	issuers, err := ParseIssuers(c.CISNotifyIssuers)
	if err != nil {
		return auth.CompactConfig{}, &core.FieldError{Field: "USSP_CIS_NOTIFY_ISSUERS", Reason: err.Error()}
	}
	allow := make(map[string]auth.IssuerConfig, len(issuers))
	for _, iss := range issuers {
		allow[iss.Issuer] = auth.IssuerConfig{JWKSURL: iss.JWKSURL}
	}
	return auth.CompactConfig{Issuers: allow, Audiences: append([]string(nil), c.Audiences...)}, nil
}
