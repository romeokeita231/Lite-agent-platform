package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/romeokeita231/Lite-agent-platform/internal/domain"
)

type traceSpanResponse struct {
	ID         string          `json:"id"`
	Attempt    int             `json:"attempt"`
	Name       string          `json:"name"`
	StartedAt  time.Time       `json:"started_at"`
	EndedAt    time.Time       `json:"ended_at"`
	Attributes json.RawMessage `json:"attributes"`
	Error      string          `json:"error"`
}

type traceStepResponse struct {
	StepIndex int                 `json:"step_index"`
	Spans     []traceSpanResponse `json:"spans"`
}

type traceResponse struct {
	RunID string              `json:"run_id"`
	Steps []traceStepResponse `json:"steps"`
}

func (a *API) getRunTrace(w http.ResponseWriter, r *http.Request) {
	spans, err := a.store.ListSpans(r.Context(), r.PathValue("id"))
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "run not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "get trace failed")
		return
	}
	out := traceResponse{RunID: r.PathValue("id"), Steps: []traceStepResponse{}}
	positions := map[int]int{}
	for _, span := range spans {
		attributes := span.Attributes
		if len(attributes) == 0 {
			attributes = json.RawMessage(`{}`)
		}
		position, ok := positions[span.StepIndex]
		if !ok {
			position = len(out.Steps)
			positions[span.StepIndex] = position
			out.Steps = append(out.Steps, traceStepResponse{StepIndex: span.StepIndex, Spans: []traceSpanResponse{}})
		}
		out.Steps[position].Spans = append(out.Steps[position].Spans, traceSpanResponse{
			ID:         span.ID,
			Attempt:    span.Attempt,
			Name:       span.Name,
			StartedAt:  span.StartedAt,
			EndedAt:    span.EndedAt,
			Attributes: attributes,
			Error:      span.Error,
		})
	}
	sort.Slice(out.Steps, func(i, j int) bool { return out.Steps[i].StepIndex < out.Steps[j].StepIndex })
	writeJSON(w, http.StatusOK, out)
}
