package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"xloom/internal/board"
	"xloom/internal/worker"
)

type decisionReadPage struct {
	StateVersion string            `json:"state_version"`
	Revision     int64             `json:"revision"`
	Total        int               `json:"total"`
	NextOffset   *int              `json:"next_offset"`
	Items        []json.RawMessage `json:"items"`
}

func decisionRead(t *testing.T, f *executionProtocolFixture, section, version string) decisionReadPage {
	t.Helper()
	var page decisionReadPage
	f.request("POST", f.base()+"/state/read", worker.GraphRequest{RequestID: strings.Repeat("a", 32), Op: "read_graph", Section: section, ExpectedVersion: version}, true, http.StatusOK, &page)
	return page
}

func TestDecisionReadsSurviveConcurrentFactsAndKeepWritesCurrent(t *testing.T) {
	f, store := newSnapshotHTTPFixture(t)
	executor := *f
	executor.run, executor.lease = "evidence-run", "planner@evidence-run"
	prepareSnapshot(t, &executor, snapshotTemplate(&executor, "explore"))
	_, job := prepareSnapshot(t, f, snapshotTemplate(f, "reason"))
	initial := decisionRead(t, f, "facts", job.InputSnapshot.StateVersion)
	view := initial
	for n := 0; n < 5; n++ {
		executor.action("fact", fmt.Sprintf("observation-%d", n), map[string]any{
			"description": fmt.Sprintf("Business observation %d", n), "scope": "fixture", "observed_at": "2026-09-22T10:00:00Z",
			"evidence": []board.EvidenceRef{{RunID: executor.run, Path: "observation.txt", Excerpt: fmt.Sprintf("observed %d", n)}},
		})
		if got := decisionRead(t, f, "facts", view.StateVersion); !reflect.DeepEqual(got, view) {
			t.Fatalf("concurrent observation %d changed the pinned input", n)
		}
		stale := board.DecisionBatch{ExpectedVersion: view.StateVersion, Actions: []board.DecisionAction{batchAction("step", "probe", `{"action":"add","from":["origin"],"description":"Inspect fixture"}`)}}
		before := f.state()
		f.decision("preview", stale, http.StatusConflict)
		f.decision("commit", stale, http.StatusConflict)
		if !reflect.DeepEqual(before, f.state()) {
			t.Fatal("stale batch published changes")
		}
		if n == 1 {
			version := decisionRead(t, f, "overview", "").StateVersion
			view = decisionRead(t, f, "facts", version)
			if view.Total != initial.Total+2 || version == initial.StateVersion {
				t.Fatal("overview did not refresh the read view")
			}
		}
	}
	var frozen decisionReadPage
	f.request("POST", f.base()+"/executions/"+f.run+"/input/read", worker.GraphRequest{RequestID: strings.Repeat("b", 32), Op: "read_snapshot", Section: "facts", ExpectedVersion: initial.StateVersion}, true, http.StatusOK, &frozen)
	if !reflect.DeepEqual(frozen, initial) {
		t.Fatal("refresh changed the original immutable input")
	}
	// Reopen the database and rebuild the HTTP server: an in-memory cache would
	// silently replace this view with either the original or current graph.
	var sequence int
	var name, database string
	if err := store.Do(context.Background(), func(tx *board.Tx) error {
		return tx.QueryRow("PRAGMA database_list").Scan(&sequence, &name, &database)
	}); err != nil {
		t.Fatal(err)
	}
	clock := store.Now
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := board.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	reopened.Now = clock
	f.handler = New(reopened)
	if got := decisionRead(t, f, "facts", view.StateVersion); !reflect.DeepEqual(got, view) {
		t.Fatal("server recovery lost the refreshed view")
	}
	for n := 0; n < 3; n++ {
		decisionRead(t, f, "overview", "")
	}
	var views int
	if err = reopened.Do(context.Background(), func(tx *board.Tx) error {
		return tx.QueryRow("SELECT count(*) FROM xloom_decision_read_views WHERE project_id=? AND execution_id=?", f.project, f.run).Scan(&views)
	}); err != nil || views != 1 {
		t.Fatalf("refresh retained historical full graphs: count=%d err=%v", views, err)
	}
	current := decisionRead(t, f, "facts", "")
	if current.Total != initial.Total+5 {
		t.Fatal("refresh did not expose all new observations")
	}
	f.decision("commit", board.DecisionBatch{ExpectedVersion: current.StateVersion, Actions: []board.DecisionAction{}}, http.StatusOK)
}

func TestDecisionReadPagesAndFragmentsKeepTheirView(t *testing.T) {
	f, store := newSnapshotHTTPFixture(t)
	prepareSnapshot(t, f, snapshotTemplate(f, "reason"))
	content := strings.Repeat("fragment \"证据\" ", 12000)
	var hint board.Hint
	f.request("POST", f.base()+"/hints", map[string]string{"creator": "fixture", "content": "legacy oversized input"}, false, http.StatusCreated, &hint)
	for n := 0; n < 2; n++ {
		f.request("POST", f.base()+"/hints", map[string]string{"creator": "fixture", "content": fmt.Sprintf("next page %d", n)}, false, http.StatusCreated, nil)
	}
	// Current HTTP writes reject oversized mandatory input. Seed a historical
	// record directly to exercise the lossless reader's compatibility contract.
	replaceHint := func(value string) {
		t.Helper()
		if err := store.Do(context.Background(), func(tx *board.Tx) error {
			_, err := tx.Exec("UPDATE hints SET content=? WHERE project_id=? AND id=?", value, f.project, hint.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	replaceHint(content)
	hint.Content = content
	version := decisionRead(t, f, "overview", "").StateVersion
	read := worker.GraphRequest{RequestID: strings.Repeat("c", 32), Op: "read_graph", Section: "hints", ExpectedVersion: version, Limit: 1}
	var page decisionReadPage
	f.request("POST", f.base()+"/state/read", read, true, http.StatusOK, &page)
	var ref struct {
		RecordOmitted bool   `json:"record_omitted"`
		RecordVersion string `json:"record_version"`
	}
	if len(page.Items) != 1 || json.Unmarshal(page.Items[0], &ref) != nil || !ref.RecordOmitted || page.NextOffset == nil {
		t.Fatal("fixture did not produce a paginated oversized record")
	}
	nextPage := *page.NextOffset
	read.RecordVersion = ref.RecordVersion
	var recovered strings.Builder
	for offset := 0; ; {
		replaceHint(fmt.Sprintf("changed while reading fragment %d", offset))
		read.ByteOffset = &offset
		var fragment struct {
			StateVersion   string `json:"state_version"`
			Content        string `json:"content"`
			NextByteOffset *int   `json:"next_byte_offset"`
		}
		f.request("POST", f.base()+"/state/read", read, true, http.StatusOK, &fragment)
		if fragment.StateVersion != version {
			t.Fatal("fragment crossed read views")
		}
		recovered.WriteString(fragment.Content)
		if fragment.NextByteOffset == nil {
			break
		}
		offset = *fragment.NextByteOffset
	}
	var restored board.Hint
	if err := json.Unmarshal([]byte(recovered.String()), &restored); err != nil || !reflect.DeepEqual(restored, hint) {
		t.Fatalf("fragmented record changed: %v", err)
	}
	read.ByteOffset, read.RecordVersion, read.Offset = nil, "", nextPage
	for {
		page = decisionReadPage{}
		f.request("POST", f.base()+"/state/read", read, true, http.StatusOK, &page)
		if page.Total != 3 || page.StateVersion != version {
			t.Fatal("pagination included evidence published after the view")
		}
		if page.NextOffset == nil {
			break
		}
		read.Offset = *page.NextOffset
	}
	decisionRead(t, f, "overview", "")
	f.request("POST", f.base()+"/state/read", read, true, http.StatusConflict, nil)
}

func TestDecisionReadViewDoesNotBypassFences(t *testing.T) {
	for _, operation := range []string{"stop", "restart"} {
		t.Run(operation, func(t *testing.T) {
			f, store := newSnapshotHTTPFixture(t)
			prepareSnapshot(t, f, snapshotTemplate(f, "reason"))
			view := decisionRead(t, f, "overview", "")
			if operation == "stop" {
				f.request("PUT", f.base()+"/status", map[string]string{"status": "stopped"}, false, http.StatusOK, nil)
			} else {
				f.request("POST", f.base()+"/restart", map[string]int{"expected_generation": 0}, false, http.StatusOK, nil)
				var count int
				if err := store.Do(context.Background(), func(tx *board.Tx) error {
					return tx.QueryRow("SELECT count(*) FROM xloom_decision_read_views WHERE project_id=?", f.project).Scan(&count)
				}); err != nil || count != 0 {
					t.Fatalf("restart retained stale views: %d %v", count, err)
				}
			}
			f.request("POST", f.base()+"/state/read", worker.GraphRequest{RequestID: strings.Repeat("d", 32), Op: "read_graph", Section: "facts", ExpectedVersion: view.StateVersion}, true, http.StatusConflict, nil)
		})
	}
}

func TestUnversionedDecisionGraphReadsRemainLive(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	live := true
	f.registerWithFields("reason", &live, 1, map[string]any{"decision": nil}, http.StatusCreated)
	initial := decisionRead(t, f, "hints", "")
	f.request("POST", f.base()+"/hints", map[string]string{"creator": "fixture", "content": "live compatibility update"}, false, http.StatusCreated, nil)
	if got := decisionRead(t, f, "hints", ""); got.Total != 1 || got.StateVersion == initial.StateVersion {
		t.Fatal("unversioned reason was unexpectedly pinned")
	}
}

func TestVersionOneDecisionReadsStayLiveAfterOwnWrites(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	live := true
	f.register("reason", &live, 1)
	initial := decisionRead(t, f, "overview", "")
	added := f.action("step", "own-write", map[string]any{"action": "add", "from": []string{"origin"}, "description": "Existing individual-write protocol"})
	page := decisionRead(t, f, "steps", added.StateVersion)
	if page.StateVersion == initial.StateVersion || page.Total != 1 {
		t.Fatal("version 1 did not read its own current write")
	}
	f.request("POST", f.base()+"/hints", map[string]string{"creator": "fixture", "content": "new live observation"}, false, http.StatusCreated, nil)
	f.request("POST", f.base()+"/state/read", worker.GraphRequest{RequestID: strings.Repeat("e", 32), Op: "read_graph", Section: "steps", ExpectedVersion: page.StateVersion}, true, http.StatusConflict, nil)
}
