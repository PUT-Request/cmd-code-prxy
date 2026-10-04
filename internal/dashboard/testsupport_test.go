package dashboard

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dev2k6/command-code-proxy-server/internal/config"
)

const testConfigBody = `# proxy config
host: "127.0.0.1"
port: "55990"
debug: false

api_keys:
  - key: "sk-client-one-1111"
    models: []
    command_code_keys:
      - "cc-upstream-aaaa"
      - "cc-upstream-bbbb"
    rate_limit: 0

dashboard:
  enabled: true
  username: "admin"
  password: "s3cret-pass"
  session_hours: 24
`

func dashboardConfig(enabled bool, user, pass string) config.DashboardConfig {
	return config.DashboardConfig{
		Enabled:      enabled,
		Username:     user,
		Password:     pass,
		SessionHours: 24,
	}
}

// newTestConfig writes a temporary config file and returns its path.
func newTestConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := writeFile(path, body); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func writeFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o600)
}

// newTestDashboard returns a mounted dashboard backed by a temp config.
func newTestDashboard(t *testing.T) http.Handler {
	t.Helper()
	path := newTestConfig(t, testConfigBody)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	d, err := New(cfg.Dashboard, cfg.Host, path, cfg.APIKeys, NewStore())
	if err != nil {
		t.Fatalf("new dashboard: %v", err)
	}
	mux := http.NewServeMux()
	d.Register(mux)
	return &testHandler{ServeMux: mux, dash: d}
}

// NewHandlerFor mounts a dashboard for the given config, with no key file.
func NewHandlerFor(t *testing.T, dc config.DashboardConfig) http.Handler {
	t.Helper()
	d, err := New(dc, "127.0.0.1", newTestConfig(t, testConfigBody), nil, NewStore())
	if err != nil {
		t.Fatalf("new dashboard: %v", err)
	}
	mux := http.NewServeMux()
	d.Register(mux)
	return &testHandler{ServeMux: mux, dash: d}
}

func serveJSON(t *testing.T, h http.Handler, method, path string, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	return serveRaw(t, h, method, path, body, cookie)
}

// serveRaw issues a request with an arbitrary content type (JSON or form).
func serveRaw(t *testing.T, h http.Handler, method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// loginRec posts the login form with the right content type.
func loginRec(t *testing.T, h http.Handler, user, pass string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"username": {user}, "password": {pass}}
	r := httptest.NewRequest(http.MethodPost, "/dashboard/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// configPathOf returns the config file path backing a test dashboard by reading
// it from the dashboard instance the harness creates.
func configPathOf(t *testing.T, h http.Handler) string {
	t.Helper()
	d, ok := h.(*testHandler)
	if !ok {
		t.Fatalf("unexpected handler type %T", h)
	}
	return d.dash.configPath
}

// testHandler wraps a Dashboard so tests can reach its config path.
type testHandler struct {
	*http.ServeMux
	dash *Dashboard
}

func (th *testHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	th.ServeMux.ServeHTTP(w, r)
}
