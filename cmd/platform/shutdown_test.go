package main

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/romeokeita231/Lite-agent-platform/internal/domain"
	"github.com/romeokeita231/Lite-agent-platform/internal/ids"
	"github.com/romeokeita231/Lite-agent-platform/internal/repository"
	"github.com/romeokeita231/Lite-agent-platform/internal/testutil"
)

func TestGracefulShutdownDrainsInflightRunAndRecovers(t *testing.T) {
	env := testutil.New(t)
	store := repository.New(env.Pool)

	const (
		secret   = "shutdown-secret"
		apiKey   = "shutdown-api-key-0123456789"
		tenantID = "tenant-shutdown"
	)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	tenantCtx := repository.WithTenant(context.Background(), tenantID)

	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce, releaseOnce sync.Once
	var calls int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if atomic.AddInt64(&calls, 1) == 1 {
			startOnce.Do(func() { close(started) })
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"graceful"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	}))
	t.Cleanup(upstream.Close)
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	policy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "unreadable body", http.StatusUnauthorized)
			return
		}
		timestamp := r.Header.Get("X-Timestamp")
		if !hmac.Equal([]byte(recoverySign(secret, r.Method, r.URL.Path, body, timestamp)), []byte(r.Header.Get("X-Signature"))) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		var req struct {
			StepIndex int `json:"step_index"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if req.StepIndex == 0 {
			_, _ = w.Write([]byte(`{"action":"llm_call","route":"route:default","messages":[{"role":"user","content":"hi"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"action":"done","output":{"answer":"done"}}`))
	}))
	t.Cleanup(policy.Close)

	if err := store.CreateTenant(tenantCtx, domain.Tenant{
		ID:           tenantID,
		Name:         tenantID,
		APIKeyHash:   recoveryHash(apiKey),
		APIKeyPrefix: apiKey[:8],
	}); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if _, err := store.UpsertModelRoute(tenantCtx, domain.ModelRoute{
		TenantID: tenantID,
		Label:    "default",
		Chain:    []string{"route:default"},
	}); err != nil {
		t.Fatalf("create model route: %v", err)
	}
	agent, err := store.CreateAgent(tenantCtx, domain.Agent{
		ID:        ids.New(),
		TenantID:  tenantID,
		Name:      "shutdown",
		PolicyURL: policy.URL,
		SecretEnv: "POLICY_SECRET_SHUTDOWN",
	})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}

	bin := buildPlatform(t)
	client := &http.Client{}

	addr1 := recoveryFreeAddr(t)
	base1 := "http://" + addr1
	proc1 := startShutdownPlatform(t, bin, env.URL, addr1, secret, upstream.URL+"/v1")
	waitRecoveryReady(t, ctx, base1)

	status, raw := recoveryDo(t, ctx, client, http.MethodPost, base1+"/v1/runs", apiKey, map[string]any{
		"agent_id": agent.ID,
		"input":    map[string]string{"order_id": "A1"},
	})
	if status != http.StatusCreated {
		t.Fatalf("create run status = %d body=%s", status, raw)
	}
	var run domain.Run
	if err := json.Unmarshal(raw, &run); err != nil {
		t.Fatalf("decode created run: %v", err)
	}

	select {
	case <-started:
	case <-ctx.Done():
		t.Fatalf("inflight model call did not start: %v", ctx.Err())
	}

	signalShutdown(t, proc1)

	waitShutdownNotReady(t, ctx, base1)

	if status := getStatus(t, base1+"/healthz"); status != http.StatusOK {
		t.Fatalf("healthz during shutdown = %d, want 200", status)
	}

	status, raw = recoveryDo(t, ctx, client, http.MethodPost, base1+"/v1/runs", apiKey, map[string]any{"agent_id": agent.ID})
	if status != http.StatusServiceUnavailable {
		t.Fatalf("create run during shutdown status = %d body=%s", status, raw)
	}
	var errBody struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &errBody); err != nil {
		t.Fatalf("decode shutdown error %s: %v", raw, err)
	}
	if errBody.Error != "unavailable" {
		t.Fatalf("shutdown error code = %q, want unavailable", errBody.Error)
	}

	status, raw = recoveryDo(t, ctx, client, http.MethodGet, base1+"/v1/runs/"+run.ID, apiKey, nil)
	if status != http.StatusOK {
		t.Fatalf("get run during shutdown status = %d body=%s", status, raw)
	}
	var inflight domain.Run
	if err := json.Unmarshal(raw, &inflight); err != nil {
		t.Fatalf("decode inflight run: %v", err)
	}
	if inflight.Status != domain.RunRunning {
		t.Fatalf("inflight run status = %q, want running", inflight.Status)
	}

	releaseOnce.Do(func() { close(release) })

	exitErr := make(chan error, 1)
	go func() { exitErr <- proc1.cmd.Wait() }()
	select {
	case err := <-exitErr:
		if err != nil {
			t.Fatalf("platform exited with error after graceful shutdown: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("platform did not exit after graceful shutdown")
	}

	var runStatus string
	if err := env.Pool.QueryRow(context.Background(),
		`select status from runs where id::text = $1`, run.ID).Scan(&runStatus); err != nil {
		t.Fatalf("read run status: %v", err)
	}
	if runStatus != string(domain.RunQueued) {
		t.Fatalf("run status after shutdown = %q, want queued", runStatus)
	}
	var running int
	if err := env.Pool.QueryRow(context.Background(),
		`select count(*) from runs where status = 'running'`).Scan(&running); err != nil {
		t.Fatalf("count running runs: %v", err)
	}
	if running != 0 {
		t.Fatalf("running runs after shutdown = %d, want 0", running)
	}
	var checkpointCount, distinctSteps int
	if err := env.Pool.QueryRow(context.Background(),
		`select count(*), count(distinct step_index) from checkpoints where run_id::text = $1`, run.ID,
	).Scan(&checkpointCount, &distinctSteps); err != nil {
		t.Fatalf("count checkpoints: %v", err)
	}
	if checkpointCount != 1 || distinctSteps != 1 {
		t.Fatalf("checkpoints after shutdown = %d (distinct %d), want 1", checkpointCount, distinctSteps)
	}
	var checkpointType string
	if err := env.Pool.QueryRow(context.Background(),
		`select step_type from checkpoints where run_id::text = $1 and step_index = 0`, run.ID).Scan(&checkpointType); err != nil {
		t.Fatalf("read checkpoint type: %v", err)
	}
	if checkpointType != string(domain.StepLLMCall) {
		t.Fatalf("checkpoint step_type = %q, want llm_call", checkpointType)
	}

	addr2 := recoveryFreeAddr(t)
	base2 := "http://" + addr2
	startShutdownPlatform(t, bin, env.URL, addr2, secret, upstream.URL+"/v1")
	waitRecoveryReady(t, ctx, base2)

	completed := waitRecoveryRun(t, ctx, client, base2, apiKey, run.ID, domain.RunCompleted)
	if string(completed.Output) != `{"answer":"done"}` {
		t.Fatalf("completed run output = %s", completed.Output)
	}

	steps := getRecoverySteps(t, ctx, client, base2, apiKey, run.ID)
	if len(steps) != 2 {
		t.Fatalf("steps after restart = %d, want 2", len(steps))
	}
	for i, step := range steps {
		if step.StepIndex != i {
			t.Fatalf("step indices duplicated or unordered: %+v", steps)
		}
	}
	if steps[0].StepType != "llm_call" || steps[1].StepType != "done" {
		t.Fatalf("step types = %q, %q", steps[0].StepType, steps[1].StepType)
	}
	var llm struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(steps[0].Result, &llm); err != nil {
		t.Fatalf("decode step 0 result: %v", err)
	}
	if llm.Content != "graceful" {
		t.Fatalf("step 0 content = %q, want graceful", llm.Content)
	}
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Fatalf("model upstream calls = %d, want 1", got)
	}
	if err := env.Pool.QueryRow(context.Background(),
		`select count(*) from runs where status = 'running'`).Scan(&running); err != nil {
		t.Fatalf("count running runs after restart: %v", err)
	}
	if running != 0 {
		t.Fatalf("running runs after restart = %d, want 0", running)
	}
}

func startShutdownPlatform(t *testing.T, bin, dbURL, addr, secret, modelBaseURL string) *platformProcess {
	t.Helper()
	logFile, err := os.CreateTemp(t.TempDir(), "platform-*.log")
	if err != nil {
		t.Fatalf("create log file: %v", err)
	}
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"DATABASE_URL="+dbURL,
		"HTTP_ADDR="+addr,
		"POLICY_SECRET_SHUTDOWN="+secret,
		"MODEL_BASE_URL="+modelBaseURL,
		"POLL_INTERVAL=50ms",
		"STEP_TIMEOUT=5s",
		"MAX_STEP_ATTEMPTS=3",
	)
	cmd.SysProcAttr = shutdownSysProcAttr()
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatalf("start platform: %v", err)
	}
	proc := &platformProcess{cmd: cmd, log: logFile, path: logFile.Name()}
	t.Cleanup(func() {
		proc.stop()
		if t.Failed() {
			if raw, err := os.ReadFile(proc.path); err == nil {
				t.Logf("platform log:\n%s", raw)
			}
		}
		_ = os.Remove(proc.path)
	})
	return proc
}

func waitShutdownNotReady(t *testing.T, ctx context.Context, base string) {
	t.Helper()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/readyz", nil)
		if err == nil {
			resp, callErr := http.DefaultClient.Do(req)
			if callErr == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					if resp.StatusCode != http.StatusServiceUnavailable {
						t.Fatalf("readyz during shutdown = %d, want 503", resp.StatusCode)
					}
					return
				}
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("readyz stayed ready during shutdown: %v", ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
}
