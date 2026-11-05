// Package webui serves the web client, embedded from static/app. The
// directory is produced by building web/ (scripts/build-web.ps1) and is not
// committed; a binary built without it serves a page that says so.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:static
var static embed.FS

// Built reports whether the web client was embedded.
func Built() bool {
	_, err := fs.Stat(static, "static/app/index.html")
	return err == nil
}

// Handler serves the web client. Paths without a file are client-side
// routes and get index.html; hashed assets are cached indefinitely.
func Handler() http.Handler {
	if !Built() {
		return http.HandlerFunc(notBuilt)
	}
	app, err := fs.Sub(static, "static/app")
	if err != nil {
		panic(err)
	}
	files := http.FileServerFS(app)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "" {
			name = "index.html"
		}
		if _, err := fs.Stat(app, name); err != nil {
			r = r.Clone(r.Context())
			r.URL.Path = "/"
			name = "index.html"
		}
		if strings.HasPrefix(name, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

func notBuilt(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><title>Linea</title>
<p>The web client is not built into this binary. Run <code>./scripts/build-web.ps1</code>
and rebuild, or use the API under <a href="/api/v1/openapi.json">/api/v1</a>.</p>`))
}
