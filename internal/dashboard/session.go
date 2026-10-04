package dashboard

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const sessionCookie = "ccp_session"

// secret derives the session signing key from the configured password, so no
// extra secret has to be added to config.yaml.
func secret(password string) []byte {
	sum := sha256.Sum256([]byte("ccp-dashboard-session|" + password))
	return sum[:]
}

// sign returns "<expiryUnix>.<sig>" where sig is HMAC-SHA256 over the expiry.
func sign(password string, expiry time.Time) string {
	exp := strconv.FormatInt(expiry.Unix(), 10)
	mac := hmac.New(sha256.New, secret(password))
	mac.Write([]byte(exp))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return exp + "." + sig
}

// verify checks the cookie value and its expiry. Constant-time on both the
// signature and the password comparison.
func verify(password, value string, now time.Time) bool {
	parts := strings.SplitN(value, ".", 2)
	if len(parts) != 2 {
		return false
	}
	mac := hmac.New(sha256.New, secret(password))
	mac.Write([]byte(parts[0]))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(want), []byte(parts[1])) != 1 {
		return false
	}
	expUnix, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return false
	}
	return now.Unix() < expUnix
}

// CheckCredentials compares the submitted username and password in constant
// time so the login form cannot be used as a timing oracle.
func CheckCredentials(wantUser, wantPass, gotUser, gotPass string) bool {
	u := subtle.ConstantTimeCompare([]byte(wantUser), []byte(gotUser))
	p := subtle.ConstantTimeCompare([]byte(wantPass), []byte(gotPass))
	return u&p == 1
}

// sessionSecure, when set, forces the Secure cookie flag. It exists because the
// Secure flag cannot be derived from config alone: the cookie is written on the
// API listener, but the browser may reach the same host over plain HTTP on a
// different port, where a Secure cookie is silently dropped.
var sessionSecure = os.Getenv("CC_DASHBOARD_SECURE_COOKIE") == "1"

// setSessionCookie writes the signed session cookie.
func setSessionCookie(w http.ResponseWriter, password string, hours int, secure bool) {
	expiry := time.Now().Add(time.Duration(hours) * time.Hour)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    sign(password, expiry),
		Path:     "/",
		HttpOnly: true,
		// SameSite=Lax (not Strict) so the post-login redirect and ordinary
		// navigation still carry the session when the panel is reached by a
		// different hostname (e.g. LAN IP vs localhost). The cookie is still
		// not sent on cross-site POSTs.
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		Expires:  expiry,
	})
}

// clearSessionCookie expires the session.
func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

// authenticated reports whether the request carries a valid session.
func (d *Dashboard) authenticated(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return false
	}
	return verify(d.cfg.Password, c.Value, time.Now())
}

// secureCookies reports whether to set the Secure flag.
//
// Previously this returned true for any non-loopback host, which broke plain
// HTTP deployments: host 0.0.0.0 or a LAN IP makes the cookie Secure, and the
// browser silently drops it over http://, so login loops forever. The flag is
// now opt-in via CC_DASHBOARD_SECURE_COOKIE=1, which is what you want when the
// panel is served over HTTPS.
func secureCookies() bool { return sessionSecure }
