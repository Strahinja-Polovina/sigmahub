// Package config resolves control-plane configuration from the environment.
package config

import (
	"fmt"
	"os"
)

type Config struct {
	// Addr is the HTTP listen address, e.g. ":8080".
	Addr string
	// DatabaseURL is the PostgreSQL connection string.
	DatabaseURL string
	// Env is "dev" or "prod"; switches log format.
	Env string
	// ServiceToken gates dashboard-facing endpoints (placeholder until the
	// P0-6 token model). Defaults in dev, required in prod.
	ServiceToken string
}

func FromEnv() (Config, error) {
	cfg := Config{
		Addr:         getenv("CP_ADDR", ":8080"),
		DatabaseURL:  os.Getenv("CP_DATABASE_URL"),
		Env:          getenv("CP_ENV", "dev"),
		ServiceToken: os.Getenv("CP_SERVICE_TOKEN"),
	}
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("CP_DATABASE_URL is required")
	}
	// Validate Env explicitly: a typo (e.g. "production") must not silently
	// fall through to the dev defaults below (fail-open on auth).
	switch cfg.Env {
	case "dev", "prod":
	default:
		return Config{}, fmt.Errorf(`CP_ENV must be "dev" or "prod", got %q`, cfg.Env)
	}
	if cfg.ServiceToken == "" {
		if cfg.Env != "dev" {
			return Config{}, fmt.Errorf("CP_SERVICE_TOKEN is required unless CP_ENV=dev")
		}
		cfg.ServiceToken = "dev-service-token"
	}
	return cfg, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
