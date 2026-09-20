package docker

import (
	"archive/tar"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"xloom/internal/board"
	"xloom/internal/config"
	"xloom/internal/worker"
)

func TestGraphBridgePublishesBoundedResponseAndRejectsTraversal(t *testing.T) {
	var reply worker.GraphResponse
	var move []string
	var calls int
	c := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "PUT":
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
					reply = worker.GraphResponse{}
					if !strings.HasSuffix(h.Name, ".json.tmp") {
						t.Error("response was not staged", h.Name)
					}
					json.NewDecoder(tr).Decode(&reply)
				}
			}
		case strings.HasSuffix(r.URL.Path, "/exec"):
			var input struct{ Cmd []string }
			json.NewDecoder(r.Body).Decode(&input)
			move = input.Cmd
			io.WriteString(w, `{"Id":"reply-publish"}`)
		case strings.HasSuffix(r.URL.Path, "/start"):
		case strings.HasSuffix(r.URL.Path, "/json"):
			io.WriteString(w, `{"Running":false,"ExitCode":0}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	c.SetGraphHandler(func(_ context.Context, j worker.Job, r worker.GraphRequest) (any, error) {
		calls++
		if j.RunID != "run-1" || r.Op != "read_graph" {
			t.Error(j, r)
		}
		return map[string]any{"value": "read"}, nil
	})
	bridge := c.graphBridge(context.Background(), "project-container", "/workspace/.xloom/runs/run-1", worker.Job{RunID: "run-1", Kind: "reason", GraphRPC: true})
	request := worker.GraphRequest{RequestID: strings.Repeat("a", 32), Op: "read_graph"}
	if err := bridge(request); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || reply.Error != "" || !strings.Contains(string(reply.Result), "read") || len(move) != 5 || move[0] != "/bin/mv" || !strings.HasSuffix(move[4], ".json") {
		t.Fatal(calls, reply, move)
	}
	request.RequestID = "../escape"
	if err := bridge(request); err == nil || calls != 1 {
		t.Fatal("unsafe request reached handler", calls, err)
	}
	request.RequestID = strings.Repeat("b", 32)
	request.Op = "graph_action"
	request.Action = board.StateAction{Op: "fact", IdempotencyKey: "key", Payload: json.RawMessage(`{}`)}
	if err := bridge(request); err != nil || reply.Error == "" || calls != 1 {
		t.Fatal("permission rejection not returned as tool error", reply, calls, err)
	}
	c.SetGraphHandler(func(context.Context, worker.Job, worker.GraphRequest) (any, error) {
		return strings.Repeat("x", worker.MaxGraphRPCBytes), nil
	})
	request.RequestID = strings.Repeat("c", 32)
	request.Op = "read_graph"
	if err := bridge(request); err != nil || reply.Error == "" || len(reply.Result) != 0 {
		t.Fatal("oversized result not rejected", reply, err)
	}
}

func TestGraphResultSinkHandlesFragmentsAndMissingBridge(t *testing.T) {
	request := worker.GraphRequestEvent{Type: "graph_request", Request: worker.GraphRequest{RequestID: strings.Repeat("a", 32), Op: "read_graph"}}
	raw, _ := json.Marshal(request)
	raw = append(raw, '\n')
	calls := 0
	sink := &resultSink{graph: func(r worker.GraphRequest) error {
		calls++
		if r.RequestID != request.Request.RequestID {
			t.Fatal(r)
		}
		return nil
	}}
	if _, err := sink.Write(raw[:10]); err != nil {
		t.Fatal(err)
	}
	if _, err := sink.Write(raw[10:]); err != nil || calls != 1 {
		t.Fatal(calls, err)
	}
	if _, err := (&resultSink{}).Write(raw); err == nil {
		t.Fatal("silently ignored graph request")
	}
}

func TestInfrastructureInterruptUsesResumableWorkerHelper(t *testing.T) {
	var command []string
	c := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/exec"):
			var request struct{ Cmd []string }
			json.NewDecoder(r.Body).Decode(&request)
			command = request.Cmd
			io.WriteString(w, `{"Id":"interrupt"}`)
		case strings.HasSuffix(r.URL.Path, "/start"):
		case strings.HasSuffix(r.URL.Path, "/json"):
			io.WriteString(w, `{"Running":false,"ExitCode":0}`)
		default:
			t.Errorf("unexpected container lifecycle call: %s", r.URL.Path)
		}
	})
	if err := c.interrupt("project", "/workspace/.xloom/runs/one"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(command, " ") != "/usr/local/bin/xloom worker --interrupt /workspace/.xloom/runs/one" {
		t.Fatal(command)
	}
}

func TestRunSeparatesStreamInterruptionFromHardCancellation(t *testing.T) {
	for _, mode := range []string{"stream", "shutdown", "hard"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			commands := [][]string{}
			c := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/images/xloom-test/json":
					io.WriteString(w, `{"Id":"image"}`)
				case r.URL.Path == "/containers/test-dispatch-p/json":
					io.WriteString(w, `{"Image":"image","State":{"Running":true},"Config":{"Labels":{"xloom.namespace":"test","xloom.project":"p"}}}`)
				case r.Method == "PUT":
				case strings.HasSuffix(r.URL.Path, "/exec"):
					var input struct{ Cmd []string }
					json.NewDecoder(r.Body).Decode(&input)
					commands = append(commands, input.Cmd)
					if len(commands) == 1 {
						io.WriteString(w, `{"Id":"worker"}`)
					} else {
						io.WriteString(w, `{"Id":"helper"}`)
					}
				case r.URL.Path == "/exec/worker/start":
					if mode == "shutdown" {
						cancel(worker.ErrInterrupted)
					} else if mode == "hard" {
						cancel(nil)
					}
					w.Write([]byte{1, 0, 0})
				case r.URL.Path == "/exec/helper/start":
				case strings.HasSuffix(r.URL.Path, "/json"):
					io.WriteString(w, `{"Running":false,"ExitCode":0}`)
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
				}
			})
			if _, err := c.Run(ctx, config.Worker{}, worker.Job{RunID: "run", Graph: board.Graph{Project: board.Project{ID: "p"}}}); err == nil {
				t.Fatal("expected transport interruption")
			}
			if len(commands) < 2 {
				t.Fatal("worker not stopped", commands)
			}
			if mode == "hard" {
				if len(commands) != 3 || commands[1][2] != "--cancel" {
					t.Fatal(commands)
				}
			} else if len(commands) != 2 || commands[1][2] != "--interrupt" {
				t.Fatal(commands)
			}
		})
	}
}
