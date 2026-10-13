package testif

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/national/client"
	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// The operator scopes the test operator's client uses: telemetry for the
// injection, intents for the flight plans (the binding is the portal
// session's).
var operatorScopes = []string{auth.ScopeTelemetry, auth.ScopeIntents}

// maxSecretBytes bounds a credential file read.
const maxSecretBytes = 16 << 10

// readSecret reads a credential file: at most maxSecretBytes, trimmed,
// not empty. The error names the file, never its content.
func readSecret(path string) (string, error) {
	if path == "" {
		return "", errors.New("no file configured")
	}
	f, err := os.Open(path) //nolint:gosec // G304: the path is configuration (USSP_TEST_INTERFACES_*_FILE), never request input
	if err != nil {
		return "", fmt.Errorf("%s cannot be read", path)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxSecretBytes+1))
	if err != nil || len(b) > maxSecretBytes {
		return "", fmt.Errorf("%s cannot be read or is larger than %d bytes", path, maxSecretBytes)
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return s, nil
}

// USSP is this USSP as the test operator reaches it: its published base
// URL, an HTTP client that trusts its certificate, the operator client's
// token and the two session files. Nothing else: no database, no NATS
// (D13).
type USSP struct {
	Base        string
	HTTP        *http.Client
	Tokens      *auth.Outgoing
	Session     string // file of the operator_admin's portal session
	Console     string // file of a staff console session
	CallTimeout time.Duration
}

// NewUSSP builds the USSP client of cfg; the operator secret is read
// once, here.
func NewUSSP(cfg Config) (*USSP, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("USSP_TEST_INTERFACES_CA_FILE: %s cannot be read", cfg.CAFile)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("USSP_TEST_INTERFACES_CA_FILE: %s holds no PEM certificate", cfg.CAFile)
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	timeout := time.Duration(cfg.CallTimeoutS) * time.Second
	hc := &http.Client{Transport: tr, Timeout: timeout}
	secret, err := readSecret(cfg.OperatorSecretFile)
	if err != nil {
		return nil, fmt.Errorf("USSP_TEST_INTERFACES_OPERATOR_SECRET_FILE: %w", err)
	}
	tokens, err := auth.NewOutgoing(auth.OutgoingConfig{TokenURL: cfg.USSPBaseURL + "/oauth/token",
		ClientID: cfg.OperatorClientID, ClientSecret: secret, HTTPClient: hc})
	if err != nil {
		return nil, err
	}
	return &USSP{Base: cfg.USSPBaseURL, HTTP: hc, Tokens: tokens, Session: cfg.SessionFile, Console: cfg.ConsoleSessionFile, CallTimeout: timeout}, nil
}

// OperatorToken is a token of the test operator's client for this USSP.
func (u *USSP) OperatorToken(ctx context.Context) (string, error) {
	return u.Tokens.Token(ctx, u.Base, operatorScopes...)
}

func bearer(tok string) client.RequestEditorFn {
	return func(_ context.Context, r *http.Request) error {
		r.Header.Set("Authorization", "Bearer "+tok)
		return nil
	}
}

// Operator is the national client with the operator client's token.
func (u *USSP) Operator(ctx context.Context) (*client.ClientWithResponses, error) {
	tok, err := u.OperatorToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("the test operator's token: %w", err)
	}
	return client.NewClientWithResponses(u.Base, client.WithHTTPClient(u.HTTP), client.WithRequestEditorFn(bearer(tok)))
}

// sessionClient is the national client with the session of file, read
// at each use so that the hook may replace an expired one.
func (u *USSP) sessionClient(file, what string) (*client.ClientWithResponses, error) {
	tok, err := readSecret(file)
	if err != nil {
		return nil, fmt.Errorf("the %s session: %w", what, err)
	}
	return client.NewClientWithResponses(u.Base, client.WithHTTPClient(u.HTTP), client.WithRequestEditorFn(bearer(tok)))
}

// Portal is the national client with the operator_admin's portal session.
func (u *USSP) Portal() (*client.ClientWithResponses, error) {
	return u.sessionClient(u.Session, "portal")
}

// Staff is the national client with the staff console session.
func (u *USSP) Staff() (*client.ClientWithResponses, error) {
	return u.sessionClient(u.Console, "console")
}

// Probe is the readiness of the USSP's api as an operator sees it:
// GET /readyz through the published base URL.
func (u *USSP) Probe(ctx context.Context) (obs.State, string) {
	c, err := client.NewClientWithResponses(u.Base, client.WithHTTPClient(u.HTTP))
	if err != nil {
		return obs.StateDown, err.Error()
	}
	resp, err := c.GetReadyzWithResponse(ctx)
	switch {
	case err != nil:
		return obs.StateDown, "the USSP does not answer at its base URL"
	case resp.StatusCode() == http.StatusOK:
		return obs.StateUp, ""
	case resp.StatusCode() == http.StatusServiceUnavailable:
		return obs.StateDegraded, "the USSP's api answers /readyz 503 (not ready)"
	default:
		return obs.StateDown, fmt.Sprintf("the USSP's api answers /readyz %d", resp.StatusCode())
	}
}
