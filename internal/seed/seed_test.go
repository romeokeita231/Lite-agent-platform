package seed

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/romeokeita231/Lite-agent-platform/internal/testutil"
)

func TestSeedKeysUsesEnvAndGeneratesRandomDefaults(t *testing.T) {
	t.Setenv("DEMO_API_KEY_A", "")
	t.Setenv("DEMO_API_KEY_B", "")
	keyA, keyB, err := seedKeys()
	if err != nil {
		t.Fatalf("seedKeys: %v", err)
	}
	if len(keyA) != 64 || len(keyB) != 64 {
		t.Fatalf("generated keys = %q %q", keyA, keyB)
	}
	if keyA == keyB || keyA == "demo-a-api-key" || keyB == "demo-b-api-key" {
		t.Fatalf("generated keys are not random: %q %q", keyA, keyB)
	}

	t.Setenv("DEMO_API_KEY_A", "env-key-a")
	keyA, keyB, err = seedKeys()
	if err != nil {
		t.Fatalf("seedKeys with env: %v", err)
	}
	if keyA != "env-key-a" || len(keyB) != 64 {
		t.Fatalf("seedKeys with env = %q %q", keyA, keyB)
	}
}

func TestSeedIsIdempotentAndStoresHashedKeys(t *testing.T) {
	env := testutil.New(t)
	const (
		policyURL = "http://policy.example:9090"
		keyA      = "key-demo-a-0123456789"
		keyB      = "key-demo-b-0123456789"
	)
	var first bytes.Buffer
	result, err := seed(context.Background(), env.Pool, policyURL, keyA, keyB, &first)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if len(result.Tenants) != 2 || result.PolicyURL != policyURL {
		t.Fatalf("seed result = %+v", result)
	}
	var decoded Result
	if err := json.Unmarshal(first.Bytes(), &decoded); err != nil {
		t.Fatalf("decode seed output %q: %v", first.String(), err)
	}
	if decoded.Tenants[0].ID != "demo-a" || decoded.Tenants[0].APIKey != keyA {
		t.Fatalf("seed output tenant = %+v", decoded.Tenants[0])
	}
	if len(decoded.Tenants[0].Agents) != 4 {
		t.Fatalf("seed output agents = %+v", decoded.Tenants[0].Agents)
	}

	var hash, prefix string
	if err := env.Pool.QueryRow(context.Background(),
		`select api_key_hash, api_key_prefix from tenants where id = 'demo-a'`,
	).Scan(&hash, &prefix); err != nil {
		t.Fatalf("read tenant: %v", err)
	}
	sum := sha256.Sum256([]byte(keyA))
	if hash != hex.EncodeToString(sum[:]) {
		t.Fatalf("api_key_hash = %q, want %q", hash, hex.EncodeToString(sum[:]))
	}
	if prefix != keyA[:8] {
		t.Fatalf("api_key_prefix = %q, want %q", prefix, keyA[:8])
	}

	var tenants, agents, tools, routes int
	if err := env.Pool.QueryRow(context.Background(),
		`select
		   (select count(*) from tenants),
		   (select count(*) from agents),
		   (select count(*) from tool_registry),
		   (select count(*) from model_routes)`,
	).Scan(&tenants, &agents, &tools, &routes); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if tenants != 2 || agents != 8 || tools != 4 || routes != 6 {
		t.Fatalf("after first seed tenants=%d agents=%d tools=%d routes=%d", tenants, agents, tools, routes)
	}

	var second bytes.Buffer
	if _, err := seed(context.Background(), env.Pool, policyURL, keyA, keyB, &second); err != nil {
		t.Fatalf("second seed: %v", err)
	}
	if second.String() != first.String() {
		t.Fatalf("seed output changed between runs:\n%s\n%s", first.String(), second.String())
	}
	if err := env.Pool.QueryRow(context.Background(),
		`select
		   (select count(*) from tenants),
		   (select count(*) from agents),
		   (select count(*) from tool_registry),
		   (select count(*) from model_routes)`,
	).Scan(&tenants, &agents, &tools, &routes); err != nil {
		t.Fatalf("count rows after second seed: %v", err)
	}
	if tenants != 2 || agents != 8 || tools != 4 || routes != 6 {
		t.Fatalf("after second seed tenants=%d agents=%d tools=%d routes=%d", tenants, agents, tools, routes)
	}

	var endpoint string
	if err := env.Pool.QueryRow(context.Background(),
		`select endpoint from tool_registry where tenant_id = 'demo-a' and tool_name = 'echo'`,
	).Scan(&endpoint); err != nil {
		t.Fatalf("read echo tool: %v", err)
	}
	if endpoint != policyURL+"/demo/tool/echo" {
		t.Fatalf("echo endpoint = %q", endpoint)
	}
}
