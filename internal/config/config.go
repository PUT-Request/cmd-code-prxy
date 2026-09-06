package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// APIKeyDef defines a client API key with model restrictions and upstream key pool.
type APIKeyDef struct {
	Key              string   `yaml:"key"`
	Models           []string `yaml:"models"`
	CommandCodeKeys  []string `yaml:"command_code_keys"`
	RateLimit        int      `yaml:"rate_limit"`
}

// Config holds all runtime configuration for the proxy server.
type Config struct {
	Host         string       `yaml:"host"`
	Port         string       `yaml:"port"`
	APIKeys      []APIKeyDef  `yaml:"api_keys"`
	Debug        bool         `yaml:"debug"`

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
