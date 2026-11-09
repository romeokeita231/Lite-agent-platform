package httpapi_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/romeokeita231/Lite-agent-platform/internal/domain"
	"github.com/romeokeita231/Lite-agent-platform/internal/httpapi"
	"github.com/romeokeita231/Lite-agent-platform/internal/modelgw"
	"github.com/romeokeita231/Lite-agent-platform/internal/repository"
	agentruntime "github.com/romeokeita231/Lite-agent-platform/internal/runtime"
	"github.com/romeokeita231/Lite-agent-platform/internal/testutil"
	"github.com/romeokeita231/Lite-agent-platform/internal/toolgw"
)

func traceSign(secret, method, path string, body []byte, timestamp string) string {
	sum := sha256.Sum256(body)
	payload := method + "\n" + path + "\n" + hex.EncodeToString(sum[:]) + "\n" + timestamp
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

func newTracePolicy(t *testing.T, secret string, respond func(step int) string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "bad body", http.StatusUnauthorized)
			return
		}
		if !hmac.Equal([]byte(traceSign(secret, r.Method, r.URL.Path, body, r.Header.Get("X-Timestamp"))),
			[]byte(r.Header.Get("X-Signature"))) {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		var request struct {
			StepIndex int `json:"step_index"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(respond(request.StepIndex)))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTraceUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		var request struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &request)
		if request.Model == "m1" || request.Model == "bad" || request.Model == "worse" {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%q}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`, request.Model+"-answer")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTraceEnv(t *testing.T, upstreamURL string) *apiEnv {
	t.Helper()
	base := testutil.New(t)
	store := repository.New(base.Pool)
	e := &apiEnv{
		env: base,
		key: map[string]string{
			"tenant-a": "key-tenant-a-0123456789",
			"tenant-b": "key-tenant-b-0123456789",
		},
	}
	for tenantID, key := range e.key {
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
	gateway := modelgw.NewWithStore(store, upstreamURL, "", 2*time.Second)
	dispatcher := agentruntime.New(store, gateway, toolgw.New(store, 2*time.Second), agentruntime.Options{
		StepTimeout:       2 * time.Second,
		MaxStepAttempts:   3,
		PollInterval:      10 * time.Millisecond,
		TenantConcurrency: 4,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		dispatcher.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	e.srv = httptest.NewServer(httpapi.NewMux(alwaysReady{}, store, toolgw.New(store, 2*time.Second), alwaysReady{}, gateway))
	t.Cleanup(e.srv.Close)
	return e
}

type traceSpanPayload struct {
	ID         string         `json:"id"`
	Attempt    int            `json:"attempt"`
	Name       string         `json:"name"`
	StartedAt  time.Time      `json:"started_at"`
	EndedAt    time.Time      `json:"ended_at"`
	Attributes map[string]any `json:"attributes"`
	Error      string         `json:"error"`
}

type traceStepPayload struct {
	StepIndex int                `json:"step_index"`
	Spans     []traceSpanPayload `json:"spans"`
}

type tracePayload struct {
	RunID string             `json:"run_id"`
	Steps []traceStepPayload `json:"steps"`
}

func waitTraceRun(t *testing.T, e *apiEnv, runID string, want domain.RunStatus, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status, raw := e.do(t, http.MethodGet, "/v1/runs/"+runID, "tenant-a", nil)
		if status != http.StatusOK {
			t.Fatalf("get run status = %d body=%s", status, raw)
		}
		var run domain.Run
		if err := json.Unmarshal(raw, &run); err != nil {
			t.Fatalf("decode run: %v", err)
		}
		if run.Status == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("run %s did not reach %s", runID, want)
}

func TestRunTraceKeepsRetriedFallbackAtRealStepIndex(t *testing.T) {
	const secret = "trace-secret"
	t.Setenv("POLICY_SECRET_ALPHA", secret)
	policy := newTracePolicy(t, secret, func(step int) string {
		if step == 0 {
			return `{"action":"llm_call","route":"ok","messages":[{"role":"user","content":"hi"}]}`
		}
		return `{"action":"llm_call","route":"route:broken","messages":[{"role":"user","content":"hi"}]}`
	})
	upstream := newTraceUpstream(t)
	e := newTraceEnv(t, upstream.URL+"/v1")
	ctx := repository.WithTenant(context.Background(), "tenant-a")
	if _, err := repository.New(e.env.Pool).UpsertModelRoute(ctx, domain.ModelRoute{
		TenantID: "tenant-a", Label: "broken", Chain: []string{"bad", "worse"},
	}); err != nil {
		t.Fatalf("upsert route: %v", err)
	}

	status, raw := e.do(t, http.MethodPost, "/v1/agents", "tenant-a", map[string]string{
		"name":       "alpha",
		"policy_url": policy.URL,
		"secret_env": "POLICY_SECRET_ALPHA",
	})
	if status != http.StatusCreated {
		t.Fatalf("create agent status = %d body=%s", status, raw)
	}
	var agent domain.Agent
	if err := json.Unmarshal(raw, &agent); err != nil {
		t.Fatalf("decode agent: %v", err)
	}
	status, raw = e.do(t, http.MethodPost, "/v1/runs", "tenant-a", map[string]any{"agent_id": agent.ID})
	if status != http.StatusCreated {
		t.Fatalf("create run status = %d body=%s", status, raw)
	}
	var run domain.Run
	if err := json.Unmarshal(raw, &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	waitTraceRun(t, e, run.ID, domain.RunFailed, 10*time.Second)

	status, raw = e.do(t, http.MethodGet, "/v1/runs/"+run.ID+"/trace", "tenant-a", nil)
	if status != http.StatusOK {
		t.Fatalf("trace status = %d body=%s", status, raw)
	}
	var trace tracePayload
	if err := json.Unmarshal(raw, &trace); err != nil {
		t.Fatalf("decode trace %s: %v", raw, err)
	}
	if len(trace.Steps) != 2 || trace.Steps[0].StepIndex != 0 || trace.Steps[1].StepIndex != 1 {
		t.Fatalf("trace steps = %+v", trace.Steps)
	}
	if step0 := trace.Steps[0].Spans; len(step0) != 1 || step0[0].Name != "model.ok" {
		t.Fatalf("step 0 spans = %+v", step0)
	}
	step1 := trace.Steps[1].Spans
	if len(step1) != 6 {
		t.Fatalf("step 1 spans = %d, want 6", len(step1))
	}
	for _, span := range step1 {
		if span.Name != "model.bad" && span.Name != "model.worse" {
			t.Fatalf("step 1 span = %+v", span)
		}
	}
}

func TestRunTraceGroupsSpansByStepAndKeepsFallbackAttempts(t *testing.T) {
	const secret = "trace-secret"
	t.Setenv("POLICY_SECRET_ALPHA", secret)
	policy := newTracePolicy(t, secret, func(step int) string {
		if step == 0 {
			return `{"action":"llm_call","route":"route:cheap","messages":[{"role":"user","content":"hi"}]}`
		}
		return `{"action":"done","output":{"answer":"done"}}`
	})
	upstream := newTraceUpstream(t)
	e := newTraceEnv(t, upstream.URL+"/v1")
	ctx := repository.WithTenant(context.Background(), "tenant-a")
	if _, err := repository.New(e.env.Pool).UpsertModelRoute(ctx, domain.ModelRoute{
		TenantID: "tenant-a", Label: "cheap", Chain: []string{"m1", "m2"},
	}); err != nil {
		t.Fatalf("upsert route: %v", err)
	}

	status, raw := e.do(t, http.MethodPost, "/v1/agents", "tenant-a", map[string]string{
		"name":       "alpha",
		"policy_url": policy.URL,
		"secret_env": "POLICY_SECRET_ALPHA",
	})
	if status != http.StatusCreated {
		t.Fatalf("create agent status = %d body=%s", status, raw)
	}
	var agent domain.Agent
	if err := json.Unmarshal(raw, &agent); err != nil {
		t.Fatalf("decode agent: %v", err)
	}
	status, raw = e.do(t, http.MethodPost, "/v1/runs", "tenant-a", map[string]any{"agent_id": agent.ID})
	if status != http.StatusCreated {
		t.Fatalf("create run status = %d body=%s", status, raw)
	}
	var run domain.Run
	if err := json.Unmarshal(raw, &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	waitTraceRun(t, e, run.ID, domain.RunCompleted, 10*time.Second)

	status, raw = e.do(t, http.MethodGet, "/v1/runs/"+run.ID+"/trace", "tenant-a", nil)
	if status != http.StatusOK {
		t.Fatalf("trace status = %d body=%s", status, raw)
	}
	var trace tracePayload
	if err := json.Unmarshal(raw, &trace); err != nil {
		t.Fatalf("decode trace %s: %v", raw, err)
	}
	if trace.RunID != run.ID {
		t.Fatalf("trace run_id = %q, want %q", trace.RunID, run.ID)
	}
	if len(trace.Steps) != 2 {
		t.Fatalf("trace steps = %+v", trace.Steps)
	}
	if trace.Steps[0].StepIndex != 0 || trace.Steps[1].StepIndex != 1 {
		t.Fatalf("trace step order = %d, %d", trace.Steps[0].StepIndex, trace.Steps[1].StepIndex)
	}

	llm := trace.Steps[0].Spans
	if len(llm) != 2 {
		t.Fatalf("step 0 spans = %+v", llm)
	}
	if llm[0].Name != "model.m1" || llm[1].Name != "model.m2" {
		t.Fatalf("step 0 span names = %q, %q", llm[0].Name, llm[1].Name)
	}
	if llm[1].StartedAt.Before(llm[0].StartedAt) || llm[1].EndedAt.Before(llm[0].EndedAt) {
		t.Fatalf("step 0 spans are not time ordered: %+v", llm)
	}
	for i, span := range llm {
		if span.ID == "" || span.EndedAt.Before(span.StartedAt) {
			t.Fatalf("step 0 span %d = %+v", i, span)
		}
		for _, key := range []string{"requested_model", "effective_model", "status", "latency_ms", "attempt"} {
			if _, ok := span.Attributes[key]; !ok {
				t.Fatalf("step 0 span %d attributes missing %q: %+v", i, key, span.Attributes)
			}
		}
		if span.Attributes["requested_model"] != "route:cheap" {
			t.Fatalf("step 0 span %d requested_model = %v", i, span.Attributes["requested_model"])
		}
	}
	if llm[0].Attributes["effective_model"] != "m1" || llm[0].Attributes["status"] != domain.ModelStatusError ||
		llm[0].Error == "" {
		t.Fatalf("first fallback attempt = %+v", llm[0])
	}
	if llm[1].Attributes["effective_model"] != "m2" || llm[1].Attributes["status"] != domain.ModelStatusOK ||
		llm[1].Error != "" {
		t.Fatalf("second fallback attempt = %+v", llm[1])
	}
	firstAttempt, _ := llm[0].Attributes["attempt"].(float64)
	secondAttempt, _ := llm[1].Attributes["attempt"].(float64)
	if firstAttempt != 1 || secondAttempt != 2 {
		t.Fatalf("fallback attempts = %v, %v", llm[0].Attributes["attempt"], llm[1].Attributes["attempt"])
	}

	if done := trace.Steps[1].Spans; len(done) != 1 || done[0].Name != "step.done" {
		t.Fatalf("step 1 spans = %+v", done)
	}

	total := 0
	for _, step := range trace.Steps {
		total += len(step.Spans)
	}
	if total <= len(trace.Steps) {
		t.Fatalf("total spans = %d, steps = %d", total, len(trace.Steps))
	}

	status, raw = e.do(t, http.MethodGet, "/v1/runs/"+run.ID+"/trace", "tenant-b", nil)
	if status != http.StatusNotFound {
		t.Fatalf("cross-tenant trace status = %d body=%s", status, raw)
	}
	if body := decodeError(t, raw); body.Error != "not_found" {
		t.Fatalf("cross-tenant trace body = %+v", body)
	}

	status, raw = e.do(t, http.MethodGet, "/v1/runs/00000000-0000-4000-8000-000000000000/trace", "tenant-a", nil)
	if status != http.StatusNotFound {
		t.Fatalf("missing run trace status = %d body=%s", status, raw)
	}
	if body := decodeError(t, raw); body.Error != "not_found" {
		t.Fatalf("missing run trace body = %+v", body)
	}

	resp, err := http.Get(e.srv.URL + "/v1/runs/" + run.ID + "/trace")
	if err != nil {
		t.Fatalf("unauthenticated trace: %v", err)
	}
	defer resp.Body.Close()
	raw, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated trace status = %d body=%s", resp.StatusCode, raw)
	}
	if body := decodeError(t, raw); body.Error != "unauthorized" {
		t.Fatalf("unauthenticated trace body = %+v", body)
	}
}
