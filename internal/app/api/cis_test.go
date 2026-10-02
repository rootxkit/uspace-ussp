package api

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/rootxkit/uspace-ussp/internal/config"
)

func TestParseBBox(t *testing.T) {
	if b, err := parseBBox(""); b != nil || err != nil {
		t.Fatalf("empty: %v %v", b, err)
	}
	if b, err := parseBBox(" 40, 41,46.5,43.6 "); err != nil || !slices.Equal(b, []float64{40, 41, 46.5, 43.6}) {
		t.Fatalf("Georgia: %v %v", b, err)
	}
	for _, s := range []string{"1,2,3", "a,1,2,3", "NaN,1,2,3", "181,1,182,2", "10,1,5,2", "1,-91,2,0", "1,0,2,91", "1,5,2,4"} {
		if _, err := parseBBox(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}

// An issuer is the CISP or the ANSP by the host of its JWKS URL; one
// served from neither refuses the start (its pull_url could not be
// checked against anything).
func TestNotifySenders(t *testing.T) {
	cfg := config.Config{
		CISPBaseURL: "https://cisp.example", ANSPBaseURL: "https://ansp.example:8443",
		CISNotifyIssuers: []string{"https://cisp.example=https://cisp.example/.well-known/jwks.json",
			"https://ansp.example=https://ansp.example:8443/.well-known/jwks.json"},
	}
	s, err := notifySenders(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if c := s["https://cisp.example"]; c.ANSP || c.BaseHost != "cisp.example" {
		t.Fatalf("CISP: %+v", c)
	}
	if a := s["https://ansp.example"]; !a.ANSP || a.BaseHost != "ansp.example" {
		t.Fatalf("ANSP: %+v", a)
	}
	cfg.CISNotifyIssuers = append(cfg.CISNotifyIssuers, "https://other.example=https://other.example/jwks")
	if _, err := notifySenders(cfg); err == nil {
		t.Fatal("an issuer on another host was accepted")
	}
	cfg.CISNotifyIssuers = []string{"no-jwks"}
	if _, err := notifySenders(cfg); err == nil {
		t.Fatal("an issuer without a JWKS URL was accepted")
	}
}

func TestOutgoingTokens(t *testing.T) {
	if o, err := outgoingTokens(config.Config{}); o != nil || err != nil {
		t.Fatalf("unconfigured: %v %v", o, err)
	}
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{TokenIssuers: []string{"https://auth.example=https://auth.example/jwks"}, TokenClientSecretFile: secret, SystemID: "USSP-DEV"}
	if o, err := outgoingTokens(cfg); o == nil || err != nil {
		t.Fatalf("configured: %v %v", o, err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]config.Config{
		"absent file": {TokenIssuers: cfg.TokenIssuers, TokenClientSecretFile: filepath.Join(dir, "absent")},
		"empty file":  {TokenIssuers: cfg.TokenIssuers, TokenClientSecretFile: empty},
		"bad issuer":  {TokenIssuers: []string{"no-jwks"}, TokenClientSecretFile: secret},
	} {
		if _, err := outgoingTokens(c); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
