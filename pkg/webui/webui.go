// Package webui embeds the built management console (web/ → dist) so the
// API server ships it in the same binary — the self-hosting story stays one
// artifact. `make web` rebuilds dist from web/; the built assets are
// committed so `go build` alone always produces a working console.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

// Handler serves the console SPA: real files from the embedded dist, and
// index.html for every client-routed path. API namespaces must be excluded
// by the caller (this handler is mounted as the router's NoRoute fallback).
func Handler() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic("webui: embedded dist missing: " + err.Error())
	}
	files := http.FS(sub)
	server := http.FileServer(files)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p != "" {
			if f, err := files.Open("/" + p); err == nil {
				f.Close()
				server.ServeHTTP(w, r)
				return
			}
		}
		// Client-side route (or /): serve the app shell. No caching — the
		// hashed asset files carry the version, the shell must not stick.
		w.Header().Set("Cache-Control", "no-cache")
		r.URL.Path = "/"
		server.ServeHTTP(w, r)
	})
}
