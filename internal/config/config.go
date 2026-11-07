package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	DatabaseURL       string
	HTTPAddr          string
	StepTimeout       time.Duration
	MaxStepAttempts   int
	TenantConcurrency int
	TenantRate        float64
	ModelBaseURL      string
	ModelAPIKey       string
	ModelChainBudget  time.Duration
	ContextRecent     int
	ContextMaxKeep    int
	PollInterval      time.Duration
}

func Load() (Config, error) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}

	stepTimeout, err := durationEnv("STEP_TIMEOUT", 10*time.Second)
	if err != nil {
		return Config{}, err
	}
	maxStepAttempts, err := intEnv("MAX_STEP_ATTEMPTS", 4)
	if err != nil {
		return Config{}, err
	}
	tenantConcurrency, err := intEnv("TENANT_CONCURRENCY", 4)
	if err != nil {
		return Config{}, err
	}
	tenantRate, err := floatEnv("TENANT_RATE", 0)
	if err != nil {
		return Config{}, err
	}
	if tenantConcurrency <= 0 {
		return Config{}, fmt.Errorf("TENANT_CONCURRENCY must be a positive integer")
	}
	if tenantRate < 0 {
		return Config{}, fmt.Errorf("TENANT_RATE must not be negative")
	}
	modelChainBudget, err := durationEnv("MODEL_CHAIN_BUDGET", stepTimeout)
	if err != nil {
		return Config{}, err
	}
	contextRecent, err := intEnv("CONTEXT_RECENT", 8)
	if err != nil {
		return Config{}, err
	}
	contextMaxKeep, err := intEnv("CONTEXT_MAX_KEEP", 16)
	if err != nil {
		return Config{}, err
	}
	pollInterval, err := durationEnv("POLL_INTERVAL", 100*time.Millisecond)
	if err != nil {
		return Config{}, err
	}

	return Config{
		DatabaseURL:       databaseURL,
		HTTPAddr:          envOr("HTTP_ADDR", ":8080"),
		StepTimeout:       stepTimeout,
		MaxStepAttempts:   maxStepAttempts,
		TenantConcurrency: tenantConcurrency,
		TenantRate:        tenantRate,
		ModelBaseURL:      os.Getenv("MODEL_BASE_URL"),
		ModelAPIKey:       os.Getenv("MODEL_API_KEY"),
		ModelChainBudget:  modelChainBudget,
		ContextRecent:     contextRecent,
		ContextMaxKeep:    contextMaxKeep,
		PollInterval:      pollInterval,
	}, nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func durationEnv(key string, fallback time.Duration) (time.Duration, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return parsed, nil
}

func intEnv(key string, fallback int) (int, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return parsed, nil
}

func floatEnv(key string, fallback float64) (float64, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return parsed, nil
}
