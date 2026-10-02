// Package web serves intagent's dashboard: static files embedded in the
// binary that read the board and the live event stream from the API.
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var static embed.FS

// Handler serves the dashboard.
func Handler() http.Handler {
	sub, err := fs.Sub(static, "static")
	if err != nil {
		panic(err) // the embedded tree is fixed at build time
	}
	files := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		files.ServeHTTP(w, r)
	})
}
