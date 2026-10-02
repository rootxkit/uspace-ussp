package accounts

import (
	"encoding/base32"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/httpx"
)

// RFC 4226 Appendix D: the HOTP values of the secret "12345678901234567890"
// for counters 0 to 9, as pquerna/otp v1.5.0 pins them in
// hotp/hotp_test.go (rfcMatrixTCs), not written from memory (E-03).
var rfc4226 = []string{"755224", "287082", "359152", "969429", "338314", "254676", "287922", "162583", "399871", "520489"}

func TestHOTPMatchesRFC4226(t *testing.T) {
	for i, want := range rfc4226 {
		if got := hotp([]byte("12345678901234567890"), uint64(i)); got != want {
			t.Errorf("counter %d: %s, want %s", i, got, want)
		}
	}
}

// TOTP is HOTP of the 30 s step: the RFC 4226 counters are the steps at
// 30 s * counter.
func TestVerifyTOTPAcceptsOnceWithinSkew(t *testing.T) {
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte("12345678901234567890"))
	at := func(step int64) time.Time { return time.Unix(step*30, 0) }
	step, ok := VerifyTOTP(secret, rfc4226[5], at(5), 0)
	if !ok || step != 5 {
		t.Fatalf("the code of the step: %d %v", step, ok)
	}
	// One step of skew either way.
	if s, ok := VerifyTOTP(secret, rfc4226[5], at(6), 0); !ok || s != 5 {
		t.Fatal("previous step refused")
	}
	if s, ok := VerifyTOTP(secret, rfc4226[5], at(4), 0); !ok || s != 5 {
		t.Fatal("next step refused")
	}
	if _, ok := VerifyTOTP(secret, rfc4226[5], at(7), 0); ok {
		t.Fatal("two steps late accepted")
	}
	// E-01: once the step is used, the same code is refused (RFC 6238 §5.2).
	if _, ok := VerifyTOTP(secret, rfc4226[5], at(5), 5); ok {
		t.Fatal("a code accepted twice")
	}
	for _, bad := range []string{"", "12345", "1234567", "12a456", "７５５２２４"} {
		if _, ok := VerifyTOTP(secret, bad, at(0), -1); ok {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, ok := VerifyTOTP("!!!", rfc4226[0], at(0), -1); ok {
		t.Fatal("a malformed secret accepted")
	}
	if s, ok := VerifyTOTP(" "+strings.ToLower(secret)+" ", " "+rfc4226[0]+" ", at(0), -1); !ok || s != 0 {
		t.Fatal("lower-case secret or spaced code refused")
	}
}

func TestNewTOTPSecretAndURI(t *testing.T) {
	s, uri, err := NewTOTPSecret("USSP-DEV", "admin")
	if err != nil || len(s) != 32 {
		t.Fatalf("%q %v", s, err)
	}
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "otpauth" || u.Host != "totp" || u.Query().Get("secret") != s || u.Query().Get("issuer") != "USSP-DEV" {
		t.Fatalf("uri %s", uri)
	}
	s2, _, _ := NewTOTPSecret("x", "y")
	if s == s2 {
		t.Fatal("two secrets equal")
	}
}

func TestSealerOpensOnlyForItsAccountAndKey(t *testing.T) {
	key := make([]byte, 32)
	key[0] = 1
	s, err := NewSealer(key)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := s.Seal("JBSWY3DPEHPK3PXP", "admin")
	if err != nil || !strings.HasPrefix(ref, sealedPrefix) || strings.Contains(ref, "JBSWY3DPEHPK3PXP") {
		t.Fatalf("%q %v", ref, err)
	}
	if got, err := s.Open(ref, "admin"); err != nil || got != "JBSWY3DPEHPK3PXP" {
		t.Fatalf("open: %q %v", got, err)
	}
	if _, err := s.Open(ref, "other"); !errors.Is(err, ErrSealed) {
		t.Fatal("opened for another account")
	}
	key[0] = 2
	other, _ := NewSealer(key)
	if _, err := other.Open(ref, "admin"); !errors.Is(err, ErrSealed) {
		t.Fatal("opened with another key")
	}
	for _, bad := range []string{"plain", sealedPrefix + "!!", sealedPrefix + "AAAA"} {
		if _, err := s.Open(bad, "admin"); !errors.Is(err, ErrSealed) {
			t.Errorf("%q opened", bad)
		}
	}
	if _, err := NewSealer([]byte("short")); err == nil {
		t.Fatal("a short key accepted")
	}
}

func TestLoadSealer(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "k")
	if err := os.WriteFile(good, []byte("AQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eHyA=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSealer(good); err != nil {
		t.Fatal(err)
	}
	short := filepath.Join(dir, "s")
	_ = os.WriteFile(short, []byte("AQID"), 0o600)
	for _, p := range []string{short, filepath.Join(dir, "absent")} {
		if _, err := LoadSealer(p); err == nil || !strings.Contains(err.Error(), "USSP_MFA_KEY_FILE") {
			t.Errorf("%s: %v", p, err)
		}
	}
}

func fieldOf(t *testing.T, err error) string {
	t.Helper()
	var fe *core.FieldError
	if !errors.As(err, &fe) {
		t.Fatalf("%v is not a field error", err)
	}
	return fe.Field
}

func TestInputValidation(t *testing.T) {
	if v, err := username("u", "  Ops.Admin@GEO "); err != nil || v != "ops.admin@geo" {
		t.Fatalf("%q %v", v, err)
	}
	for _, bad := range []string{"ab", strings.Repeat("a", 65), "a b c", "имя-user", "a\x00bc"} {
		if _, err := username("u", bad); err == nil || fieldOf(t, err) != "u" {
			t.Errorf("username %q accepted", bad)
		}
	}
	if err := password("p", "short"); err == nil {
		t.Error("a short password accepted")
	}
	if err := password("p", strings.Repeat("x", 1025)); err == nil {
		t.Error("an over-long password accepted")
	}
	if err := password("p", "twelve chars"); err != nil {
		t.Error(err)
	}
	if v, err := email(" ops@example.test "); err != nil || v != "ops@example.test" {
		t.Fatalf("%q %v", v, err)
	}
	for _, bad := range []string{"", "not an address", "Name <ops@example.test>", "ops@exa\x01mple.test"} {
		if _, err := email(bad); err == nil {
			t.Errorf("email %q accepted", bad)
		}
	}
	if v, err := registrationNumber(" geo-test-0001 "); err != nil || v != "GEO-TEST-0001" {
		t.Fatalf("%q %v", v, err)
	}
	for _, bad := range []string{"", "GEO TEST", strings.Repeat("A", 33), "GEO\x01"} {
		if _, err := registrationNumber(bad); err == nil {
			t.Errorf("registration %q accepted", bad)
		}
	}
	if _, err := fieldText("display_name", strings.Repeat("ა", 201), 200); err == nil {
		t.Error("201 runes accepted")
	}
	if v, err := fieldText("display_name", strings.Repeat("ა", 200), 200); err != nil || v == "" {
		t.Errorf("200 runes refused: %v", err)
	}
}

func FuzzInputValidation(f *testing.F) {
	f.Add("ops.admin", "GEO-TEST-0001", "ops@example.test", "Operator")
	f.Fuzz(func(t *testing.T, user, number, mail, name string) {
		if v, err := username("u", user); err == nil && (len(v) < 3 || len(v) > 64 || strings.ContainsAny(v, " \x00\n")) {
			t.Fatalf("username %q", v)
		}
		if v, err := registrationNumber(number); err == nil && (v == "" || len(v) > 4*32 || strings.ContainsAny(v, " \t\n")) {
			t.Fatalf("registration %q", v)
		}
		if v, err := email(mail); err == nil && (v == "" || strings.ContainsAny(v, "<>\n")) {
			t.Fatalf("email %q", v)
		}
		if v, err := fieldText("n", name, 200); err == nil && (v == "" || strings.ContainsAny(v, "\x00\n\r")) {
			t.Fatalf("text %q", v)
		}
		_ = clip(name)
	})
}

func TestJudgeRegistryAnswers(t *testing.T) {
	for answer, want := range map[string]string{RegistryValid: OperatorActive, RegistryUnknown: OperatorPending, "suspended": OperatorRefused, "": OperatorRefused} {
		if got := judge(answer); got != want {
			t.Errorf("%q: %s, want %s", answer, got, want)
		}
	}
}

// An accounts refusal is its own problem through httpx.
func TestErrorIsAStatusError(t *testing.T) {
	var se httpx.StatusError = &Error{Status: 409, Slug: "operator_exists", Detail: "d"}
	p := httpx.ProblemFromError(se)
	if p.Status != 409 || p.Slug() != "operator_exists" || p.Detail != "d" || se.Error() == "" {
		t.Fatalf("%+v", p)
	}
}
