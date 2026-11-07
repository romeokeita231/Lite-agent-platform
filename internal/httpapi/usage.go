package httpapi

import (
	"net/http"
	"time"

	"github.com/romeokeita231/Lite-agent-platform/internal/domain"
	"github.com/romeokeita231/Lite-agent-platform/internal/repository"
)

type usageResponse struct {
	TenantID string       `json:"tenant_id"`
	From     *time.Time   `json:"from"`
	To       *time.Time   `json:"to"`
	GroupBy  string       `json:"group_by"`
	Groups   []usageGroup `json:"groups"`
}

type usageGroup struct {
	Model        string  `json:"model"`
	RunID        *string `json:"run_id"`
	Calls        int     `json:"calls"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
}

func (a *API) tenantUsage(w http.ResponseWriter, r *http.Request) {
	tenantID, err := repository.TenantOf(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "missing tenant in request context")
		return
	}
	if r.PathValue("id") != tenantID {
		writeError(w, http.StatusNotFound, "not_found", "tenant not found")
		return
	}
	groupBy := r.URL.Query().Get("group_by")
	if groupBy == "" {
		groupBy = domain.UsageGroupByModel
	}
	if groupBy != domain.UsageGroupByModel && groupBy != domain.UsageGroupByRun {
		writeError(w, http.StatusBadRequest, "bad_request", "group_by must be model or run")
		return
	}
	from, ok := parseUsageTime(r, "from")
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_request", "from must be RFC3339")
		return
	}
	to, ok := parseUsageTime(r, "to")
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_request", "to must be RFC3339")
		return
	}

	groups, err := a.store.ModelUsage(r.Context(), groupBy, from, to)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "query usage failed")
		return
	}
	out := make([]usageGroup, 0, len(groups))
	for _, group := range groups {
		item := usageGroup{
			Calls:        group.Calls,
			InputTokens:  group.InputTokens,
			OutputTokens: group.OutputTokens,
		}
		if groupBy == domain.UsageGroupByRun {
			item.RunID = group.RunID
		} else {
			item.Model = group.Model
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, usageResponse{
		TenantID: tenantID,
		From:     from,
		To:       to,
		GroupBy:  groupBy,
		Groups:   out,
	})
}

func parseUsageTime(r *http.Request, name string) (*time.Time, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return nil, true
	}
	value, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, false
	}
	return &value, true
}
