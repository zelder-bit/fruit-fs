// Package web holds the browser app, built into the binary.
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var static embed.FS

// Handler serves the app. Assets are revalidated on each load (cheap with ETags
// on the LAN), so a new version shows up on the next refresh.
func Handler() http.Handler {
	sub, _ := fs.Sub(static, "static")
	fsrv := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		fsrv.ServeHTTP(w, r)
	})
}
