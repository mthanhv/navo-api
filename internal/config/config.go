// Package config reads settings from the environment. Secrets never have defaults.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"navo-api/internal/mailer"
)

// Config holds runtime settings.
type Config struct {
	Addr           string
	DatabaseURL    string
	SessionTTL     time.Duration
	CookieSecure   bool
	AllowedOrigins []string
	Timezone       *time.Location
	FilesDir       string
	// AnthropicAPIKey enables AI drafts (O-17); empty keeps the feature off.
	AnthropicAPIKey string
	AIModel         string
	// SMTP enables product-news emails to customers; empty host keeps the feature off.
	SMTP mailer.Config
}

// Load reads the environment.
func Load() (Config, error) {
	cfg := Config{
		Addr:            envOr("NAVO_ADDR", ":8080"),
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		FilesDir:        envOr("NAVO_FILES_DIR", "var/files"),
		AnthropicAPIKey: os.Getenv("ANTHROPIC_API_KEY"),
		AIModel:         envOr("NAVO_AI_MODEL", "claude-opus-5-5"),
		SMTP: mailer.Config{Host: os.Getenv("NAVO_SMTP_HOST"), Port: envOr("NAVO_SMTP_PORT", "587"), Username: os.Getenv("NAVO_SMTP_USERNAME"),
			Password: os.Getenv("NAVO_SMTP_PASSWORD"), From: os.Getenv("NAVO_MAIL_FROM")},
	}
	if _, err := mailer.New(cfg.SMTP); err != nil {
		return cfg, err
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
