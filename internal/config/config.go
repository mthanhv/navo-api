// Package config reads settings from the environment. Secrets never have defaults.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds runtime settings.
type Config struct {
	Addr           string
	DatabaseURL    string
	SessionTTL     time.Duration
	CookieSecure   bool
	AllowedOrigins []string
	Timezone       *time.Location
}

// Load reads the environment.
func Load() (Config, error) {
	cfg := Config{
		Addr:        envOr("NAVO_ADDR", ":8080"),
		DatabaseURL: os.Getenv("DATABASE_URL"),
	}
	if cfg.DatabaseURL == "" {
		return cfg, fmt.Errorf("DATABASE_URL is required")
	}
	ttl, err := time.ParseDuration(envOr("NAVO_SESSION_TTL", "168h"))
	if err != nil || ttl < time.Hour {
		return cfg, fmt.Errorf("NAVO_SESSION_TTL must be a duration of at least 1h")
	}
	cfg.SessionTTL = ttl
	secure, err := strconv.ParseBool(envOr("NAVO_COOKIE_SECURE", "true"))
	if err != nil {
		return cfg, fmt.Errorf("NAVO_COOKIE_SECURE must be true or false")
	}
	cfg.CookieSecure = secure
	for _, o := range strings.Split(os.Getenv("NAVO_ALLOWED_ORIGINS"), ",") {
		if o = strings.TrimSpace(o); o != "" {
			cfg.AllowedOrigins = append(cfg.AllowedOrigins, o)
		}
	}
	loc, err := time.LoadLocation(envOr("NAVO_TIMEZONE", "Asia/Ho_Chi_Minh"))
	if err != nil {
		return cfg, fmt.Errorf("NAVO_TIMEZONE: %w", err)
	}
	cfg.Timezone = loc
	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
