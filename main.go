package main

import (
	"fmt"
	"log"
	"os"

	"github.com/dev2k6/command-code-proxy-server/internal/config"
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

	srv := server.NewServer(keyMap, p)
	srv.SetPort(cfg.Port)
	srv.SetHost(cfg.Host)

	printStartupInfo(srv, cfg, configFile)

	srv.Start()
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

func printStartupInfo(srv *server.Server, cfg *config.Config, configFile string) {
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
	fmt.Println("  Base URL:     https://api.commandcode.ai")
	fmt.Printf("  API Keys:     %d configured\n", len(cfg.APIKeys))
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
