package main

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed web
var webAssets embed.FS

func (s *Server) webHandler() http.Handler {
	sub, err := fs.Sub(webAssets, "web")
	if err != nil {
		return http.NotFoundHandler()
	}
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/v1/") {
			http.NotFound(w, r)
			return
		}
		if !s.adminAuthed(r) {
			// Only the login page and its minimal assets are reachable
			// before authentication.
			if path == "/login.js" || path == "/style.css" {
				files.ServeHTTP(w, r)
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			r.URL.Path = "/login.html"
		}
		if path == "/" {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}
