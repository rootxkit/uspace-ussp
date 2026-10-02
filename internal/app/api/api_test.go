package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	coreauth "github.com/rootxkit/uspace-core/auth"

	"github.com/rootxkit/uspace-ussp/internal/app/proc"
	"github.com/rootxkit/uspace-ussp/internal/config"
)

func keyFile(t *testing.T) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "issuer.pem")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The issuer configuration both ways: no key is no issuer (api starts
// and says so); a key loads with iss defaulting to https://<first
// audience>; a key without an audience, a previous key without a
// current one, an unreadable key and the same key twice refuse the
// start.
func TestIssuerFromConfig(t *testing.T) {
	if iss, err := IssuerFromConfig(config.Config{}); iss != nil || err != nil {
		t.Fatalf("no key: %v %v", iss, err)
	}
	key := keyFile(t)
	iss, err := IssuerFromConfig(config.Config{IssuerKeyFile: key, Audiences: []string{"ussp.test", "ussp-api"}})
	if err != nil || iss.URL != "https://ussp.test" || iss.Audience != "ussp.test" {
		t.Fatalf("key: %+v %v", iss, err)
	}
	iss, err = IssuerFromConfig(config.Config{IssuerKeyFile: key, IssuerPreviousKeyFile: keyFile(t), IssuerURL: "https://issuer.test/x", Audiences: []string{"ussp.test"}})
	if err != nil || iss.URL != "https://issuer.test/x" || iss.Keys().Previous == nil {
		t.Fatalf("with previous: %+v %v", iss, err)
	}
	for name, c := range map[string]config.Config{
		"no audience":     {IssuerKeyFile: key},
		"previous only":   {IssuerPreviousKeyFile: key, Audiences: []string{"ussp.test"}},
		"unreadable":      {IssuerKeyFile: filepath.Join(t.TempDir(), "absent"), Audiences: []string{"ussp.test"}},
		"unreadable prev": {IssuerKeyFile: key, IssuerPreviousKeyFile: filepath.Join(t.TempDir(), "absent"), Audiences: []string{"ussp.test"}},
		"same key twice":  {IssuerKeyFile: key, IssuerPreviousKeyFile: key, Audiences: []string{"ussp.test"}},
	} {
		if _, err := IssuerFromConfig(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRefuseAllRefusesEveryToken(t *testing.T) {
	_, err := refuseAll{}.Verify(context.Background(), "x")
	var te *coreauth.TokenError
	if !errors.As(err, &te) || te.Counter != coreauth.CounterRejectedAudience {
		t.Fatalf("%v", err)
	}
}

// staff-add refuses a wrong usage and a missing database before it
// touches anything (its success is TestIntegrationStaffAdminTOTP's
// CreateStaff path).
func TestStaffAddRefusesBadUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := staffAdd(context.Background(), config.Config{}, []string{"only-one"}, strings.NewReader(""), &out, &errOut); code != proc.ExitConfig {
		t.Fatalf("usage: %d", code)
	}
	errOut.Reset()
	if code := staffAdd(context.Background(), config.Config{}, []string{"admin1", "admin"}, strings.NewReader("pw\n"), &out, &errOut); code != proc.ExitConfig ||
		!strings.Contains(errOut.String(), "USSP_PG_URL") {
		t.Fatalf("no database: %d %s", code, errOut.String())
	}
	if out.Len() != 0 {
		t.Fatal("printed on refusal")
	}
	if osUser() == "" {
		t.Fatal("no user")
	}
}
