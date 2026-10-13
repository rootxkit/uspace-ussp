package testif

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"

	"github.com/rootxkit/uspace-ussp/internal/national/client"
)

// The files Seed writes in its state directory (all 0600; tokens and
// secrets only ever there, never printed).
const (
	SeedAdminPassword = "admin-password"
	SeedPortalSession = "portal-session"
	SeedClientID      = "operator-client-id"
	SeedClientSecret  = "operator-secret"
	SeedConsole       = "console-session"
	SeedResultFile    = "seed.json"
)

// ClientUnavailable is the client id Seed writes when the operator could
// not be given a client (it is not active: the registry has not said
// valid). test-interfaces then starts, and every call it makes fails
// with the token service's refusal, which the run records.
const ClientUnavailable = "unavailable"

// SeedConfig is what Seed needs: the USSP as the hook reaches it from
// the stack's network, the fixture, the state directory, and optionally
// a staff user (created by `ussp-api staff-add`) for the console session.
type SeedConfig struct {
	USSP                *USSP
	Fixture             Fixture
	StateDir            string
	ConsoleUser         string
	ConsolePasswordFile string
}

// SeedResult is what Seed did, written to seed.json for the run's record.
type SeedResult struct {
	OperatorID     string   `json:"operator_id"`
	OperatorStatus string   `json:"operator_status"`
	Validation     string   `json:"validation_status"`
	ClientID       string   `json:"client_id"`
	Bound          []string `json:"bound"`
	NotBound       []string `json:"not_bound"`
	Console        bool     `json:"console_session"`
	Notes          []string `json:"notes"`
}

// writeState writes name in dir, 0600, through a rename.
func writeState(dir, name, value string) error {
	p := filepath.Join(dir, name)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(value+"\n"), 0o600); err != nil {
		return fmt.Errorf("%s cannot be written", p)
	}
	if err := os.Rename(tmp, p); err != nil {
		return fmt.Errorf("%s cannot be written", p)
	}
	return nil
}

// stateOrNew reads name in dir, or writes a new random password there.
func stateOrNew(dir, name string) (string, error) {
	if v, err := readSecret(filepath.Join(dir, name)); err == nil {
		return v, nil
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	v := "Wp21-" + base64.RawURLEncoding.EncodeToString(b)
	return v, writeState(dir, name, v)
}

// Seed creates the fixture's test operator at the USSP through the
// national API only, never by SQL (brief WP-21): the self-registration
// (POST /v1/accounts/operators; 409 when it exists: the same operator),
// the operator_admin's portal session, then, once the operator is
// active, a machine client with the fixture's scopes and the binding of
// every pool serial; and a staff console session when a staff user is
// given. Re-run on the same state directory it reuses the password and
// the client. What it could not do (the operator pending validation:
// the lab registry seed) is in the result, not an error: the run goes on
// and records it.
func Seed(ctx context.Context, cfg SeedConfig, logger *slog.Logger) (SeedResult, error) {
	res := SeedResult{Bound: []string{}, NotBound: []string{}, Notes: []string{}}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return res, fmt.Errorf("%s cannot be created", cfg.StateDir)
	}
	fx := cfg.Fixture
	pw, err := stateOrNew(cfg.StateDir, SeedAdminPassword)
	if err != nil {
		return res, err
	}
	anon, err := client.NewClientWithResponses(cfg.USSP.Base, client.WithHTTPClient(cfg.USSP.HTTP))
	if err != nil {
		return res, err
	}
	reg, err := anon.RegisterOperatorWithResponse(ctx, client.OperatorRegistration{
		RegistrationNumber: fx.Operator.RegistrationNumber, DisplayName: fx.Operator.DisplayName,
		ContactEmail: fx.Operator.ContactEmail, AdminUsername: fx.Operator.AdminUsername, AdminPassword: pw,
	})
	switch {
	case err != nil:
		return res, fmt.Errorf("POST /v1/accounts/operators: the USSP did not answer: %w", err)
	case reg.JSON201 != nil:
		logger.Info("test operator registered", slog.String("operator_id", reg.JSON201.Id), slog.String("status", string(reg.JSON201.Status)))
	case reg.StatusCode() == http.StatusConflict:
		res.Notes = append(res.Notes, "the operator was registered before: signing in as its admin")
	default:
		return res, fmt.Errorf("POST /v1/accounts/operators: %w", problemOf(reg.StatusCode(), reg.Body))
	}
	login, err := anon.LoginWithResponse(ctx, client.LoginRequest{Realm: client.Portal, Username: fx.Operator.AdminUsername, Password: pw})
	if err != nil {
		return res, fmt.Errorf("POST /v1/accounts/login: the USSP did not answer: %w", err)
	}
	if login.JSON200 == nil || login.JSON200.OperatorId == nil {
		return res, fmt.Errorf("the operator_admin's sign-in: %w", problemOf(login.StatusCode(), login.Body))
	}
	if err := writeState(cfg.StateDir, SeedPortalSession, login.JSON200.Token); err != nil {
		return res, err
	}
	res.OperatorID = *login.JSON200.OperatorId
	cfg.USSP.Session = filepath.Join(cfg.StateDir, SeedPortalSession)
	portal, err := cfg.USSP.Portal()
	if err != nil {
		return res, err
	}
	var opID client.OperatorID
	if err := opID.UnmarshalText([]byte(res.OperatorID)); err != nil {
		return res, fmt.Errorf("the session names operator %q, not a UUID", clip(res.OperatorID, 40))
	}
	op, err := portal.GetOperatorWithResponse(ctx, opID)
	if err != nil || op.JSON200 == nil {
		return res, fmt.Errorf("GET /v1/accounts/operators/{id}: %w", errors.Join(err, problemOrNil(op)))
	}
	res.OperatorStatus = string(op.JSON200.Status)
	if op.JSON200.ValidationStatus != nil {
		res.Validation = *op.JSON200.ValidationStatus
	}
	clientID, err := seedClient(ctx, cfg, portal, opID, res.OperatorStatus, &res)
	if err != nil {
		return res, err
	}
	res.ClientID = clientID
	if clientID != ClientUnavailable {
		for _, sn := range fx.Serials {
			b, err := portal.BindSerialWithResponse(ctx, opID, clientID, client.SerialBindingRequest{Serial: sn})
			if err != nil || b.JSON201 == nil {
				res.NotBound = append(res.NotBound, sn)
				res.Notes = append(res.Notes, fmt.Sprintf("binding %s: %v", sn, errors.Join(err, problemOrNil(b))))
				continue
			}
			res.Bound = append(res.Bound, sn)
		}
	}
	if cfg.ConsoleUser != "" {
		cpw, err := readSecret(cfg.ConsolePasswordFile)
		if err != nil {
			return res, fmt.Errorf("the console password: %w", err)
		}
		cl, err := anon.LoginWithResponse(ctx, client.LoginRequest{Realm: client.Console, Username: cfg.ConsoleUser, Password: cpw})
		if err != nil || cl.JSON200 == nil {
			res.Notes = append(res.Notes, fmt.Sprintf("the staff console sign-in: %v", errors.Join(err, problemOrNil(cl))))
		} else {
			if err := writeState(cfg.StateDir, SeedConsole, cl.JSON200.Token); err != nil {
				return res, err
			}
			res.Console = true
		}
	}
	b, _ := json.MarshalIndent(res, "", "  ")
	return res, writeState(cfg.StateDir, SeedResultFile, string(b))
}

// seedClient is the test operator's machine client: the one of the
// state directory when its files are there, else a new one; the
// placeholder when the operator is not active (no client may be made).
func seedClient(ctx context.Context, cfg SeedConfig, portal *client.ClientWithResponses, opID client.OperatorID, status string, res *SeedResult) (string, error) {
	if id, err := readSecret(filepath.Join(cfg.StateDir, SeedClientID)); err == nil && id != ClientUnavailable {
		if _, err := readSecret(filepath.Join(cfg.StateDir, SeedClientSecret)); err == nil {
			res.Notes = append(res.Notes, "the client of an earlier seed is reused")
			return id, nil
		}
	}
	if status != "active" {
		res.Notes = append(res.Notes, "the operator is "+status+", not active: no client can be created (the registry must answer valid for "+
			cfg.Fixture.Operator.RegistrationNumber+"; the lab registry seed, brief WP-21 'Blocked by')")
		if err := writeState(cfg.StateDir, SeedClientID, ClientUnavailable); err != nil {
			return "", err
		}
		return ClientUnavailable, writeState(cfg.StateDir, SeedClientSecret, ClientUnavailable)
	}
	scopes := make([]client.OperatorScope, 0, len(cfg.Fixture.Client.Scopes))
	for _, s := range cfg.Fixture.Client.Scopes {
		scopes = append(scopes, client.OperatorScope(s))
	}
	slices.Sort(scopes)
	cr, err := portal.CreateClientWithResponse(ctx, opID, client.ClientRequest{Scopes: scopes})
	if err != nil || cr.JSON201 == nil {
		return "", fmt.Errorf("POST /v1/accounts/operators/{id}/clients: %w", errors.Join(err, problemOrNil(cr)))
	}
	if err := writeState(cfg.StateDir, SeedClientSecret, cr.JSON201.ClientSecret); err != nil {
		return "", err
	}
	return cr.JSON201.ClientId, writeState(cfg.StateDir, SeedClientID, cr.JSON201.ClientId)
}

// response is the part of a generated response problemOrNil reads.
type response interface {
	StatusCode() int
}

// problemOrNil is the refusal of a generated response, nil without one.
func problemOrNil(r response) error {
	if r == nil {
		return nil
	}
	switch v := r.(type) {
	case *client.GetOperatorResponse:
		if v == nil {
			return nil
		}
		return problemOf(v.StatusCode(), v.Body)
	case *client.BindSerialResponse:
		if v == nil {
			return nil
		}
		return problemOf(v.StatusCode(), v.Body)
	case *client.LoginResponse:
		if v == nil {
			return nil
		}
		return problemOf(v.StatusCode(), v.Body)
	case *client.CreateClientResponse:
		if v == nil {
			return nil
		}
		return problemOf(v.StatusCode(), v.Body)
	}
	return nil
}
