// Package config manages the on-disk configuration, including OAuth tokens.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultMatchThreshold is the minimum score for an automatic track match.
const DefaultMatchThreshold = 0.72

// Config is the YAML configuration file.
type Config struct {
	Spotify Spotify `yaml:"spotify"`
	Yandex  Yandex  `yaml:"yandex"`
	Match   Match   `yaml:"match"`
}

// Spotify stores the Spotify app and OAuth state. Tokens are secrets; the
// file is written with mode 0600.
type Spotify struct {
	ClientID     string    `yaml:"client_id,omitempty"`
	AccessToken  string    `yaml:"access_token,omitempty"`
	RefreshToken string    `yaml:"refresh_token,omitempty"`
	Expiry       time.Time `yaml:"expiry,omitempty"`
	UserID       string    `yaml:"user_id,omitempty"`
	DisplayName  string    `yaml:"display_name,omitempty"`
}

// Connected reports whether Spotify has an access token.
func (s Spotify) Connected() bool { return s.AccessToken != "" || s.RefreshToken != "" }

// Yandex stores a Yandex Music OAuth token.
type Yandex struct {
	Token       string `yaml:"token,omitempty"`
	UID         string `yaml:"uid,omitempty"`
	DisplayName string `yaml:"display_name,omitempty"`
}

// Connected reports whether Yandex Music has a token.
func (y Yandex) Connected() bool { return y.Token != "" }

// Match tunes automatic matching.
type Match struct {
	Threshold float64 `yaml:"threshold,omitempty"`
	// DurationToleranceMS is reserved for future matching strategies. The
	// matcher currently uses its own tiered duration scoring.
	DurationToleranceMS int `yaml:"duration_tolerance_ms,omitempty"`
}

// Default returns a configuration with sane defaults.
func Default() *Config {
	return &Config{
		Match: Match{Threshold: DefaultMatchThreshold},
	}
}

// DefaultPath returns the path of the configuration file.
//
// Precedence:
//  1. MUSIC_SYNCER_CONFIG
//  2. <os.UserConfigDir()>/music-syncer/config.yaml
func DefaultPath() (string, error) {
	if p := strings.TrimSpace(os.Getenv("MUSIC_SYNCER_CONFIG")); p != "" {
		return expandHome(p)
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config dir: %w", err)
	}
	return filepath.Join(dir, "music-syncer", "config.yaml"), nil
}

func expandHome(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if p == "~" {
			return home, nil
		}
		return filepath.Join(home, p[2:]), nil
	}
	return p, nil
}

// Load reads the configuration. A missing file is not an error: the returned
// config has defaults and an empty token set.
func Load(path string) (*Config, error) {
	cfg := Default()

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		applyEnv(cfg)
		return cfg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if cfg.Match.Threshold <= 0 || cfg.Match.Threshold > 1 {
		cfg.Match.Threshold = DefaultMatchThreshold
	}
	applyEnv(cfg)
	return cfg, nil
}

func applyEnv(cfg *Config) {
	if token := strings.TrimSpace(os.Getenv("YANDEX_MUSIC_TOKEN")); token != "" {
		cfg.Yandex.Token = token
	}
	if id := strings.TrimSpace(os.Getenv("SPOTIFY_CLIENT_ID")); id != "" {
		cfg.Spotify.ClientID = id
	}
}

// Save writes the configuration atomically with mode 0600.
func Save(path string, cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.yaml")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod config: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close config: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}

// Dir returns the directory containing the config file. State, caches and
// reports live next to the config by default.
func Dir(path string) string { return filepath.Dir(path) }

// WarnIfPermissionsAreLoose warns when the config contains secrets and can be
// read by other users. It is intentionally best-effort.
func WarnIfPermissionsAreLoose(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("config %s is readable by other users (mode %o); run: chmod 600 %s",
			path, info.Mode().Perm(), path)
	}
	return nil
}

// MaskSecret returns a partially masked secret for status output.
func MaskSecret(s string) string {
	if s == "" {
		return "(not set)"
	}
	if len(s) <= 8 {
		return "****"
	}
	return s[:4] + "…" + s[len(s)-4:]
}
