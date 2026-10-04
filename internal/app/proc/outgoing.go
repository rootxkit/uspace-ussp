package proc

import (
	"net/url"
	"os"
	"strings"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/config"
)

// OutgoingTokens is the token client of the calls a process makes (POST
// /oauth/token on the origin that serves the first USSP_TOKEN_ISSUERS
// entry's JWKS, the token service; client ussp-<code>-01), or nil when no
// client secret is configured.
func OutgoingTokens(cfg config.Config) (*auth.Outgoing, error) {
	if len(cfg.TokenIssuers) == 0 || cfg.TokenClientSecretFile == "" {
		return nil, nil
	}
	issuers, err := config.ParseIssuers(cfg.TokenIssuers)
	if err != nil {
		return nil, core.Fieldf("USSP_TOKEN_ISSUERS", "%v", err)
	}
	b, err := os.ReadFile(cfg.TokenClientSecretFile)
	if err != nil {
		return nil, core.Fieldf("USSP_TOKEN_CLIENT_SECRET_FILE", "cannot be read")
	}
	secret := strings.TrimSpace(string(b))
	if secret == "" {
		return nil, core.Fieldf("USSP_TOKEN_CLIENT_SECRET_FILE", "is empty")
	}
	tokenURL, err := url.Parse(issuers[0].JWKSURL)
	if err != nil {
		return nil, core.Fieldf("USSP_TOKEN_ISSUERS", "the JWKS URL does not parse")
	}
	tokenURL.Path, tokenURL.RawQuery, tokenURL.Fragment = "/oauth/token", "", ""
	return auth.NewOutgoing(auth.OutgoingConfig{
		TokenURL: tokenURL.String(),
		ClientID: auth.ClientIDFor(cfg.SystemID), ClientSecret: secret,
	})
}
