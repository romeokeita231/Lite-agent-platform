package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/romeokeita231/Lite-agent-platform/internal/demo"
	"github.com/romeokeita231/Lite-agent-platform/internal/seed"
)

const (
	demoPolicySecret = "demo-policy-secret"
	demoAPIKeyA      = "demo-a-api-key"
	demoAPIKeyB      = "demo-b-api-key"
	demoStopTimeout  = 15 * time.Second

	defaultMetricsAddr = "127.0.0.1:9091"
)

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
	ports := supervisor.Ports()
	if err := checkListenAddress(ports.HTTPAddr); err != nil {
		return fmt.Errorf("start http: %v; stop the process using it (the compose stack uses port 8080) or set HTTP_ADDR to a different port", err)
	}
	metricsAddr := os.Getenv("METRICS_ADDR")
	if metricsAddr == "" {
		metricsAddr = defaultMetricsAddr
	}
	if err := checkListenAddress(metricsAddr); err != nil {
		fmt.Fprintf(os.Stderr, "demo: warning: metrics will not be served: %v; stop the process using it (the compose stack uses port 9091) or set METRICS_ADDR to a different port\n", err)
	}
	dsn, err := supervisor.StartDatabase(ctx)
	if err != nil {
		return err
	}
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

	result, err := seed.Run(io.Discard)
	if err != nil {
		return fmt.Errorf("seed demo data: %w", err)
	}
	if len(result.Tenants) == 0 {
		return errors.New("seed demo data: seed produced no tenants")
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

func checkListenAddress(addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err == nil {
		return listener.Close()
	}
	_, port, splitErr := net.SplitHostPort(addr)
	if splitErr != nil {
		port = addr
	}
	return fmt.Errorf("port %s is already in use by another process", port)
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
