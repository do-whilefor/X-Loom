//go:build linux

package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"xloom/internal/agent"
)

func TestCompactedSessionKeepsReplayViewInJournal(t *testing.T) {
	job, dir := scenarioJob(t, "", "explore"), t.TempDir()
	identity, err := identityFor(job, dir)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := openJournal(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.file.Close()
	saved := session{SchemaVersion: sessionSchemaVersion, Identity: identity, RunID: job.RunID, Kind: job.Kind, StartedAt: time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)}
	turns := 0
	loop := agent.Loop{ContextBytes: 12000, SummaryBytes: 4096, RecentBytes: 128, TaskPrompt: "Inspect the fixed fixture.",
		History: []agent.Message{agent.Text("user", "Inspect the fixed fixture."), agent.Text("assistant", strings.Repeat("Unverified observation 中文🙂. ", 1600))},
		Provider: scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
			turns++
			if turns == 1 {
				return agent.Text("assistant", `{"notes":"Recheck the original fixture.","quotes":[]}`), nil
			}
			return agent.Text("assistant", "done"), nil
		}),
		Emit: func(event agent.Event) {
			if err := journal.append(event); err != nil {
				t.Fatal(err)
			}
		},
		SaveState: func(history []agent.Message, checkpoint *agent.ContextCheckpoint) error {
			saved.History, saved.ContextCheckpoint = history, checkpoint
			return saved.save(dir, journal)
		},
	}
	if _, err := loop.Run(context.Background(), ""); err != nil || turns != 2 {
		t.Fatalf("compaction run: turns=%d err=%v", turns, err)
	}
	if err := saved.save(dir, journal); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	journalRaw, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var record *agent.CompactionRecord
	for _, line := range bytes.Split(bytes.TrimSpace(journalRaw), []byte{'\n'}) {
		var event agent.Event
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatal(err)
		}
		if event.Type == "context_compacted" {
			record = event.Compaction
		}
	}
	if record == nil || len(record.View) == 0 || !reflect.DeepEqual(record.View, saved.History[:len(saved.History)-1]) || saved.History[len(saved.History)-1].Sequence <= record.AtSequence {
		t.Fatal("journal view cannot replay saved history with the final message")
	}
	var legacy map[string]json.RawMessage
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatal(err)
	}
	var checkpoint map[string]json.RawMessage
	if err := json.Unmarshal(legacy["context_checkpoint"], &checkpoint); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(checkpoint["last_compaction"], []byte(`"view"`)) {
		t.Fatal("session duplicated the journal view")
	}
	checkpoint["last_compaction"], _ = json.Marshal(record)
	legacy["context_checkpoint"], _ = json.Marshal(checkpoint)
	legacyRaw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	var restored session
	if err := json.Unmarshal(legacyRaw, &restored); err != nil {
		t.Fatal(err)
	}
	var current session
	if err := json.Unmarshal(raw, &current); err != nil {
		t.Fatal(err)
	}
	if err := restored.validate(identity); err != nil || !reflect.DeepEqual(restored, current) {
		t.Fatalf("old full checkpoint changed recovered session: %v", err)
	}
	if err := restored.save(dir, journal); err != nil {
		t.Fatal(err)
	}
	rewritten, err := os.ReadFile(filepath.Join(dir, "session.json"))
	if err != nil || !bytes.Equal(rewritten, raw) {
		t.Fatalf("old session did not rewrite to the reduced format: %v", err)
	}
	t.Logf("fixed long-history full session: legacy=%d bytes, metadata=%d bytes, saved=%d bytes", len(legacyRaw), len(raw), len(legacyRaw)-len(raw))
}
