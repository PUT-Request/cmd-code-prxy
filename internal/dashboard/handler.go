package dashboard

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dev2k6/command-code-proxy-server/internal/config"
)

//go:embed web/*.html web/static/*
var webFS embed.FS

// KeyStatusFunc reports live runtime state for an upstream key, e.g. cooldown.
type KeyStatusFunc func(upstreamKey string) string

// Dashboard serves the management UI and its JSON API.
type Dashboard struct {
	cfg        config.DashboardConfig
	host       string
	configPath string
	usage      *Store

	// mu guards apiKeys, the live copy of config.yaml's api_keys.
	mu      sync.Mutex
	apiKeys []config.APIKeyDef

	keyStatus KeyStatusFunc

	// onChange fires after a persisted key edit so the server can refresh its
	// live key map without a restart.
	onChange func([]config.APIKeyDef)

	pages map[string]*template.Template
}

// New creates a dashboard bound to a config file path. apiKeys is copied, so
// the dashboard can rewrite it and persist without aliasing caller state.
func New(cfg config.DashboardConfig, host, configPath string, apiKeys []config.APIKeyDef, usage *Store) (*Dashboard, error) {
	cp := make([]config.APIKeyDef, len(apiKeys))
	copy(cp, apiKeys)
	if usage == nil {
		usage = NewStore()
	}

	pages, err := loadPages()
	if err != nil {
		return nil, err
	}
	return &Dashboard{
		cfg:        cfg,
		host:       host,
		configPath: configPath,
		usage:      usage,
		apiKeys:    cp,
		pages:      pages,
	}, nil
}

// OnKeysChanged registers a callback invoked after the dashboard persists a key
// change, so edits take effect without a restart. Called with d.mu held.
func (d *Dashboard) OnKeysChanged(fn func([]config.APIKeyDef)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onChange = fn
}

// notifyChanged fires the change callback. Called with d.mu held, so the
// callback must not call back into the dashboard.
func (d *Dashboard) notifyChanged() {
	if d.onChange != nil {
		d.onChange(d.apiKeys)
	}
}

// SetKeyStatusFunc installs the live upstream-key status reporter.
func (d *Dashboard) SetKeyStatusFunc(fn KeyStatusFunc) { d.keyStatus = fn }

// Usage exposes the store so the proxy can record completed requests.
func (d *Dashboard) Usage() *Store { return d.usage }

// Enabled reports whether the dashboard has a usable configuration.
func (d *Dashboard) Enabled() bool {
	return d.cfg.Enabled && d.cfg.Username != "" && d.cfg.Password != ""
}

func (d *Dashboard) sessionHours() int {
	if d.cfg.SessionHours <= 0 {
		return 24
	}
	return d.cfg.SessionHours
}

func loadPages() (map[string]*template.Template, error) {
	pages := make(map[string]*template.Template, 4)
	for _, name := range []string{"login", "dashboard", "keys", "logout"} {
		t, err := template.ParseFS(webFS, "web/"+name+".html")
		if err != nil {
			return nil, fmt.Errorf("parse %s.html: %w", name, err)
		}
		pages[name] = t
	}
	return pages, nil
}

// Register mounts the dashboard routes. Everything under /dashboard/ requires
// a session except the login page and the static assets.
func (d *Dashboard) Register(mux *http.ServeMux) {
	if !d.Enabled() {
		return
	}
	d.registerRoutes(mux)
}

func (d *Dashboard) registerRoutes(mux *http.ServeMux) {
	mux.Handle("/dashboard/static/", http.StripPrefix("/dashboard/static/", http.FileServer(http.FS(assetFS()))))
	mux.HandleFunc("/dashboard/login", d.handleLogin)
	mux.HandleFunc("/dashboard/logout", d.handleLogout)
	mux.HandleFunc("/dashboard/api/usage", d.api(d.handleUsage))
	mux.HandleFunc("/dashboard/api/keys", d.api(d.handleKeys))
	mux.HandleFunc("/dashboard/api/keys/save", d.api(d.handleSaveKey))
	mux.HandleFunc("/dashboard/api/keys/delete", d.api(d.handleDeleteKey))
	mux.HandleFunc("/dashboard/keys", d.page(d.handleKeysPage))
	mux.HandleFunc("/dashboard/", d.page(d.handleDashboard))
}

// Handler returns a standalone mux containing only the dashboard. Used when the
// panel runs on its own listener, so the API port is never exposed to the UI.
func (d *Dashboard) Handler() http.Handler {
	mux := http.NewServeMux()
	d.registerRoutes(mux)
	return mux
}

// Port reports the dedicated dashboard port, or "" when it shares the API port.
func (d *Dashboard) Port() string { return d.cfg.Port }

// Host reports the listen host for a dedicated dashboard listener.
func (d *Dashboard) Host() string { return d.host }

// page serves HTML for a valid session, else redirects to login.
func (d *Dashboard) page(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !d.authenticated(r) {
			http.Redirect(w, r, "/dashboard/login", http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

// api serves JSON for a valid session, else 401.
func (d *Dashboard) api(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !d.authenticated(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not authenticated"})
			return
		}
		next(w, r)
	}
}

func (d *Dashboard) render(w http.ResponseWriter, status int, page string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := d.pages[page].Execute(w, data); err != nil {
		log.Printf("[DASHBOARD] render %s: %v", page, err)
	}
}

// ── pages ────────────────────────────────────────────────────────────

func (d *Dashboard) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if d.authenticated(r) {
			http.Redirect(w, r, "/dashboard/", http.StatusSeeOther)
			return
		}
		d.render(w, http.StatusOK, "login", nil)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	user, pass := r.FormValue("username"), r.FormValue("password")
	if !CheckCredentials(d.cfg.Username, d.cfg.Password, user, pass) {
		log.Printf("[DASHBOARD] failed login for %q from %s", user, r.RemoteAddr)
		d.render(w, http.StatusUnauthorized, "login", "Invalid credentials")
		return
	}
	setSessionCookie(w, d.cfg.Password, d.sessionHours(), secureCookies())
	http.Redirect(w, r, "/dashboard/", http.StatusSeeOther)
}

func (d *Dashboard) handleLogout(w http.ResponseWriter, r *http.Request) {
	clearSessionCookie(w)
	d.render(w, http.StatusOK, "logout", nil)
}

func (d *Dashboard) handleDashboard(w http.ResponseWriter, r *http.Request) {
	d.render(w, http.StatusOK, "dashboard", nil)
}

func (d *Dashboard) handleKeysPage(w http.ResponseWriter, r *http.Request) {
	d.render(w, http.StatusOK, "keys", nil)
}

// ── API ──────────────────────────────────────────────────────────────

func (d *Dashboard) handleUsage(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, d.usage.Snapshot(r.URL.Query().Get("range")))
}

// publicKey is the browser-safe view of a key: no full secrets, ever.
type publicKey struct {
	Masked      string   `json:"masked_key"`
	Models      []string `json:"models"`
	RateLimit   int      `json:"rate_limit"`
	Upstream    []string `json:"upstream"`
	UpstreamAll int      `json:"upstream_count"`
}

func (d *Dashboard) handleKeys(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, d.publicKeys())
}

// MaskSecret renders a secret as a fingerprint safe for display and logs.
func MaskSecret(s string) string {
	if len(s) <= 4 {
		return "****"
	}
	return "****" + s[len(s)-4:]
}

// maskMatchesMasked reports whether a submitted value is the unchanged mask of
// the stored secret, meaning the caller did not edit it.
func maskMatchesMasked(stored, submitted string) bool {
	return submitted != "" && submitted == MaskSecret(stored)
}

func (d *Dashboard) publicKeys() []publicKey {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.publicKeysLocked()
}

// publicKeysLocked builds the browser-safe key list. Callers must hold d.mu.
// Go mutexes are not reentrant, and the save and delete handlers already hold
// the lock when they respond.
func (d *Dashboard) publicKeysLocked() []publicKey {
	defs := make([]config.APIKeyDef, len(d.apiKeys))
	copy(defs, d.apiKeys)

	out := make([]publicKey, 0, len(defs))
	for _, k := range defs {
		up := make([]string, 0, len(k.CommandCodeKeys))
		for _, u := range k.CommandCodeKeys {
			up = append(up, MaskSecret(u))
		}
		models := k.Models
		if models == nil {
			models = []string{}
		}
		out = append(out, publicKey{
			Masked:      MaskSecret(k.Key),
			Models:      models,
			RateLimit:   k.RateLimit,
			Upstream:    up,
			UpstreamAll: len(k.CommandCodeKeys),
		})
	}
	return out
}

// keyRequest is the create/update payload from the browser.
type keyRequest struct {
	Index           *int     `json:"index"`
	Key             string   `json:"key"`
	Models          []string `json:"models"`
	CommandCodeKeys []string `json:"command_code_keys"`
	RateLimit       int      `json:"rate_limit"`
}

func (d *Dashboard) handleSaveKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req keyRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	// Editing an existing key: masked secrets mean "unchanged", so keep the
	// stored value instead of overwriting it with the mask.
	idx := -1
	if req.Index != nil && *req.Index >= 0 && *req.Index < len(d.apiKeys) {
		idx = *req.Index
		def := d.apiKeys[idx]
		if !maskMatchesMasked(def.Key, req.Key) {
			if strings.TrimSpace(req.Key) == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "key required"})
				return
			}
			def.Key = strings.TrimSpace(req.Key)
		}
		def.Models = cleanList(req.Models)
		def.RateLimit = req.RateLimit
		def.CommandCodeKeys = mergeUpstream(def.CommandCodeKeys, req.CommandCodeKeys)
		d.apiKeys[idx] = def
	} else {
		key := strings.TrimSpace(req.Key)
		if key == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "key required"})
			return
		}
		for _, existing := range d.apiKeys {
			if existing.Key == key {
				writeJSON(w, http.StatusConflict, map[string]string{"error": "key already exists"})
				return
			}
		}
		d.apiKeys = append(d.apiKeys, config.APIKeyDef{
			Key:             key,
			Models:          cleanList(req.Models),
			CommandCodeKeys: cleanList(req.CommandCodeKeys),
			RateLimit:       req.RateLimit,
		})
	}

	if err := config.UpdateAPIKeys(d.configPath, d.apiKeys); err != nil {
		log.Printf("[DASHBOARD] persist keys: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to write config.yaml"})
		return
	}
	// Lock still held: build the response without re-locking.
	out := d.publicKeysLocked()
	d.notifyChanged()
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

// mergeUpstream keeps stored upstream keys whose mask was submitted unchanged,
// and appends genuinely new ones.
func mergeUpstream(stored, submitted []string) []string {
	out := make([]string, 0, len(submitted))
	for _, s := range submitted {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if maskMatchesMaskedAny(stored, s) {
			// Preserve the original value for this position.
			for _, orig := range stored {
				if MaskSecret(orig) == s {
					out = append(out, orig)
					break
				}
			}
			continue
		}
		out = append(out, s)
	}
	return out
}

func maskMatchesMaskedAny(stored []string, submitted string) bool {
	for _, s := range stored {
		if MaskSecret(s) == submitted {
			return true
		}
	}
	return false
}

func cleanList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (d *Dashboard) handleDeleteKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req struct {
		Index *int `json:"index"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if req.Index == nil || *req.Index < 0 || *req.Index >= len(d.apiKeys) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "index out of range"})
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if *req.Index >= len(d.apiKeys) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "index out of range"})
		return
	}
	d.apiKeys = append(d.apiKeys[:*req.Index], d.apiKeys[*req.Index+1:]...)
	if err := config.UpdateAPIKeys(d.configPath, d.apiKeys); err != nil {
		log.Printf("[DASHBOARD] persist keys after delete: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to write config.yaml"})
		return
	}
	out := d.publicKeysLocked()
	d.notifyChanged()
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("[DASHBOARD] encode json: %v", err)
	}
}

// assetFS returns the embedded static assets rooted at web/static, so URLs look
// like /dashboard/static/pico.min.css.
func assetFS() fs.FS {
	sub, err := fs.Sub(webFS, "web/static")
	if err != nil {
		panic(err) // embed layout is compile-time verified
	}
	return sub
}

// RegisterUsage attaches a recorder so the proxy can report token usage without
// importing this package directly (avoids an import cycle).
func RegisterUsage(store *Store) func(key, model string, input, output int) {
	return func(key, model string, input, output int) { store.Record(key, model, input, output) }
}

// Now is injectable for tests that need deterministic timestamps.
var Now = time.Now
