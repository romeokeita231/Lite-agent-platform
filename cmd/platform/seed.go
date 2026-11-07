package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/romeokeita231/Lite-agent-platform/internal/config"
	"github.com/romeokeita231/Lite-agent-platform/internal/ids"
	"github.com/romeokeita231/Lite-agent-platform/internal/migrate"
)

const (
	defaultPolicyURL = "http://policy-example:9090"
)

type seededTenant struct {
	ID     string   `json:"id"`
	APIKey string   `json:"api_key"`
	Agents []string `json:"agents"`
}

type seedResult struct {
	PolicyURL string         `json:"policy_url"`
	Tenants   []seededTenant `json:"tenants"`
}

func runSeed(w io.Writer) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := migrate.Run(ctx, pool); err != nil {
		return err
	}
	keyA, keyB, err := seedKeys()
	if err != nil {
		return err
	}
	policyURL := defaultPolicyURL
	if value := os.Getenv("POLICY_URL"); value != "" {
		policyURL = value
	}
	_, err = seed(ctx, pool, policyURL, keyA, keyB, w)
	return err
}

func seedKeys() (string, string, error) {
	keyA := os.Getenv("DEMO_API_KEY_A")
	keyB := os.Getenv("DEMO_API_KEY_B")
	var err error
	if keyA == "" {
		if keyA, err = newAPIKey(); err != nil {
			return "", "", err
		}
	}
	if keyB == "" {
		if keyB, err = newAPIKey(); err != nil {
			return "", "", err
		}
	}
	return keyA, keyB, nil
}

func seed(ctx context.Context, pool *pgxpool.Pool, policyURL, keyA, keyB string, w io.Writer) (seedResult, error) {
	result := seedResult{PolicyURL: policyURL, Tenants: []seededTenant{}}
	for _, tenant := range []struct {
		id  string
		key string
	}{
		{id: "demo-a", key: keyA},
		{id: "demo-b", key: keyB},
	} {
		if err := seedTenant(ctx, pool, tenant.id, tenant.key); err != nil {
			return seedResult{}, err
		}
		agents, err := seedAgents(ctx, pool, tenant.id, policyURL)
		if err != nil {
			return seedResult{}, err
		}
		if err := seedModelsAndTools(ctx, pool, tenant.id, policyURL); err != nil {
			return seedResult{}, err
		}
		result.Tenants = append(result.Tenants, seededTenant{
			ID:     tenant.id,
			APIKey: tenant.key,
			Agents: agents,
		})
	}
	if err := json.NewEncoder(w).Encode(result); err != nil {
		return seedResult{}, err
	}
	return result, nil
}

func seedTenant(ctx context.Context, pool *pgxpool.Pool, id, key string) error {
	sum := sha256.Sum256([]byte(key))
	_, err := pool.Exec(ctx,
		`insert into tenants (id, name, api_key_hash, api_key_prefix) values ($1, $2, $3, $4)
		 on conflict (id) do update set name = excluded.name, api_key_hash = excluded.api_key_hash,
		     api_key_prefix = excluded.api_key_prefix`,
		id, id, hex.EncodeToString(sum[:]), keyPrefix(key))
	return err
}

func seedAgents(ctx context.Context, pool *pgxpool.Pool, tenantID, policyURL string) ([]string, error) {
	agents := []struct {
		name   string
		secret string
	}{
		{name: "demo", secret: "POLICY_SECRET_DEMO"},
		{name: "tool-slow", secret: "POLICY_SECRET_TOOL_SLOW"},
		{name: "human", secret: "POLICY_SECRET_HUMAN"},
	}
	names := make([]string, 0, len(agents))
	for _, agent := range agents {
		if _, err := pool.Exec(ctx,
			`insert into agents (id, tenant_id, name, policy_url, secret_env) values ($1, $2, $3, $4, $5)
			 on conflict (tenant_id, name) do update set policy_url = excluded.policy_url, secret_env = excluded.secret_env`,
			ids.New(), tenantID, agent.name, policyURL, agent.secret); err != nil {
			return nil, err
		}
		names = append(names, agent.name)
	}
	return names, nil
}

func seedModelsAndTools(ctx context.Context, pool *pgxpool.Pool, tenantID, policyURL string) error {
	if _, err := pool.Exec(ctx,
		`insert into model_routes (tenant_id, label, chain) values ($1, 'default', '["demo-model"]')
		 on conflict (tenant_id, label) do update set chain = excluded.chain`,
		tenantID); err != nil {
		return err
	}
	base := strings.TrimSuffix(policyURL, "/")
	for _, tool := range []struct {
		name     string
		endpoint string
	}{
		{name: "echo", endpoint: base + "/demo/tool/echo"},
		{name: "slow", endpoint: base + "/demo/tool/slow"},
	} {
		if _, err := pool.Exec(ctx,
			`insert into tool_registry (tenant_id, tool_name, version, description, "schema", endpoint, is_default)
			 values ($1, $2, '1', 'demo tool', '{}', $3, true)
			 on conflict (tenant_id, tool_name, version) do update set description = excluded.description,
			     "schema" = excluded."schema", endpoint = excluded.endpoint, is_default = excluded.is_default`,
			tenantID, tool.name, tool.endpoint); err != nil {
			return err
		}
	}
	return nil
}

func keyPrefix(key string) string {
	if len(key) < 8 {
		return key
	}
	return key[:8]
}
