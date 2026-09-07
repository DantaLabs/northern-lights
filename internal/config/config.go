// Package config loads northern-lights configuration from a YAML file
// with NL_ prefixed environment variable overrides.
package config

import (
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// baseURLs maps a Workiva region to its API base URL.
var baseURLs = map[string]string{
	"eu":   "https://api.eu.wdesk.com",
	"us":   "https://api.app.wdesk.com",
	"apac": "https://api.apac.wdesk.com",
}

// Config holds all runtime configuration for the server.
type Config struct {
	Region                   string
	WorkivaClientID          string
	WorkivaClientSecret      string
	DBPath                   string
	ListenAddr               string
	ReadCacheTTL             time.Duration
	RequireWriteConfirmation bool
}

// rawConfig mirrors the YAML file. Pointer fields distinguish an absent
// key from an explicitly zero value.
type rawConfig struct {
	Region                   *string `yaml:"region"`
	DBPath                   *string `yaml:"db_path"`
	ListenAddr               *string `yaml:"listen_addr"`
	ReadCacheTTL             *string `yaml:"read_cache_ttl"`
	RequireWriteConfirmation *bool   `yaml:"require_write_confirmation"`
}

// Load reads the YAML file at path (a missing file is fine, defaults
// apply), then applies NL_ environment overrides, then validates the
// region. Workiva credentials come only from NL_WORKIVA_CLIENT_ID and
// NL_WORKIVA_CLIENT_SECRET, never from YAML.
func Load(path string) (*Config, error) {
	cfg := &Config{
		Region:                   "eu",
		DBPath:                   "./northern-lights.db",
		ListenAddr:               ":8080",
		ReadCacheTTL:             30 * time.Second,
		RequireWriteConfirmation: true,
	}

	if path != "" {
		if err := loadYAML(path, cfg); err != nil {
			return nil, err
		}
	}

	applyEnv(cfg)

	if _, ok := baseURLs[cfg.Region]; !ok {
		return nil, fmt.Errorf("invalid region %q: must be one of eu, us, apac", cfg.Region)
	}
	return cfg, nil
}

func loadYAML(path string, cfg *Config) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read config file: %w", err)
	}
	var raw rawConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parse config file %s: %w", path, err)
	}
	if raw.Region != nil {
		cfg.Region = *raw.Region
	}
	if raw.DBPath != nil {
		cfg.DBPath = *raw.DBPath
	}
	if raw.ListenAddr != nil {
		cfg.ListenAddr = *raw.ListenAddr
	}
	if raw.ReadCacheTTL != nil {
		d, err := time.ParseDuration(*raw.ReadCacheTTL)
		if err != nil {
			return fmt.Errorf("invalid read_cache_ttl %q: %w", *raw.ReadCacheTTL, err)
		}
		cfg.ReadCacheTTL = d
	}
	if raw.RequireWriteConfirmation != nil {
		cfg.RequireWriteConfirmation = *raw.RequireWriteConfirmation
	}
	return nil
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("NL_REGION"); v != "" {
		cfg.Region = v
	}
	cfg.WorkivaClientID = os.Getenv("NL_WORKIVA_CLIENT_ID")
	cfg.WorkivaClientSecret = os.Getenv("NL_WORKIVA_CLIENT_SECRET")
}

// BaseURL returns the Workiva API base URL for the configured region.
func (c *Config) BaseURL() string {
	return baseURLs[c.Region]
}
