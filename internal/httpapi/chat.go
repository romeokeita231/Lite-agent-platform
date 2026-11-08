package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/romeokeita231/Lite-agent-platform/internal/domain"
	"github.com/romeokeita231/Lite-agent-platform/internal/ids"
)

type chatCompletionResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   chatUsage    `json:"usage"`
	Lite    chatLite     `json:"lite"`
}

type chatChoice struct {
	Index        int         `json:"index"`
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type chatLite struct {
	EffectiveModel string     `json:"effective_model"`
	Chain          []string   `json:"chain"`
	FallbackFrom   *string    `json:"fallback_from"`
	Usage          liteTokens `json:"usage"`
}

type liteTokens struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

func (a *API) chatCompletions(w http.ResponseWriter, r *http.Request) {
	if a.gateway == nil {
		writeError(w, http.StatusInternalServerError, "internal", "model gateway is not configured")
		return
	}
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	var model string
	if value, ok := raw["model"]; !ok || json.Unmarshal(value, &model) != nil || model == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "model is required")
		return
	}
	var messages []domain.ModelMessage
	if value, ok := raw["messages"]; !ok || json.Unmarshal(value, &messages) != nil || len(messages) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "messages are required")
		return
	}
	extra := make(map[string]json.RawMessage, len(raw))
	for key, value := range raw {
		if key == "model" || key == "messages" {
			continue
		}
		extra[key] = value
	}
	tenantID, err := domain.TenantOf(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "missing tenant in request context")
		return
	}

	result, callErr := a.gateway.Generate(r.Context(), domain.ModelRequest{
		Route:    model,
		Messages: messages,
		Extra:    extra,
	})
	if errors.Is(callErr, domain.ErrModelRouteNotFound) {
		writeError(w, http.StatusBadRequest, "bad_request", "model or route not found")
		return
	}

	log := domain.ModelCallLog{
		TenantID:       tenantID,
		RouteLabel:     result.RouteLabel,
		RequestedModel: result.RequestedModel,
		Chain:          result.Chain,
		EffectiveModel: result.Model,
		FallbackFrom:   result.FallbackFrom,
		InputTokens:    result.InputTokens,
		OutputTokens:   result.OutputTokens,
		LatencyMS:      result.LatencyMS,
		Status:         result.Status,
	}
	spans := make([]domain.Span, 0, len(result.Attempts))
	for _, attempt := range result.Attempts {
		attributes, _ := json.Marshal(map[string]any{
			"requested_model": result.RequestedModel,
			"effective_model": attempt.Model,
			"status":          attempt.Status,
			"latency_ms":      attempt.LatencyMS,
			"attempt":         attempt.Attempt,
		})
		spans = append(spans, domain.Span{
			ID:         ids.New(),
			TenantID:   tenantID,
			StepIndex:  0,
			Attempt:    attempt.Attempt,
			Name:       "model." + attempt.Model,
			StartedAt:  attempt.StartedAt,
			EndedAt:    attempt.EndedAt,
			Attributes: attributes,
			Error:      attempt.Error,
		})
	}
	if err := a.store.RecordModelCallAttempts(r.Context(), spans, log); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "record model call failed")
		return
	}
	if callErr != nil {
		writeError(w, http.StatusBadGateway, "unavailable", "model upstream failed")
		return
	}

	var fallbackFrom *string
	if result.FallbackFrom != "" {
		value := result.FallbackFrom
		fallbackFrom = &value
	}
	writeJSON(w, http.StatusOK, chatCompletionResponse{
		ID:      ids.New(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   result.Model,
		Choices: []chatChoice{{
			Index:        0,
			Message:      chatMessage{Role: "assistant", Content: result.Content},
			FinishReason: "stop",
		}},
		Usage: chatUsage{
			PromptTokens:     result.InputTokens,
			CompletionTokens: result.OutputTokens,
			TotalTokens:      result.InputTokens + result.OutputTokens,
		},
		Lite: chatLite{
			EffectiveModel: result.Model,
			Chain:          result.Chain,
			FallbackFrom:   fallbackFrom,
			Usage: liteTokens{
				InputTokens:  result.InputTokens,
				OutputTokens: result.OutputTokens,
			},
		},
	})
}

type modelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type modelList struct {
	Object string        `json:"object"`
	Data   []modelObject `json:"data"`
}

func (a *API) listModels(w http.ResponseWriter, r *http.Request) {
	routes, err := a.store.ListModelRoutes(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "list model routes failed")
		return
	}
	created := time.Now().Unix()
	seen := map[string]bool{}
	data := make([]modelObject, 0, len(routes))
	for _, route := range routes {
		for _, model := range route.Chain {
			if seen[model] {
				continue
			}
			seen[model] = true
			data = append(data, modelObject{ID: model, Object: "model", Created: created, OwnedBy: "platform"})
		}
	}
	for _, route := range routes {
		data = append(data, modelObject{
			ID:      "route:" + route.Label,
			Object:  "model",
			Created: created,
			OwnedBy: "platform",
		})
	}
	writeJSON(w, http.StatusOK, modelList{Object: "list", Data: data})
}
