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

// operator is the test operator's id at the USSP: GET /v1/accounts/me
// with the operator_admin's portal session, remembered once read.
func (s *Server) operator(ctx context.Context, portal *client.ClientWithResponses) (client.OperatorID, error) {
	var none client.OperatorID
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if s.operatorID != none {
		return s.operatorID, nil
	}
	resp, err := portal.GetMeWithResponse(ctx)
	if err != nil {
		return none, fmt.Errorf("GET /v1/accounts/me: the USSP did not answer: %w", err)
	}
	if resp.JSON200 == nil {
		return none, problemOf(resp.StatusCode(), resp.Body)
	}
	var id client.OperatorID
	if resp.JSON200.OperatorId == nil || id.UnmarshalText([]byte(*resp.JSON200.OperatorId)) != nil {
		return none, &RefusalError{Status: http.StatusForbidden, Reason: "no_operator", Detail: "the portal session names no operator"}
	}
	s.operatorID = id
	return id, nil
}

// bind binds every planned flight's serial to the test operator's client
// through the portal's binding route (POST
// /v1/accounts/operators/{id}/clients/{id}/serials): the real path, a
// binding of the same client again is the same binding, one of another
// client a refusal (409, permanent). Without a portal session nothing
// is bound here; the hook binds the fixture's pool at seed time and an
// unbound serial is then the ingest's refusal.
func (s *Server) bind(ctx context.Context, plans []planned) error {
	if s.USSP.Session == "" {
		return nil
	}
	portal, err := s.USSP.Portal()
	if err != nil {
		return err
	}
	op, err := s.operator(ctx, portal)
	if err != nil {
		return err
	}
	for _, p := range plans {
		resp, err := portal.BindSerialWithResponse(ctx, op, s.cfg.OperatorClientID, client.SerialBindingRequest{Serial: p.serial})
		if err != nil {
			return fmt.Errorf("binding %s: the USSP did not answer: %w", p.serial, err)
		}
		if resp.JSON201 == nil {
			ref := problemOf(resp.StatusCode(), resp.Body)
			ref.Detail = "binding " + p.serial + ": " + ref.Detail
			return ref
		}
		s.Counters.Inc(CounterBindingsMade)
	}
	return nil
}
