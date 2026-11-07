package runtime_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/romeokeita231/Lite-agent-platform/internal/domain"
	"github.com/romeokeita231/Lite-agent-platform/internal/modelgw"
	"github.com/romeokeita231/Lite-agent-platform/internal/repository"
	agentruntime "github.com/romeokeita231/Lite-agent-platform/internal/runtime"
	"github.com/romeokeita231/Lite-agent-platform/internal/testutil"
)

type metricSample struct {
	name   string
	labels map[string]string
	value  float64
}

func TestMetricsEndpointExposesContract(t *testing.T) {
	const (
		foldSecret  = "metrics-fold-secret"
		fallbackEnv = "POLICY_SECRET_FOLD"
		toolEnv     = "POLICY_SECRET_TOOL"
		toolSecret  = "metrics-tool-secret"
	)
	t.Setenv(fallbackEnv, foldSecret)
	t.Setenv(toolEnv, toolSecret)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		var call struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &call); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		if call.Model == "bad" {
			http.Error(w, "upstream failure", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	}))
	t.Cleanup(upstream.Close)

	base := testutil.New(t)
	store := repository.New(base.Pool)
	gateway := modelgw.NewWithStore(store, upstream.URL+"/v1", "", 2*time.Second)
	e := newEnvWithRuntimeOptions(t, base, store, gateway, agentruntime.Options{
		StepTimeout:     2 * time.Second,
		MaxStepAttempts: 2,
		PollInterval:    10 * time.Millisecond,
		ContextRecent:   2,
		ContextMaxKeep:  2,
	})
	ctxA := repository.WithTenant(context.Background(), "tenant-a")
	if _, err := e.store.UpsertModelRoute(ctxA, domain.ModelRoute{
		TenantID: "tenant-a",
		Label:    "default",
		Chain:    []string{"bad", "good"},
	}); err != nil {
		t.Fatalf("upsert model route: %v", err)
	}

	foldPolicy := newPolicyServerFunc(t, foldSecret, func(request policyRequest) string {
		if request.StepIndex < 3 {
			return `{"action":"llm_call","route":"route:default","messages":[{"role":"user","content":"hi"}]}`
		}
		return `{"action":"done","output":{"answer":"done"}}`
	})
	foldAgent := e.createAgent(t, "tenant-a", "fold", foldPolicy.srv.URL, fallbackEnv)
	foldRun := e.submitRun(t, "tenant-a", foldAgent.ID)
	e.waitRun(t, "tenant-a", foldRun.ID, domain.RunCompleted, 10*time.Second)

	toolPolicy := newPolicyServer(t, toolSecret, `{"action":"tool_call","tool":"lookup","args":{"order_id":"A1"}}`)
	failingTool := newFakeToolEndpoint(t, 10)
	e.registerTool(t, "tenant-a", "lookup", "1", failingTool.srv.URL, true)
	toolAgent := e.createAgent(t, "tenant-a", "tool", toolPolicy.srv.URL, toolEnv)
	toolRun := e.submitRun(t, "tenant-a", toolAgent.ID)
	e.waitRun(t, "tenant-a", toolRun.ID, domain.RunFailed, 10*time.Second)

	if status := probeStatus(t, e.srv.URL+"/healthz"); status != http.StatusOK {
		t.Fatalf("healthz = %d, want 200", status)
	}
	if status := probeStatus(t, e.srv.URL+"/readyz"); status != http.StatusOK {
		t.Fatalf("readyz = %d, want 200", status)
	}

	want := runStatusCounts(t, e, foldRun.ID, toolRun.ID)
	samples := waitMetricsSamples(t, e.srv.URL+"/metrics", want)

	families := []string{
		"platform_runs",
		"platform_step_retries_total",
		"platform_model_fallbacks_total",
		"platform_model_call_duration_seconds",
		"platform_tool_calls_total",
		"platform_context_tokens",
		"platform_context_dropped_steps_total",
		"platform_tenant_inflight",
	}
	for _, family := range families {
		if !hasFamily(samples, family) {
			t.Fatalf("metric family %q missing", family)
		}
	}

	for _, status := range []domain.RunStatus{
		domain.RunQueued, domain.RunRunning, domain.RunWaitingHuman,
		domain.RunCompleted, domain.RunFailed, domain.RunCancelled,
	} {
		value, ok := lookup(samples, "platform_runs", map[string]string{"status": string(status)})
		if !ok {
			t.Fatalf("platform_runs{status=%q} is missing", status)
		}
		if value != float64(want[status]) {
			t.Fatalf("platform_runs{status=%q} = %v, want %d", status, value, want[status])
		}
	}

	counter := func(name string) float64 {
		value, ok := lookup(samples, name, nil)
		if !ok {
			t.Fatalf("%s is missing", name)
		}
		return value
	}
	if counter("platform_step_retries_total") <= 0 {
		t.Fatalf("platform_step_retries_total did not increase")
	}
	if counter("platform_model_fallbacks_total") <= 0 {
		t.Fatalf("platform_model_fallbacks_total did not increase")
	}
	if counter("platform_context_dropped_steps_total") <= 0 {
		t.Fatalf("platform_context_dropped_steps_total did not increase")
	}

	if value, ok := lookup(samples, "platform_tool_calls_total", map[string]string{"status": "error"}); !ok || value <= 0 {
		t.Fatalf("platform_tool_calls_total{status=\"error\"} = %v, present=%v", value, ok)
	}
	if _, ok := lookup(samples, "platform_tool_calls_total", map[string]string{"status": "ok"}); !ok {
		t.Fatalf("platform_tool_calls_total{status=\"ok\"} is missing")
	}

	for _, histogram := range []string{
		"platform_model_call_duration_seconds",
		"platform_context_tokens",
		"platform_tenant_inflight",
	} {
		count, ok := lookup(samples, histogram+"_count", nil)
		if !ok || count <= 0 {
			t.Fatalf("%s_count = %v, present=%v", histogram, count, ok)
		}
		sum, ok := lookup(samples, histogram+"_sum", nil)
		if !ok || sum <= 0 {
			t.Fatalf("%s_sum = %v, present=%v", histogram, sum, ok)
		}
	}

	for _, sample := range samples {
		if _, ok := sample.labels["tenant_id"]; ok {
			t.Fatalf("metric %q has a tenant_id label: %+v", sample.name, sample.labels)
		}
		if !strings.HasPrefix(sample.name, "platform_tenant_inflight") {
			continue
		}
		if strings.HasSuffix(sample.name, "_bucket") {
			if len(sample.labels) != 1 {
				t.Fatalf("%s has labels %+v, want only le", sample.name, sample.labels)
			}
		} else if len(sample.labels) != 0 {
			t.Fatalf("%s has labels %+v, want none", sample.name, sample.labels)
		}
	}
}

func runStatusCounts(t *testing.T, e *env, runIDs ...string) map[domain.RunStatus]int64 {
	t.Helper()
	rows, err := e.env.Pool.Query(context.Background(), `select status, count(*) from runs group by status`)
	if err != nil {
		t.Fatalf("query run counts: %v", err)
	}
	defer rows.Close()
	counts := make(map[domain.RunStatus]int64)
	for rows.Next() {
		var status string
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			t.Fatalf("scan run counts: %v", err)
		}
		counts[domain.RunStatus(status)] = count
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate run counts: %v", err)
	}
	if counts[domain.RunCompleted] != 1 || counts[domain.RunFailed] != 1 {
		t.Fatalf("unexpected run counts after %v: %+v", runIDs, counts)
	}
	return counts
}

func waitMetricsSamples(t *testing.T, url string, want map[domain.RunStatus]int64) []metricSample {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var samples []metricSample
	for {
		samples = parseMetricSamples(t, fetchMetrics(t, url))
		matched := true
		for _, status := range []domain.RunStatus{
			domain.RunQueued, domain.RunRunning, domain.RunWaitingHuman,
			domain.RunCompleted, domain.RunFailed, domain.RunCancelled,
		} {
			value, ok := lookup(samples, "platform_runs", map[string]string{"status": string(status)})
			if !ok || value != float64(want[status]) {
				matched = false
				break
			}
		}
		if matched {
			return samples
		}
		if time.Now().After(deadline) {
			t.Fatalf("platform_runs did not converge to %+v", want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func fetchMetrics(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", url, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read metrics: %v", err)
	}
	return string(body)
}

func probeStatus(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func parseMetricSamples(t *testing.T, body string) []metricSample {
	t.Helper()
	var samples []metricSample
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		sample := metricSample{labels: map[string]string{}}
		open := strings.IndexByte(line, '{')
		if open < 0 {
			fields := strings.Fields(line)
			if len(fields) != 2 {
				t.Fatalf("unparseable metric line %q", line)
			}
			sample.name = fields[0]
			sample.value = parseMetricValue(t, fields[1])
		} else {
			close := strings.LastIndexByte(line, '}')
			if close < open {
				t.Fatalf("unparseable metric line %q", line)
			}
			sample.name = line[:open]
			for _, pair := range strings.Split(line[open+1:close], ",") {
				if pair == "" {
					continue
				}
				key, value, ok := strings.Cut(pair, "=")
				if !ok {
					t.Fatalf("unparseable label %q", pair)
				}
				sample.labels[key] = strings.Trim(value, `"`)
			}
			sample.value = parseMetricValue(t, strings.TrimSpace(line[close+1:]))
		}
		samples = append(samples, sample)
	}
	return samples
}

func parseMetricValue(t *testing.T, text string) float64 {
	t.Helper()
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		t.Fatalf("parse metric value %q: %v", text, err)
	}
	return value
}

func hasFamily(samples []metricSample, family string) bool {
	for _, sample := range samples {
		if sample.name == family || strings.HasPrefix(sample.name, family+"_") {
			return true
		}
	}
	return false
}

func lookup(samples []metricSample, name string, labels map[string]string) (float64, bool) {
	for _, sample := range samples {
		if sample.name != name {
			continue
		}
		if len(sample.labels) != len(labels) {
			continue
		}
		matched := true
		for key, value := range labels {
			if sample.labels[key] != value {
				matched = false
				break
			}
		}
		if matched {
			return sample.value, true
		}
	}
	return 0, false
}
