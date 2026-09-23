package docker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"xloom/internal/config"
)

func TestEnsureChecksRuntimeConfigBeforeReusingWorkspace(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		actualNetwork, wantNetwork string
		actualCaps, wantCaps       []string
		mismatch                   bool
	}{
		{name: "network changed", actualNetwork: "bridge", wantNetwork: "host", mismatch: true},
		{name: "capability added", actualNetwork: "bridge", wantNetwork: "bridge", wantCaps: []string{"NET_ADMIN"}, mismatch: true},
		{name: "capability removed", actualNetwork: "host", wantNetwork: "host", actualCaps: []string{"NET_ADMIN"}, mismatch: true},
		{name: "default network", actualNetwork: "default", wantNetwork: "bridge"},
		{name: "omitted network", actualNetwork: "bridge", wantNetwork: ""},
		{name: "capability aliases", actualNetwork: "bridge", wantNetwork: "default", actualCaps: []string{"CAP_NET_RAW", "CAP_NET_ADMIN"}, wantCaps: []string{"net_admin", "NET_RAW", "CAP_NET_ADMIN"}},
		{name: "all capabilities", actualNetwork: "host", wantNetwork: "host", actualCaps: []string{"ALL"}, wantCaps: []string{"net_raw", "all"}},
		{name: "invalid ALL alias", actualNetwork: "host", wantNetwork: "host", actualCaps: []string{"ALL"}, wantCaps: []string{"CAP_ALL"}, mismatch: true},
	} {
		for _, running := range []bool{true, false} {
			t.Run(tc.name+"/"+map[bool]string{true: "running", false: "stopped"}[running], func(t *testing.T) {
				starts := 0
				engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case r.Method == http.MethodGet && r.URL.Path == "/containers/test-dispatch-p/json":
						_ = json.NewEncoder(w).Encode(map[string]any{
							"Image":      "sha256:same",
							"Config":     map[string]any{"Labels": map[string]string{"xloom.namespace": "test", "xloom.project": "p"}},
							"State":      map[string]bool{"Running": running},
							"HostConfig": map[string]any{"NetworkMode": tc.actualNetwork, "CapAdd": tc.actualCaps},
						})
					case r.Method == http.MethodGet && r.URL.Path == "/images/worker:current/json":
						_ = json.NewEncoder(w).Encode(map[string]string{"Id": "sha256:same"})
					case r.Method == http.MethodPost && r.URL.Path == "/containers/test-dispatch-p/start":
						starts++
					default:
						t.Errorf("unexpected engine mutation/request: %s %s", r.Method, r.URL.Path)
						w.WriteHeader(500)
					}
				}))
				defer engine.Close()
				client := &Client{Config: config.Container{Namespace: "test", Image: "worker:current", Network: tc.wantNetwork, CapAdd: tc.wantCaps}, http: &http.Client{Transport: graphBridgeTestTransport{base: http.DefaultTransport, endpoint: engine.URL}}}
				name, err := client.ensure(context.Background(), "p")
				if tc.mismatch {
					if err == nil || name != "" || starts != 0 || !strings.Contains(err.Error(), "preserve and migrate its workspace") {
						t.Fatalf("changed environment accepted or workspace touched: name=%q err=%v starts=%d", name, err, starts)
					}
					return
				}
				wantStarts := 0
				if !running {
					wantStarts = 1
				}
				if err != nil || name != "test-dispatch-p" || starts != wantStarts {
					t.Fatalf("equivalent configuration rejected: name=%q err=%v starts=%d", name, err, starts)
				}
			})
		}
	}
}

func TestEnsureChecksRuntimeConfigAfterCreateRace(t *testing.T) {
	inspects := 0
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/containers/test-dispatch-p/json":
			inspects++
			if inspects == 1 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Image":      "sha256:same",
				"Config":     map[string]any{"Labels": map[string]string{"xloom.namespace": "test", "xloom.project": "p"}},
				"State":      map[string]bool{"Running": false},
				"HostConfig": map[string]any{"NetworkMode": "bridge"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/images/worker:current/json":
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": "sha256:same"})
		case r.Method == http.MethodPost && r.URL.Path == "/containers/create":
			w.WriteHeader(http.StatusConflict)
		default:
			t.Errorf("unexpected engine mutation/request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer engine.Close()
	client := &Client{Config: config.Container{Namespace: "test", Image: "worker:current", Network: "host"}, http: &http.Client{Transport: graphBridgeTestTransport{base: http.DefaultTransport, endpoint: engine.URL}}}
	if _, err := client.ensure(context.Background(), "p"); err == nil || !strings.Contains(err.Error(), "preserve and migrate its workspace") {
		t.Fatalf("raced container accepted with old configuration: %v", err)
	}
}
