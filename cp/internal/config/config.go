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
}

func FromEnv() (Config, error) {
	cfg := Config{
		Addr:        getenv("CP_ADDR", ":8080"),
		DatabaseURL: os.Getenv("CP_DATABASE_URL"),
		Env:         getenv("CP_ENV", "dev"),
	}
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("CP_DATABASE_URL is required")
	}
	return cfg, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
