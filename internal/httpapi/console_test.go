package httpapi_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/romeokeita231/Lite-agent-platform/internal/httpapi"
)

func TestConsoleShellDoesNotRequireAuth(t *testing.T) {
	srv := httptest.NewServer(httpapi.NewMux(alwaysReady{}, nil, nil, alwaysReady{}))
	defer srv.Close()

	status := func(method, path string, headers map[string]string) int {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		for name, value := range headers {
			req.Header.Set(name, value)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	if got := status(http.MethodGet, "/console", nil); got != http.StatusOK {
		t.Fatalf("GET /console = %d, want 200", got)
	}
	index, err := http.Get(srv.URL + "/console")
	if err != nil {
		t.Fatalf("GET /console: %v", err)
	}
	body, _ := io.ReadAll(index.Body)
	index.Body.Close()
	if !strings.Contains(string(body), "/console/static/app.js") {
		t.Fatalf("GET /console does not serve the console index")
	}

	if got := status(http.MethodGet, "/console/does-not-exist", nil); got != http.StatusNotFound {
		t.Fatalf("GET /console/does-not-exist = %d, want 404", got)
	}
	runID := "run_missing"
	// Hand-maintained list: when a route is added to NewMux in router.go, add it here too.
	routes := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/v1/agents"},
		{http.MethodPost, "/v1/agents"},
		{http.MethodGet, "/v1/runs"},
		{http.MethodPost, "/v1/runs"},
		{http.MethodGet, "/v1/runs/" + runID},
		{http.MethodGet, "/v1/runs/" + runID + "/steps"},
		{http.MethodGet, "/v1/runs/" + runID + "/trace"},
		{http.MethodPost, "/v1/runs/" + runID + "/human-input"},
		{http.MethodPost, "/v1/runs/" + runID + "/cancel"},
		{http.MethodGet, "/v1/tools"},
		{http.MethodPost, "/v1/tools"},
		{http.MethodPost, "/v1/tools/some-tool/invoke"},
		{http.MethodPost, "/v1/chat/completions"},
		{http.MethodGet, "/v1/models"},
		{http.MethodGet, "/v1/tenants/tenant_missing/usage"},
		{http.MethodPost, "/v1/evals"},
		{http.MethodGet, "/v1/evals/eval_missing"},
	}
	for _, route := range routes {
		if got := status(route.method, route.path, nil); got != http.StatusUnauthorized {
			t.Fatalf("%s %s without bearer = %d, want 401", route.method, route.path, got)
		}
	}
	if got := status(http.MethodGet, "/metrics", nil); got != http.StatusNotFound {
		t.Fatalf("GET /metrics on the business port = %d, want 404", got)
	}
}
