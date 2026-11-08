package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"
)

type usageGroup struct {
	Model        string  `json:"model"`
	RunID        *string `json:"run_id"`
	Calls        int     `json:"calls"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
}

type usageBody struct {
	TenantID string       `json:"tenant_id"`
	From     *time.Time   `json:"from"`
	To       *time.Time   `json:"to"`
	GroupBy  string       `json:"group_by"`
	Groups   []usageGroup `json:"groups"`
}

type usageRow struct {
	tenant string
	runID  *string
	model  string
	input  int
	output int
	at     time.Time
}

func seedUsage(t *testing.T, api *apiEnv, rows []usageRow) {
	t.Helper()
	for _, row := range rows {
		if _, err := api.env.Pool.Exec(context.Background(),
			`insert into model_call_logs (tenant_id, run_id, route_label, requested_model, chain,
			                             effective_model, fallback_from, input_tokens, output_tokens,
			                             latency_ms, status, created_at)
			 values ($1, $2::uuid, 'route:default', $3, '[]', $3, '', $4, $5, 0, 'ok', $6)`,
			row.tenant, row.runID, row.model, row.input, row.output, row.at); err != nil {
			t.Fatalf("seed usage row: %v", err)
		}
	}
}

func decodeUsage(t *testing.T, raw []byte) usageBody {
	t.Helper()
	var body usageBody
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode usage body %s: %v", raw, err)
	}
	return body
}

func TestTenantUsageGroupedByModel(t *testing.T) {
	api := newAPI(t)
	at := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	runA := "11111111-1111-4111-8111-111111111111"
	runB := "22222222-2222-4222-8222-222222222222"
	seedUsage(t, api, []usageRow{
		{tenant: "tenant-a", runID: &runA, model: "m1", input: 10, output: 4, at: at},
		{tenant: "tenant-a", runID: &runA, model: "m1", input: 20, output: 8, at: at},
		{tenant: "tenant-a", runID: &runB, model: "m1", input: 5, output: 2, at: at},
		{tenant: "tenant-a", model: "m2", input: 7, output: 3, at: at},
		{tenant: "tenant-b", runID: &runB, model: "m1", input: 100, output: 100, at: at},
	})

	status, raw := api.do(t, http.MethodGet, "/v1/tenants/tenant-a/usage", "tenant-a", nil)
	if status != http.StatusOK {
		t.Fatalf("usage status = %d body=%s", status, raw)
	}
	body := decodeUsage(t, raw)
	if body.TenantID != "tenant-a" || body.GroupBy != "model" || body.From != nil || body.To != nil {
		t.Fatalf("usage envelope = %+v", body)
	}
	if len(body.Groups) != 2 {
		t.Fatalf("groups = %+v", body.Groups)
	}
	if g := body.Groups[0]; g.Model != "m1" || g.RunID != nil || g.Calls != 3 || g.InputTokens != 35 || g.OutputTokens != 14 {
		t.Fatalf("m1 group = %+v", g)
	}
	if g := body.Groups[1]; g.Model != "m2" || g.RunID != nil || g.Calls != 1 || g.InputTokens != 7 || g.OutputTokens != 3 {
		t.Fatalf("m2 group = %+v", g)
	}
	if !bytes.Contains(raw, []byte(`"run_id":null`)) {
		t.Fatalf("model groups lack null run_id: %s", raw)
	}

	status, raw = api.do(t, http.MethodGet, "/v1/tenants/tenant-b/usage", "tenant-b", nil)
	if status != http.StatusOK {
		t.Fatalf("tenant b usage status = %d body=%s", status, raw)
	}
	body = decodeUsage(t, raw)
	if len(body.Groups) != 1 || body.Groups[0].Model != "m1" ||
		body.Groups[0].Calls != 1 || body.Groups[0].InputTokens != 100 || body.Groups[0].OutputTokens != 100 {
		t.Fatalf("tenant b groups = %+v", body.Groups)
	}

	status, raw = api.do(t, http.MethodGet, "/v1/tenants/tenant-b/usage", "tenant-a", nil)
	if status != http.StatusNotFound {
		t.Fatalf("cross-tenant usage status = %d body=%s", status, raw)
	}
	if body := decodeError(t, raw); body.Error != "not_found" {
		t.Fatalf("cross-tenant usage body = %+v", body)
	}

	status, raw = api.do(t, http.MethodGet, "/v1/tenants/tenant-c/usage", "tenant-a", nil)
	if status != http.StatusNotFound {
		t.Fatalf("unknown tenant usage status = %d body=%s", status, raw)
	}

	status, raw = api.do(t, http.MethodGet, "/v1/tenants/tenant-a/usage", "", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated usage status = %d body=%s", status, raw)
	}
}

func TestTenantUsageGroupedByRun(t *testing.T) {
	api := newAPI(t)
	at := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	runA := "11111111-1111-4111-8111-111111111111"
	runB := "22222222-2222-4222-8222-222222222222"
	seedUsage(t, api, []usageRow{
		{tenant: "tenant-a", runID: &runA, model: "m1", input: 10, output: 4, at: at},
		{tenant: "tenant-a", runID: &runA, model: "m1", input: 20, output: 8, at: at},
		{tenant: "tenant-a", runID: &runB, model: "m2", input: 5, output: 2, at: at},
		{tenant: "tenant-a", model: "m1", input: 7, output: 3, at: at},
		{tenant: "tenant-b", runID: &runB, model: "m1", input: 100, output: 100, at: at},
	})

	status, raw := api.do(t, http.MethodGet, "/v1/tenants/tenant-a/usage?group_by=run", "tenant-a", nil)
	if status != http.StatusOK {
		t.Fatalf("usage by run status = %d body=%s", status, raw)
	}
	body := decodeUsage(t, raw)
	if body.GroupBy != "run" || len(body.Groups) != 3 {
		t.Fatalf("usage by run body = %+v", body)
	}
	found := map[string]usageGroup{}
	for _, group := range body.Groups {
		if group.Model != "" {
			t.Fatalf("run group carries model %q", group.Model)
		}
		key := "null"
		if group.RunID != nil {
			key = *group.RunID
		}
		found[key] = group
	}
	if group, ok := found[runA]; !ok || group.Calls != 2 || group.InputTokens != 30 || group.OutputTokens != 12 {
		t.Fatalf("run a group = %+v", found[runA])
	}
	if group, ok := found[runB]; !ok || group.Calls != 1 || group.InputTokens != 5 || group.OutputTokens != 2 {
		t.Fatalf("run b group = %+v", found[runB])
	}
	if group, ok := found["null"]; !ok || group.Calls != 1 || group.InputTokens != 7 || group.OutputTokens != 3 {
		t.Fatalf("standalone group = %+v", found["null"])
	}

	status, raw = api.do(t, http.MethodGet, "/v1/tenants/tenant-a/usage?group_by=step", "tenant-a", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("invalid group_by status = %d body=%s", status, raw)
	}
	if body := decodeError(t, raw); body.Error != "bad_request" {
		t.Fatalf("invalid group_by body = %+v", body)
	}
}

func TestTenantUsageTimeWindow(t *testing.T) {
	api := newAPI(t)
	early := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	mid := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	late := time.Date(2026, 1, 1, 11, 0, 0, 0, time.UTC)
	seedUsage(t, api, []usageRow{
		{tenant: "tenant-a", model: "m1", input: 10, output: 1, at: early},
		{tenant: "tenant-a", model: "m1", input: 20, output: 2, at: mid},
		{tenant: "tenant-a", model: "m1", input: 30, output: 3, at: late},
	})

	query := "/v1/tenants/tenant-a/usage?from=" + url.QueryEscape(mid.Format(time.RFC3339)) +
		"&to=" + url.QueryEscape(late.Format(time.RFC3339))
	status, raw := api.do(t, http.MethodGet, query, "tenant-a", nil)
	if status != http.StatusOK {
		t.Fatalf("window usage status = %d body=%s", status, raw)
	}
	body := decodeUsage(t, raw)
	if body.From == nil || body.To == nil || !body.From.Equal(mid) || !body.To.Equal(late) {
		t.Fatalf("window echo = %+v", body)
	}
	if len(body.Groups) != 1 || body.Groups[0].Calls != 2 ||
		body.Groups[0].InputTokens != 50 || body.Groups[0].OutputTokens != 5 {
		t.Fatalf("window groups = %+v", body.Groups)
	}

	status, raw = api.do(t, http.MethodGet, "/v1/tenants/tenant-a/usage?to="+url.QueryEscape(mid.Format(time.RFC3339)), "tenant-a", nil)
	if status != http.StatusOK {
		t.Fatalf("to-only usage status = %d body=%s", status, raw)
	}
	body = decodeUsage(t, raw)
	if body.From != nil || len(body.Groups) != 1 || body.Groups[0].Calls != 2 ||
		body.Groups[0].InputTokens != 30 || body.Groups[0].OutputTokens != 3 {
		t.Fatalf("to-only usage = %+v", body)
	}

	status, raw = api.do(t, http.MethodGet, "/v1/tenants/tenant-a/usage?from="+url.QueryEscape(mid.Format(time.RFC3339)), "tenant-a", nil)
	if status != http.StatusOK {
		t.Fatalf("from-only usage status = %d body=%s", status, raw)
	}
	body = decodeUsage(t, raw)
	if body.To != nil || len(body.Groups) != 1 || body.Groups[0].Calls != 2 ||
		body.Groups[0].InputTokens != 50 || body.Groups[0].OutputTokens != 5 {
		t.Fatalf("from-only usage = %+v", body)
	}

	status, raw = api.do(t, http.MethodGet, "/v1/tenants/tenant-a/usage?from=yesterday", "tenant-a", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("invalid from status = %d body=%s", status, raw)
	}
	if body := decodeError(t, raw); body.Error != "bad_request" {
		t.Fatalf("invalid from body = %+v", body)
	}

	status, raw = api.do(t, http.MethodGet, "/v1/tenants/tenant-a/usage?to=2030-13-01T00:00:00Z", "tenant-a", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("invalid to status = %d body=%s", status, raw)
	}
	if body := decodeError(t, raw); body.Error != "bad_request" {
		t.Fatalf("invalid to body = %+v", body)
	}
}
