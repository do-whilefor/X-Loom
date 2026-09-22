package server

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestWorkspaceServesItsBundledAssets(t *testing.T) {
	handler := New(nil)
	for _, path := range []string{"/", "/static/index.html"} {
		t.Run(path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
			if response.Code != http.StatusOK {
				t.Fatalf("workspace returned HTTP %d", response.Code)
			}
			if !strings.HasPrefix(response.Header().Get("Content-Type"), "text/html") {
				t.Fatalf("workspace content type = %q", response.Header().Get("Content-Type"))
			}
			body := response.Body.String()
			if !strings.Contains(body, "X-Loom · 工作台") || !strings.Contains(body, `id="graph-host"`) {
				t.Fatal("X-Loom workspace is missing")
			}
			if strings.Contains(body, "legacy.html") || strings.Contains(body, "经典管理界面") {
				t.Fatal("workspace still links to the removed classic interface")
			}
			assets := regexp.MustCompile(`(?:src|href)="(/static/[^"#]+)"`).FindAllStringSubmatch(body, -1)
			if len(assets) == 0 {
				t.Fatal("workspace has no bundled asset references")
			}
			for _, asset := range assets {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, asset[1], nil))
				if response.Code != http.StatusOK || response.Body.Len() == 0 {
					t.Errorf("asset %s: HTTP %d, %d bytes", asset[1], response.Code, response.Body.Len())
				}
			}
		})
	}
}

func TestClassicInterfaceIsUnavailable(t *testing.T) {
	handler := New(nil)
	for _, path := range []string{
		"/legacy.html", "/static/legacy.html", "/static/favicon.svg",
		"/static/vendor/alpine.min.js", "/static/vendor/tailwindcss.js",
		"/static/vendor/cola.min.js", "/static/vendor/cytoscape-cola.js",
		"/static/vendor/klay.js", "/static/vendor/cytoscape-klay.js",
		"/static/vendor/elk.bundled.js", "/static/vendor/cytoscape-elk.js",
	} {
		t.Run(path, func(t *testing.T) {
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(method, path, nil))
				if response.Code != http.StatusNotFound {
					t.Errorf("%s %s: HTTP %d, want 404", method, path, response.Code)
				}
			}
		})
	}
}
