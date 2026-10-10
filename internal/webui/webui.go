// Package webui is the browser UI the daemon serves: the build of web/ (web/dist), copied
// into app/ by `make web` and embedded in the binary. A binary built without it still
// compiles and serves a page that says how to build it.
//
// The UI is static and holds no secret: it is the same files as the public repo's build,
// so it is served without a sign-in. What it reads and changes goes through the API,
// which needs one (see the daemon's web.go).
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:app
var embedded embed.FS

// app is the build, rooted at its index.html.
var app, _ = fs.Sub(embedded, "app")

// Built reports whether this binary carries the UI.
func Built() bool { return built(app) }

func built(fsys fs.FS) bool {
	_, err := fs.Stat(fsys, "index.html")
	return err == nil
}

// CSP is the pages' Content-Security-Policy: everything from the daemon's own origin, no
// inline script, no framing (an answer is a click, so no page may overlay this one), and
// no form posting anywhere. React sets styles through the DOM, which style-src allows.
const CSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; " +
	"frame-ancestors 'none'; form-action 'self'"

// Handler serves the UI: / is index.html and other paths are files of the build. Routes
// are in the URL's fragment, so nothing else needs to resolve to the app. Without a
// build, / is a page saying how to make one.
func Handler() http.Handler { return serve(app) }

func serve(fsys fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", CSP)
		h.Set("Referrer-Policy", "no-referrer")
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "" {
			name = "index.html"
		}
		if name == "index.html" && !built(fsys) {
			NotBuilt(w, r)
			return
		}
		// No dotfiles (the build directory's .gitignore) and nothing outside the build.
		if strings.HasPrefix(path.Base(name), ".") || !fs.ValidPath(name) {
			http.NotFound(w, r)
			return
		}
		if st, err := fs.Stat(fsys, name); err != nil || st.IsDir() {
			http.NotFound(w, r)
			return
		}
		// Vite names assets by their content, so they never change; index.html does.
		if strings.HasPrefix(name, "assets/") {
			h.Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			h.Set("Cache-Control", "no-cache")
		}
		http.ServeFileFS(w, r, fsys, name)
	})
}

// NotBuilt answers with a page that says this binary has no UI and how to build one.
func NotBuilt(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write([]byte(notBuiltPage))
}

const notBuiltPage = `<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><meta name="color-scheme" content="light dark"><link rel="icon" href="data:,"><title>Shepherd: UI not built</title></head>
<body>
<h1>The web UI is not built into this binary</h1>
<p>This shepherd was built without the browser UI. The API still answers.</p>
<p>To get the UI, build Shepherd with <code>make build</code> (it needs Node 22 or later to build <code>web/</code>),
or run the development server: <code>cd web &amp;&amp; npm install &amp;&amp; npm run dev</code>.</p>
</body>
</html>
`
