package modelgw_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/romeokeita231/Lite-agent-platform/internal/domain"
	"github.com/romeokeita231/Lite-agent-platform/internal/modelgw"
	"github.com/romeokeita231/Lite-agent-platform/internal/repository"
	"github.com/romeokeita231/Lite-agent-platform/internal/testutil"
)

func TestChainBudgetStopsBeforeSecondModel(t *testing.T) {
	env := testutil.New(t)
	store := repository.New(env.Pool)
	ctx := repository.WithTenant(context.Background(), "tenant-a")
	if err := store.CreateTenant(ctx, domain.Tenant{ID: "tenant-a", Name: "tenant-a"}); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if _, err := store.UpsertModelRoute(ctx, domain.ModelRoute{
		TenantID: "tenant-a",
		Label:    "default",
		Chain:    []string{"m1", "m2"},
	}); err != nil {
		t.Fatalf("upsert route: %v", err)
	}

	var firstCalls, secondCalls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &request)
		switch request.Model {
		case "m1":
			atomic.AddInt32(&firstCalls, 1)
			time.Sleep(120 * time.Millisecond)
			http.Error(w, "slow failure", http.StatusInternalServerError)
		case "m2":
			atomic.AddInt32(&secondCalls, 1)
			time.Sleep(120 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"m2"}}]}`))
		default:
			http.Error(w, "unknown model", http.StatusNotFound)
		}
	}))
	t.Cleanup(upstream.Close)

	gateway := modelgw.NewWithStore(store, upstream.URL+"/v1", "", 200*time.Millisecond)
	result, err := gateway.Generate(ctx, domain.ModelRequest{
		Route:    "route:default",
		Messages: []domain.ModelMessage{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatalf("generate error = nil, result = %+v", result)
	}
	if atomic.LoadInt32(&firstCalls) != 1 {
		t.Fatalf("first model calls = %d", firstCalls)
	}
	if atomic.LoadInt32(&secondCalls) != 0 {
		t.Fatalf("second model calls = %d, want 0 after budget exhaustion", secondCalls)
	}
	if len(result.Attempts) != 1 || result.Attempts[0].Model != "m1" || result.Attempts[0].Status != domain.ModelStatusError {
		t.Fatalf("attempts = %+v", result.Attempts)
	}
	if result.Status != domain.ModelStatusError || result.Model != "" {
		t.Fatalf("result = %+v", result)
	}
	if len(result.Chain) != 2 || result.Chain[0] != "m1" || result.Chain[1] != "m2" {
		t.Fatalf("chain = %v", result.Chain)
	}
}

func TestConcreteModelIsSingleEntryChain(t *testing.T) {
	env := testutil.New(t)
	store := repository.New(env.Pool)
	ctx := repository.WithTenant(context.Background(), "tenant-a")
	if err := store.CreateTenant(ctx, domain.Tenant{ID: "tenant-a", Name: "tenant-a"}); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	var calls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`))
	}))
	t.Cleanup(upstream.Close)

	gateway := modelgw.NewWithStore(store, upstream.URL+"/v1", "", 2*time.Second)
	result, err := gateway.Generate(ctx, domain.ModelRequest{Route: "m9", Messages: []domain.ModelMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if result.Model != "m9" || len(result.Chain) != 1 || result.Chain[0] != "m9" || result.FallbackFrom != "" {
		t.Fatalf("result = %+v", result)
	}
	if result.Content != "ok" || result.InputTokens != 1 || result.OutputTokens != 2 {
		t.Fatalf("result content/usage = %+v", result)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("calls = %d", calls)
	}
}

func TestUnknownRouteReturnsNotFound(t *testing.T) {
	env := testutil.New(t)
	store := repository.New(env.Pool)
	ctx := repository.WithTenant(context.Background(), "tenant-a")
	if err := store.CreateTenant(ctx, domain.Tenant{ID: "tenant-a", Name: "tenant-a"}); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	gateway := modelgw.NewWithStore(store, "http://127.0.0.1:1/v1", "", time.Second)
	result, err := gateway.Generate(ctx, domain.ModelRequest{Route: "route:missing", Messages: []domain.ModelMessage{{Role: "user", Content: "hi"}}})
	if err == nil || !isNotFound(err) {
		t.Fatalf("error = %v, want model route not found", err)
	}
	if len(result.Attempts) != 0 {
		t.Fatalf("attempts = %+v", result.Attempts)
	}
}

func isNotFound(err error) bool {
	for err != nil {
		if err == domain.ErrModelRouteNotFound {
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}
