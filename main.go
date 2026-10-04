package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/dev2k6/command-code-proxy-server/internal/config"
	"github.com/dev2k6/command-code-proxy-server/internal/dashboard"
	"github.com/dev2k6/command-code-proxy-server/internal/proxy"
	"github.com/dev2k6/command-code-proxy-server/internal/server"
	"github.com/dev2k6/command-code-proxy-server/internal/update"
)

const appVersion = "v1.0.8"
const repositoryURL = "https://github.com/dev2k6/command-code-proxy-server"
const defaultConfigFile = "config.yaml"

func main() {
	// Allow config file path via first positional arg or CONFIG env var,
	// falling back to the default.
	configFile := defaultConfigFile
	if len(os.Args) > 1 {
		configFile = os.Args[1]
	}
	if v := os.Getenv("CONFIG"); v != "" {
		configFile = v
	}

	cfg, err := loadConfig(configFile)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	if len(cfg.APIKeys) == 0 {
		log.Fatalf("No api_keys defined in %s. At least one key is required.", configFile)
	}

	// Validate that at least one key has at least one upstream CC key.
	hasUpstream := false
	for _, k := range cfg.APIKeys {
		if len(k.CommandCodeKeys) > 0 {
			hasUpstream = true
			break
		}
	}
	if !hasUpstream {
		log.Fatalf("No command_code_keys configured under any api_key in %s.", configFile)
	}

	// Build the lookup map: client API key → definition
	keyMap := make(map[string]*config.APIKeyDef, len(cfg.APIKeys))
	for i := range cfg.APIKeys {
		k := &cfg.APIKeys[i]
		if k.Key == "" {
			log.Fatalf("api_keys[%d].key must not be empty", i)
		}
		if _, dup := keyMap[k.Key]; dup {
			log.Fatalf("Duplicate api_key %q in %s", k.Key, configFile)
		}
		keyMap[k.Key] = k
	}

	p := proxy.NewProxy(server.GetAPIKeyDef)
	p.Debug = cfg.Debug
	if cfg.BaseURL != "" {
		p.BaseURL = cfg.BaseURL
	}

	// Dashboard (optional): serves the management UI and records token usage.
	var dash *dashboard.Dashboard
	if cfg.Dashboard.Enabled && cfg.Dashboard.Username != "" && cfg.Dashboard.Password != "" {
		store := dashboard.NewStore()
		dash, err = dashboard.New(cfg.Dashboard, cfg.Host, configFile, cfg.APIKeys, store)
		if err != nil {
			log.Fatalf("Failed to init dashboard: %v", err)
		}
		p.SetUsageRecorder(func(clientKey, model string, input, output int) {
			store.Record(clientKey, model, input, output)
		})
		dash.SetKeyStatusFunc(func(upstreamKey string) string {
			if d := p.KeyCooldownRemaining(upstreamKey); d > 0 {
				return fmt.Sprintf("cooldown %v", d.Round(time.Second))
			}
			return "ready"
		})
	}

	var mount []func(*http.ServeMux)
	if dash != nil && dash.Port() == "" {
		mount = append(mount, dash.Register)
	}

	srv := server.NewServer(keyMap, p, mount...)
	srv.SetPort(cfg.Port)
	srv.SetHost(cfg.Host)

	// Dashboard key edits must take effect immediately: after persisting to
	// config.yaml, refresh the server's live key map.
	if dash != nil {
		dash.OnKeysChanged(func(defs []config.APIKeyDef) { srv.SetKeys(defs) })
	}

	printStartupInfo(srv, cfg, configFile, dash)

	if dash != nil && dash.Port() != "" {
		// Dedicated listener: the API port stays untouched.
		go serveDashboard(dash)
	}

	srv.Start()
}

// serveDashboard runs the dashboard on its own port.
func serveDashboard(dash *dashboard.Dashboard) {
	addr := dash.Host() + ":" + dash.Port()
	log.Printf("[DASHBOARD] listening on http://%s/dashboard/", addr)
	if err := http.ListenAndServe(addr, dash.Handler()); err != nil {
		log.Fatalf("Dashboard failed: %v", err)
	}
}

func loadConfig(path string) (*config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("Config file %s not found, using defaults", path)
			return &config.Config{}, nil
		}
		return nil, err
	}
	return cfg, nil
}

func versionText() string {
	latest, hasUpdate, err := update.LatestVersion(appVersion)
	if err != nil || !hasUpdate {
		return appVersion
	}
	return fmt.Sprintf("%s (latest: %s)", appVersion, latest)
}

// baseURLOf reports the upstream endpoint in use.
func baseURLOf(cfg *config.Config) string {
	if cfg.BaseURL != "" {
		return cfg.BaseURL
	}
	return "https://api.commandcode.ai"
}

func printStartupInfo(srv *server.Server, cfg *config.Config, configFile string, dash *dashboard.Dashboard) {
	fmt.Println("")
	fmt.Println("========================================")
	fmt.Println("  CommandCode Proxy Server")
	fmt.Println("========================================")
	fmt.Println("")
	fmt.Printf("  Version:      %s\n", versionText())
	fmt.Printf("  Repository:   %s\n", repositoryURL)
	fmt.Printf("  Config:       %s\n", configFile)
	fmt.Printf("  Host:         %s\n", srv.GetHost())
	fmt.Printf("  Port:         %s\n", srv.GetPort())
	fmt.Println("  Base URL:     " + baseURLOf(cfg))
	fmt.Printf("  API Keys:     %d configured\n", len(cfg.APIKeys))
	if dash != nil && dash.Enabled() {
		dashURL := "http://" + srv.GetHost() + ":" + srv.GetPort() + "/dashboard/"
		if dash.Port() != "" {
			dashURL = "http://" + dash.Host() + ":" + dash.Port() + "/dashboard/"
		}
		fmt.Println("")
		fmt.Println("  Dashboard:    " + dashURL)
	}
	fmt.Println("")
	fmt.Println("  Endpoints:")
	fmt.Println("    POST /v1/chat/completions  (OpenAI-compatible)")
	fmt.Println("    POST /chat/completions     (alias)")
	fmt.Println("    POST /v1/responses         (OpenAI Responses-compatible)")
	fmt.Println("    GET  /v1/models            (list allowed models)")
	fmt.Println("    GET  /health               (health check, no auth)")
	fmt.Println("")
	fmt.Println("  Auth:        Bearer <api_key> via Authorization header")
	fmt.Printf("  Server running on http://%s:%s\n", srv.GetHost(), srv.GetPort())
	fmt.Println("")
	fmt.Println("  Press Ctrl+C to stop")
	fmt.Println("========================================")
}
