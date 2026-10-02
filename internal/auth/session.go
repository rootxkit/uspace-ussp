package auth

import (
	"net/http"
	"time"
)

// SetSessionCookies sets uspace_session (the session JWT) and
// uspace_csrf (the double-submit value) as the contract fixes them
// (M21): HttpOnly, Secure, SameSite=Strict, Path=/, expiring with the
// session. The BFF reads both server-side, so neither is readable by
// page script.
func SetSessionCookies(w http.ResponseWriter, token, csrf string, expires time.Time) {
	for name, v := range map[string]string{CookieSession: token, CookieCSRF: csrf} {
		http.SetCookie(w, &http.Cookie{
			Name: name, Value: v, Path: "/", Expires: expires.UTC(), MaxAge: int(time.Until(expires).Seconds()),
			HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
		})
	}
}

// ClearSessionCookies expires both cookies (logout).
func ClearSessionCookies(w http.ResponseWriter) {
	for _, name := range []string{CookieSession, CookieCSRF} {
		http.SetCookie(w, &http.Cookie{
			Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
		})
	}
}
