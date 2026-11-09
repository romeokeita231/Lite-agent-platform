package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/romeokeita231/Lite-agent-platform/internal/domain"
	"github.com/romeokeita231/Lite-agent-platform/internal/ids"
)

type Readiness interface {
	Ready() bool
}

type Draining interface {
	Draining() bool
}

type API struct {
	store    domain.Store
	tools    domain.ToolGateway
	gateway  domain.ModelGateway
	draining Draining
}

func NewMux(ready Readiness, store domain.Store, tools domain.ToolGateway, draining Draining, metrics http.Handler, gateways ...domain.ModelGateway) *http.ServeMux {
	mux := http.NewServeMux()
	api := &API{store: store, tools: tools, draining: draining}
	if len(gateways) > 0 {
		api.gateway = gateways[0]
	}

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Ready() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready\n"))
	})

	if metrics != nil {
		mux.Handle("GET /metrics", metrics)
	}

	mux.Handle("POST /v1/agents", api.authenticated(http.HandlerFunc(api.createAgent)))
	mux.Handle("GET /v1/agents", api.authenticated(http.HandlerFunc(api.listAgents)))
	mux.Handle("POST /v1/runs", api.authenticated(http.HandlerFunc(api.createRun)))
	mux.Handle("GET /v1/runs", api.authenticated(http.HandlerFunc(api.listRuns)))
	mux.Handle("GET /v1/runs/{id}", api.authenticated(http.HandlerFunc(api.getRun)))
	mux.Handle("GET /v1/runs/{id}/steps", api.authenticated(http.HandlerFunc(api.getRunSteps)))
	mux.Handle("GET /v1/runs/{id}/trace", api.authenticated(http.HandlerFunc(api.getRunTrace)))
	mux.Handle("POST /v1/runs/{id}/human-input", api.authenticated(http.HandlerFunc(api.submitHumanInput)))
	mux.Handle("POST /v1/runs/{id}/cancel", api.authenticated(http.HandlerFunc(api.cancelRun)))
	mux.Handle("POST /v1/tools", api.authenticated(http.HandlerFunc(api.createTool)))
	mux.Handle("GET /v1/tools", api.authenticated(http.HandlerFunc(api.listTools)))
	mux.Handle("POST /v1/tools/{tool_name}/invoke", api.authenticated(http.HandlerFunc(api.invokeTool)))
	mux.Handle("POST /v1/chat/completions", api.authenticated(http.HandlerFunc(api.chatCompletions)))
	mux.Handle("GET /v1/models", api.authenticated(http.HandlerFunc(api.listModels)))
	mux.Handle("GET /v1/tenants/{id}/usage", api.authenticated(http.HandlerFunc(api.tenantUsage)))
	mux.Handle("POST /v1/evals", api.authenticated(http.HandlerFunc(api.createEval)))
	mux.Handle("GET /v1/evals/{id}", api.authenticated(http.HandlerFunc(api.getEval)))
	return mux
}

func (a *API) authenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
			return
		}
		tenant, err := a.store.LookupTenantByAPIKey(r.Context(), domain.HashAPIKey(key))
		if errors.Is(err, domain.ErrNotFound) {
			writeError(w, http.StatusUnauthorized, "unauthorized", "unknown API key")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "tenant lookup failed")
			return
		}
		next.ServeHTTP(w, r.WithContext(domain.WithTenant(r.Context(), tenant.ID)))
	})
}

type createAgentRequest struct {
	Name      string `json:"name"`
	PolicyURL string `json:"policy_url"`
	SecretEnv string `json:"secret_env"`
}

func (a *API) createAgent(w http.ResponseWriter, r *http.Request) {
	var req createAgentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.Name == "" || req.PolicyURL == "" || req.SecretEnv == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "name, policy_url and secret_env are required")
		return
	}
	tenantID, err := domain.TenantOf(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "missing tenant in request context")
		return
	}
	agent, err := a.store.CreateAgent(r.Context(), domain.Agent{
		ID:        ids.New(),
		TenantID:  tenantID,
		Name:      req.Name,
		PolicyURL: req.PolicyURL,
		SecretEnv: req.SecretEnv,
	})
	if errors.Is(err, domain.ErrConflict) {
		writeError(w, http.StatusConflict, "conflict", "agent name already exists")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "create agent failed")
		return
	}
	writeJSON(w, http.StatusCreated, agent)
}

func (a *API) listAgents(w http.ResponseWriter, r *http.Request) {
	agents, err := a.store.ListAgents(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "list agents failed")
		return
	}
	writeJSON(w, http.StatusOK, agents)
}

type createRunRequest struct {
	AgentID string          `json:"agent_id"`
	Input   json.RawMessage `json:"input"`
}

func (a *API) createRun(w http.ResponseWriter, r *http.Request) {
	if a.draining != nil && a.draining.Draining() {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "platform is shutting down")
		return
	}
	var req createRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.AgentID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "agent_id is required")
		return
	}
	if len(req.Input) == 0 {
		req.Input = json.RawMessage("{}")
	}
	tenantID, err := domain.TenantOf(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "missing tenant in request context")
		return
	}
	if _, err := a.store.GetAgent(r.Context(), req.AgentID); errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "agent not found")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "get agent failed")
		return
	}
	run, err := a.store.CreateRun(r.Context(), domain.Run{
		ID:       ids.New(),
		TenantID: tenantID,
		AgentID:  req.AgentID,
		Status:   domain.RunQueued,
		Input:    req.Input,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "create run failed")
		return
	}
	writeJSON(w, http.StatusCreated, run)
}

const (
	defaultRunListLimit = 20
	maxRunListLimit     = 100
)

var runListStatuses = []domain.RunStatus{
	domain.RunQueued,
	domain.RunRunning,
	domain.RunWaitingHuman,
	domain.RunCompleted,
	domain.RunFailed,
	domain.RunCancelled,
}

type runListResponse struct {
	Runs   []domain.Run               `json:"runs"`
	Counts map[domain.RunStatus]int64 `json:"counts"`
}

func validRunListStatus(status string) bool {
	for _, candidate := range runListStatuses {
		if string(candidate) == status {
			return true
		}
	}
	return false
}

func (a *API) listRuns(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	status := query.Get("status")
	if status != "" && !validRunListStatus(status) {
		writeError(w, http.StatusBadRequest, "bad_request",
			"status must be one of queued, running, waiting_human, completed, failed, cancelled")
		return
	}
	limit := defaultRunListLimit
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > maxRunListLimit {
			writeError(w, http.StatusBadRequest, "bad_request", "limit must be between 1 and 100")
			return
		}
		limit = parsed
	}
	runs, err := a.store.ListRuns(r.Context(), domain.RunStatus(status), query.Get("agent_id"), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "list runs failed")
		return
	}
	tenantCounts, err := a.store.CountTenantRunsByStatus(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "count tenant runs failed")
		return
	}
	response := runListResponse{
		Runs:   runs,
		Counts: make(map[domain.RunStatus]int64, len(runListStatuses)),
	}
	for _, candidate := range runListStatuses {
		response.Counts[candidate] = tenantCounts[candidate]
	}
	writeJSON(w, http.StatusOK, response)
}

type humanInputRequest struct {
	StepIndex int             `json:"step_index"`
	Input     json.RawMessage `json:"input"`
}

func (a *API) submitHumanInput(w http.ResponseWriter, r *http.Request) {
	var req humanInputRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	tenantID, err := domain.TenantOf(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "missing tenant in request context")
		return
	}
	run, err := a.store.GetRun(r.Context(), r.PathValue("id"))
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "run not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "get run failed")
		return
	}
	input := req.Input
	if len(input) == 0 {
		input = json.RawMessage("null")
	}
	var pending struct {
		Keep                []int `json:"keep"`
		ContextTokens       int   `json:"context_tokens"`
		ContextDroppedSteps int   `json:"context_dropped_steps"`
	}
	if len(run.PendingStep) > 0 {
		_ = json.Unmarshal(run.PendingStep, &pending)
	}
	result, err := json.Marshal(struct {
		Input json.RawMessage `json:"input"`
		Keep  []int           `json:"keep,omitempty"`
	}{Input: input, Keep: pending.Keep})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "encode human input failed")
		return
	}
	attributes, err := json.Marshal(map[string]any{
		"context_tokens":        pending.ContextTokens,
		"context_dropped_steps": pending.ContextDroppedSteps,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "encode human input failed")
		return
	}
	now := time.Now()
	runID := run.ID
	checkpoint := domain.Checkpoint{
		RunID:     run.ID,
		TenantID:  tenantID,
		StepIndex: req.StepIndex,
		StepType:  domain.StepHumanReview,
		Result:    result,
		CreatedAt: now,
	}
	span := domain.Span{
		ID:         ids.New(),
		TenantID:   tenantID,
		RunID:      &runID,
		StepIndex:  req.StepIndex,
		Attempt:    run.Attempt,
		Name:       "step.human_review",
		StartedAt:  now,
		EndedAt:    now,
		Attributes: attributes,
	}
	updated, err := a.store.SubmitHumanInput(r.Context(), checkpoint, span)
	if errors.Is(err, domain.ErrConflict) {
		writeError(w, http.StatusConflict, "conflict", "run is not waiting for this step")
		return
	}
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "run not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "submit human input failed")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (a *API) cancelRun(w http.ResponseWriter, r *http.Request) {
	run, err := a.store.RequestCancel(r.Context(), r.PathValue("id"))
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "run not found")
		return
	}
	if errors.Is(err, domain.ErrConflict) {
		writeError(w, http.StatusConflict, "conflict", "run is already terminal")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cancel run failed")
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (a *API) getRun(w http.ResponseWriter, r *http.Request) {
	run, err := a.store.GetRun(r.Context(), r.PathValue("id"))
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "run not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "get run failed")
		return
	}
	writeJSON(w, http.StatusOK, run)
}

type stepResponse struct {
	StepIndex int             `json:"step_index"`
	StepType  domain.StepType `json:"step_type"`
	Result    json.RawMessage `json:"result"`
	CreatedAt time.Time       `json:"created_at"`
}

func (a *API) getRunSteps(w http.ResponseWriter, r *http.Request) {
	steps, err := a.store.GetSteps(r.Context(), r.PathValue("id"))
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "run not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "get steps failed")
		return
	}
	out := make([]stepResponse, 0, len(steps))
	for _, step := range steps {
		out = append(out, stepResponse{
			StepIndex: step.StepIndex,
			StepType:  step.StepType,
			Result:    step.Result,
			CreatedAt: step.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func bearerToken(header string) (string, bool) {
	const scheme = "Bearer "
	if !strings.HasPrefix(header, scheme) {
		return "", false
	}
	key := strings.TrimSpace(strings.TrimPrefix(header, scheme))
	if key == "" {
		return "", false
	}
	return key, true
}

type errorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorBody{Error: code, Message: message})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
