package console

import (
	"embed"
	"net/http"
	"path"
	"strings"
)

//go:embed assets
var assets embed.FS

func Handler() http.Handler {
	return http.HandlerFunc(serve)
}

func serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/console" || r.URL.Path == "/console/":
		writeAsset(w, r, "index.html")
	case strings.HasPrefix(r.URL.Path, "/console/static/"):
		name := strings.TrimPrefix(r.URL.Path, "/console/static/")
		if name == "" || name != path.Base(name) || strings.HasPrefix(name, ".") {
			http.NotFound(w, r)
			return
		}
		writeAsset(w, r, name)
	default:
		http.NotFound(w, r)
	}
}

func writeAsset(w http.ResponseWriter, r *http.Request, name string) {
	data, err := assets.ReadFile("assets/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch path.Ext(name) {
	case ".html":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case ".css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case ".js":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}
