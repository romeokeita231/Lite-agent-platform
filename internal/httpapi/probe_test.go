package httpapi_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/romeokeita231/Lite-agent-platform/internal/httpapi"
	"github.com/romeokeita231/Lite-agent-platform/internal/observability"
)

func TestProbesReflectReadinessAndDraining(t *testing.T) {
	ready := observability.NewReadiness()
	srv := httptest.NewServer(httpapi.NewMux(ready, nil, nil, ready, nil))
	defer srv.Close()

	check := func(path string, want int) {
		t.Helper()
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode != want {
			t.Fatalf("GET %s = %d, want %d", path, resp.StatusCode, want)
		}
	}

	check("/healthz", http.StatusOK)
	check("/readyz", http.StatusServiceUnavailable)

	ready.SetReady(true)
	check("/readyz", http.StatusOK)

	ready.SetDraining(true)
	check("/healthz", http.StatusOK)
	check("/readyz", http.StatusServiceUnavailable)
}
