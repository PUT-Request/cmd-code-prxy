package main

import (
	"flag"
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
	configFile := flag.String("config", defaultConfigFile, "Path to the YAML config file")
	port := flag.String("port", "", "Port to run the server on (overrides config)")
	host := flag.String("host", "", "Host to bind to (overrides config)")
	apiKey := flag.String("api-key", "", "Default CommandCode API key (overrides config)")
	serverAPIKey := flag.String("server-api-key", "", "API key required to access the exposed API (overrides config)")
	showVersion := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(versionText())
		return
	}

	cfg, err := loadConfig(*configFile)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	if cfg.ServerAPIKey == "" {
		log.Fatalf("server_api_key is required. Set it in %s or pass -server-api-key.", *configFile)
	}

	proxy := proxy.NewProxy(cfg.APIKey)
	proxy.Debug = cfg.Debug

	srv := server.NewServer(proxy)
	srv.SetPort(cfg.Port)
	srv.SetHost(cfg.Host)
	srv.SetServerAPIKey(cfg.ServerAPIKey)

	// CLI flags take precedence over config file values
	srv.SetPort(*port)
	srv.SetHost(*host)
	if *apiKey != "" {
		proxy.APIKey = *apiKey
	}
	if *serverAPIKey != "" {
		srv.SetServerAPIKey(*serverAPIKey)
	}

	printStartupInfo(srv, *configFile)

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

func printStartupInfo(srv *server.Server, configFile string) {
	fmt.Println("")
	fmt.Println("========================================")
	fmt.Println("  CommandCode Proxy Server")
	fmt.Println("========================================")
	fmt.Println("")
	fmt.Printf("  Version:     %s\n", versionText())
	fmt.Printf("  Repository:  %s\n", repositoryURL)
	fmt.Printf("  Config:      %s\n", configFile)
	fmt.Printf("  Host:        %s\n", srv.GetHost())
	fmt.Printf("  Port:        %s\n", srv.GetPort())
	fmt.Println("  Base URL:    https://api.commandcode.ai")
	fmt.Println("")
	fmt.Println("  Endpoints:")
	fmt.Println("    POST /v1/chat/completions  (OpenAI-compatible, API key required)")
	fmt.Println("    POST /chat/completions     (OpenAI-compatible alias, API key required)")
	fmt.Println("    POST /v1/responses         (OpenAI Responses-compatible, API key required)")
	fmt.Println("    GET  /v1/models            (list models, API key required)")
	fmt.Println("    GET  /health               (health check, no auth)")
	fmt.Println("")
	fmt.Printf("  Auth:        Bearer <server_api_key> via Authorization header\n")
	fmt.Printf("  Server running on http://%s:%s\n", srv.GetHost(), srv.GetPort())
	fmt.Println("")
	fmt.Println("  Press Ctrl+C to stop")
	fmt.Println("========================================")
}
