package httpapi_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/romeokeita231/Lite-agent-platform/internal/domain"
	"github.com/romeokeita231/Lite-agent-platform/internal/httpapi"
	"github.com/romeokeita231/Lite-agent-platform/internal/repository"
	"github.com/romeokeita231/Lite-agent-platform/internal/testutil"
)

type alwaysReady struct{}

func (alwaysReady) Ready() bool { return true }

type apiEnv struct {
	env *testutil.Env
	srv *httptest.Server
	key map[string]string
}

func newAPI(t *testing.T) *apiEnv {
	t.Helper()
	env := testutil.New(t)
	store := repository.New(env.Pool)
	env2 := &apiEnv{
		env: env,
		key: map[string]string{
			"tenant-a": "key-tenant-a-0123456789",
			"tenant-b": "key-tenant-b-0123456789",
		},
	}
	for tenantID, key := range env2.key {
		ctx := repository.WithTenant(context.Background(), tenantID)
		if err := store.CreateTenant(ctx, domain.Tenant{
			ID:           tenantID,
			Name:         tenantID,
			APIKeyHash:   hashHex(key),
			APIKeyPrefix: key[:8],
		}); err != nil {
			t.Fatalf("create tenant %s: %v", tenantID, err)
		}
	}
	env2.srv = httptest.NewServer(httpapi.NewMux(alwaysReady{}, store))
	t.Cleanup(env2.srv.Close)
	return env2
}

type errorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

func (e *apiEnv) do(t *testing.T, method, path, tenant string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if tenant != "" {
		req.Header.Set("Authorization", "Bearer "+e.key[tenant])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("%s %s content-type = %q", method, path, got)
	}
	return resp.StatusCode, raw
}

func decodeError(t *testing.T, raw []byte) errorBody {
	t.Helper()
	var body errorBody
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode error body %s: %v", raw, err)
	}
	return body
}

func TestAPIAuthentication(t *testing.T) {
	api := newAPI(t)

	if status, raw := api.do(t, http.MethodGet, "/v1/agents", "", nil); status != http.StatusUnauthorized {
		t.Fatalf("missing key status = %d body=%s", status, raw)
	} else if body := decodeError(t, raw); body.Error != "unauthorized" || body.Message == "" {
		t.Fatalf("missing key body = %+v", body)
	}

	req, err := http.NewRequest(http.MethodGet, api.srv.URL+"/v1/agents", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer not-a-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unknown key status = %d body=%s", resp.StatusCode, raw)
	}
	if body := decodeError(t, raw); body.Error != "unauthorized" {
		t.Fatalf("unknown key body = %+v", body)
	}

	if status, raw := api.do(t, http.MethodGet, "/v1/agents", "tenant-a", nil); status != http.StatusOK {
		t.Fatalf("valid key status = %d body=%s", status, raw)
	}
}

func TestAgentRegistrationAndListAreTenantScoped(t *testing.T) {
	api := newAPI(t)

	status, raw := api.do(t, http.MethodPost, "/v1/agents", "tenant-a", map[string]string{
		"name":       "alpha",
		"policy_url": "http://policy-a",
		"secret_env": "POLICY_SECRET_ALPHA",
	})
	if status != http.StatusCreated {
		t.Fatalf("create agent status = %d body=%s", status, raw)
	}
	var created domain.Agent
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatalf("decode agent: %v", err)
	}
	if created.ID == "" || created.TenantID != "tenant-a" || created.Name != "alpha" ||
		created.PolicyURL != "http://policy-a" || created.SecretEnv != "POLICY_SECRET_ALPHA" {
		t.Fatalf("created agent = %+v", created)
	}

	status, raw = api.do(t, http.MethodGet, "/v1/agents", "tenant-a", nil)
	if status != http.StatusOK {
		t.Fatalf("list agents a status = %d body=%s", status, raw)
	}
	var agentsA []domain.Agent
	if err := json.Unmarshal(raw, &agentsA); err != nil {
		t.Fatalf("decode agents a: %v", err)
	}
	if len(agentsA) != 1 || agentsA[0].ID != created.ID {
		t.Fatalf("agents a = %+v", agentsA)
	}

	status, raw = api.do(t, http.MethodGet, "/v1/agents", "tenant-b", nil)
	if status != http.StatusOK {
		t.Fatalf("list agents b status = %d body=%s", status, raw)
	}
	var agentsB []domain.Agent
	if err := json.Unmarshal(raw, &agentsB); err != nil {
		t.Fatalf("decode agents b: %v", err)
	}
	if len(agentsB) != 0 {
		t.Fatalf("tenant b sees agents = %+v", agentsB)
	}

	status, raw = api.do(t, http.MethodPost, "/v1/agents", "tenant-a", map[string]string{
		"name":       "alpha",
		"policy_url": "http://policy-a2",
		"secret_env": "POLICY_SECRET_ALPHA",
	})
	if status != http.StatusConflict {
		t.Fatalf("duplicate agent status = %d body=%s", status, raw)
	}
	if body := decodeError(t, raw); body.Error != "conflict" {
		t.Fatalf("duplicate agent body = %+v", body)
	}

	status, raw = api.do(t, http.MethodPost, "/v1/agents", "tenant-a", map[string]string{
		"name": "alpha",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("invalid agent status = %d body=%s", status, raw)
	}
	if body := decodeError(t, raw); body.Error != "bad_request" {
		t.Fatalf("invalid agent body = %+v", body)
	}
}

func TestRunSubmissionAndQueryAreTenantScoped(t *testing.T) {
	api := newAPI(t)

	status, raw := api.do(t, http.MethodPost, "/v1/agents", "tenant-a", map[string]string{
		"name":       "alpha",
		"policy_url": "http://policy-a",
		"secret_env": "POLICY_SECRET_ALPHA",
	})
	if status != http.StatusCreated {
		t.Fatalf("create agent status = %d body=%s", status, raw)
	}
	var agent domain.Agent
	if err := json.Unmarshal(raw, &agent); err != nil {
		t.Fatalf("decode agent: %v", err)
	}

	status, raw = api.do(t, http.MethodPost, "/v1/runs", "tenant-a", map[string]any{
		"agent_id": agent.ID,
		"input":    map[string]string{"order_id": "A1"},
	})
	if status != http.StatusCreated {
		t.Fatalf("create run status = %d body=%s", status, raw)
	}
	var created domain.Run
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if created.ID == "" || created.TenantID != "tenant-a" || created.AgentID != agent.ID {
		t.Fatalf("created run = %+v", created)
	}
	if created.Status != domain.RunQueued || created.Attempt != 0 {
		t.Fatalf("created run status/attempt = %s/%d", created.Status, created.Attempt)
	}
	var input struct {
		OrderID string `json:"order_id"`
	}
	if err := json.Unmarshal(created.Input, &input); err != nil || input.OrderID != "A1" {
		t.Fatalf("created run input = %s (%v)", created.Input, err)
	}
	if created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
		t.Fatalf("created run timestamps are zero")
	}

	status, raw = api.do(t, http.MethodGet, "/v1/runs/"+created.ID, "tenant-a", nil)
	if status != http.StatusOK {
		t.Fatalf("get run status = %d body=%s", status, raw)
	}
	var fetched domain.Run
	if err := json.Unmarshal(raw, &fetched); err != nil {
		t.Fatalf("decode fetched run: %v", err)
	}
	if fetched.ID != created.ID || fetched.Status != domain.RunQueued ||
		fetched.TenantID != "tenant-a" || fetched.AgentID != agent.ID {
		t.Fatalf("fetched run = %+v", fetched)
	}
	if fetched.TerminalReason != nil || fetched.EndedAt != nil || !rawNull(fetched.Output) || !rawNull(fetched.PendingStep) {
		t.Fatalf("queued run has terminal fields: %+v", fetched)
	}
	if !bytes.Contains(raw, []byte(`"terminal_reason":null`)) {
		t.Fatalf("fetched run body lacks terminal_reason: %s", raw)
	}

	status, raw = api.do(t, http.MethodGet, "/v1/runs/"+created.ID, "tenant-b", nil)
	if status != http.StatusNotFound {
		t.Fatalf("cross-tenant get status = %d body=%s", status, raw)
	}
	if body := decodeError(t, raw); body.Error != "not_found" {
		t.Fatalf("cross-tenant get body = %+v", body)
	}

	status, raw = api.do(t, http.MethodGet, "/v1/runs/00000000-0000-4000-8000-000000000000", "tenant-a", nil)
	if status != http.StatusNotFound {
		t.Fatalf("missing run status = %d body=%s", status, raw)
	}

	status, raw = api.do(t, http.MethodPost, "/v1/runs", "tenant-a", map[string]any{
		"input": map[string]string{"order_id": "A2"},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("run without agent status = %d body=%s", status, raw)
	}
	if body := decodeError(t, raw); body.Error != "bad_request" {
		t.Fatalf("run without agent body = %+v", body)
	}
}

func rawNull(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}

func hashHex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
