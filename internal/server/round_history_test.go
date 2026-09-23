package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"xloom/internal/board"
)

func TestRoundHistoryHTTPPreservesEvidenceAndFencesOldWorker(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	live := true
	f.register("explore", &live, 2)
	evidence := make([]board.EvidenceRef, 8)
	for n := range evidence {
		evidence[n] = board.EvidenceRef{RunID: f.run, Path: fmt.Sprintf("/workspace/evidence-%d.txt", n), Excerpt: strings.Repeat("证据🙂", 600)}
	}
	payload := map[string]any{"description": "Large retained evidence", "scope": "fixture", "observed_at": "2026-09-22T10:00:00Z", "evidence": evidence}
	f.action("fact", "large-evidence", payload)
	original, _ := json.Marshal(f.state())
	f.request("POST", f.base()+"/restart", map[string]any{"expected_generation": 0}, false, http.StatusOK, nil)
	f.request("POST", f.base()+"/restart", map[string]any{"expected_generation": 0}, false, http.StatusConflict, nil)
	var rounds board.RoundPage
	f.request("GET", f.base()+"/rounds?limit=1", nil, false, http.StatusOK, &rounds)
	if len(rounds.Items) != 1 || rounds.Items[0].Generation != 0 || rounds.NextCursor != nil || f.state().Graph.Project.Generation != 1 {
		t.Fatalf("wrong archived/current generations: %+v", rounds)
	}
	var stateEntry board.RoundEntry
	counts, cursor, pages := map[string]int{}, int64(0), 0
	for {
		var entries board.RoundEntryPage
		f.request("GET", fmt.Sprintf("%s/rounds/0/entries?limit=1&cursor=%d", f.base(), cursor), nil, false, http.StatusOK, &entries)
		pages++
		if len(entries.Items) != 1 || entries.Items[0].Cursor <= cursor {
			t.Fatalf("entry pagination failed: %+v", entries)
		}
		entry := entries.Items[0]
		counts[entry.Kind]++
		if entry.Kind == "state" {
			stateEntry = entry
		}
		if entries.NextCursor == nil {
			break
		}
		cursor = *entries.NextCursor
	}
	for _, kind := range []string{"state", "event", "action", "execution"} {
		if counts[kind] == 0 {
			t.Fatalf("HTTP archive lost %s entries: %v", kind, counts)
		}
	}
	if pages < 4 || stateEntry.Bytes <= 32<<10 {
		t.Fatal("fixture did not exercise record and byte pagination")
	}
	entryPath := fmt.Sprintf("%s/rounds/0/entries/%d", f.base(), stateEntry.Cursor)
	var restored []byte
	offset, chunks := int64(0), 0
	for {
		var chunk board.RoundEntryChunk
		body := f.request("GET", fmt.Sprintf("%s?limit=32768&offset=%d", entryPath, offset), nil, false, http.StatusOK, &chunk)
		if len(body) > 45<<10 || chunk.Encoding != "base64" || chunk.Offset != offset || chunk.TotalBytes != stateEntry.Bytes || chunk.SHA256 != stateEntry.SHA256 || len(chunk.Data) > 32<<10 {
			t.Fatal("unbounded response or inconsistent chunk metadata")
		}
		restored = append(restored, chunk.Data...)
		chunks++
		if chunk.NextOffset == nil {
			break
		}
		if *chunk.NextOffset != int64(len(restored)) || *chunk.NextOffset <= offset {
			t.Fatal("byte continuation did not advance exactly")
		}
		offset = *chunk.NextOffset
	}
	sum := sha256.Sum256(restored)
	if chunks < 2 || !bytes.Equal(restored, original) || hex.EncodeToString(sum[:]) != stateEntry.SHA256 {
		t.Fatal("HTTP archive did not preserve all original evidence bytes")
	}
	for _, route := range []string{f.base() + "/rounds", f.base() + "/rounds/0/entries", entryPath} {
		f.request("GET", route, nil, true, http.StatusForbidden, nil)
	}
	f.request("POST", f.base()+"/restart", map[string]any{"expected_generation": 1}, true, http.StatusForbidden, nil)
	f.request("POST", f.base()+"/state/actions", map[string]any{"op": "fact", "idempotency_key": "late-worker", "payload": payload}, true, http.StatusConflict, nil)
	f.request("POST", f.base()+"/state/actions", map[string]any{"op": "fact", "idempotency_key": "large-evidence", "payload": payload}, true, http.StatusConflict, nil)
	f.request("POST", f.base()+"/intents/"+f.intent+"/heartbeat", map[string]any{"worker": f.lease}, true, http.StatusConflict, nil)
	if current := f.state(); current.Graph.Project.Generation != 1 || current.Revision != 0 || len(current.Graph.Facts) != 2 {
		t.Fatal("old worker changed the new round")
	}
	// Byte cursors are scoped by project and generation, never a global ID lookup.
	f.request("GET", f.base()+"/rounds/1/entries", nil, false, http.StatusNotFound, nil)
	f.request("GET", strings.Replace(entryPath, "/rounds/0/", "/rounds/1/", 1), nil, false, http.StatusNotFound, nil)
	f.request("GET", entryPath+"?limit=32769", nil, false, http.StatusUnprocessableEntity, nil)
	f.request("GET", fmt.Sprintf("%s?offset=%d", entryPath, stateEntry.Bytes+1), nil, false, http.StatusUnprocessableEntity, nil)
	f.request("GET", f.base()+"/rounds?cursor=-2", nil, false, http.StatusUnprocessableEntity, nil)
	var end board.RoundEntryChunk
	f.request("GET", fmt.Sprintf("%s?offset=%d", entryPath, stateEntry.Bytes), nil, false, http.StatusOK, &end)
	if len(end.Data) != 0 || end.NextOffset != nil || end.SHA256 != stateEntry.SHA256 {
		t.Fatal("reading exactly at archive EOF is inconsistent")
	}
	var other board.Graph
	f.request("POST", "/projects", map[string]any{"title": "Other project", "origin": "Independent input", "goal": "Independent goal", "bootstrap_enabled": false}, false, http.StatusCreated, &other)
	f.request("GET", strings.Replace(entryPath, f.base(), "/projects/"+other.Project.ID, 1), nil, false, http.StatusNotFound, nil)
}
