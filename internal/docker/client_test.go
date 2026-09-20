package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"xloom/internal/board"
	"xloom/internal/config"
	"xloom/internal/worker"
)

func TestResultSinkHandlesFragments(t *testing.T) {
	s := &resultSink{}
	for _, p := range []string{"{\"type\":\"text_delta\",\"text\":\"x\"}\n{\"type\":", "\"result\",\"status\":\"success\",\"text\":\"ok\"}\n"} {
		if _, err := s.Write([]byte(p)); err != nil {
			t.Fatal(err)
		}
	}
	if !s.found || s.result.Text != "ok" {
		t.Fatalf("%+v", s)
	}
}

type redirectTransport struct{ target *url.URL }

func (r redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	copy := req.Clone(req.Context())
	u := *copy.URL
	u.Scheme, u.Host = r.target.Scheme, r.target.Host
	copy.URL = &u
	return http.DefaultTransport.RoundTrip(copy)
}
func mockClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return &Client{Config: config.Container{Image: "xloom-test", Network: "bridge", Namespace: "test", CompletedAction: "stop"}, http: &http.Client{Transport: redirectTransport{u}}}
}
func frame(stream byte, data string) []byte {
	h := make([]byte, 8)
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(data)))
	return append(h, []byte(data)...)
}

func TestRunArchivesJobAndDemultiplexesOutput(t *testing.T) {
	var gotJob worker.Job
	var gotCommand []string
	var launchToken string
	var created, started, archived bool
	c := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /images/xloom-test/json":
			io.WriteString(w, `{"Id":"sha256:configured-image"}`)
		case "GET /containers/test-dispatch-p/json":
			w.WriteHeader(404)
		case "POST /containers/create":
			created = true
			var request struct {
				Image, WorkingDir string
				Labels            map[string]string
			}
			_ = json.NewDecoder(r.Body).Decode(&request)
			if request.Image != "sha256:configured-image" || request.WorkingDir != "/workspace" || request.Labels["xloom.project"] != "p" {
				t.Errorf("bad create request: %+v", request)
			}
			w.WriteHeader(201)
			io.WriteString(w, `{"Id":"container"}`)
		case "POST /containers/test-dispatch-p/start":
			started = true
			w.WriteHeader(204)
		case "PUT /containers/test-dispatch-p/archive":
			archived = true
			if r.URL.Query().Get("path") != "/workspace" {
				t.Error("wrong archive root")
			}
			tr := tar.NewReader(r.Body)
			for {
				h, err := tr.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Error(err)
					break
				}
				if h.Typeflag != tar.TypeDir {
					if h.Name == ".xloom/runs/run-1/launch-token" {
						raw, _ := io.ReadAll(tr)
						launchToken = string(raw)
						continue
					}
					if h.Name != ".xloom/runs/run-1/job.json" || h.Mode != 0600 {
						t.Errorf("bad archive header: %+v", h)
					}
					_ = json.NewDecoder(tr).Decode(&gotJob)
				}
			}
		case "POST /containers/test-dispatch-p/exec":
			var request struct {
				Cmd, Env []string
				Tty      bool
			}
			_ = json.NewDecoder(r.Body).Decode(&request)
			gotCommand = request.Cmd
			if request.Tty || len(request.Env) != 2 || request.Env[0] != "TEST=value" || request.Env[1] != "XLOOM_LAUNCH_TOKEN="+launchToken || len(launchToken) != 32 {
				t.Errorf("bad exec request: %+v", request)
			}
			io.WriteString(w, `{"Id":"exec-1"}`)
		case "POST /exec/exec-1/start":
			w.Write(frame(2, "stderr diagnostic\n"))
			w.Write(frame(1, "{\"type\":\"event\"}\n"))
			w.Write(frame(1, `{"type":"result","status":"success","text":"done"}`))
		case "GET /exec/exec-1/json":
			io.WriteString(w, `{"Running":false,"ExitCode":0}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	})
	result, err := c.Run(context.Background(), config.Worker{Env: map[string]string{"TEST": "value", "XLOOM_LAUNCH_TOKEN": "untrusted-override"}}, worker.Job{RunID: "run-1", Kind: "reason", Graph: board.Graph{Project: board.Project{ID: "p"}}})
	if err != nil || result.Text != "done" {
		t.Fatalf("%+v %v", result, err)
	}
	if !created || !started || !archived || gotJob.RunID != "run-1" || strings.Join(gotCommand, " ") != "/usr/local/bin/xloom worker --job /workspace/.xloom/runs/run-1/job.json" {
		t.Fatal("incomplete worker launch")
	}
}

func TestConcurrentEnsureCreatesOneContainer(t *testing.T) {
	var mu sync.Mutex
	created, starts := false, 0
	c := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/images/xloom-test/json":
			io.WriteString(w, `{"Id":"sha256:configured-image"}`)
		case r.Method == "GET":
			if !created {
				w.WriteHeader(404)
				return
			}
			io.WriteString(w, `{"Image":"sha256:configured-image","State":{"Running":true},"Config":{"Labels":{"xloom.namespace":"test","xloom.project":"p"}}}`)
		case strings.HasSuffix(r.URL.Path, "/create"):
			if created {
				t.Error("created twice")
			}
			created = true
			w.WriteHeader(201)
		case strings.HasSuffix(r.URL.Path, "/start"):
			starts++
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.ensure(context.Background(), "p"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if starts != 1 {
		t.Fatalf("started %d times", starts)
	}
}

func TestDockerRejectsForeignContainerAndUnsafeRunPaths(t *testing.T) {
	c := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"State":{"Running":true},"Config":{"Labels":{"xloom.namespace":"other","xloom.project":"p"}}}`)
	})
	if _, err := c.ensure(context.Background(), "p"); err == nil {
		t.Fatal("used foreign container")
	}
	for _, id := range []string{"", "../other", "run/child", "a b"} {
		if _, err := c.Run(context.Background(), config.Worker{}, worker.Job{RunID: id}); err == nil {
			t.Fatalf("accepted %q", id)
		}
	}
	if err := c.archive(context.Background(), "container", "/workspace/.xloom/runs/../../etc/secret", nil); err == nil {
		t.Fatal("accepted traversal")
	}
}

func TestExecRejectsBadFramesAndExitStatus(t *testing.T) {
	for name, data := range map[string][]byte{"truncated": {1, 0, 0}, "oversized": {1, 0, 0, 0, 127, 255, 255, 255}, "exit": frame(1, "ok")} {
		t.Run(name, func(t *testing.T) {
			c := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/exec") {
					io.WriteString(w, `{"Id":"e"}`)
				} else if strings.HasSuffix(r.URL.Path, "/start") {
					w.Write(data)
				} else {
					io.WriteString(w, `{"Running":false,"ExitCode":7}`)
				}
			})
			if _, err := c.exec(context.Background(), "p", []string{"test"}, nil, io.Discard); err == nil {
				t.Fatal("accepted invalid exec")
			}
		})
	}
}

func TestCleanupAndProjectEnumeration(t *testing.T) {
	var requests []string
	c := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		if r.URL.Path == "/containers/json" {
			if !strings.Contains(r.URL.Query().Get("filters"), "xloom.namespace=test") {
				t.Error("missing namespace filter")
			}
			io.WriteString(w, `[{"Labels":{"xloom.project":"b"}},{"Labels":{"xloom.project":"a"}},{"Labels":{}}]`)
			return
		}
		if r.Method == "GET" && r.URL.Path == "/containers/test-dispatch-p/json" {
			io.WriteString(w, `{"Id":"verified-container","Config":{"Labels":{"xloom.namespace":"test","xloom.project":"p"}}}`)
			return
		}
		w.WriteHeader(404)
	})
	ids, err := c.Projects(context.Background())
	if err != nil || strings.Join(ids, ",") != "a,b" {
		t.Fatalf("%v %v", ids, err)
	}
	if err = c.Cleanup(context.Background(), "p", "stopped"); err != nil {
		t.Fatal(err)
	}
	if err = c.Cleanup(context.Background(), "p", "deleted"); err != nil {
		t.Fatal(err)
	}
	c.Config.CompletedAction = "remove"
	if err = c.Cleanup(context.Background(), "p", "completed"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(requests, ";") != "GET /containers/json;GET /containers/test-dispatch-p/json;POST /containers/verified-container/stop;GET /containers/test-dispatch-p/json;DELETE /containers/verified-container;GET /containers/test-dispatch-p/json;DELETE /containers/verified-container" {
		t.Fatal(requests)
	}
}

func TestCleanupRejectsForeignOrUnidentifiedContainers(t *testing.T) {
	for name, response := range map[string]string{
		"foreign namespace": `{"Id":"foreign","Config":{"Labels":{"xloom.namespace":"other","xloom.project":"p"}}}`,
		"foreign project":   `{"Id":"foreign","Config":{"Labels":{"xloom.namespace":"test","xloom.project":"other"}}}`,
		"missing labels":    `{"Id":"foreign","Config":{"Labels":{}}}`,
		"missing ID":        `{"Config":{"Labels":{"xloom.namespace":"test","xloom.project":"p"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			mutated := false
			c := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					mutated = true
					w.WriteHeader(204)
					return
				}
				io.WriteString(w, response)
			})
			for _, state := range []string{"stopped", "deleted", "completed"} {
				if err := c.Cleanup(context.Background(), "p", state); err == nil {
					t.Fatal("accepted unverified cleanup")
				}
			}
			if mutated {
				t.Fatal("cleanup mutated a foreign or unidentified container")
			}
		})
	}
}

func TestCleanupUsesVerifiedIDAfterNameReplacement(t *testing.T) {
	for _, state := range []string{"stopped", "deleted"} {
		t.Run(state, func(t *testing.T) {
			inspected, mutated := false, false
			c := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					if inspected {
						t.Error("cleanup unexpectedly re-resolved the replaced name")
					}
					inspected = true
					io.WriteString(w, `{"Id":"original-id","Config":{"Labels":{"xloom.namespace":"test","xloom.project":"p"}}}`)
					// A foreign container now owns test-dispatch-p. The original
					// container has disappeared, so its stable ID returns 404.
					return
				}
				mutated = true
				if !inspected || (r.URL.Path != "/containers/original-id" && r.URL.Path != "/containers/original-id/stop") {
					t.Errorf("targeted replacement: %s", r.URL.Path)
				}
				w.WriteHeader(404)
			})
			if err := c.Cleanup(context.Background(), "p", state); err != nil {
				t.Fatal(err)
			}
			if !inspected || !mutated {
				t.Fatal("cleanup did not inspect and target the original ID")
			}
		})
	}
}

func TestCleanupMissingContainerIsIdempotent(t *testing.T) {
	c := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Error("cleanup mutated after inspect reported missing")
		}
		w.WriteHeader(404)
	})
	if err := c.Cleanup(context.Background(), "p", "deleted"); err != nil {
		t.Fatal(err)
	}
}

func TestResultSinkRejectsDuplicateAndOversizedEvents(t *testing.T) {
	s := &resultSink{}
	if _, err := s.Write([]byte("{\"type\":\"result\"}\n{\"type\":\"result\"}\n")); err == nil {
		t.Fatal("accepted duplicate results")
	}
	s = &resultSink{}
	if _, err := s.Write(bytes.Repeat([]byte{'x'}, (32<<20)+1)); err == nil {
		t.Fatal("accepted oversized event")
	}
}

func TestCancelUsesOnlyTheRequestedExecutionHelper(t *testing.T) {
	var commands [][]string
	c := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/exec"):
			var request struct{ Cmd []string }
			_ = json.NewDecoder(r.Body).Decode(&request)
			commands = append(commands, request.Cmd)
			io.WriteString(w, `{"Id":"cancel"}`)
		case strings.HasSuffix(r.URL.Path, "/start"):
		case strings.HasSuffix(r.URL.Path, "/json"):
			io.WriteString(w, `{"Running":false,"ExitCode":0}`)
		default:
			t.Errorf("cancellation touched container lifecycle: %s %s", r.Method, r.URL.Path)
		}
	})
	c.cancel("test-dispatch-p", "/workspace/.xloom/runs/run-1")
	if len(commands) != 2 {
		t.Fatalf("commands=%v", commands)
	}
	if strings.Join(commands[0], " ") != "/usr/local/bin/xloom worker --cancel /workspace/.xloom/runs/run-1" || strings.Join(commands[1], " ") != "/usr/local/bin/xloom worker --cancel /workspace/.xloom/runs/run-1 --force" {
		t.Fatal(commands)
	}
}

func TestEnsureRechecksOwnershipAfterCreateConflict(t *testing.T) {
	gets := 0
	c := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/images/xloom-test/json" {
			io.WriteString(w, `{"Id":"sha256:configured-image"}`)
			return
		}
		if r.Method == "GET" {
			gets++
			if gets == 1 {
				w.WriteHeader(404)
			} else {
				io.WriteString(w, `{"Config":{"Labels":{"xloom.namespace":"foreign"}}}`)
			}
			return
		}
		if strings.HasSuffix(r.URL.Path, "/create") {
			w.WriteHeader(409)
			return
		}
		t.Error("started foreign container")
	})
	if _, err := c.ensure(context.Background(), "p"); err == nil {
		t.Fatal("accepted conflict without ownership verification")
	}
}

func TestRunRejectsSavedContainerFromPreviousImage(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(fmt.Sprintf("running=%t", running), func(t *testing.T) {
			c := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Errorf("image mismatch caused a mutation: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(500)
					return
				}
				switch r.URL.Path {
				case "/containers/test-dispatch-p/json":
					// Config.Image deliberately remains the same after a :dev rebuild.
					fmt.Fprintf(w, `{"Image":"sha256:old","State":{"Running":%t},"Config":{"Image":"xloom-test","Labels":{"xloom.namespace":"test","xloom.project":"p"}}}`, running)
				case "/images/xloom-test/json":
					io.WriteString(w, `{"Id":"sha256:new"}`)
				default:
					t.Errorf("unexpected image mismatch request: %s", r.URL.Path)
					w.WriteHeader(500)
				}
			})
			_, err := c.Run(context.Background(), config.Worker{}, worker.Job{RunID: "new-run", Graph: board.Graph{Project: board.Project{ID: "p"}}})
			if err == nil || !strings.Contains(err.Error(), "sha256:old") || !strings.Contains(err.Error(), "sha256:new") || !strings.Contains(err.Error(), "migrate") {
				t.Fatalf("expected actionable image mismatch: %v", err)
			}
		})
	}
}

func TestEnsureAllowsDifferentReferencesToSameImage(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(fmt.Sprintf("running=%t", running), func(t *testing.T) {
			starts := 0
			c := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method + " " + r.URL.Path {
				case "GET /containers/test-dispatch-p/json":
					fmt.Fprintf(w, `{"Image":"sha256:same","State":{"Running":%t},"Config":{"Image":"previous-alias","Labels":{"xloom.namespace":"test","xloom.project":"p"}}}`, running)
				case "GET /images/xloom-test/json":
					io.WriteString(w, `{"Id":"sha256:same"}`)
				case "POST /containers/test-dispatch-p/start":
					starts++
					w.WriteHeader(204)
				default:
					t.Errorf("unexpected compatible container request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(500)
				}
			})
			if _, err := c.ensure(context.Background(), "p"); err != nil {
				t.Fatal(err)
			}
			if running && starts != 0 || !running && starts != 1 {
				t.Fatalf("running=%t starts=%d", running, starts)
			}
		})
	}
}

func TestEnsureRequiresLocallyResolvedImage(t *testing.T) {
	for _, code := range []int{200, 404, 500} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			c := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Errorf("unresolved image caused a mutation: %s %s", r.Method, r.URL.Path)
					return
				}
				if r.URL.Path == "/images/xloom-test/json" {
					w.WriteHeader(code)
					io.WriteString(w, `{}`)
					return
				}
				io.WriteString(w, `{"Image":"sha256:old","State":{"Running":false},"Config":{"Labels":{"xloom.namespace":"test","xloom.project":"p"}}}`)
			})
			if _, err := c.ensure(context.Background(), "p"); err == nil || !strings.Contains(err.Error(), "configured worker image") {
				t.Fatalf("expected a configured-image error: %v", err)
			}
		})
	}
}

func TestEnsureRechecksImageAfterCreateConflict(t *testing.T) {
	gets := 0
	c := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /images/xloom-test/json":
			io.WriteString(w, `{"Id":"sha256:new"}`)
		case "GET /containers/test-dispatch-p/json":
			gets++
			if gets == 1 {
				w.WriteHeader(404)
			} else {
				io.WriteString(w, `{"Image":"sha256:old","Config":{"Labels":{"xloom.namespace":"test","xloom.project":"p"}}}`)
			}
		case "POST /containers/create":
			var input struct{ Image string }
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Image != "sha256:new" {
				t.Errorf("create must pin the resolved image: %+v %v", input, err)
			}
			w.WriteHeader(409)
		default:
			t.Errorf("image conflict caused an unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	})
	if _, err := c.ensure(context.Background(), "p"); err == nil || !strings.Contains(err.Error(), "migrate") {
		t.Fatalf("expected conflicting image rejection: %v", err)
	}
}
