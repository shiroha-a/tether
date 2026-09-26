// Package config loads tether runtime settings from environment variables.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config holds all runtime settings.
type Config struct {
	Addr  string
	Allow string
	Hosts []string
	// Auth enables bearer-token authentication (TETHER_AUTH, default off).
	Auth           bool
	Token          string
	Root           string
	DataDir        string
	ClaudeBin      string
	DiscordWebhook string
	IdleTimeout    time.Duration
}

// Load reads the configuration from the environment and validates it.
func Load() (*Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home: %w", err)
	}
	c := &Config{
		// 全インターフェースで待ち受け、接続元はAllow（既定はloopbackとtailscale）で絞る
		Addr:           env("TETHER_ADDR", ":3100"),
		Allow:          env("TETHER_ALLOW", "loopback,tailscale"),
		Token:          os.Getenv("TETHER_TOKEN"),
		Root:           env("TETHER_ROOT", home),
		DataDir:        env("TETHER_DATA_DIR", filepath.Join(home, ".local", "share", "tether")),
		ClaudeBin:      env("TETHER_CLAUDE_BIN", "claude"),
		DiscordWebhook: os.Getenv("TETHER_DISCORD_WEBHOOK"),
		IdleTimeout:    24 * time.Hour,
	}
	for _, h := range strings.Split(os.Getenv("TETHER_HOSTS"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			c.Hosts = append(c.Hosts, h)
		}
	}
	auth, err := parseSwitch(os.Getenv("TETHER_AUTH"))
	if err != nil {
		return nil, fmt.Errorf("TETHER_AUTH: %w", err)
	}
	c.Auth = auth
	// 認証を有効にしたつもりでトークンが空だと無認証で動いてしまうので、起動させない
	if c.Auth && c.Token == "" {
		return nil, fmt.Errorf("TETHER_AUTH=on requires TETHER_TOKEN")
	}
	if v := os.Getenv("TETHER_IDLE_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("TETHER_IDLE_TIMEOUT: %w", err)
		}
		c.IdleTimeout = d
	}
	// ROOTはシンボリックリンク解決後の実体パスで比較するため、ここで正規化しておく
	root, err := filepath.EvalSymlinks(c.Root)
	if err != nil {
		return nil, fmt.Errorf("TETHER_ROOT: %w", err)
	}
	c.Root = root
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	return c, nil
}

// AuthToken returns the token the server must require, or "" when
// authentication is off (a TETHER_TOKEN left in the config file is ignored).
func (c *Config) AuthToken() string {
	if !c.Auth {
		return ""
	}
	return c.Token
}

// parseSwitch parses on/off style values; empty means off.
func parseSwitch(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "off", "false", "0", "no":
		return false, nil
	case "on", "true", "1", "yes":
		return true, nil
	}
	return false, fmt.Errorf("invalid value %q (use on or off)", v)
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
