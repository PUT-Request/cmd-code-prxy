package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// APIKeyDef defines a client API key with model restrictions and upstream key pool.
type APIKeyDef struct {
	Key             string   `yaml:"key"`
	Models          []string `yaml:"models"`
	CommandCodeKeys []string `yaml:"command_code_keys"`
	RateLimit       int      `yaml:"rate_limit"`
}

// DashboardConfig controls the management UI.
type DashboardConfig struct {
	Enabled      bool   `yaml:"enabled"`
	Username     string `yaml:"username"`
	Password     string `yaml:"password"`
	SessionHours int    `yaml:"session_hours"`
	// Port serves the dashboard on its own listener instead of the API port.
	// Zero shares the API listener. A separate port lets you expose only the
	// panel, or keep the API on its existing port.
	Port string `yaml:"port"`
	// DBPath is where token usage is persisted (SQLite). Empty keeps
	// usage in memory only.
	DBPath string `yaml:"db_path"`
}

// ModelsConfig controls the upstream model-list registry (live /v1/models).
type ModelsConfig struct {
	// RefreshSeconds is how often the upstream model list is re-fetched.
	RefreshSeconds int `yaml:"refresh_seconds"`
}

// RouterConfig controls the built-in free:auto model router.
type RouterConfig struct {
	// Enabled exposes the free:auto virtual model (routes across free models).
	Enabled bool `yaml:"enabled"`
	// CooldownMinutes is how long a failed model is skipped.
	CooldownMinutes int `yaml:"cooldown_minutes"`
	// MaxModelsPerRequest caps how many models one request may try before
	// giving up (bounded failover).
	MaxModelsPerRequest int `yaml:"max_models_per_request"`
	// Models overrides the auto-scanned free pool when non-empty.
	Models []string `yaml:"models"`
}

// LimitsConfig bounds router request size. Zero falls back to the defaults
// (256k input, 32k output). Limits apply to free:auto only.
type LimitsConfig struct {
	InputTokens  int `yaml:"input_tokens"`
	OutputTokens int `yaml:"output_tokens"`
}

// DumpConfig archives every request/response 1:1 to JSONL files for later
// distillation.
type DumpConfig struct {
	Enabled bool   `yaml:"enabled"`
	Dir     string `yaml:"dir"`
	// IncludeImages keeps base64 image payloads in the archive. Off by
	// default: screenshots dwarf everything else on disk.
	IncludeImages bool `yaml:"include_images"`
	// MaxGB prunes oldest files once the folder exceeds this size. 0 keeps
	// everything forever.
	MaxGB int `yaml:"max_gb"`
}

// Config holds all runtime configuration for the proxy server.
type Config struct {
	Host      string          `yaml:"host"`
	Port      string          `yaml:"port"`
	APIKeys   []APIKeyDef     `yaml:"api_keys"`
	Debug     bool            `yaml:"debug"`
	Dashboard DashboardConfig `yaml:"dashboard"`
	Models    ModelsConfig    `yaml:"models"`
	Router    RouterConfig    `yaml:"router"`
	Limits    LimitsConfig    `yaml:"limits"`
	Dump      DumpConfig      `yaml:"dump"`

	// BaseURL overrides the CommandCode API endpoint. Empty uses the default.
	BaseURL string `yaml:"base_url"`

	// Deprecated: kept for backward compatibility. Migrated to APIKeys.
	APIKey       string `yaml:"api_key"`
	ServerAPIKey string `yaml:"server_api_key"`
}

// Load reads and parses a YAML configuration file from the given path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file %s: %w", path, err)
	}

	cfg.migrate()

	return cfg, nil
}

// migrate converts legacy single-key config into the APIKeys format.
func (c *Config) migrate() {
	if len(c.APIKeys) > 0 {
		return
	}

	// Legacy: server_api_key was the client-facing key, api_key was the upstream CC key.
	clientKey := c.ServerAPIKey
	ccKey := c.APIKey
	if clientKey == "" && ccKey == "" {
		return
	}

	ccKeys := []string{}
	if ccKey != "" {
		ccKeys = append(ccKeys, ccKey)
	}

	c.APIKeys = []APIKeyDef{{
		Key:             clientKey,
		Models:          []string{}, // all models allowed
		CommandCodeKeys: ccKeys,
	}}
}
