//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"xloom/internal/agent"
)

type graphWriter func([]byte) (int, error)

func (f graphWriter) Write(p []byte) (int, error) { return f(p) }

func TestDecideReceivesOnlyGraphToolsAndInlineConstraints(t *testing.T) {
	j := job(t, "reason")
	dir := t.TempDir()
	calls := 0
	r, err := Run(context.Background(), j, Options{RunDir: dir, Tools: []agent.Tool{noExecutionTool(t, "bash")}, Provider: modelFunc(func(_ context.Context, m []agent.Message, d []agent.Definition, _ agent.Emit) (agent.Message, error) {
		calls++
		if len(d) != 2 || d[0].Name != "read_graph" || d[1].Name != "graph_action" {
			t.Fatal("Decide exposed environment capabilities", d)
		}
		if calls == 1 {
			if strings.Contains(m[0].Text(), "graph.yaml") || !strings.Contains(m[0].Text(), "<task_graph>") {
				t.Fatal("Decide requires file access", m[0].Text())
			}
			request := toolCall("read_graph")
			request.Content[0].Input = json.RawMessage(`{"section":"facts","limit":1}`)
			return request, nil
		}
		result := lastToolResults(m)
		if len(result) != 1 || result[0].IsError || !strings.Contains(string(result[0].Content), "next_offset") {
			t.Fatal("cannot page snapshot graph", result)
		}
		return agent.Text("assistant", declined), nil
	})})
	if err != nil || r.Status != "success" || calls != 2 {
		t.Fatal(r, calls, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "graph.yaml")); !os.IsNotExist(err) {
		t.Fatal("Decide wrote graph file", err)
	}
}

func TestGraphBridgeIntentIsDurableAndResultReturnsToSameSession(t *testing.T) {
	j := job(t, "reason")
	j.GraphRPC = true
	dir := t.TempDir()
	calls := 0
	requests := 0
	out := graphWriter(func(raw []byte) (int, error) {
		var event GraphRequestEvent
		if err := json.Unmarshal(raw, &event); err != nil {
			return 0, err
		}
		if event.Type != "graph_request" {
			return len(raw), nil
		}
		requests++
		if event.Request.Action.IdempotencyKey != j.RunID+":add-one" {
			t.Fatal("missing run-scoped idempotency key", event.Request)
		}
		saved := loadSession(t, dir)
		last := saved.History[len(saved.History)-1]
		if last.Role != "assistant" || last.Content[0].Name != "graph_action" {
			t.Fatal("graph mutation preceded durable tool intent")
		}
		reply, _ := json.Marshal(GraphResponse{RequestID: event.Request.RequestID, Result: json.RawMessage(`{"id":"i2"}`)})
		if err := os.WriteFile(filepath.Join(dir, "graph-response-"+event.Request.RequestID+".json"), reply, 0600); err != nil {
			return 0, err
		}
		return len(raw), nil
	})
	r, err := Run(context.Background(), j, Options{RunDir: dir, Output: out, Provider: modelFunc(func(_ context.Context, m []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
		calls++
		if calls == 1 {
			request := toolCall("graph_action")
			request.Content[0].Input = json.RawMessage(`{"op":"step","idempotency_key":"add-one","payload":{"action":"add","from":["origin"],"description":"verify"}}`)
			return request, nil
		}
		result := lastToolResults(m)
		if len(result) != 1 || result[0].IsError || !strings.Contains(string(result[0].Content), "i2") {
			t.Fatal(result)
		}
		return agent.Text("assistant", declined), nil
	})})
	if err != nil || r.Status != "success" || requests != 1 {
		t.Fatal(r, requests, err)
	}
}

func TestRuntimeGraphActionPermissionAndUnavailableBridge(t *testing.T) {
	for _, kind := range []string{"reason", "explore", "bootstrap"} {
		j := job(t, kind)
		var emitted bool
		o := Options{RunDir: t.TempDir(), Output: graphWriter(func(p []byte) (int, error) { emitted = true; return len(p), nil })}
		if err := ConfigureRuntimeTools(j, &o); err != nil {
			t.Fatal(err)
		}
		var action agent.Tool
		for _, tool := range o.Tools {
			if tool.Name == "graph_action" {
				action = tool
			}
		}
		for _, op := range []string{"goal", "step", "fact_relation", "fact", "finding"} {
			raw, _ := json.Marshal(map[string]any{"op": op, "idempotency_key": "key", "payload": map[string]any{}})
			_, err := action.Execute(context.Background(), raw)
			if err == nil {
				t.Fatal("mutation without dispatcher", kind, op)
			}
			allowed := (kind == "reason" && (op == "goal" || op == "step" || op == "fact_relation")) || (kind != "reason" && (op == "fact" || op == "finding"))
			if !allowed && !strings.Contains(err.Error(), "not allowed") {
				t.Fatal(kind, op, err)
			}
		}
		if emitted {
			t.Fatal("invalid graph request reached output")
		}
	}
}

func TestGraphReplyRejectsSpecialFilesWrongIdentityAndOversize(t *testing.T) {
	id := strings.Repeat("a", 32)
	for _, kind := range []string{"symlink", "fifo", "oversize", "wrong_id", "partial"} {
		t.Run(kind, func(t *testing.T) {
			name := filepath.Join(t.TempDir(), "response")
			switch kind {
			case "symlink":
				target := filepath.Join(t.TempDir(), "target")
				os.WriteFile(target, []byte(`{}`), 0600)
				os.Symlink(target, name)
			case "fifo":
				syscall.Mkfifo(name, 0600)
			case "oversize":
				os.WriteFile(name, []byte(strings.Repeat("x", MaxGraphRPCBytes+1)), 0600)
			case "wrong_id":
				os.WriteFile(name, []byte(`{"request_id":"other","result":{}}`), 0600)
			case "partial":
				os.WriteFile(name, []byte(`{"request_id":`), 0600)
			}
			if _, err := readGraphResponse(name, id); err == nil {
				t.Fatal("accepted bad reply")
			}
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if _, err := graphRPC(ctx, t.TempDir(), io.Discard, GraphRequest{RequestID: id, Op: "read_graph"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("bridge did not respect cancellation", err)
	}
}
