package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/romeokeita231/Lite-agent-platform/internal/domain"
	"github.com/romeokeita231/Lite-agent-platform/internal/ids"
	"github.com/romeokeita231/Lite-agent-platform/internal/repository"
)

type Readiness interface {
	Ready() bool
}

type API struct {
	store domain.Store
	tools domain.ToolGateway
}

func NewMux(ready Readiness, store domain.Store, tools domain.ToolGateway) *http.ServeMux {
	mux := http.NewServeMux()
	api := &API{store: store, tools: tools}

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

	mux.Handle("POST /v1/agents", api.authenticated(http.HandlerFunc(api.createAgent)))
	mux.Handle("GET /v1/agents", api.authenticated(http.HandlerFunc(api.listAgents)))
	mux.Handle("POST /v1/runs", api.authenticated(http.HandlerFunc(api.createRun)))
	mux.Handle("GET /v1/runs/{id}", api.authenticated(http.HandlerFunc(api.getRun)))
	mux.Handle("GET /v1/runs/{id}/steps", api.authenticated(http.HandlerFunc(api.getRunSteps)))
	mux.Handle("POST /v1/runs/{id}/human-input", api.authenticated(http.HandlerFunc(api.submitHumanInput)))
	mux.Handle("POST /v1/tools", api.authenticated(http.HandlerFunc(api.createTool)))
	mux.Handle("GET /v1/tools", api.authenticated(http.HandlerFunc(api.listTools)))
	mux.Handle("POST /v1/tools/{tool_name}/invoke", api.authenticated(http.HandlerFunc(api.invokeTool)))
	return mux
}

func (a *API) authenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
			return
		}
		sum := sha256.Sum256([]byte(key))
		tenant, err := a.store.LookupTenantByAPIKey(r.Context(), hex.EncodeToString(sum[:]))
		if errors.Is(err, domain.ErrNotFound) {
			writeError(w, http.StatusUnauthorized, "unauthorized", "unknown API key")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "tenant lookup failed")
			return
		}
		next.ServeHTTP(w, r.WithContext(repository.WithTenant(r.Context(), tenant.ID)))
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
	tenantID, err := repository.TenantOf(r.Context())
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
	tenantID, err := repository.TenantOf(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "missing tenant in request context")
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
	tenantID, err := repository.TenantOf(r.Context())
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
	result, err := json.Marshal(struct {
		Input json.RawMessage `json:"input"`
	}{Input: input})
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
		Attributes: json.RawMessage(`{}`),
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
