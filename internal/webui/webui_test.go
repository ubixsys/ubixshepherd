package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func get(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w
}

func TestServesTheBuild(t *testing.T) {
	h := serve(fstest.MapFS{
		"index.html":          {Data: []byte("<!doctype html><title>Shepherd</title>")},
		"assets/index-abc.js": {Data: []byte("console.log(1)")},
		".gitignore":          {Data: []byte("*")},
		"assets/.secret":      {Data: []byte("x")},
		"assets/nested/x.css": {Data: []byte("a{}")},
	})
	w := get(t, h, "GET", "/")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "<title>Shepherd") {
		t.Fatalf("/: %d %q", w.Code, w.Body.String())
	}
	if w.Header().Get("Content-Security-Policy") != CSP || w.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("index headers: %v", w.Header())
	}
	w = get(t, h, "GET", "/assets/index-abc.js")
	if w.Code != 200 || !strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("asset: %d %v", w.Code, w.Header())
	}
	for _, p := range []string{"/.gitignore", "/assets/.secret", "/assets", "/missing.js", "/../webui.go", "/assets/../../webui.go"} {
		if w := get(t, h, "GET", p); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", p, w.Code)
		}
	}
	if w := get(t, h, "POST", "/"); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /: %d", w.Code)
	}
}

// A binary built without the UI says so, and how to build it.
func TestNotBuilt(t *testing.T) {
	h := serve(fstest.MapFS{".gitignore": {Data: []byte("*")}})
	w := get(t, h, "GET", "/")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "not built") || !strings.Contains(w.Body.String(), "make build") {
		t.Errorf("%d %q", w.Code, w.Body.String())
	}
	if w := get(t, h, "GET", "/assets/x.js"); w.Code != http.StatusNotFound {
		t.Errorf("asset without a build: %d", w.Code)
	}
}
