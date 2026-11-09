package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/romeokeita231/Lite-agent-platform/internal/demo"
	"github.com/romeokeita231/Lite-agent-platform/internal/migrate"
	"github.com/romeokeita231/Lite-agent-platform/internal/seed"
)

const (
	demoPolicySecret = "demo-policy-secret"
	demoAPIKeyA      = "demo-a-api-key"
	demoAPIKeyB      = "demo-b-api-key"
	demoStopTimeout  = 15 * time.Second
)

type demoSeedResult struct {
	Tenants []struct {
		ID     string `json:"id"`
		APIKey string `json:"api_key"`
	} `json:"tenants"`
}

func runDemo(reset bool, w io.Writer) error {
	secret := os.Getenv("POLICY_SECRET")
	if secret == "" {
		secret = demoPolicySecret
	}
	supervisor, err := demo.New(demo.Options{
		Reset:          reset,
		PostgresBinDir: os.Getenv("DEMO_POSTGRES_BIN_DIR"),
		PolicySecret:   secret,
	})
	if err != nil {
		return err
	}
	defer shutdownDemo(supervisor)

	ctx := context.Background()
	if err := supervisor.Prepare(ctx); err != nil {
		return err
	}
	dsn, err := supervisor.StartDatabase(ctx)
	if err != nil {
		return err
	}
	ports := supervisor.Ports()
	policyAddr := hostPort(ports.PolicyAddr)
	policyURL := "http://" + policyAddr

	if err := os.Setenv("DATABASE_URL", dsn); err != nil {
		return err
	}
	if err := os.Setenv("HTTP_ADDR", ports.HTTPAddr); err != nil {
		return err
	}
	defaults := [][2]string{
		{"POLICY_URL", policyURL},
		{"MODEL_BASE_URL", policyURL + "/v1"},
		{"DEMO_API_KEY_A", demoAPIKeyA},
		{"DEMO_API_KEY_B", demoAPIKeyB},
		{"POLICY_SECRET_DEMO", secret},
		{"POLICY_SECRET_TOOL_SLOW", secret},
		{"POLICY_SECRET_HUMAN", secret},
	}
	for _, pair := range defaults {
		if err := setDefaultEnv(pair[0], pair[1]); err != nil {
			return err
		}
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}
	migrationErr := migrate.Run(ctx, pool)
	pool.Close()
	if migrationErr != nil {
		return fmt.Errorf("run migrations: %w", migrationErr)
	}

	var output bytes.Buffer
	if err := seed.Run(&output); err != nil {
		return fmt.Errorf("seed demo data: %w", err)
	}
	result, err := parseDemoSeed(output.Bytes())
	if err != nil {
		return fmt.Errorf("seed demo data: %w", err)
	}

	if err := supervisor.StartPolicy(ctx); err != nil {
		return err
	}

	fmt.Fprintf(w, "demo stack ready\n")
	fmt.Fprintf(w, "console: http://%s/console\n", hostPort(ports.HTTPAddr))
	for _, tenant := range result.Tenants {
		fmt.Fprintf(w, "tenant %s api key: %s\n", tenant.ID, tenant.APIKey)
	}

	return run()
}

func shutdownDemo(supervisor *demo.Supervisor) {
	ctx, cancel := context.WithTimeout(context.Background(), demoStopTimeout)
	defer cancel()
	if err := supervisor.Stop(ctx); err != nil {
		logShutdownError(err)
	}
}

func parseDemoSeed(raw []byte) (demoSeedResult, error) {
	var result demoSeedResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return result, err
	}
	if len(result.Tenants) == 0 {
		return result, errors.New("seed produced no tenants")
	}
	return result, nil
}

func hostPort(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func setDefaultEnv(key, value string) error {
	if os.Getenv(key) != "" {
		return nil
	}
	return os.Setenv(key, value)
}

func logShutdownError(err error) {
	fmt.Fprintf(os.Stderr, "platform demo: shutdown: %v\n", err)
}
