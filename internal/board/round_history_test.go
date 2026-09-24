package board

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func roundFixture(t *testing.T) *findingFixture {
	t.Helper()
	f := newFindingFixture(t)
	evidence := make([]EvidenceRef, 8)
	for n := range evidence {
		evidence[n] = EvidenceRef{RunID: "evidence-one", Path: fmt.Sprintf("/workspace/evidence-%d.txt", n), Excerpt: strings.Repeat("证据🙂", 600)}
	}
	f.action("fact", "large-evidence", map[string]any{"description": "Retain every evidence byte", "scope": "fixture", "observed_at": "2026-09-22T10:00:00Z", "evidence": evidence})
	f.action("finding", "finding", findingInput(f.first))
	return f
}

func TestRestartArchivesCompleteRoundAndReadsEveryByte(t *testing.T) {
	f := roundFixture(t)
	before := f.state()
	expected := map[string][]byte{}
	expected["state/state"], _ = json.Marshal(before)
	f.tx(func(tx *Tx) error {
		events, err := tx.StateEvents("proj_001", 0)
		if err != nil {
			return err
		}
		for _, event := range events {
			expected[fmt.Sprintf("event/%d", event.Revision)], _ = json.Marshal(event)
		}
		rows, err := tx.Query("SELECT idempotency_key,request,response FROM xloom_state_actions WHERE project_id=?", "proj_001")
		if err != nil {
			return err
		}
		for rows.Next() {
			var key, request, response string
			if err = rows.Scan(&key, &request, &response); err != nil {
				rows.Close()
				return err
			}
			expected["action/"+key], _ = json.Marshal(map[string]string{"request": request, "response": response})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		execution, err := tx.Execution("proj_001", "evidence-one")
		if err != nil {
			return err
		}
		expected["execution/"+execution.ID], _ = json.Marshal(execution)
		return nil
	})
	if len(expected["state/state"]) < 32<<10 {
		t.Fatal("fixture does not exercise byte continuation")
	}
	zero := int64(0)
	f.tx(func(tx *Tx) error { _, err := tx.RestartProject("proj_001", &zero); return err })
	current := f.state()
	if current.Graph.Project.Generation != 1 || len(current.Graph.Facts) != 2 || len(current.Steps) != 0 || len(current.Findings) != 0 || current.Revision != 0 {
		t.Fatalf("restart did not begin a clean projection: %+v", current)
	}
	seen, cursor, pages := map[string]bool{}, int64(0), 0
	for {
		var page RoundEntryPage
		f.tx(func(tx *Tx) (err error) { page, err = tx.RoundEntries("proj_001", 0, cursor, 2); return err })
		pages++
		if len(page.Items) > 2 {
			t.Fatal("entry pagination exceeded limit")
		}
		for _, entry := range page.Items {
			key := entry.Kind + "/" + entry.ID
			want, ok := expected[key]
			if !ok || seen[key] || entry.Cursor <= cursor {
				t.Fatalf("unexpected or duplicate archive entry: %+v", entry)
			}
			seen[key] = true
			var restored []byte
			offset := int64(0)
			for {
				var chunk RoundEntryChunk
				// This prime byte length intentionally splits multi-byte UTF-8.
				f.tx(func(tx *Tx) (err error) {
					chunk, err = tx.ReadRoundEntry("proj_001", 0, entry.Cursor, offset, 1009)
					return err
				})
				if chunk.Encoding != "base64" || chunk.Offset != offset || chunk.TotalBytes != entry.Bytes || chunk.SHA256 != entry.SHA256 || len(chunk.Data) > 1009 {
					t.Fatalf("inconsistent chunk metadata for %s", key)
				}
				restored = append(restored, chunk.Data...)
				if chunk.NextOffset == nil {
					break
				}
				if *chunk.NextOffset != int64(len(restored)) || *chunk.NextOffset <= offset {
					t.Fatal("archive continuation did not advance exactly")
				}
				offset = *chunk.NextOffset
			}
			sum := sha256.Sum256(restored)
			if !bytes.Equal(restored, want) || entry.Bytes != int64(len(want)) || entry.SHA256 != hex.EncodeToString(sum[:]) {
				t.Fatalf("archive changed bytes or checksum for %s", key)
			}
		}
		if page.NextCursor == nil {
			break
		}
		if *page.NextCursor <= cursor {
			t.Fatal("archive cursor did not advance")
		}
		cursor = *page.NextCursor
	}
	if len(seen) != len(expected) || pages < 2 {
		t.Fatalf("incomplete archive: read=%d expected=%d pages=%d", len(seen), len(expected), pages)
	}
	// A second restart adds a round without changing any prior archived bytes.
	one := int64(1)
	f.tx(func(tx *Tx) error { _, err := tx.RestartProject("proj_001", &one); return err })
	f.tx(func(tx *Tx) error {
		var prior []byte
		if err := tx.QueryRow("SELECT data FROM xloom_round_entries WHERE project_id=? AND generation=0 AND kind='state' AND id='state'", "proj_001").Scan(&prior); err != nil {
			return err
		}
		if !bytes.Equal(prior, expected["state/state"]) {
			t.Fatal("second restart changed the first archived round")
		}
		first, err := tx.RoundHistory("proj_001", -1, 1)
		if err != nil {
			return err
		}
		if len(first.Items) != 1 || first.Items[0].Generation != 0 || first.NextCursor == nil || *first.NextCursor != 0 {
			t.Fatalf("first generation page: %+v", first)
		}
		last, err := tx.RoundHistory("proj_001", *first.NextCursor, 1)
		if len(last.Items) != 1 || last.Items[0].Generation != 1 || last.NextCursor != nil {
			t.Fatalf("second generation page: %+v", last)
		}
		return err
	})
}

func TestRestartArchiveFailureRollsBackEntireRound(t *testing.T) {
	f := roundFixture(t)
	before, _ := json.Marshal(f.state())
	f.tx(func(tx *Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER fail_round_archive BEFORE INSERT ON xloom_round_entries WHEN NEW.kind='event' BEGIN SELECT RAISE(ABORT,'archive write failed'); END`)
		return err
	})
	zero := int64(0)
	err := f.store.Do(context.Background(), func(tx *Tx) error { _, err := tx.RestartProject("proj_001", &zero); return err })
	if err == nil || !strings.Contains(err.Error(), "archive write failed") {
		t.Fatalf("archive failure was not returned: %v", err)
	}
	after, _ := json.Marshal(f.state())
	if !bytes.Equal(before, after) {
		t.Fatal("archive failure modified live state")
	}
	f.tx(func(tx *Tx) error {
		for _, table := range []string{"xloom_round_history", "xloom_round_entries", "xloom_revoked_runs"} {
			var count int
			if err := tx.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE project_id=?", "proj_001").Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				t.Fatalf("failed restart left %d rows in %s", count, table)
			}
		}
		if _, err := tx.Execution("proj_001", "evidence-one"); err != nil {
			return err
		}
		_, err := tx.Exec("DROP TRIGGER fail_round_archive")
		return err
	})
	f.tx(func(tx *Tx) error { _, err := tx.RestartProject("proj_001", &zero); return err })
	err = f.store.Do(context.Background(), func(tx *Tx) error { _, err := tx.RestartProject("proj_001", &zero); return err })
	var api *APIError
	if !errors.As(err, &api) || api.Status != 409 || f.state().Graph.Project.Generation != 1 {
		t.Fatalf("replayed expected_generation restarted again: %v", err)
	}
}
