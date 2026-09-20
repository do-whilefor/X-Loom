//go:build linux

package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"xloom/internal/agent"
	"xloom/internal/board"
	"xloom/internal/config"
	"xloom/internal/tools"
)

type modelFunc func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error)

func (f modelFunc) Generate(c context.Context, m []agent.Message, d []agent.Definition, e agent.Emit) (agent.Message, error) {
	return f(c, m, d, e)
}

func TestExpiredExplorationBudgetConcludesBeforeResumedRequest(t *testing.T) {
	j := job(t, "explore")
	runDir := t.TempDir()
	writeSession(t, runDir, j, session{RunID: j.RunID, Kind: j.Kind, StartedAt: time.Now().Add(-time.Minute), History: []agent.Message{agent.Text("user", "original task")}})
	r, err := Run(context.Background(), j, Options{RunDir: runDir, Provider: modelFunc(func(_ context.Context, m []agent.Message, d []agent.Definition, _ agent.Emit) (agent.Message, error) {
		if !strings.Contains(m[len(m)-1].Text(), "Stop exploration") {
			t.Fatal("resumed without conclusion")
		}
		if d != nil {
			t.Fatal("resumed conclusion exposed tools", d)
		}
		return agent.Text("assistant", declined), nil
	})})
	if err != nil || !r.Conclude || r.Status != "success" {
		t.Fatal(r, err)
	}
}

func TestConclusionRejectsReadWithoutOpeningFIFO(t *testing.T) {
	j := job(t, "explore")
	runDir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(j.Workspace, "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	close(stop)
	calls := 0
	set := tools.Set{Dir: j.Workspace, RunDir: runDir}
	r, err := Run(context.Background(), j, Options{RunDir: runDir, Tools: set.All(), SoftStop: stop, Provider: modelFunc(func(_ context.Context, m []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
		calls++
		if calls == 1 {
			v := toolCall("read")
			v.Content[0].Input = json.RawMessage(`{"path":"pipe"}`)
			return v, nil
		}
		results := lastToolResults(m)
		if len(results) != 1 || !results[0].IsError || !strings.Contains(string(results[0].Content), "all tools are disabled") {
			t.Fatal("conclusion tried to read a FIFO instead of refusing the tool")
		}
		return agent.Text("assistant", declined), nil
	})})
	if err != nil || r.Status != "success" || !r.Conclude || calls != 2 {
		t.Fatal(r, calls, err)
	}
}
func job(t *testing.T, kind string) Job {
	t.Helper()
	return Job{RunID: "run-test", Kind: kind, WorkerType: "go", Workspace: t.TempDir(), Budget: config.Task{Timeout: 10, ConcludeTimeout: 1, MaxIntents: 3}, Intent: &board.Intent{ID: "i1", Description: "Read confirmed evidence"}, Graph: board.Graph{Project: board.Project{ID: "p1", Status: "active"}, Facts: []board.Fact{{ID: "origin", Description: "origin"}, {ID: "goal", Description: "goal"}}, Intents: []board.Intent{{ID: "i1"}}}}
}
func toolCall(name string) agent.Message {
	return agent.Message{Role: "assistant", StopReason: "tool_use", Content: []agent.Block{{Type: "tool_use", ID: "t1", Name: name, Input: json.RawMessage(`{}`)}}}
}

const conclusion = `{"accepted":true,"data":{"description":"Confirmed evidence"}}`
const declined = `{"accepted":false,"reason":"No verified finding"}`

func TestBudgetConcludesAtSettledBoundaryInSameSession(t *testing.T) {
	j := job(t, "explore")
	now := time.Now()
	calls := 0
	executed := false
	p := modelFunc(func(ctx context.Context, m []agent.Message, defs []agent.Definition, _ agent.Emit) (agent.Message, error) {
		calls++
		if calls == 1 {
			if _, ok := ctx.Deadline(); ok {
				t.Fatal("soft budget installed a canceling context")
			}
			return toolCall("bash"), nil
		}
		if !executed || len(m) != 4 || m[2].Content[0].ToolUseID != "t1" || !strings.Contains(m[3].Text(), "Stop exploration") {
			t.Fatalf("not a settled same-session boundary: %#v", m)
		}
		if end, ok := ctx.Deadline(); !ok || time.Until(end) > time.Second {
			t.Fatal("missing independent conclusion deadline")
		}
		if defs != nil {
			t.Fatal("conclusion exposed tools", defs)
		}
		return agent.Text("assistant", conclusion), nil
	})
	r, err := Run(context.Background(), j, Options{RunDir: t.TempDir(), Now: func() time.Time { return now }, Provider: p, Tools: []agent.Tool{
		{Definition: agent.Definition{Name: "bash"}, Execute: func(ctx context.Context, _ json.RawMessage) (string, error) {
			if ctx.Err() != nil {
				t.Fatal("soft timeout canceled active tool")
			}
			executed = true
			now = now.Add(11 * time.Second)
			return "evidence", nil
		}},
		{Definition: agent.Definition{Name: "read"}, Conclude: true},
	}})
	if err != nil || r.Status != "success" || !r.Conclude || calls != 2 {
		t.Fatal(r, calls, err)
	}
}
func TestInvalidOutputRequestsConclusionAndBlocksNewExploration(t *testing.T) {
	calls := 0
	runDir := t.TempDir()
	p := modelFunc(func(_ context.Context, m []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
		calls++
		switch calls {
		case 1:
			return agent.Text("assistant", "I have some ideas"), nil
		case 2:
			return toolCall("bash"), nil
		default:
			results := lastToolResults(m)
			if len(results) != 1 || !results[0].IsError {
				t.Fatal("late exploration not rejected")
			}
			return agent.Text("assistant", declined), nil
		}
	})
	r, err := Run(context.Background(), job(t, "explore"), Options{RunDir: runDir, Provider: p, Tools: []agent.Tool{{Definition: agent.Definition{Name: "bash"}, Execute: func(context.Context, json.RawMessage) (string, error) {
		t.Fatal("executed bash during conclusion")
		return "", nil
	}}}})
	if err != nil || r.Status != "success" || !r.Conclude || calls != 3 {
		t.Fatal(r, calls, err)
	}
	var saved session
	raw, _ := os.ReadFile(filepath.Join(runDir, "session.json"))
	if err = json.Unmarshal(raw, &saved); err != nil || !saved.Concluding || saved.ConcludeStartedAt.IsZero() {
		t.Fatal(saved, err)
	}
}
func TestHardCancellationNeverStartsConclusion(t *testing.T) {
	for _, where := range []string{"model", "tool"} {
		t.Run(where, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			var out bytes.Buffer
			runDir := t.TempDir()
			p := modelFunc(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
				calls++
				if where == "model" {
					cancel()
					return agent.Message{}, context.Canceled
				}
				return toolCall("bash"), nil
			})
			r, err := Run(ctx, job(t, "explore"), Options{RunDir: runDir, Output: &out, Provider: p, Tools: []agent.Tool{{Definition: agent.Definition{Name: "bash"}, Execute: func(context.Context, json.RawMessage) (string, error) { cancel(); return "", context.Canceled }}}})
			if !errors.Is(err, context.Canceled) || calls != 1 || r.Type != "" || strings.Contains(out.String(), `"type":"result"`) {
				t.Fatal(r, calls, err, out.String())
			}
			if _, err := os.Stat(filepath.Join(runDir, "cancelled")); err != nil {
				t.Fatal("hard cancellation not persisted", err)
			}
		})
	}
}
func TestPreCancelledExecutionCannotStart(t *testing.T) {
	runDir := t.TempDir()
	os.WriteFile(filepath.Join(runDir, "cancelled"), []byte("stop"), 0600)
	_, err := Run(context.Background(), job(t, "explore"), Options{RunDir: runDir, Provider: modelFunc(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
		t.Fatal("started cancelled execution")
		return agent.Message{}, nil
	})})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func TestConclusionHasHardIndependentDeadline(t *testing.T) {
	j := job(t, "explore")
	now := time.Now()
	calls := 0
	p := modelFunc(func(ctx context.Context, _ []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
		calls++
		if calls == 1 {
			now = now.Add(20 * time.Second)
			return agent.Text("assistant", "partial"), nil
		}
		<-ctx.Done()
		return agent.Message{}, ctx.Err()
	})
	r, err := Run(context.Background(), j, Options{RunDir: t.TempDir(), Provider: p, Now: func() time.Time { return now }})
	if err != nil || r.Status != "failed" || !r.Conclude || !strings.Contains(r.Error, "deadline") || calls != 2 {
		t.Fatal(r, calls, err)
	}
}
func writeSession(t *testing.T, dir string, j Job, s session) {
	t.Helper()
	var err error
	s.SchemaVersion = sessionSchemaVersion
	s.Identity, err = identityFor(j, dir)
	if err != nil {
		t.Fatal(err)
	}
	if j.Budget.Timeout > 0 {
		s.ExecutionDeadline = s.StartedAt.Add(time.Duration(j.Budget.Timeout) * time.Second)
	}
	if s.Concluding && s.ConcludeDeadline.IsZero() {
		s.ConcludeDeadline = s.ConcludeStartedAt.Add(time.Duration(j.Budget.ConcludeTimeout) * time.Second)
	}
	journal, err := openJournal(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.file.Close()
	s.ContextCheckpoint = &agent.ContextCheckpoint{Version: agent.ContextCheckpointVersion}
	for i := range s.History {
		s.History[i].Sequence = uint64(i + 1)
		s.ContextCheckpoint.LastSequence = uint64(i + 1)
		if err := journal.append(agent.Event{Type: "message_end", Message: &s.History[i]}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.save(dir, journal); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryDoesNotReplayUncertainActionsOrDuplicatePrompt(t *testing.T) {
	j := job(t, "explore")
	runDir := t.TempDir()
	writeSession(t, runDir, j, session{RunID: j.RunID, Kind: j.Kind, StartedAt: time.Now(), History: []agent.Message{agent.Text("user", "original task"), toolCall("write")}})
	p := modelFunc(func(_ context.Context, m []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
		if len(m) != 3 || m[0].Text() != "original task" || !m[2].Content[0].IsError {
			t.Fatalf("bad recovery: %#v", m)
		}
		return agent.Text("assistant", conclusion), nil
	})
	o := Options{RunDir: runDir, Provider: p, Tools: []agent.Tool{{Definition: agent.Definition{Name: "write"}, Execute: func(context.Context, json.RawMessage) (string, error) {
		t.Fatal("replayed uncertain side effect")
		return "", nil
	}}}}
	r, err := Run(context.Background(), j, o)
	if err != nil || r.Status != "success" {
		t.Fatal(r, err)
	}
	o.Provider = modelFunc(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
		t.Fatal("replayed a completed result")
		return agent.Message{}, nil
	})
	if r, err = Run(context.Background(), j, o); err != nil || r.Status != "success" {
		t.Fatal(r, err)
	}
	j.RunID = "another-run"
	if _, err = Run(context.Background(), j, o); err == nil {
		t.Fatal("accepted another execution's session")
	}
}
func TestExpiredConclusionIsNotResetOnRecovery(t *testing.T) {
	j := job(t, "explore")
	runDir := t.TempDir()
	writeSession(t, runDir, j, session{RunID: j.RunID, Kind: j.Kind, StartedAt: time.Now().Add(-time.Hour), Concluding: true, ConcludeStartedAt: time.Now().Add(-time.Minute), History: []agent.Message{agent.Text("user", "summarize")}})
	r, err := Run(context.Background(), j, Options{RunDir: runDir, Provider: modelFunc(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
		t.Fatal("reset expired conclusion budget")
		return agent.Message{}, nil
	})})
	if err != nil || !r.Conclude || r.Status != "failed" {
		t.Fatal(r, err)
	}
}
func TestFinalContractsAndBootstrapCompletion(t *testing.T) {
	for _, tc := range []struct{ kind, text, status string }{
		{"bootstrap", `{"accepted":true,"data":{"fact":{"description":"verified"},"complete":{"description":"goal met"}}}`, "success"},
		{"reason", `{"accepted":true,"data":{}}`, "success"},
		{"reason", `{"accepted":true,"data":{"intents":"invalid"}}`, "failed"},
	} {
		t.Run(tc.kind+tc.status, func(t *testing.T) {
			r, err := Run(context.Background(), job(t, tc.kind), Options{RunDir: t.TempDir(), Provider: modelFunc(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
				return agent.Text("assistant", tc.text), nil
			})})
			if err != nil || r.Status != tc.status || r.Conclude {
				t.Fatal(r, err)
			}
		})
	}
}
