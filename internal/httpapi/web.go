package httpapi

import (
	"io/fs"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/ziad/liveness-verifier/web"
)

// mountDemo serves the demo page.
//
// Served from the binary rather than a mounted directory: the demo is part of
// the release, and a page that has drifted away from the API it calls is a
// confusing way to discover a version mismatch.
//
// It sits outside /v1 and outside the API key check. It is a static page that
// asks the operator for a key and sends it as a header — putting the page
// itself behind the key would mean typing the key into a browser dialog before
// there is anywhere to type it.
func mountDemo(r chi.Router) error {
	assets, err := fs.Sub(web.FS, "static")
	if err != nil {
		return err
	}

	fileServer := http.FileServer(http.FS(assets))

	r.Get("/demo", func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, "/demo/", http.StatusMovedPermanently)
	})
	r.Handle("/demo/*", http.StripPrefix("/demo/", noDirectoryListing(fileServer)))

	return nil
}

// noDirectoryListing hides the index of a directory while still serving its
// index.html.
func noDirectoryListing(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "" && r.URL.Path[len(r.URL.Path)-1] == '/' && r.URL.Path != "/" {
			r.URL.Path += "index.html"
		}
		next.ServeHTTP(w, r)
	})
}
