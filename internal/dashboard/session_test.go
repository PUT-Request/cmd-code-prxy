package dashboard

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const testPassword = "s3cret-pass"

func TestCheckCredentials(t *testing.T) {
	if !CheckCredentials("admin", testPassword, "admin", testPassword) {
		t.Error("valid credentials should pass")
	}
	if CheckCredentials("admin", testPassword, "admin", "wrong") {
		t.Error("wrong password must fail")
	}
	if CheckCredentials("admin", testPassword, "root", testPassword) {
		t.Error("wrong username must fail")
	}
	if CheckCredentials("admin", testPassword, "", "") {
		t.Error("empty credentials must fail")
	}
}

func TestSignAndVerify(t *testing.T) {
	expiry := time.Now().Add(time.Hour)
	value := sign(testPassword, expiry)

	if !verify(testPassword, value, time.Now()) {
		t.Error("freshly signed cookie should verify")
	}
	if verify("other-password", value, time.Now()) {
		t.Error("cookie must not verify under a different password")
	}
	if verify(testPassword, value, expiry.Add(2*time.Hour)) {
		t.Error("expired cookie must not verify")
	}
}

func TestVerifyRejectsTamperedCookie(t *testing.T) {
	value := sign(testPassword, time.Now().Add(time.Hour))
	parts := strings.SplitN(value, ".", 2)

	// Extended expiry with the original signature.
	if verify(testPassword, parts[0]+"9."+parts[1], time.Now()) {
		t.Error("extending expiry without re-signing must fail")
	}
	if verify(testPassword, "garbage", time.Now()) {
		t.Error("malformed cookie must fail")
	}
	if verify(testPassword, "", time.Now()) {
		t.Error("empty cookie must fail")
	}
	if verify(testPassword, "."+parts[1], time.Now()) {
		t.Error("cookie without expiry must fail")
	}
}

func TestSessionCookieRoundTrip(t *testing.T) {
	rec := httptest.NewRecorder()
	setSessionCookie(rec, testPassword, 1, false)

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected 1 cookie, got %d", len(cookies))
	}
	c := cookies[0]
	if !c.HttpOnly {
		t.Error("session cookie must be HttpOnly")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("session cookie must be SameSite=Lax, got %v", c.SameSite)
	}
	if !verify(testPassword, c.Value, time.Now()) {
		t.Error("cookie from setSessionCookie must verify")
	}
}

func TestClearSessionCookieExpiresIt(t *testing.T) {
	rec := httptest.NewRecorder()
	clearSessionCookie(rec)
	c := rec.Result().Cookies()
	if len(c) != 1 || c[0].MaxAge >= 0 {
		t.Errorf("logout must expire the cookie, got %+v", c)
	}
}

// Regression: the Secure flag used to be derived from the config host, so
// host 0.0.0.0 or a LAN IP produced a Secure cookie that the browser silently
// dropped over plain http://, leaving the user stuck on the login page forever.
func TestSecureCookiesIsOptInNotHostDerived(t *testing.T) {
	orig := sessionSecure
	defer func() { sessionSecure = orig }()

	sessionSecure = false
	if secureCookies() {
		t.Error("Secure must be off by default so plain-HTTP logins keep the cookie")
	}
	sessionSecure = true
	if !secureCookies() {
		t.Error("CC_DASHBOARD_SECURE_COOKIE=1 must enable Secure for HTTPS deployments")
	}
}

// SameSite must not be Strict: the panel is often reached by a different
// hostname than the one used to sign in, and Strict would drop the cookie.
func TestSessionCookieIsSameSiteLax(t *testing.T) {
	rec := httptest.NewRecorder()
	setSessionCookie(rec, testPassword, 1, false)
	c := rec.Result().Cookies()[0]
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("session cookie must be SameSite=Lax, got %v", c.SameSite)
	}
}

func TestLoginRequiresValidCredentials(t *testing.T) {
	d := newTestDashboard(t)

	// Wrong password → 401 and no session cookie.
	form := url.Values{"username": {"admin"}, "password": {"nope"}}
	req := httptest.NewRequest(http.MethodPost, "/dashboard/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("bad login should be 401, got %d", rec.Code)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Error("failed login must not set a session cookie")
	}
}

func TestLoginSetsSessionAndUnlocksAPI(t *testing.T) {
	d := newTestDashboard(t)

	form := url.Values{"username": {"admin"}, "password": {testPassword}}
	req := httptest.NewRequest(http.MethodPost, "/dashboard/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("successful login should redirect, got %d", rec.Code)
	}
	var session *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			session = c
		}
	}
	if session == nil {
		t.Fatal("login must set a session cookie")
	}

	// Unauthenticated API access is 401.
	apiReq := httptest.NewRequest(http.MethodGet, "/dashboard/api/keys", nil)
	apiRec := httptest.NewRecorder()
	d.ServeHTTP(apiRec, apiReq)
	if apiRec.Code != http.StatusUnauthorized {
		t.Errorf("API without session should be 401, got %d", apiRec.Code)
	}

	// With the cookie it succeeds.
	authReq := httptest.NewRequest(http.MethodGet, "/dashboard/api/keys", nil)
	authReq.AddCookie(session)
	authRec := httptest.NewRecorder()
	d.ServeHTTP(authRec, authReq)
	if authRec.Code != http.StatusOK {
		t.Errorf("API with session should be 200, got %d", authRec.Code)
	}
}

func TestPagesRedirectWhenNotAuthenticated(t *testing.T) {
	d := newTestDashboard(t)
	for _, path := range []string{"/dashboard/", "/dashboard/keys"} {
		rec := httptest.NewRecorder()
		d.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusSeeOther {
			t.Errorf("%s should redirect anonymous users, got %d", path, rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "/dashboard/login" {
			t.Errorf("%s should redirect to login, got %q", path, loc)
		}
	}
}

func TestStaticAssetsAreServed(t *testing.T) {
	d := newTestDashboard(t)
	for _, path := range []string{"/dashboard/static/pico.min.css", "/dashboard/static/chart.umd.js"} {
		rec := httptest.NewRecorder()
		d.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s should be served, got %d", path, rec.Code)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("%s served an empty body", path)
		}
	}
}

func TestDisabledDashboardServesNothing(t *testing.T) {
	rec := httptest.NewRecorder()
	NewHandlerFor(t, dashboardConfig(false, "admin", testPassword)).
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard/login", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("disabled dashboard should 404, got %d", rec.Code)
	}
}

func TestDashboardWithoutCredentialsIsDisabled(t *testing.T) {
	rec := httptest.NewRecorder()
	NewHandlerFor(t, dashboardConfig(true, "", "")).
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard/login", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing credentials should leave the dashboard off, got %d", rec.Code)
	}
}
