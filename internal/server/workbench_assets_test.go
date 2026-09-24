package server

import (
	"mime"
	"net/http"
	"net/http/httptest"
	"path"
	"regexp"
	"strings"
	"testing"
)

func TestWorkbenchKeepsSameOriginContentPolicy(t *testing.T) {
	handler := New(nil)
	for _, endpoint := range []string{"/", "/static/index.html"} {
		t.Run(endpoint, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, endpoint, nil))
			if response.Code != http.StatusOK {
				t.Fatalf("workspace returned HTTP %d", response.Code)
			}
			html := response.Body.String()
			policy := regexp.MustCompile(`<meta\s+http-equiv="Content-Security-Policy"\s+content="([^"]+)"`).FindStringSubmatch(html)
			if len(policy) != 2 {
				t.Fatal("workspace response is missing its content security policy")
			}
			directives := map[string]string{}
			for _, directive := range strings.Split(policy[1], ";") {
				parts := strings.Fields(directive)
				if len(parts) > 0 {
					directives[parts[0]] = strings.Join(parts[1:], " ")
				}
			}
			for directive, want := range map[string]string{
				"default-src": "'self'", "script-src": "'self'", "connect-src": "'self'",
				"object-src": "'none'", "base-uri": "'none'", "form-action": "'self'",
			} {
				if got := directives[directive]; got != want {
					t.Errorf("CSP %s = %q, want %q", directive, got, want)
				}
			}
			for _, script := range regexp.MustCompile(`(?s)<script\b([^>]*)>(.*?)</script>`).FindAllStringSubmatch(html, -1) {
				if strings.TrimSpace(script[2]) != "" || !strings.Contains(script[1], `src="/static/`) {
					t.Error("workspace must load scripts from its embedded same-origin assets")
				}
			}
		})
	}
}

func TestWorkbenchLoadsEmbeddedCanvasAssets(t *testing.T) {
	handler := New(nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	html := response.Body.String()
	loaded := map[string]bool{}
	for _, tag := range regexp.MustCompile(`<(?:script|link|img)\b[^>]*>`).FindAllString(html, -1) {
		for _, reference := range regexp.MustCompile(`(?:src|href)="([^"]+)"`).FindAllStringSubmatch(tag, -1) {
			endpoint := reference[1]
			if !strings.HasPrefix(endpoint, "/static/") {
				t.Errorf("workspace asset must use /static/: %s", endpoint)
				continue
			}
			loaded[endpoint] = true
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				t.Run(method+endpoint, func(t *testing.T) {
					asset := httptest.NewRecorder()
					handler.ServeHTTP(asset, httptest.NewRequest(method, endpoint, nil))
					if asset.Code != http.StatusOK {
						t.Fatalf("asset returned HTTP %d", asset.Code)
					}
					if method == http.MethodGet && asset.Body.Len() == 0 {
						t.Error("embedded asset is empty")
					}
					if method == http.MethodHead && asset.Body.Len() != 0 {
						t.Error("HEAD response contains an asset body")
					}
					mediaType, _, err := mime.ParseMediaType(asset.Header().Get("Content-Type"))
					want := map[string]string{".js": "javascript", ".css": "text/css", ".svg": "image/svg+xml"}[path.Ext(endpoint)]
					if err != nil || want == "" || !strings.Contains(mediaType, want) {
						t.Errorf("asset Content-Type = %q for %s", asset.Header().Get("Content-Type"), endpoint)
					}
				})
			}
		}
	}
	for _, name := range []string{
		"api.js", "data.js", "graph-data.js", "canvas.js", "layout.js", "routing.js",
		"graph-view.js", "graph.js", "app.js", "style.css", "graph.css", "mark.svg",
	} {
		if !loaded["/static/"+name] {
			t.Errorf("workspace does not load %s", name)
		}
	}
	for endpoint := range loaded {
		if strings.Contains(endpoint, "/vendor/") || strings.HasSuffix(endpoint, "/model.js") {
			t.Errorf("workspace loads a replaced renderer or demo data source: %s", endpoint)
		}
	}
}
