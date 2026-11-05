// Package config reads all settings from environment variables.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Host     string
	Port     int
	LogLevel slog.Level
	LogHTML  bool

	BrowserPath      string
	BrowserExtraArgs []string
	Headless         bool
	IdleTimeout      time.Duration
	MaxConcurrency   int
	BlockMedia       bool
	SessionTTL       time.Duration
	Lang             string

	// Default proxy, used when a request has none (same env names as FlareSolverr).
	ProxyURL      string
	ProxyUsername string
	ProxyPassword string
}

// browserCandidates are looked up in PATH when BROWSER_PATH is empty.
var browserCandidates = []string{
	"chromium", "chromium-browser", "google-chrome-stable", "google-chrome", "thorium-browser",
}

func Load() (*Config, error) {
	c := &Config{
		Host:             env("HOST", "0.0.0.0"),
		BrowserPath:      os.Getenv("BROWSER_PATH"),
		Lang:             os.Getenv("LANG"),
		ProxyURL:         os.Getenv("PROXY_URL"),
		ProxyUsername:    os.Getenv("PROXY_USERNAME"),
		ProxyPassword:    os.Getenv("PROXY_PASSWORD"),
		BrowserExtraArgs: strings.Fields(os.Getenv("BROWSER_EXTRA_ARGS")),
	}
	var err error
	if c.Port, err = envInt("PORT", 8191); err != nil {
		return nil, err
	}
	if c.MaxConcurrency, err = envInt("MAX_CONCURRENCY", 1); err != nil {
		return nil, err
	}
	if c.MaxConcurrency < 1 {
		c.MaxConcurrency = 1
	}
	if c.IdleTimeout, err = envDuration("BROWSER_IDLE_TIMEOUT", 5*time.Minute); err != nil {
		return nil, err
	}
	if c.SessionTTL, err = envDuration("SESSION_TTL", 30*time.Minute); err != nil {
		return nil, err
	}
	if c.Headless, err = envBool("HEADLESS", true); err != nil {
		return nil, err
	}
	if c.BlockMedia, err = envBool("BLOCK_MEDIA", true); err != nil {
		return nil, err
	}
	if c.LogHTML, err = envBool("LOG_HTML", false); err != nil {
		return nil, err
	}
	if c.LogLevel, err = parseLevel(env("LOG_LEVEL", "info")); err != nil {
		return nil, err
	}
	if c.BrowserPath == "" {
		for _, name := range browserCandidates {
			if p, err := exec.LookPath(name); err == nil {
				c.BrowserPath = p
				break
			}
		}
	}
	if c.BrowserPath == "" {
		return nil, fmt.Errorf("no browser found: set BROWSER_PATH")
	}
	return c, nil
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

func envBool(key string, def bool) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: %w", key, err)
	}
	return b, nil
}

// envDuration accepts Go durations ("90s", "5m") or a bare number of minutes.
func envDuration(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Minute, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("LOG_LEVEL: unknown level %q", s)
}
