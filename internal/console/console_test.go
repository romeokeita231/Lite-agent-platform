package console_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/romeokeita231/Lite-agent-platform/internal/console"
)

func get(t *testing.T, h http.Handler, path string) (int, string, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return rec.Code, rec.Header().Get("Content-Type"), string(body)
}

func TestIndexIsServedAtConsoleWithoutAuth(t *testing.T) {
	h := console.Handler()
	for _, path := range []string{"/console", "/console/"} {
		status, contentType, body := get(t, h, path)
		if status != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, status)
		}
		if !strings.HasPrefix(contentType, "text/html") {
			t.Fatalf("GET %s content type = %q, want text/html", path, contentType)
		}
		for _, want := range []string{"/console/static/app.js", "/console/static/styles.css"} {
			if !strings.Contains(body, want) {
				t.Fatalf("GET %s body is missing %q", path, want)
			}
		}
		if strings.Contains(body, "http://") || strings.Contains(body, "https://") {
			t.Fatalf("GET %s body references a remote URL; the console must stay offline", path)
		}
	}
}

func TestStaticAssetsAreEmbedded(t *testing.T) {
	h := console.Handler()
	status, contentType, body := get(t, h, "/console/static/app.js")
	if status != http.StatusOK {
		t.Fatalf("GET app.js = %d, want 200", status)
	}
	if !strings.Contains(contentType, "javascript") {
		t.Fatalf("app.js content type = %q, want javascript", contentType)
	}
	for _, want := range []string{"localStorage", "Authorization", "Bearer "} {
		if !strings.Contains(body, want) {
			t.Fatalf("app.js is missing %q", want)
		}
	}
	status, contentType, _ = get(t, h, "/console/static/styles.css")
	if status != http.StatusOK {
		t.Fatalf("GET styles.css = %d, want 200", status)
	}
	if !strings.Contains(contentType, "text/css") {
		t.Fatalf("styles.css content type = %q, want text/css", contentType)
	}
}

func TestUnknownConsolePathsReturn404(t *testing.T) {
	h := console.Handler()
	for _, path := range []string{
		"/console/unknown",
		"/console/static/missing.js",
		"/console/static/",
		"/console/static/../console.go",
		"/console/static/a/b.js",
	} {
		if status, _, _ := get(t, h, path); status != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404", path, status)
		}
	}
}
