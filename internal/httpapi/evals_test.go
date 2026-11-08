package httpapi_test

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/romeokeita231/Lite-agent-platform/internal/domain"
)

type evalPolicyCall struct {
	RunID     string
	StepIndex int
	At        time.Time
}

type evalPolicy struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls []evalPolicyCall
}

func newEvalPolicy(t *testing.T, secret string) *evalPolicy {
	t.Helper()
	p := &evalPolicy{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			RunID     string `json:"run_id"`
			StepIndex int    `json:"step_index"`
			Input     struct {
				Steps []json.RawMessage `json:"steps"`
			} `json:"input"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		p.mu.Lock()
		p.calls = append(p.calls, evalPolicyCall{RunID: request.RunID, StepIndex: request.StepIndex, At: time.Now()})
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if request.StepIndex < len(request.Input.Steps) {
			_, _ = w.Write(request.Input.Steps[request.StepIndex])
			return
		}
		_, _ = w.Write([]byte(`{"action":"done","output":{}}`))
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func newEvalEnv(t *testing.T) *apiEnv {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	t.Cleanup(upstream.Close)
	return newTraceEnv(t, upstream.URL+"/v1")
}

func registerEvalAgent(t *testing.T, api *apiEnv, policyURL string) domain.Agent {
	t.Helper()
	status, raw := api.do(t, http.MethodPost, "/v1/agents", "tenant-a", map[string]string{
		"name":       "eval-agent",
		"policy_url": policyURL,
		"secret_env": "POLICY_SECRET_EVAL",
	})
	if status != http.StatusCreated {
		t.Fatalf("create agent status = %d body=%s", status, raw)
	}
	var agent domain.Agent
	if err := json.Unmarshal(raw, &agent); err != nil {
		t.Fatalf("decode agent: %v", err)
	}
	return agent
}

type evalResultPayload struct {
	CaseIndex int             `json:"case_index"`
	Input     json.RawMessage `json:"input"`
	RunID     string          `json:"run_id"`
	Status    string          `json:"status"`
	Passed    bool            `json:"passed"`
	Failures  []string        `json:"failures"`
}

type evalReportPayload struct {
	ID            string              `json:"id"`
	Status        string              `json:"status"`
	CasesSnapshot json.RawMessage     `json:"cases_snapshot"`
	Results       []evalResultPayload `json:"results"`
	CreatedAt     time.Time           `json:"created_at"`
	EndedAt       *time.Time          `json:"ended_at"`
}

func postEval(t *testing.T, api *apiEnv, tenant string, cases any) evalReportPayload {
	t.Helper()
	status, raw := api.do(t, http.MethodPost, "/v1/evals", tenant, map[string]any{"cases": cases})
	if status != http.StatusCreated {
		t.Fatalf("create eval status = %d body=%s", status, raw)
	}
	var report evalReportPayload
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatalf("decode eval report %s: %v", raw, err)
	}
	return report
}

func caseInput(t *testing.T, steps []any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(steps)
	if err != nil {
		t.Fatalf("marshal steps: %v", err)
	}
	return map[string]any{
		"steps": json.RawMessage(raw),
	}
}

func TestEvalReportRunsCasesSeriallyAndAssertsSubsets(t *testing.T) {
	const secret = "eval-secret"
	t.Setenv("POLICY_SECRET_EVAL", secret)
	policy := newEvalPolicy(t, secret)
	api := newEvalEnv(t)
	registerEvalAgent(t, api, policy.srv.URL)
	toolServer := newHTTPToolServer(t)
	registerTool(t, api, "tenant-a", "lookup", "1", toolServer.srv.URL, true)
	registerTool(t, api, "tenant-a", "refund", "1", toolServer.srv.URL, true)

	cases := []any{
		map[string]any{
			"agent": "eval-agent",
			"input": caseInput(t, []any{
				map[string]any{"action": "tool_call", "tool": "lookup", "args": map[string]any{"order_id": "A1", "note": "extra"}},
				map[string]any{"action": "tool_call", "tool": "refund", "args": map[string]any{"order_id": "A1"}},
				map[string]any{"action": "done", "output": map[string]any{"answer": "A1", "meta": map[string]any{"extra": true}}},
			}),
			"expect": map[string]any{
				"status": "completed",
				"tool_calls": []any{
					map[string]any{"tool": "lookup", "args": map[string]any{"order_id": "A1"}},
					map[string]any{"tool": "refund", "args": map[string]any{"order_id": "A1"}},
				},
				"output": map[string]any{"answer": "A1"},
			},
		},
		map[string]any{
			"agent": "eval-agent",
			"input": caseInput(t, []any{
				map[string]any{"action": "done", "output": map[string]any{"answer": "B1"}},
			}),
			"expect": map[string]any{
				"status": "failed",
				"output": map[string]any{"answer": "B1"},
			},
		},
		map[string]any{
			"agent": "eval-agent",
			"input": caseInput(t, []any{
				map[string]any{"action": "tool_call", "tool": "lookup", "args": map[string]any{"order_id": "C1"}},
				map[string]any{"action": "tool_call", "tool": "refund", "args": map[string]any{"order_id": "C1"}},
				map[string]any{"action": "done", "output": map[string]any{"answer": "C1"}},
			}),
			"expect": map[string]any{
				"status": "completed",
				"tool_calls": []any{
					map[string]any{"tool": "refund", "args": map[string]any{"order_id": "C1"}},
					map[string]any{"tool": "lookup", "args": map[string]any{"order_id": "C1"}},
					map[string]any{"tool": "settle", "args": map[string]any{"order_id": "C1"}},
				},
				"output": map[string]any{"answer": "C1"},
			},
		},
	}

	report := postEval(t, api, "tenant-a", cases)
	if report.ID == "" || report.Status != "completed" || report.EndedAt == nil {
		t.Fatalf("eval report = %+v", report)
	}
	if len(report.Results) != 3 {
		t.Fatalf("results = %+v", report.Results)
	}

	pass := report.Results[0]
	if !pass.Passed || len(pass.Failures) != 0 || pass.Status != "completed" || pass.CaseIndex != 0 {
		t.Fatalf("passing case = %+v", pass)
	}
	statusCase := report.Results[1]
	if statusCase.Passed || len(statusCase.Failures) == 0 || statusCase.CaseIndex != 1 {
		t.Fatalf("status case = %+v", statusCase)
	}
	if !strings.Contains(strings.Join(statusCase.Failures, "\n"), "status") {
		t.Fatalf("status case failures = %+v", statusCase.Failures)
	}
	toolCase := report.Results[2]
	if toolCase.Passed || len(toolCase.Failures) == 0 || toolCase.CaseIndex != 2 {
		t.Fatalf("tool case = %+v", toolCase)
	}
	toolFailures := strings.Join(toolCase.Failures, "\n")
	if !strings.Contains(toolFailures, "tool_calls") || !strings.Contains(toolFailures, "lookup") ||
		!strings.Contains(toolFailures, "settle") {
		t.Fatalf("tool case failures = %+v", toolCase.Failures)
	}

	sent, err := json.Marshal(cases)
	if err != nil {
		t.Fatalf("marshal cases: %v", err)
	}
	var sentValue, snapshotValue any
	if err := json.Unmarshal(sent, &sentValue); err != nil {
		t.Fatalf("decode sent cases: %v", err)
	}
	if err := json.Unmarshal(report.CasesSnapshot, &snapshotValue); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if !reflect.DeepEqual(sentValue, snapshotValue) {
		t.Fatalf("cases_snapshot = %s", report.CasesSnapshot)
	}

	runs := make([]domain.Run, 0, len(report.Results))
	for _, result := range report.Results {
		status, raw := api.do(t, http.MethodGet, "/v1/runs/"+result.RunID, "tenant-a", nil)
		if status != http.StatusOK {
			t.Fatalf("get run %s status = %d body=%s", result.RunID, status, raw)
		}
		var run domain.Run
		if err := json.Unmarshal(raw, &run); err != nil {
			t.Fatalf("decode run: %v", err)
		}
		if string(run.Status) != result.Status || run.EvalID == nil || *run.EvalID != report.ID {
			t.Fatalf("run %s = %+v, result = %+v", result.RunID, run, result)
		}
		var evalID *string
		if err := api.env.Pool.QueryRow(context.Background(),
			`select eval_id from runs where id = $1::uuid`, result.RunID).Scan(&evalID); err != nil {
			t.Fatalf("query eval_id: %v", err)
		}
		if evalID == nil || *evalID != report.ID {
			t.Fatalf("stored run %s eval_id = %v", result.RunID, evalID)
		}
		if run.EndedAt == nil {
			t.Fatalf("run %s has no ended_at", result.RunID)
		}
		runs = append(runs, run)
	}
	for i := 1; i < len(runs); i++ {
		if runs[i].CreatedAt.Before(*runs[i-1].EndedAt) {
			t.Fatalf("runs overlap: case %d created %s before case %d ended %s",
				i, runs[i].CreatedAt, i-1, runs[i-1].EndedAt)
		}
	}

	status, raw := api.do(t, http.MethodGet, "/v1/evals/"+report.ID, "tenant-a", nil)
	if status != http.StatusOK {
		t.Fatalf("get eval status = %d body=%s", status, raw)
	}
	var fetched evalReportPayload
	if err := json.Unmarshal(raw, &fetched); err != nil {
		t.Fatalf("decode fetched eval: %v", err)
	}
	if fetched.ID != report.ID || fetched.Status != "completed" || len(fetched.Results) != 3 {
		t.Fatalf("fetched eval = %+v", fetched)
	}
	var fetchedSnapshot, postedSnapshot any
	if err := json.Unmarshal(fetched.CasesSnapshot, &fetchedSnapshot); err != nil {
		t.Fatalf("decode fetched snapshot: %v", err)
	}
	if err := json.Unmarshal(report.CasesSnapshot, &postedSnapshot); err != nil {
		t.Fatalf("decode posted snapshot: %v", err)
	}
	if !reflect.DeepEqual(fetchedSnapshot, postedSnapshot) {
		t.Fatalf("fetched cases_snapshot = %s", fetched.CasesSnapshot)
	}
	var fetchedResults, postedResults any
	fetchedRaw, _ := json.Marshal(fetched.Results)
	postedRaw, _ := json.Marshal(report.Results)
	_ = json.Unmarshal(fetchedRaw, &fetchedResults)
	_ = json.Unmarshal(postedRaw, &postedResults)
	if !reflect.DeepEqual(fetchedResults, postedResults) {
		t.Fatalf("fetched results = %+v", fetched.Results)
	}
}

func TestEvalReportReadsAreTenantScoped(t *testing.T) {
	const secret = "eval-scope-secret"
	t.Setenv("POLICY_SECRET_EVAL", secret)
	policy := newEvalPolicy(t, secret)
	api := newEvalEnv(t)
	registerEvalAgent(t, api, policy.srv.URL)

	report := postEval(t, api, "tenant-a", []any{
		map[string]any{
			"agent": "eval-agent",
			"input": caseInput(t, []any{
				map[string]any{"action": "done", "output": map[string]any{"answer": "ok"}},
			}),
			"expect": map[string]any{
				"status": "completed",
				"output": map[string]any{"answer": "ok"},
			},
		},
	})
	if len(report.Results) != 1 || !report.Results[0].Passed {
		t.Fatalf("report = %+v", report)
	}

	if status, raw := api.do(t, http.MethodGet, "/v1/evals/"+report.ID, "tenant-b", nil); status != http.StatusNotFound {
		t.Fatalf("cross-tenant get eval status = %d body=%s", status, raw)
	} else if body := decodeError(t, raw); body.Error != "not_found" {
		t.Fatalf("cross-tenant get eval body = %+v", body)
	}

	if status, raw := api.do(t, http.MethodGet, "/v1/evals/00000000-0000-4000-8000-000000000000", "tenant-a", nil); status != http.StatusNotFound {
		t.Fatalf("missing eval status = %d body=%s", status, raw)
	}

	resp, err := http.Get(api.srv.URL + "/v1/evals/" + report.ID)
	if err != nil {
		t.Fatalf("unauthenticated get eval: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated get eval status = %d body=%s", resp.StatusCode, raw)
	}
	if body := decodeError(t, raw); body.Error != "unauthorized" {
		t.Fatalf("unauthenticated get eval body = %+v", body)
	}
}

func TestEvalRequestValidation(t *testing.T) {
	const secret = "eval-validation-secret"
	t.Setenv("POLICY_SECRET_EVAL", secret)
	policy := newEvalPolicy(t, secret)
	api := newEvalEnv(t)
	registerEvalAgent(t, api, policy.srv.URL)

	validCase := func() map[string]any {
		return map[string]any{
			"agent": "eval-agent",
			"input": caseInput(t, []any{
				map[string]any{"action": "done", "output": map[string]any{"answer": "ok"}},
			}),
			"expect": map[string]any{"status": "completed"},
		}
	}

	unknown := validCase()
	unknown["agent"] = "missing-agent"
	if status, raw := api.do(t, http.MethodPost, "/v1/evals", "tenant-a", map[string]any{"cases": []any{unknown}}); status != http.StatusBadRequest {
		t.Fatalf("unknown agent status = %d body=%s", status, raw)
	} else if body := decodeError(t, raw); body.Error != "bad_request" {
		t.Fatalf("unknown agent body = %+v", body)
	}

	if status, raw := api.do(t, http.MethodPost, "/v1/evals", "tenant-a", map[string]any{"cases": []any{}}); status != http.StatusBadRequest {
		t.Fatalf("empty cases status = %d body=%s", status, raw)
	} else if body := decodeError(t, raw); body.Error != "bad_request" {
		t.Fatalf("empty cases body = %+v", body)
	}

	invalidStatus := validCase()
	invalidStatus["expect"] = map[string]any{"status": "nope"}
	if status, raw := api.do(t, http.MethodPost, "/v1/evals", "tenant-a", map[string]any{"cases": []any{invalidStatus}}); status != http.StatusBadRequest {
		t.Fatalf("invalid expect status = %d body=%s", status, raw)
	} else if body := decodeError(t, raw); body.Error != "bad_request" {
		t.Fatalf("invalid expect status body = %+v", body)
	}

	if status, raw := api.do(t, http.MethodPost, "/v1/evals", "", map[string]any{"cases": []any{validCase()}}); status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated create eval status = %d body=%s", status, raw)
	}
}
