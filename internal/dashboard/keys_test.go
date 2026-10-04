package dashboard

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/dev2k6/command-code-proxy-server/internal/config"
	"gopkg.in/yaml.v3"
)

// loginSession performs a real login and returns the session cookie.
func loginSession(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	rec := loginRec(t, h, "admin", testPassword)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login failed: %d %s", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatalf("login did not produce a session cookie")
	return nil
}

func TestMaskSecretNeverLeaksValue(t *testing.T) {
	cases := map[string]string{
		"sk-client-one-1111": "****1111",
		"abc":                "****",
		"":                   "****",
	}
	for in, want := range cases {
		if got := MaskSecret(in); got != want {
			t.Errorf("MaskSecret(%q) = %q, want %q", in, got, want)
		}
	}
	// The masked form must not contain the secret body.
	if strings.Contains(MaskSecret("sk-live-supersecret-9999"), "supersecret") {
		t.Error("masked secret must not expose the original body")
	}
}

func TestKeysAPINeverReturnsFullSecrets(t *testing.T) {
	h := newTestDashboard(t)
	cookie := loginSession(t, h)

	rec := serveJSON(t, h, http.MethodGet, "/dashboard/api/keys", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, secret := range []string{"sk-client-one-1111", "cc-upstream-aaaa", "cc-upstream-bbbb"} {
		if strings.Contains(body, secret) {
			t.Fatalf("API leaked the full secret %q: %s", secret, body)
		}
	}
	var keys []publicKey
	if err := json.Unmarshal(rec.Body.Bytes(), &keys); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("expected 1 key, got %d", len(keys))
	}
	if keys[0].Masked != "****1111" {
		t.Errorf("unexpected masked key %q", keys[0].Masked)
	}
	if keys[0].UpstreamAll != 2 {
		t.Errorf("expected 2 upstream keys, got %d", keys[0].UpstreamAll)
	}
}

func TestSaveNewKeyPersistsToConfig(t *testing.T) {
	h := newTestDashboard(t)
	cookie := loginSession(t, h)

	body := `{"key":"sk-new-key-2222","models":["deepseek-v4-pro"],"command_code_keys":["cc-new-cccc"],"rate_limit":5}`
	rec := serveJSON(t, h, http.MethodPost, "/dashboard/api/keys/save", body, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("save failed: %d %s", rec.Code, rec.Body.String())
	}

	// Verify it actually landed in config.yaml, with full secrets intact.
	path := configPathOf(t, h)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	var found bool
	for _, k := range cfg.APIKeys {
		if k.Key == "sk-new-key-2222" {
			found = true
			if len(k.CommandCodeKeys) != 1 || k.CommandCodeKeys[0] != "cc-new-cccc" {
				t.Errorf("upstream key not persisted verbatim: %+v", k.CommandCodeKeys)
			}
			if k.RateLimit != 5 {
				t.Errorf("rate limit not persisted: %d", k.RateLimit)
			}
			if len(k.Models) != 1 || k.Models[0] != "deepseek-v4-pro" {
				t.Errorf("models not persisted: %+v", k.Models)
			}
		}
	}
	if !found {
		t.Fatal("new key missing from config.yaml")
	}
}

// Submitting the mask back must mean "unchanged", never overwrite the secret
// with "****1111".
func TestSaveEditWithMaskPreservesStoredSecrets(t *testing.T) {
	h := newTestDashboard(t)
	cookie := loginSession(t, h)

	body := `{"index":0,"key":"****1111","models":[],"command_code_keys":["****aaaa","****bbbb"],"rate_limit":3}`
	rec := serveJSON(t, h, http.MethodPost, "/dashboard/api/keys/save", body, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("edit failed: %d %s", rec.Code, rec.Body.String())
	}

	path := configPathOf(t, h)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	k := cfg.APIKeys[0]
	if k.Key != "sk-client-one-1111" {
		t.Errorf("client key was overwritten by the mask: %q", k.Key)
	}
	if len(k.CommandCodeKeys) != 2 ||
		k.CommandCodeKeys[0] != "cc-upstream-aaaa" ||
		k.CommandCodeKeys[1] != "cc-upstream-bbbb" {
		t.Errorf("upstream keys were overwritten by masks: %+v", k.CommandCodeKeys)
	}
	if k.RateLimit != 3 {
		t.Errorf("rate limit update lost: %d", k.RateLimit)
	}
}

func TestSaveEditCanReplaceKeySecret(t *testing.T) {
	h := newTestDashboard(t)
	cookie := loginSession(t, h)

	body := `{"index":0,"key":"sk-replaced-7777","models":[],"command_code_keys":["****aaaa","cc-brand-new-dddd"],"rate_limit":0}`
	rec := serveJSON(t, h, http.MethodPost, "/dashboard/api/keys/save", body, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("edit failed: %d %s", rec.Code, rec.Body.String())
	}
	cfg, err := config.Load(configPathOf(t, h))
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	k := cfg.APIKeys[0]
	if k.Key != "sk-replaced-7777" {
		t.Errorf("new key not applied: %q", k.Key)
	}
	// One masked (preserved) plus one brand new.
	if len(k.CommandCodeKeys) != 2 || k.CommandCodeKeys[0] != "cc-upstream-aaaa" {
		t.Errorf("mixed mask/new upstream handling wrong: %+v", k.CommandCodeKeys)
	}
	if k.CommandCodeKeys[1] != "cc-brand-new-dddd" {
		t.Errorf("new upstream key missing: %+v", k.CommandCodeKeys)
	}
}

func TestSaveRejectsDuplicateKey(t *testing.T) {
	h := newTestDashboard(t)
	cookie := loginSession(t, h)

	body := `{"key":"sk-client-one-1111","models":[],"command_code_keys":[],"rate_limit":0}`
	rec := serveJSON(t, h, http.MethodPost, "/dashboard/api/keys/save", body, cookie)
	if rec.Code != http.StatusConflict {
		t.Errorf("duplicate key should be 409, got %d", rec.Code)
	}
}

func TestSaveRejectsEmptyKey(t *testing.T) {
	h := newTestDashboard(t)
	cookie := loginSession(t, h)

	rec := serveJSON(t, h, http.MethodPost, "/dashboard/api/keys/save", `{"key":"","models":[]}`, cookie)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty key should be 400, got %d", rec.Code)
	}
}

func TestDeleteKeyPersists(t *testing.T) {
	h := newTestDashboard(t)
	cookie := loginSession(t, h)

	rec := serveJSON(t, h, http.MethodPost, "/dashboard/api/keys/delete", `{"index":0}`, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete failed: %d %s", rec.Code, rec.Body.String())
	}
	cfg, err := config.Load(configPathOf(t, h))
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(cfg.APIKeys) != 0 {
		t.Errorf("key was not deleted from config.yaml: %+v", cfg.APIKeys)
	}
}

func TestDeleteRejectsOutOfRangeIndex(t *testing.T) {
	h := newTestDashboard(t)
	cookie := loginSession(t, h)

	for _, body := range []string{`{"index":99}`, `{"index":-1}`, `{}`} {
		rec := serveJSON(t, h, http.MethodPost, "/dashboard/api/keys/delete", body, cookie)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s should be 400, got %d", body, rec.Code)
		}
	}
}

func TestWritePreservesOtherConfigSections(t *testing.T) {
	path := newTestConfig(t, testConfigBody)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.APIKeys = append(cfg.APIKeys, config.APIKeyDef{Key: "sk-added-3333", CommandCodeKeys: []string{"cc-x"}})

	if err := config.UpdateAPIKeys(path, cfg.APIKeys); err != nil {
		t.Fatalf("update: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) == string(after) {
		t.Error("config should have changed")
	}
	// dashboard block and other settings survive.
	for _, want := range []string{"dashboard:", "username:", "password:", "session_hours:", "port:", "debug:"} {
		if !strings.Contains(string(after), want) {
			t.Errorf("config lost %q after rewrite:\n%s", want, after)
		}
	}
	// Header comment survives.
	if !strings.Contains(string(after), "# proxy config") {
		t.Errorf("leading comment lost:\n%s", after)
	}
	// Result is still valid YAML of the right shape.
	var out map[string]any
	if err := yaml.Unmarshal(after, &out); err != nil {
		t.Fatalf("rewritten config is not valid YAML: %v\n%s", err, after)
	}
	if _, ok := out["api_keys"]; !ok {
		t.Error("api_keys missing after rewrite")
	}
	if _, ok := out["dashboard"]; !ok {
		t.Error("dashboard section missing after rewrite")
	}
}

func TestUpdateAPIKeysAddsSectionWhenMissing(t *testing.T) {
	path := newTestConfig(t, "host: \"127.0.0.1\"\nport: \"1234\"\n")
	err := config.UpdateAPIKeys(path, []config.APIKeyDef{{Key: "sk-x", CommandCodeKeys: []string{"cc-y"}}})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(cfg.APIKeys) != 1 || cfg.APIKeys[0].Key != "sk-x" {
		t.Errorf("api_keys not added: %+v", cfg.APIKeys)
	}
	if cfg.Port != "1234" {
		t.Errorf("existing values clobbered: port=%q", cfg.Port)
	}
}

func TestUpdateAPIKeysRejectsMissingFile(t *testing.T) {
	if err := config.UpdateAPIKeys("/nonexistent/path/config.yaml", nil); err == nil {
		t.Error("expected an error for a missing config file")
	}
}
