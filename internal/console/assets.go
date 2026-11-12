package console

import (
	"embed"
	"io"
	"io/fs"
	"net/http"
	"regexp"
	"strings"
)

// embedded is web/'s build output (console plan, "Frontend"), committed so
// a plain go build serves the console without Node. all: keeps .gitkeep
// in the pattern: with no build present, the directory holds only that
// file and the embed still compiles.
//
//go:embed all:assets
var embedded embed.FS

// embeddedAssets returns the embedded build output, rooted at assets/.
func embeddedAssets() fs.FS {
	sub, err := fs.Sub(embedded, "assets")
	if err != nil {
		panic(err) // the embed pattern guarantees assets/ exists
	}
	return sub
}

// missingBuildPage is served for every non-API path when the assets carry
// no index.html. Plain HTML with no inline script or style: the CSP
// applies to it too.
const missingBuildPage = `<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><title>BMUS Bridge: console not built</title></head>
<body>
<h1>The console frontend isn't built</h1>
<p>This binary was built without the console's assets. Build them and rebuild bmus:</p>
<pre>cd web &amp;&amp; npm ci &amp;&amp; npm run build
go build ./cmd/bmus</pre>
</body>
</html>
`

// reportPageRE matches the mission report's path, /report/<order>
// (#104): one segment of an order id's characters, so no other path
// reaches it.
var reportPageRE = regexp.MustCompile(`^/report/[A-Za-z0-9-]+$`)

// assetsHandler serves the frontend from assets to GET and HEAD:
// index.html at / and at the mission report's /report/<order>, whose route the
// page reads from its URL, and every other file at its path. Directory
// listings and dotfiles are 404s. With no index.html in assets, every
// path gets missingBuildPage with a 503.
func assetsHandler(assets fs.FS) http.Handler {
	var serve http.Handler
	if _, err := fs.Stat(assets, "index.html"); err != nil {
		serve = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, missingBuildPage)
		})
	} else {
		files := http.FileServerFS(assets)
		serve = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := r.URL.Path
			if p != "/" && strings.HasSuffix(p, "/") || strings.Contains(p, "/.") {
				http.NotFound(w, r)
				return
			}
			// File names carry no content hash: revalidate every time.
			w.Header().Set("Cache-Control", "no-cache")
			if reportPageRE.MatchString(p) {
				http.ServeFileFS(w, r, assets, "index.html")
				return
			}
			files.ServeHTTP(w, r)
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		serve.ServeHTTP(w, r)
	})
}
