// Package config loads and validates process configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	KitchenDatabaseURL    string
	RestaurantDatabaseURL string
	KitchenHTTPAddr       string
	RestaurantHTTPAddr    string
	KitchenAPIURL         string
	DemoPartnerAPIKey     string
	IntegrationAPIKey     string
	WorkerPollInterval    time.Duration
	WorkerHTTPTimeout     time.Duration
	WorkerMaxAttempts     int
	ReadTimeout           time.Duration
	WriteTimeout          time.Duration
	IdleTimeout           time.Duration
	ShutdownTimeout       time.Duration
	MaxBodyBytes          int64
	LogLevel              string
}

func Load() (Config, error) {
	cfg := Config{
		KitchenDatabaseURL:    os.Getenv("KITCHEN_DATABASE_URL"),
		RestaurantDatabaseURL: os.Getenv("RESTAURANT_DATABASE_URL"),
		KitchenHTTPAddr:       env("KITCHEN_HTTP_ADDR", ":8080"),
		RestaurantHTTPAddr:    env("RESTAURANT_HTTP_ADDR", ":8081"),
		KitchenAPIURL:         env("KITCHEN_API_URL", "http://localhost:8080"),
		DemoPartnerAPIKey:     env("DEMO_PARTNER_API_KEY", "demo-partner-key-change-me"),
		IntegrationAPIKey:     env("INTEGRATION_API_KEY", "demo-integration-key-change-me"),
		LogLevel:              env("LOG_LEVEL", "info"),
	}

	var err error
	if cfg.WorkerPollInterval, err = duration("WORKER_POLL_INTERVAL", 250*time.Millisecond); err != nil {
		return Config{}, err
	}
	if cfg.WorkerHTTPTimeout, err = duration("WORKER_HTTP_TIMEOUT", 2*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.ReadTimeout, err = duration("HTTP_READ_TIMEOUT", 5*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.WriteTimeout, err = duration("HTTP_WRITE_TIMEOUT", 10*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.IdleTimeout, err = duration("HTTP_IDLE_TIMEOUT", 60*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.ShutdownTimeout, err = duration("HTTP_SHUTDOWN_TIMEOUT", 10*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.WorkerMaxAttempts, err = integer("WORKER_MAX_ATTEMPTS", 5); err != nil {
		return Config{}, err
	}
	if cfg.MaxBodyBytes, err = integer64("HTTP_MAX_BODY_BYTES", 1<<20); err != nil {
		return Config{}, err
	}
	if cfg.WorkerMaxAttempts < 1 || cfg.MaxBodyBytes < 1024 {
		return Config{}, errors.New("WORKER_MAX_ATTEMPTS must be positive and HTTP_MAX_BODY_BYTES must be at least 1024")
	}
	return cfg, nil
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func duration(name string, fallback time.Duration) (time.Duration, error) {
	raw := env(name, fallback.String())
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("invalid %s: %q", name, raw)
	}
	return value, nil
}

func integer(name string, fallback int) (int, error) {
	raw := env(name, strconv.Itoa(fallback))
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %q", name, raw)
	}
	return value, nil
}

func integer64(name string, fallback int64) (int64, error) {
	raw := env(name, strconv.FormatInt(fallback, 10))
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %q", name, raw)
	}
	return value, nil
}
