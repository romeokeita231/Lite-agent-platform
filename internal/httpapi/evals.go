package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"time"

	"github.com/romeokeita231/Lite-agent-platform/internal/domain"
	"github.com/romeokeita231/Lite-agent-platform/internal/ids"
)

const (
	evalTimeout      = 60 * time.Second
	evalPollInterval = 25 * time.Millisecond
)

type expectedToolCall struct {
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args"`
}

type evalExpect struct {
	Status    string             `json:"status"`
	ToolCalls []expectedToolCall `json:"tool_calls"`
	Output    json.RawMessage    `json:"output"`
}

type evalCase struct {
	Agent  string          `json:"agent"`
	Input  json.RawMessage `json:"input"`
	Expect evalExpect      `json:"expect"`
}

type evalCaseResult struct {
	CaseIndex int             `json:"case_index"`
	Input     json.RawMessage `json:"input"`
	RunID     string          `json:"run_id"`
	Status    string          `json:"status"`
	Passed    bool            `json:"passed"`
	Failures  []string        `json:"failures"`
}

type evalReportResponse struct {
	ID            string          `json:"id"`
	Status        string          `json:"status"`
	CasesSnapshot json.RawMessage `json:"cases_snapshot"`
	Results       json.RawMessage `json:"results"`
	CreatedAt     time.Time       `json:"created_at"`
	EndedAt       *time.Time      `json:"ended_at"`
}

func evalReportBody(report domain.EvalReport) evalReportResponse {
	return evalReportResponse{
		ID:            report.ID,
		Status:        report.Status,
		CasesSnapshot: report.CasesSnapshot,
		Results:       report.Results,
		CreatedAt:     report.CreatedAt,
		EndedAt:       report.EndedAt,
	}
}

var evalStatuses = map[domain.RunStatus]bool{
	domain.RunQueued:       true,
	domain.RunRunning:      true,
	domain.RunWaitingHuman: true,
	domain.RunCompleted:    true,
	domain.RunFailed:       true,
	domain.RunCancelled:    true,
}

func (a *API) createEval(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Cases json.RawMessage `json:"cases"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if len(req.Cases) == 0 || string(req.Cases) == "null" {
		writeError(w, http.StatusBadRequest, "bad_request", "cases is required")
		return
	}
	var cases []evalCase
	if err := json.Unmarshal(req.Cases, &cases); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "cases must be an array")
		return
	}
	if len(cases) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "cases must not be empty")
		return
	}
	tenantID, err := domain.TenantOf(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "missing tenant in request context")
		return
	}
	agents, err := a.store.ListAgents(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "list agents failed")
		return
	}
	agentIDs := make(map[string]string, len(agents))
	for _, agent := range agents {
		agentIDs[agent.Name] = agent.ID
	}
	for i, c := range cases {
		if _, ok := agentIDs[c.Agent]; !ok || c.Agent == "" {
			writeError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("cases[%d]: unknown agent %q", i, c.Agent))
			return
		}
		if !evalStatuses[domain.RunStatus(c.Expect.Status)] {
			writeError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("cases[%d]: invalid expect.status %q", i, c.Expect.Status))
			return
		}
		for j, call := range c.Expect.ToolCalls {
			if call.Tool == "" {
				writeError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("cases[%d].expect.tool_calls[%d]: tool is required", i, j))
				return
			}
			if len(call.Args) > 0 && string(call.Args) != "null" && !isJSONObject(call.Args) {
				writeError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("cases[%d].expect.tool_calls[%d]: args must be an object", i, j))
				return
			}
		}
		if len(c.Expect.Output) > 0 && string(c.Expect.Output) != "null" && !isJSONObject(c.Expect.Output) {
			writeError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("cases[%d]: expect.output must be an object", i))
			return
		}
	}

	report, err := a.store.CreateEvalReport(r.Context(), domain.EvalReport{
		ID:            ids.New(),
		TenantID:      tenantID,
		Status:        "running",
		CasesSnapshot: req.Cases,
		Results:       json.RawMessage("[]"),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "create eval report failed")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), evalTimeout)
	defer cancel()

	results := make([]evalCaseResult, 0, len(cases))
	for i, c := range cases {
		input := c.Input
		if len(input) == 0 || string(input) == "null" {
			input = json.RawMessage("{}")
		}
		evalID := report.ID
		runID := ids.New()
		if _, err := a.store.CreateRun(ctx, domain.Run{
			ID:       runID,
			TenantID: tenantID,
			AgentID:  agentIDs[c.Agent],
			Status:   domain.RunQueued,
			Input:    input,
			EvalID:   &evalID,
		}); err != nil {
			a.failEval(ctx, report.ID, results)
			writeError(w, http.StatusInternalServerError, "internal", "create run failed")
			return
		}
		run, err := a.waitEvalRun(ctx, runID)
		if err != nil {
			a.failEval(ctx, report.ID, results)
			writeError(w, http.StatusInternalServerError, "internal", "eval case did not reach a terminal state")
			return
		}
		invocations, err := a.store.ListToolInvocations(ctx, run.ID)
		if err != nil {
			a.failEval(ctx, report.ID, results)
			writeError(w, http.StatusInternalServerError, "internal", "list tool invocations failed")
			return
		}
		failures := evalFailures(c, run, invocations)
		results = append(results, evalCaseResult{
			CaseIndex: i,
			Input:     input,
			RunID:     run.ID,
			Status:    string(run.Status),
			Passed:    len(failures) == 0,
			Failures:  failures,
		})
	}

	resultsRaw, err := json.Marshal(results)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "encode eval results failed")
		return
	}
	endedAt := time.Now()
	if err := a.store.FinishEvalReport(r.Context(), report.ID, "completed", resultsRaw, endedAt); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "finish eval report failed")
		return
	}
	report.Status = "completed"
	report.Results = resultsRaw
	report.EndedAt = &endedAt
	writeJSON(w, http.StatusCreated, evalReportBody(report))
}

func (a *API) getEval(w http.ResponseWriter, r *http.Request) {
	report, err := a.store.GetEvalReport(r.Context(), r.PathValue("id"))
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "eval report not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "get eval report failed")
		return
	}
	writeJSON(w, http.StatusOK, evalReportBody(report))
}

func (a *API) failEval(ctx context.Context, reportID string, results []evalCaseResult) {
	resultsRaw, err := json.Marshal(results)
	if err != nil {
		resultsRaw = json.RawMessage("[]")
	}
	_ = a.store.FinishEvalReport(ctx, reportID, "failed", resultsRaw, time.Now())
}

func (a *API) waitEvalRun(ctx context.Context, runID string) (domain.Run, error) {
	ticker := time.NewTicker(evalPollInterval)
	defer ticker.Stop()
	for {
		run, err := a.store.GetRun(ctx, runID)
		if err != nil {
			if ctx.Err() != nil {
				return domain.Run{}, ctx.Err()
			}
			return domain.Run{}, err
		}
		if run.Status.Terminal() {
			return run, nil
		}
		select {
		case <-ctx.Done():
			return domain.Run{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func evalFailures(c evalCase, run domain.Run, invocations []domain.ToolInvocation) []string {
	failures := []string{}
	if string(run.Status) != c.Expect.Status {
		failures = append(failures, fmt.Sprintf("status: expected %q, got %q", c.Expect.Status, run.Status))
	}
	failures = append(failures, toolCallFailures(c.Expect.ToolCalls, invocations)...)
	if len(c.Expect.Output) > 0 && string(c.Expect.Output) != "null" {
		if !jsonSubset(c.Expect.Output, run.Output) {
			failures = append(failures, fmt.Sprintf("output: expected subset %s, got %s", c.Expect.Output, run.Output))
		}
	}
	return failures
}

func toolCallFailures(expected []expectedToolCall, actual []domain.ToolInvocation) []string {
	failures := []string{}
	names := make([]string, 0, len(actual))
	for _, invocation := range actual {
		names = append(names, invocation.ToolName)
	}
	position := 0
	for i, call := range expected {
		found := -1
		for j := position; j < len(actual); j++ {
			if actual[j].ToolName != call.Tool {
				continue
			}
			if len(call.Args) == 0 || string(call.Args) == "null" || jsonSubset(call.Args, actual[j].Args) {
				found = j
				break
			}
		}
		if found < 0 {
			failures = append(failures, fmt.Sprintf(
				"tool_calls[%d]: expected tool %q with args %s not found after position %d (actual: %v)",
				i, call.Tool, call.Args, position, names))
			continue
		}
		position = found + 1
	}
	return failures
}

func isJSONObject(raw json.RawMessage) bool {
	var object map[string]json.RawMessage
	return json.Unmarshal(raw, &object) == nil
}

func jsonSubset(expected, actual json.RawMessage) bool {
	var e, a any
	if err := decodeJSONValue(expected, &e); err != nil {
		return false
	}
	if err := decodeJSONValue(actual, &a); err != nil {
		return false
	}
	return valueSubset(e, a)
}

func decodeJSONValue(raw json.RawMessage, out *any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	return decoder.Decode(out)
}

func valueSubset(expected, actual any) bool {
	switch e := expected.(type) {
	case map[string]any:
		a, ok := actual.(map[string]any)
		if !ok {
			return false
		}
		for key, value := range e {
			actualValue, ok := a[key]
			if !ok || !valueSubset(value, actualValue) {
				return false
			}
		}
		return true
	case []any:
		a, ok := actual.([]any)
		if !ok || len(a) < len(e) {
			return false
		}
		for i := range e {
			if !valueSubset(e[i], a[i]) {
				return false
			}
		}
		return true
	case json.Number:
		a, ok := actual.(json.Number)
		if !ok {
			return false
		}
		ef, eerr := e.Float64()
		af, aerr := a.Float64()
		return eerr == nil && aerr == nil && ef == af
	}
	return reflect.DeepEqual(expected, actual)
}
