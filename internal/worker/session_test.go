//go:build linux

package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"xloom/internal/agent"
	"xloom/internal/provider"
)

func neverModel(t *testing.T) modelFunc {
	t.Helper()
	return func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
		t.Fatal("unexpected model request")
		return agent.Message{}, nil
	}
}

func TestSessionRejectsChangedImmutableIdentityBeforeReturningCachedResult(t *testing.T) {
	for _, field := range []string{"project", "step", "workspace", "run_directory", "run", "previous_run", "budget", "graph", "graph_rpc", "worker"} {
		t.Run(field, func(t *testing.T) {
			j := job(t, "explore")
			dir := t.TempDir()
			writeSession(t, dir, j, session{RunID: j.RunID, Kind: j.Kind, StartedAt: time.Now(), Result: &Result{Type: "result", Status: "success", Text: declined}})
			switch field {
			case "project":
				j.Graph.Project.ID = "another"
			case "step":
				j.Intent.ID = "another"
			case "workspace":
				j.Workspace = t.TempDir()
			case "run_directory":
				other := t.TempDir()
				for _, name := range []string{"session.json", "events.jsonl"} {
					data, _ := os.ReadFile(filepath.Join(dir, name))
					if err := os.WriteFile(filepath.Join(other, name), data, 0600); err != nil {
						t.Fatal(err)
					}
				}
				dir = other
			case "run":
				j.RunID = "another"
			case "previous_run":
				j.PreviousRunID = "parent"
			case "budget":
				j.Budget.ConcludeTimeout++
			case "graph":
				j.Graph.Facts[0].Description = "changed immutable input"
			case "graph_rpc":
				j.GraphRPC = true
			case "worker":
				j.WorkerType = "mock"
			}
			before, _ := os.ReadFile(filepath.Join(dir, "session.json"))
			_, err := Run(context.Background(), j, Options{RunDir: dir, Provider: neverModel(t)})
			if err == nil || !strings.Contains(err.Error(), "identity") {
				t.Fatal(err)
			}
			after, _ := os.ReadFile(filepath.Join(dir, "session.json"))
			if !bytes.Equal(before, after) {
				t.Fatal("rejection changed the original session")
			}
		})
	}
}

func TestUnboundAndFutureSessionSchemasAreRejectedWithoutMigration(t *testing.T) {
	for _, version := range []int{0, sessionSchemaVersion + 1} {
		j := job(t, "reason")
		dir := t.TempDir()
		writeSession(t, dir, j, session{RunID: j.RunID, Kind: j.Kind, StartedAt: time.Now()})
		s := loadSession(t, dir)
		s.SchemaVersion = version
		raw, _ := json.Marshal(s)
		os.WriteFile(filepath.Join(dir, "session.json"), raw, 0600)
		_, err := Run(context.Background(), j, Options{RunDir: dir, Provider: neverModel(t)})
		if err == nil || !strings.Contains(err.Error(), "schema_version") {
			t.Fatal(err)
		}
		after, _ := os.ReadFile(filepath.Join(dir, "session.json"))
		if !bytes.Equal(raw, after) {
			t.Fatal("unbound session migrated silently")
		}
	}
}

func TestSameRunInfrastructureRecoveryIsBoundedAndRetainsDeadline(t *testing.T) {
	j := job(t, "reason")
	dir := t.TempDir()
	calls := 0
	o := Options{RunDir: dir, Provider: modelFunc(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
		calls++
		return agent.Message{}, io.ErrUnexpectedEOF
	})}
	var original session
	for attempt := 0; attempt < 4; attempt++ {
		r, err := Run(context.Background(), j, o)
		if err != nil || r.Status != "failed" || r.Retryable != (attempt < 2) {
			t.Fatal(attempt, r, err)
		}
		s := loadSession(t, dir)
		if attempt == 0 {
			original = s
		}
		if !s.StartedAt.Equal(original.StartedAt) || !s.ReasonDeadline.Equal(original.ReasonDeadline) || s.RepairCount != 0 {
			t.Fatal("recovery refreshed budget or JSON repair", s)
		}
		if attempt >= 2 && (s.RecoveryCount != 2 || r.FailureKind != "recovery_exhausted") {
			t.Fatal(s, r)
		}
	}
	if calls != 3 {
		t.Fatal("recovery allowance bypassed", calls)
	}
}

func TestTransportRecoveryDuringRepairDoesNotSpendAnotherFormatAttempt(t *testing.T) {
	j := job(t, "reason")
	dir := t.TempDir()
	calls := 0
	o := Options{RunDir: dir, Provider: modelFunc(func(_ context.Context, _ []agent.Message, d []agent.Definition, _ agent.Emit) (agent.Message, error) {
		calls++
		if calls == 1 {
			return agent.Text("assistant", "invalid"), nil
		}
		if d != nil {
			t.Fatal("repair reopened tools")
		}
		if calls == 2 {
			return agent.Message{}, io.ErrUnexpectedEOF
		}
		return agent.Text("assistant", declined), nil
	})}
	r, err := Run(context.Background(), j, o)
	if err != nil || !r.Retryable {
		t.Fatal(r, err)
	}
	before := loadSession(t, dir)
	if before.RepairCount != 1 || !before.RepairPending {
		t.Fatal(before)
	}
	r, err = Run(context.Background(), j, o)
	after := loadSession(t, dir)
	if err != nil || r.Status != "success" || after.RepairCount != 1 || after.RecoveryCount != 1 || !before.ReasonDeadline.Equal(after.ReasonDeadline) || calls != 3 {
		t.Fatal(r, after, calls, err)
	}
}

func TestFailureClassificationKeepsBusinessAndBudgetsTerminal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure error
		retry   bool
	}{
		{"timeout", context.DeadlineExceeded, true},
		{"transport", io.ErrUnexpectedEOF, true},
		{"rate", &provider.HTTPError{Status: 429}, true},
		{"unavailable", &provider.HTTPError{Status: 503}, true},
		{"invalid", &provider.HTTPError{Status: 422}, false},
		{"auth", &provider.HTTPError{Status: 401}, false},
		{"protocol", errors.New("invalid model stream"), false},
		{"overflow", &agent.ModelError{Kind: agent.ErrorBudget, Err: errors.New("cannot compact")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, retry := classifyFailure(tc.failure, context.Background())
			if retry != tc.retry {
				t.Fatal(retry)
			}
		})
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if kind, retry := classifyFailure(io.ErrUnexpectedEOF, ctx); retry || kind != "budget_exhausted" {
		t.Fatal(kind, retry)
	}
	j := job(t, "reason")
	dir := t.TempDir()
	r, err := Run(context.Background(), j, Options{RunDir: dir, Provider: modelFunc(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
		return agent.Text("assistant", declined), nil
	})})
	if err != nil || r.Status != "success" || r.Retryable || r.FailureKind != "" {
		t.Fatal(r, err)
	}
}

func TestPartialEventTailIsArchivedAndCompleteUncommittedRecordsSurvive(t *testing.T) {
	j := job(t, "reason")
	dir := t.TempDir()
	writeSession(t, dir, j, session{RunID: j.RunID, Kind: j.Kind, StartedAt: time.Now(), History: []agent.Message{agent.Text("user", "original")}})
	original, _ := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	m := agent.Text("assistant", "uncommitted model response")
	m.Sequence = 80
	record, _ := json.Marshal(agent.Event{Type: "message_end", Message: &m})
	record = append(record, '\n')
	prepared, _ := json.Marshal(agent.Event{Type: "context_compaction_prepared", Compaction: &agent.CompactionRecord{Version: 1, ID: 7, Status: "prepared", Summary: "uncommitted candidate", View: []agent.Message{agent.Text("user", "uncommitted summary")}}})
	prepared = append(prepared, '\n')
	partial := []byte(`{"type":"text_delta","text":"incomplete`)
	f, _ := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_APPEND|os.O_WRONLY, 0600)
	f.Write(record)
	f.Write(prepared)
	f.Write(partial)
	f.Close()
	r, err := Run(context.Background(), j, Options{RunDir: dir, Provider: modelFunc(func(_ context.Context, m []agent.Message, _ []agent.Definition, _ agent.Emit) (agent.Message, error) {
		if len(m) != 1 || m[0].Text() != "original" {
			t.Fatal("uncommitted response entered request view", m)
		}
		return agent.Text("assistant", declined), nil
	})})
	if err != nil || r.Status != "success" {
		t.Fatal(r, err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if !bytes.HasPrefix(data, append(original, record...)) {
		t.Fatal("complete raw history was changed")
	}
	archives, _ := filepath.Glob(filepath.Join(dir, "events-incomplete-*.jsonl"))
	if len(archives) != 1 {
		t.Fatal(archives)
	}
	archived, _ := os.ReadFile(archives[0])
	if !bytes.Equal(partial, archived) {
		t.Fatal("partial evidence lost")
	}
	s := loadSession(t, dir)
	if s.ContextCheckpoint.LastSequence <= 80 {
		t.Fatal("raw message sequences reused")
	}
	if s.ContextCheckpoint.CompactionCount != 7 || s.ContextCheckpoint.LastCompaction != nil || !bytes.Contains(data, prepared) {
		t.Fatal("prepared compaction was lost, applied, or its ID reused")
	}
	if s.Log.Offset != int64(len(data)) {
		t.Fatal("result was not checkpointed")
	}
	if _, err := Run(context.Background(), j, Options{RunDir: dir, Provider: neverModel(t)}); err != nil {
		t.Fatal(err)
	}
}

func TestCommittedEventDamageCannotBeRepairedSilently(t *testing.T) {
	for _, damage := range []string{"checksum", "short", "boundary", "invalid"} {
		t.Run(damage, func(t *testing.T) {
			j := job(t, "reason")
			dir := t.TempDir()
			writeSession(t, dir, j, session{RunID: j.RunID, Kind: j.Kind, StartedAt: time.Now(), History: []agent.Message{agent.Text("user", "original")}})
			path := filepath.Join(dir, "events.jsonl")
			raw, _ := os.ReadFile(path)
			switch damage {
			case "checksum":
				raw = bytes.Replace(raw, []byte("original"), []byte("tampered"), 1)
			case "short":
				raw = raw[:len(raw)-1]
			case "invalid":
				raw[0] = '!'
			case "boundary":
				s := loadSession(t, dir)
				s.Log.Offset--
				b, _ := json.Marshal(s)
				os.WriteFile(filepath.Join(dir, "session.json"), b, 0600)
			}
			os.WriteFile(path, raw, 0600)
			if _, err := Run(context.Background(), j, Options{RunDir: dir, Provider: neverModel(t)}); err == nil {
				t.Fatal("accepted committed corruption")
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(raw, after) {
				t.Fatal("changed corrupt committed log")
			}
		})
	}
}

func TestDispatcherInterruptionPreservesResumability(t *testing.T) {
	j := job(t, "reason")
	dir := t.TempDir()
	ctx, cancel := context.WithCancelCause(context.Background())
	_, err := Run(ctx, j, Options{RunDir: dir, Provider: modelFunc(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
		cancel(ErrInterrupted)
		return agent.Message{}, context.Canceled
	})})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "cancelled")); !os.IsNotExist(err) {
		t.Fatal("dispatcher interruption persisted hard cancel", err)
	}
	r, err := Run(context.Background(), j, Options{RunDir: dir, Provider: modelFunc(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
		return agent.Text("assistant", declined), nil
	})})
	if err != nil || r.Status != "success" || loadSession(t, dir).RecoveryCount != 1 {
		t.Fatal(r, err)
	}
}

func TestStaleDockerLaunchCannotLoadOrStartSession(t *testing.T) {
	j := job(t, "reason")
	dir := t.TempDir()
	t.Setenv("XLOOM_LAUNCH_TOKEN", strings.Repeat("a", 32))
	if err := os.WriteFile(filepath.Join(dir, "launch-token"), []byte(strings.Repeat("b", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), j, Options{RunDir: dir, Provider: neverModel(t)}); err == nil || !strings.Contains(err.Error(), "superseded") {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "session.json")); !os.IsNotExist(err) {
		t.Fatal("late launch created session", err)
	}
}
