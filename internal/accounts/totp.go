package accounts

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6238 TOTP with SHA-1 is what every authenticator app reads; HMAC-SHA-1 is not broken by SHA-1 collisions
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TOTP parameters (RFC 6238 defaults every authenticator app reads):
// HMAC-SHA-1, 6 digits, 30 s period, a 160-bit secret, one step of skew
// on each side.
const (
	TOTPPeriod     = 30 * time.Second
	TOTPDigits     = 6
	TOTPSkewSteps  = 1
	TOTPSecretSize = 20
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret is a random 160-bit secret, base32 without padding (the
// form authenticator apps take), and its otpauth URI.
func NewTOTPSecret(issuer, account string) (secret, uri string, err error) {
	raw := make([]byte, TOTPSecretSize)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	secret = b32.EncodeToString(raw)
	return secret, TOTPURI(issuer, account, secret), nil
}

// TOTPURI is the otpauth://totp URI of a secret (the Key URI format of
// Google Authenticator, which every app reads).
func TOTPURI(issuer, account, secret string) string {
	q := url.Values{"secret": {secret}, "issuer": {issuer}, "algorithm": {"SHA1"}, "digits": {"6"}, "period": {"30"}}
	return "otpauth://totp/" + url.PathEscape(issuer+":"+account) + "?" + q.Encode()
}

// hotp is RFC 4226 §5.3: HMAC-SHA-1 of the 8-byte big-endian counter,
// dynamic truncation, modulo 10^digits, zero-padded.
func hotp(key []byte, counter uint64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	code := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", TOTPDigits, code%1_000_000)
}

// TOTPCode is the code of the raw key at the time step of at (RFC 6238
// §4 with T0 = 0 and the 30 s period); an authenticator app shows it.
func TOTPCode(key []byte, at time.Time) string {
	return hotp(key, uint64(at.Unix()/int64(TOTPPeriod/time.Second)))
}

// VerifyTOTP checks code against the base32 secret at now, within
// TOTPSkewSteps of skew, and returns the time step it matched. A step at
// or before lastStep is refused, so each code is accepted once (RFC 6238
// §5.2); the caller stores the step returned. Codes are compared in
// constant time.
func VerifyTOTP(secret, code string, now time.Time, lastStep int64) (int64, bool) {
	code = strings.TrimSpace(code)
	if len(code) != TOTPDigits {
		return 0, false
	}
	for _, c := range code {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	key, err := b32.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil || len(key) == 0 {
		return 0, false
	}
	step := now.Unix() / int64(TOTPPeriod/time.Second)
	matched, found := int64(0), false
	for d := int64(-TOTPSkewSteps); d <= TOTPSkewSteps; d++ {
		s := step + d
		if s <= lastStep || s < 0 {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(hotp(key, uint64(s))), []byte(code)) == 1 && !found {
			matched, found = s, true
		}
	}
	return matched, found
}
