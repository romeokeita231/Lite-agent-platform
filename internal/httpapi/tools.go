package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/romeokeita231/Lite-agent-platform/internal/domain"
)

type createToolRequest struct {
	ToolName    string          `json:"tool_name"`
	Version     string          `json:"version"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
	Endpoint    string          `json:"endpoint"`
	IsDefault   bool            `json:"is_default"`
}

func (a *API) createTool(w http.ResponseWriter, r *http.Request) {
	var req createToolRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.ToolName == "" || req.Version == "" || req.Endpoint == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "tool_name, version and endpoint are required")
		return
	}
	if len(req.Schema) == 0 || string(req.Schema) == "null" {
		writeError(w, http.StatusBadRequest, "bad_request", "schema must be an object")
		return
	}
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(req.Schema, &schema); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "schema must be an object")
		return
	}
	tenantID, err := domain.TenantOf(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "missing tenant in request context")
		return
	}
	tool, err := a.store.CreateTool(r.Context(), domain.Tool{
		TenantID:    tenantID,
		ToolName:    req.ToolName,
		Version:     req.Version,
		Description: req.Description,
		Schema:      req.Schema,
		Endpoint:    req.Endpoint,
		IsDefault:   req.IsDefault,
	})
	if errors.Is(err, domain.ErrConflict) {
		writeError(w, http.StatusConflict, "conflict", "tool version already exists")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "create tool failed")
		return
	}
	writeJSON(w, http.StatusCreated, tool)
}

func (a *API) listTools(w http.ResponseWriter, r *http.Request) {
	tools, err := a.store.ListTools(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "list tools failed")
		return
	}
	writeJSON(w, http.StatusOK, tools)
}

type invokeToolRequest struct {
	Version string          `json:"version"`
	Args    json.RawMessage `json:"args"`
}

type invokeToolResponse struct {
	Tool    string          `json:"tool"`
	Version string          `json:"version"`
	Result  json.RawMessage `json:"result"`
}

func (a *API) invokeTool(w http.ResponseWriter, r *http.Request) {
	var req invokeToolRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if len(req.Args) == 0 || string(req.Args) == "null" {
		writeError(w, http.StatusBadRequest, "bad_request", "args must be an object")
		return
	}
	var args map[string]json.RawMessage
	if err := json.Unmarshal(req.Args, &args); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "args must be an object")
		return
	}
	tool, err := a.tools.Resolve(r.Context(), r.PathValue("tool_name"), req.Version)
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusBadRequest, "bad_request", "tool or version not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "resolve tool failed")
		return
	}
	if err := a.tools.Validate(tool, req.Args); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "args do not match tool schema")
		return
	}
	result, err := a.tools.Invoke(r.Context(), tool, req.Args, "")
	if err != nil {
		writeError(w, http.StatusBadGateway, "unavailable", "tool invocation failed")
		return
	}
	writeJSON(w, http.StatusOK, invokeToolResponse{
		Tool:    tool.ToolName,
		Version: tool.Version,
		Result:  result,
	})
}
