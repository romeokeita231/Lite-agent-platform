package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/romeokeita231/Lite-agent-platform/internal/domain"
	"github.com/romeokeita231/Lite-agent-platform/internal/ids"
	"github.com/romeokeita231/Lite-agent-platform/internal/repository"
	"github.com/romeokeita231/Lite-agent-platform/internal/testutil"
)

type recoveryPolicyRequest struct {
	RunID     string          `json:"run_id"`
	Agent     string          `json:"agent"`
	StepIndex int             `json:"step_index"`
	Attempt   int             `json:"attempt"`
	Input     json.RawMessage `json:"input"`
}

func TestRecoveryReclaimsRunAfterProcessKill(t *testing.T) {
	env := testutil.New(t)
	store := repository.New(env.Pool)

	const (
		secret   = "recovery-secret"
		apiKey   = "recovery-api-key-0123456789"
		tenantID = "tenant-recovery"
	)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	tenantCtx := repository.WithTenant(context.Background(), tenantID)

	blocked := make(chan struct{})
	release := make(chan struct{})
	var blockOnce, releaseOnce sync.Once
	var calls int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		n := atomic.AddInt64(&calls, 1)
		if n == 2 {
			blockOnce.Do(func() { close(blocked) })
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		content := "first"
		switch {
		case n == 2:
			content = "blocked"
		case n >= 3:
			content = "after-restart"
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%q}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`, content)
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
		var req recoveryPolicyRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var probe struct {
			Probe string `json:"probe"`
		}
		_ = json.Unmarshal(req.Input, &probe)
		if probe.Probe == "recovery" {
			_, _ = w.Write([]byte(`{"action":"done","output":{"answer":"recovery"}}`))
			return
		}
		if req.StepIndex <= 1 {
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
		Name:      "recovery",
		PolicyURL: policy.URL,
		SecretEnv: "POLICY_SECRET_RECOVERY",
	})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}

	probeRun, err := store.CreateRun(tenantCtx, domain.Run{
		ID:       ids.New(),
		TenantID: tenantID,
		AgentID:  agent.ID,
		Status:   domain.RunRunning,
		Input:    json.RawMessage(`{"probe":"recovery"}`),
	})
	if err != nil {
		t.Fatalf("create probe run: %v", err)
	}
	if _, err := env.Pool.Exec(context.Background(),
		`update runs set attempt = 2, next_attempt_at = now() where id::text = $1`, probeRun.ID); err != nil {
		t.Fatalf("seed probe run: %v", err)
	}

	bin := buildPlatform(t)
	client := &http.Client{}

	addr1 := recoveryFreeAddr(t)
	base1 := "http://" + addr1
	proc1 := startPlatform(t, bin, env.URL, addr1, secret, upstream.URL+"/v1")
	waitRecoveryReady(t, ctx, base1)

	waitRecoveryRun(t, ctx, client, base1, apiKey, probeRun.ID, domain.RunCompleted)

	status, raw := recoveryDo(t, ctx, client, http.MethodPost, base1+"/v1/runs", apiKey, map[string]any{
		"agent_id": agent.ID,
		"input":    map[string]string{"order_id": "A1"},
	})
	if status != http.StatusCreated {
		t.Fatalf("create run status = %d body=%s", status, raw)
	}
	var mainRun domain.Run
	if err := json.Unmarshal(raw, &mainRun); err != nil {
		t.Fatalf("decode created run: %v", err)
	}

	select {
	case <-blocked:
	case <-ctx.Done():
		t.Fatalf("second model call did not start: %v", ctx.Err())
	}
	proc1.kill(t)

	addr2 := recoveryFreeAddr(t)
	base2 := "http://" + addr2
	startPlatform(t, bin, env.URL, addr2, secret, upstream.URL+"/v1")
	waitRecoveryReady(t, ctx, base2)

	completed := waitRecoveryRun(t, ctx, client, base2, apiKey, mainRun.ID, domain.RunCompleted)
	if string(completed.Output) != `{"answer":"done"}` {
		t.Fatalf("completed run output = %s", completed.Output)
	}

	steps := getRecoverySteps(t, ctx, client, base2, apiKey, mainRun.ID)
	if len(steps) != 3 {
		t.Fatalf("steps = %d, want 3", len(steps))
	}
	for i, step := range steps {
		if step.StepIndex != i {
			t.Fatalf("step indices not ordered: %+v", steps)
		}
	}
	if steps[0].StepType != "llm_call" || steps[1].StepType != "llm_call" || steps[2].StepType != "done" {
		t.Fatalf("step types = %q, %q, %q", steps[0].StepType, steps[1].StepType, steps[2].StepType)
	}
	var firstLLM struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(steps[0].Result, &firstLLM); err != nil {
		t.Fatalf("decode step 0 result: %v", err)
	}
	if firstLLM.Content != "first" {
		t.Fatalf("step 0 content = %q, want %q", firstLLM.Content, "first")
	}
	var secondLLM struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(steps[1].Result, &secondLLM); err != nil {
		t.Fatalf("decode step 1 result: %v", err)
	}
	if secondLLM.Content != "after-restart" {
		t.Fatalf("step 1 content = %q, want %q", secondLLM.Content, "after-restart")
	}
	if got := atomic.LoadInt64(&calls); got != 3 {
		t.Fatalf("model upstream calls = %d, want 3", got)
	}

	probe := waitRecoveryRun(t, ctx, client, base2, apiKey, probeRun.ID, domain.RunCompleted)
	if string(probe.Output) != `{"answer":"recovery"}` {
		t.Fatalf("probe run output = %s", probe.Output)
	}

	var running int
	if err := env.Pool.QueryRow(context.Background(),
		`select count(*) from runs where status = 'running'`).Scan(&running); err != nil {
		t.Fatalf("count running runs: %v", err)
	}
	if running != 0 {
		t.Fatalf("running runs after completion = %d", running)
	}
}

type platformProcess struct {
	cmd  *exec.Cmd
	log  *os.File
	path string
	once sync.Once
}

func (p *platformProcess) kill(t *testing.T) {
	t.Helper()
	if p.cmd.Process == nil {
		return
	}
	if err := p.cmd.Process.Kill(); err != nil {
		t.Fatalf("kill platform: %v", err)
	}
	_ = p.cmd.Wait()
}

func (p *platformProcess) stop() {
	p.once.Do(func() {
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
		_ = p.cmd.Wait()
		_ = p.log.Close()
	})
}

func startPlatform(t *testing.T, bin, dbURL, addr, secret, modelBaseURL string) *platformProcess {
	t.Helper()
	logFile, err := os.CreateTemp(t.TempDir(), "platform-*.log")
	if err != nil {
		t.Fatalf("create log file: %v", err)
	}
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"DATABASE_URL="+dbURL,
		"HTTP_ADDR="+addr,
		"POLICY_SECRET_RECOVERY="+secret,
		"MODEL_BASE_URL="+modelBaseURL,
		"POLL_INTERVAL=50ms",
		"STEP_TIMEOUT=5s",
		"MAX_STEP_ATTEMPTS=3",
	)
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

func buildPlatform(t *testing.T) string {
	t.Helper()
	_, file, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test file")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	bin := filepath.Join(t.TempDir(), "platform")
	if goruntime.GOOS == "windows" {
		bin += ".exe"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", bin, "./cmd/platform")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build platform: %v\n%s", err, out)
	}
	return bin
}

func recoveryFreeAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	return addr
}

func waitRecoveryReady(t *testing.T, ctx context.Context, base string) {
	t.Helper()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/readyz", nil)
		if err == nil {
			resp, callErr := http.DefaultClient.Do(req)
			if callErr == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return
				}
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("platform did not become ready: %v", ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func waitRecoveryRun(t *testing.T, ctx context.Context, client *http.Client, base, apiKey, id string, want domain.RunStatus) domain.Run {
	t.Helper()
	var last domain.Run
	for {
		status, raw := recoveryDo(t, ctx, client, http.MethodGet, base+"/v1/runs/"+id, apiKey, nil)
		if status == http.StatusOK {
			if err := json.Unmarshal(raw, &last); err != nil {
				t.Fatalf("decode run: %v", err)
			}
			if last.Status == want {
				return last
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("run %s status = %q, want %q: %v", id, last.Status, want, ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

type recoveryStep struct {
	StepIndex int             `json:"step_index"`
	StepType  string          `json:"step_type"`
	Result    json.RawMessage `json:"result"`
}

func getRecoverySteps(t *testing.T, ctx context.Context, client *http.Client, base, apiKey, id string) []recoveryStep {
	t.Helper()
	status, raw := recoveryDo(t, ctx, client, http.MethodGet, base+"/v1/runs/"+id+"/steps", apiKey, nil)
	if status != http.StatusOK {
		t.Fatalf("get steps status = %d body=%s", status, raw)
	}
	var steps []recoveryStep
	if err := json.Unmarshal(raw, &steps); err != nil {
		t.Fatalf("decode steps: %v", err)
	}
	return steps
}

func recoveryDo(t *testing.T, ctx context.Context, client *http.Client, method, target, apiKey string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := client.Do(req)
	if err != nil {
		return 0, []byte(err.Error())
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, []byte(err.Error())
	}
	return resp.StatusCode, raw
}

func recoveryHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func recoverySign(secret, method, path string, body []byte, timestamp string) string {
	sum := sha256.Sum256(body)
	payload := method + "\n" + path + "\n" + hex.EncodeToString(sum[:]) + "\n" + timestamp
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}
