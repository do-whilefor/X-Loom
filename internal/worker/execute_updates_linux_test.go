//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"xloom/internal/agent"
	"xloom/internal/board"
	"xloom/internal/config"
)

var errUpdateSessionSave = errors.New("injected session save failure")

// Exercise the real journal and atomic session file. The failure switch acts
// after Loop has appended message_end, at the same save boundary as Run.
type updateSessionFixture struct {
	t        *testing.T
	dir      string
	job      Job
	state    *session
	loop     *agent.Loop
	journal  *eventJournal
	failSave bool
	logErr   error
}

func newUpdateSessionFixture(t *testing.T) *updateSessionFixture {
	t.Helper()
	f := &updateSessionFixture{t: t, dir: t.TempDir()}
	f.job = Job{RunID: "update-run", Kind: "explore", GraphRPC: true, Workspace: f.dir,
		Graph: board.Graph{Project: board.Project{ID: "project", Generation: 2}}, Intent: &board.Intent{ID: "step", From: []string{"F1"}},
		InputSnapshot: &board.InputSnapshot{ProjectID: "project", Generation: 2, Revision: 5}, Budget: config.Task{Timeout: 90, ConcludeTimeout: 30}}
	id, err := identityFor(f.job, f.dir)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	f.state = &session{SchemaVersion: sessionSchemaVersion, Identity: id, RunID: f.job.RunID, Kind: f.job.Kind,
		StartedAt: started, ExecutionDeadline: started.Add(90 * time.Second), RecoveryCount: 1,
		ContextCheckpoint: &agent.ContextCheckpoint{Version: agent.ContextCheckpointVersion}}
	f.journal, err = openJournal(f.dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.journal.file.Close() })
	f.bindLoop()
	if err := f.loop.AppendInstruction("Immutable original task"); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *updateSessionFixture) bindLoop() {
	f.loop = &agent.Loop{History: f.state.History, Checkpoint: f.state.ContextCheckpoint, ContextData: f.state.ExecuteUpdates.contextData(), Emit: func(event agent.Event) {
		if f.logErr == nil {
			f.logErr = f.journal.append(event)
		}
	}}
	f.loop.SaveState = func(history []agent.Message, _ *agent.ContextCheckpoint) error { return f.save(history) }
}

func (f *updateSessionFixture) save(history []agent.Message) error {
	if f.logErr != nil {
		return f.logErr
	}
	if f.failSave {
		return errUpdateSessionSave
	}
	before := *f.state
	f.state.History, f.state.ContextCheckpoint = history, f.loop.Checkpoint
	if err := f.state.save(f.dir, f.journal); err != nil {
		*f.state = before
		return err
	}
	return nil
}

func (f *updateSessionFixture) diskState() *session {
	f.t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.dir, "session.json"))
	if err != nil {
		f.t.Fatal(err)
	}
	var saved session
	if err := json.Unmarshal(raw, &saved); err != nil {
		f.t.Fatal(err)
	}
	if err := saved.validate(f.state.Identity); err != nil {
		f.t.Fatal(err)
	}
	if err := validateExecuteUpdateState(f.job, &saved); err != nil {
		f.t.Fatal(err)
	}
	return &saved
}

func (f *updateSessionFixture) reopen() {
	f.t.Helper()
	saved := f.diskState()
	if err := f.journal.file.Close(); err != nil {
		f.t.Fatal(err)
	}
	var err error
	f.journal, err = openJournal(f.dir, &saved.Log)
	if err != nil {
		f.t.Fatal(err)
	}
	// Match Run's sequence reservation: uncommitted events remain auditable,
	// but their messages and cursors are not restored as committed context.
	if saved.ContextCheckpoint.LastSequence < f.journal.lastSequence {
		saved.ContextCheckpoint.LastSequence = f.journal.lastSequence
	}
	f.state, f.failSave, f.logErr = saved, false, nil
	f.bindLoop()
}

func (f *updateSessionFixture) refresh(request func(context.Context, GraphRequest) (string, error)) error {
	return refreshExecutionUpdates(context.Background(), f.job, request, f.state, f.loop, f.save)
}

func (f *updateSessionFixture) response(from, through int64, pending string) board.ExecuteUpdates {
	return board.ExecuteUpdates{Version: 1, ExecuteUpdateCursor: board.ExecuteUpdateCursor{ProjectID: f.job.Graph.Project.ID, Generation: 2, StepID: "step", RunID: f.job.RunID, Revision: from},
		FromRevision: from, ToRevision: through, StateVersion: strings.Repeat("a", 64), Complete: pending == "", PendingReason: pending,
		Facts:     []board.FactRecord{{ID: "F1", Status: "refuted"}, {ID: "F2", Status: "valid", Evidence: []board.EvidenceRef{{RunID: "producer", Path: "evidence.txt", Excerpt: "Exact correction evidence"}}}},
		Relations: []board.FactRelation{{Kind: "refutes", Target: "F1", Source: "F2", Reason: "Session identity changed"}}, InvalidSources: []string{"F1"}, Omitted: map[string]int{}, ReadMore: "read_graph facts/relations"}
}

func updateResponseReader(t *testing.T, response board.ExecuteUpdates, wantRevision int64) func(context.Context, GraphRequest) (string, error) {
	t.Helper()
	return func(_ context.Context, request GraphRequest) (string, error) {
		if request.Op != "read_updates" || request.Updates == nil || request.Updates.Revision != wantRevision {
			t.Fatalf("request skipped original/saved cursor: %+v", request)
		}
		raw, err := json.Marshal(response)
		return string(raw), err
	}
}

func updateNoticeCount(history []agent.Message) int {
	count := 0
	for _, message := range history {
		if strings.HasPrefix(message.Text(), executeUpdateNotice) {
			count++
		}
	}
	return count
}

func TestExecuteUpdateCheckpointRecoversAfterJournalBeforeSessionSave(t *testing.T) {
	f := newUpdateSessionFixture(t)
	before := f.diskState()
	f.failSave = true
	read := updateResponseReader(t, f.response(5, 8, ""), 5)
	if err := f.refresh(read); !errors.Is(err, errUpdateSessionSave) {
		t.Fatalf("did not fail between journal and session: %v", err)
	}
	saved := f.diskState()
	if saved.ExecuteUpdates != nil || updateNoticeCount(saved.History) != 0 || f.state.ExecuteUpdates != nil || len(f.loop.ContextData) != 0 || saved.Log != before.Log {
		t.Fatal("failed save committed a cursor without its notice")
	}
	if f.journal.offset <= saved.Log.Offset {
		t.Fatal("failure did not leave the intended uncommitted journal message")
	}
	f.reopen()
	if f.journal.uncommitted <= 0 || f.loop.Checkpoint.LastSequence != 2 {
		t.Fatal("recovery lost uncommitted provenance or reused its sequence")
	}
	if err := f.refresh(read); err != nil {
		t.Fatal(err)
	}
	saved = f.diskState()
	if saved.ExecuteUpdates.Cursor.Revision != 8 || saved.ExecuteUpdates.NoticeSequence != 3 || updateNoticeCount(saved.History) != 1 {
		t.Fatalf("recovery skipped/repeated the delivered correction: %+v", saved.ExecuteUpdates)
	}
	if saved.StartedAt != before.StartedAt || saved.ExecutionDeadline != before.ExecutionDeadline || saved.RecoveryCount != before.RecoveryCount || !reflect.DeepEqual(saved.Identity.OriginalBudget, before.Identity.OriginalBudget) {
		t.Fatal("dependency recovery refreshed original budget or recovery allowance")
	}
}

func TestExecuteUpdateCheckpointSavedBeforeRequestSurvivesRecovery(t *testing.T) {
	f := newUpdateSessionFixture(t)
	if err := f.refresh(updateResponseReader(t, f.response(5, 8, ""), 5)); err != nil {
		t.Fatal(err)
	}
	committed := f.diskState()
	f.reopen() // No Provider has consumed the newly saved history yet.
	quiet := f.response(8, 8, "")
	quiet.Facts, quiet.Relations, quiet.InvalidSources = nil, nil, nil
	if err := f.refresh(updateResponseReader(t, quiet, 8)); err != nil {
		t.Fatal(err)
	}
	saved := f.diskState()
	if saved.ExecuteUpdates.Cursor.Revision != 8 || saved.ExecuteUpdates.NoticeSequence != committed.ExecuteUpdates.NoticeSequence || updateNoticeCount(saved.History) != 1 || !strings.Contains(saved.History[len(saved.History)-1].Text(), "Exact correction evidence") {
		t.Fatal("saved-before-request recovery lost or duplicated the correction")
	}
	if f.journal.uncommitted != 0 {
		t.Fatal("fully committed session was treated as a journal-only update")
	}
}

func TestExecuteUpdatePendingReadFailureAndBudgetDeduplicateWithoutAcknowledging(t *testing.T) {
	for _, reason := range []string{"read_failed", "context_budget"} {
		t.Run(reason, func(t *testing.T) {
			f := newUpdateSessionFixture(t)
			through := int64(8)
			read := func(_ context.Context, request GraphRequest) (string, error) {
				if request.Updates == nil || request.Updates.Revision != 5 {
					t.Fatal("incomplete update advanced the cursor")
				}
				if reason == "read_failed" {
					return "", errors.New("synthetic unavailable bridge")
				}
				response := f.response(5, through, reason)
				response.Omitted["facts"] = 1
				raw, err := json.Marshal(response)
				return string(raw), err
			}
			if err := f.refresh(read); err != nil {
				t.Fatal(err)
			}
			first := f.diskState()
			through++
			if err := f.refresh(read); err != nil {
				t.Fatal(err)
			}
			saved := f.diskState()
			if saved.ExecuteUpdates.Cursor.Revision != 5 || saved.ExecuteUpdates.PendingReason != reason || updateNoticeCount(saved.History) != 1 || saved.ExecuteUpdates.NoticeSequence != first.ExecuteUpdates.NoticeSequence || saved.ExecuteUpdates.NoticeDigest != first.ExecuteUpdates.NoticeDigest {
				t.Fatalf("repeated pending review was acknowledged or reinjected: %+v", saved.ExecuteUpdates)
			}
			wantThrough := int64(5)
			if reason == "context_budget" {
				wantThrough = 9
			}
			if saved.ExecuteUpdates.ObservedThrough != wantThrough {
				t.Fatal("readable upper boundary was conflated with acknowledged revision")
			}
		})
	}
}

func TestExecuteUpdateRejectsOversizedResponseBeforeMutation(t *testing.T) {
	f := newUpdateSessionFixture(t)
	before := f.diskState()
	err := f.refresh(func(context.Context, GraphRequest) (string, error) {
		return strings.Repeat("x", board.MaxExecuteUpdateBytes+1), nil
	})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("unbounded update response accepted: %v", err)
	}
	after := f.diskState()
	if after.ExecuteUpdates != nil || f.state.ExecuteUpdates != nil || after.Log != before.Log || !reflect.DeepEqual(after.History, before.History) {
		t.Fatal("invalid large response mutated the checkpoint")
	}
}

func TestExecuteUpdateReadFailurePreservesReviewPinsAcrossRecoveryAndResolution(t *testing.T) {
	f := newUpdateSessionFixture(t)
	if err := f.refresh(updateResponseReader(t, f.response(5, 8, ""), 5)); err != nil {
		t.Fatal(err)
	}
	review := f.diskState().ExecuteUpdates.Review
	reviewSequence := f.state.ExecuteUpdates.ReviewSequence
	if review == "" || reviewSequence == 0 || !reflect.DeepEqual(f.loop.ContextData, []string{review}) {
		t.Fatal("delivered correction was not pinned verbatim")
	}
	wantCursor := int64(8)
	failed := func(_ context.Context, request GraphRequest) (string, error) {
		if request.Updates == nil || request.Updates.Revision != wantCursor {
			t.Fatalf("read failure changed the saved boundary: %+v", request)
		}
		return "", errors.New("synthetic bridge outage")
	}
	if err := f.refresh(failed); err != nil {
		t.Fatal(err)
	}
	saved := f.diskState()
	if saved.ExecuteUpdates.Review != review || saved.ExecuteUpdates.ReviewSequence != reviewSequence || saved.ExecuteUpdates.StatusNotice == "" || saved.ExecuteUpdates.PendingReason != "read_failed" || saved.ExecuteUpdates.Cursor.Revision != 8 || !reflect.DeepEqual(f.loop.ContextData, saved.ExecuteUpdates.contextData()) || len(f.loop.ContextData) != 2 {
		t.Fatal("read failure replaced correction evidence with a status warning")
	}
	failedSequence := saved.ExecuteUpdates.NoticeSequence
	f.reopen()
	if !reflect.DeepEqual(f.loop.ContextData, saved.ExecuteUpdates.contextData()) {
		t.Fatal("recovery did not restore exact correction and status pins")
	}
	if err := f.refresh(failed); err != nil {
		t.Fatal(err)
	}
	saved = f.diskState()
	if saved.ExecuteUpdates.NoticeSequence != failedSequence || updateNoticeCount(saved.History) != 2 {
		t.Fatal("recovery reinjected an unchanged bridge failure")
	}
	quiet := f.response(8, 10, "")
	quiet.Facts, quiet.Relations, quiet.InvalidSources = nil, nil, nil
	if err := f.refresh(updateResponseReader(t, quiet, 8)); err != nil {
		t.Fatal(err)
	}
	saved = f.diskState()
	var resolved board.ExecuteUpdates
	if err := json.Unmarshal([]byte(strings.TrimPrefix(saved.ExecuteUpdates.StatusNotice, executeUpdateNotice)), &resolved); err != nil {
		t.Fatal(err)
	}
	if saved.ExecuteUpdates.PendingReason != "" || saved.ExecuteUpdates.Cursor.Revision != 10 || saved.ExecuteUpdates.Review != review || saved.ExecuteUpdates.ReviewSequence != reviewSequence || !resolved.Complete || resolved.FromRevision != 8 || resolved.ToRevision != 10 || resolved.PendingReason != "" || updateNoticeCount(saved.History) != 3 || !reflect.DeepEqual(f.loop.ContextData, saved.ExecuteUpdates.contextData()) {
		t.Fatal("successful empty review did not publish one resolution while retaining evidence")
	}
	resolvedSequence := saved.ExecuteUpdates.NoticeSequence
	quiet.FromRevision, quiet.Revision = 10, 10
	if err := f.refresh(updateResponseReader(t, quiet, 10)); err != nil {
		t.Fatal(err)
	}
	if saved = f.diskState(); saved.ExecuteUpdates.NoticeSequence != resolvedSequence || updateNoticeCount(saved.History) != 3 {
		t.Fatal("a second unchanged success repeated the resolution notice")
	}
	wantCursor = 10
	if err := f.refresh(failed); err != nil {
		t.Fatal(err)
	}
	saved = f.diskState()
	if saved.ExecuteUpdates.PendingReason != "read_failed" || saved.ExecuteUpdates.NoticeSequence <= resolvedSequence || saved.ExecuteUpdates.Review != review || saved.ExecuteUpdates.ReviewSequence != reviewSequence || updateNoticeCount(saved.History) != 4 || !reflect.DeepEqual(f.loop.ContextData, saved.ExecuteUpdates.contextData()) {
		t.Fatal("a new outage after resolution was silently deduplicated")
	}
}

func TestExecuteUpdateOldSessionStartsAtOriginalSnapshot(t *testing.T) {
	f := newUpdateSessionFixture(t)
	f.reopen()
	if f.state.ExecuteUpdates != nil {
		t.Fatal("fixture already has an update cursor")
	}
	if err := f.refresh(updateResponseReader(t, f.response(5, 11, ""), 5)); err != nil {
		t.Fatal(err)
	}
	saved := f.diskState()
	if saved.ExecuteUpdates.Cursor.Revision != 11 || !strings.Contains(saved.History[len(saved.History)-1].Text(), `"from_revision":5`) {
		t.Fatal("old bound session started at the current revision")
	}
}

func TestExecuteUpdateResponseRejectsCrossIdentityAndIllegalBoundaries(t *testing.T) {
	mutations := map[string]func(*board.ExecuteUpdates){
		"schema":                    func(u *board.ExecuteUpdates) { u.Version++ },
		"project":                   func(u *board.ExecuteUpdates) { u.ProjectID = "other" },
		"generation":                func(u *board.ExecuteUpdates) { u.Generation++ },
		"step":                      func(u *board.ExecuteUpdates) { u.StepID = "other" },
		"run":                       func(u *board.ExecuteUpdates) { u.RunID = "other" },
		"skipped_cursor":            func(u *board.ExecuteUpdates) { u.FromRevision, u.Revision = 6, 6 },
		"revision_mismatch":         func(u *board.ExecuteUpdates) { u.Revision = 4 },
		"backwards":                 func(u *board.ExecuteUpdates) { u.ToRevision = 4 },
		"invalid_version":           func(u *board.ExecuteUpdates) { u.StateVersion = "unversioned" },
		"pending_complete":          func(u *board.ExecuteUpdates) { u.PendingReason = "event_gap" },
		"incomplete_without_reason": func(u *board.ExecuteUpdates) { u.Complete = false },
		"silent_omission":           func(u *board.ExecuteUpdates) { u.Omitted["facts"] = 1 },
		"negative_omission":         func(u *board.ExecuteUpdates) { u.Omitted["facts"] = -1 },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			f := newUpdateSessionFixture(t)
			response := f.response(5, 8, "")
			mutate(&response)
			if err := f.refresh(updateResponseReader(t, response, 5)); err == nil {
				t.Fatal("accepted invalid update response")
			}
			if f.diskState().ExecuteUpdates != nil || f.state.ExecuteUpdates != nil || updateNoticeCount(f.loop.History) != 0 {
				t.Fatal("invalid response changed the saved or live cursor")
			}
		})
	}
}

func TestExecuteUpdateFrozenPhasesOnlyPersistDeferredStatus(t *testing.T) {
	for _, phase := range []string{"conclude", "repair"} {
		t.Run(phase, func(t *testing.T) {
			f := newUpdateSessionFixture(t)
			f.loop.Concluding, f.loop.Repairing = phase == "conclude", phase == "repair"
			before := f.diskState()
			read := func(context.Context, GraphRequest) (string, error) {
				t.Fatal("frozen phase performed a graph read")
				return "", nil
			}
			for i := 0; i < 2; i++ {
				if err := f.refresh(read); err != nil {
					t.Fatal(err)
				}
			}
			saved := f.diskState()
			if saved.ExecuteUpdates.DeferredPhase != phase || saved.ExecuteUpdates.Cursor.Revision != 5 || saved.ExecuteUpdates.ObservedThrough != 5 || !reflect.DeepEqual(saved.History, before.History) || saved.Log != before.Log {
				t.Fatalf("frozen input changed while deferring %s: %+v", phase, saved.ExecuteUpdates)
			}
		})
	}
}

func TestExecuteUpdateCheckpointLocatesExactOriginalJournalPayload(t *testing.T) {
	f := newUpdateSessionFixture(t)
	if err := f.refresh(updateResponseReader(t, f.response(5, 8, ""), 5)); err != nil {
		t.Fatal(err)
	}
	saved := f.diskState()
	raw, err := os.ReadFile(filepath.Join(f.dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var event agent.Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event.Message == nil || event.Message.Sequence != saved.ExecuteUpdates.NoticeSequence {
			continue
		}
		found++
		var payload board.ExecuteUpdates
		text := strings.TrimPrefix(event.Message.Text(), executeUpdateNotice)
		if err := json.Unmarshal([]byte(text), &payload); err != nil || payload.FromRevision != 5 || payload.ToRevision != 8 || payload.RunID != f.job.RunID || payload.Facts[1].Evidence[0].Excerpt != "Exact correction evidence" {
			t.Fatalf("checkpoint lost exact source payload: %s, %v", text, err)
		}
	}
	if found != 1 || saved.ExecuteUpdates.NoticeSequence > saved.ContextCheckpoint.LastSequence {
		t.Fatal("notice checkpoint does not locate one original journal message")
	}
	// The checkpoint's identity and sequence bounds are independently checked
	// before any later attempt can read or advance the graph cursor.
	for _, mutate := range []func(*session){
		func(s *session) { s.ExecuteUpdates.Cursor.RunID = "other" },
		func(s *session) { s.ExecuteUpdates.Cursor.Generation++ },
		func(s *session) { s.ExecuteUpdates.Cursor.Revision = 4 },
		func(s *session) { s.ExecuteUpdates.Cursor.Revision = s.ExecuteUpdates.ObservedThrough + 1 },
		func(s *session) { s.ExecuteUpdates.NoticeSequence = s.ContextCheckpoint.LastSequence + 1 },
		func(s *session) { s.ExecuteUpdates.NoticeDigest = "" },
	} {
		bad := f.diskState()
		mutate(bad)
		if err := validateExecuteUpdateState(f.job, bad); err == nil {
			t.Fatal("invalid saved update checkpoint accepted")
		}
	}
}
