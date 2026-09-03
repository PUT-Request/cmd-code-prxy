package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config holds all runtime configuration for the proxy server.
type Config struct {
	Host         string `yaml:"host"`
	Port         string `yaml:"port"`
	APIKey       string `yaml:"api_key"`
	ServerAPIKey string `yaml:"server_api_key"`
	Debug        bool   `yaml:"debug"`
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

	return cfg, nil
}
