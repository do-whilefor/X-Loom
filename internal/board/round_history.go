package board

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
)

const roundHistorySchema = `
CREATE TABLE IF NOT EXISTS xloom_round_history(
 project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 generation INTEGER NOT NULL,archived_at TEXT NOT NULL,
 PRIMARY KEY(project_id,generation));
CREATE TABLE IF NOT EXISTS xloom_round_entries(
 project_id TEXT NOT NULL,generation INTEGER NOT NULL,kind TEXT NOT NULL,id TEXT NOT NULL,
 data BLOB NOT NULL,sha256 TEXT NOT NULL,
 PRIMARY KEY(project_id,generation,kind,id),
 FOREIGN KEY(project_id,generation) REFERENCES xloom_round_history(project_id,generation) ON DELETE CASCADE);`

// Archive the prior round before removing its current projections. The archive
// and reset share the caller's transaction; any failure preserves the live round.
// These records have no update path and never grant authority to an old Worker.
func (t *Tx) archiveRound(project string) error {
	state, err := t.State(project)
	if err != nil {
		return err
	}
	generation := state.Graph.Project.Generation
	if _, err = t.Exec("INSERT INTO xloom_round_history(project_id,generation,archived_at) VALUES(?,?,?)", project, generation, t.Now); err != nil {
		return err
	}
	save := func(kind, id string, raw []byte) error {
		sum := sha256.Sum256(raw)
		_, err := t.Exec("INSERT INTO xloom_round_entries(project_id,generation,kind,id,data,sha256) VALUES(?,?,?,?,?,?)", project, generation, kind, id, raw, hex.EncodeToString(sum[:]))
		return err
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err = save("state", "state", raw); err != nil {
		return err
	}
	// Stream records one by one rather than materializing every historical Job
	// or all evidence-bearing events in an archive-sized in-memory slice.
	rows, err := t.Query("SELECT revision,event FROM xloom_state_events WHERE project_id=? ORDER BY revision", project)
	if err != nil {
		return err
	}
	for rows.Next() {
		var revision int64
		var event []byte
		if err = rows.Scan(&revision, &event); err == nil {
			err = save("event", strconv.FormatInt(revision, 10), event)
		}
		if err != nil {
			rows.Close()
			return err
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	rows, err = t.Query("SELECT idempotency_key,request,response FROM xloom_state_actions WHERE project_id=? ORDER BY rowid", project)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, request, response string
		if err = rows.Scan(&id, &request, &response); err == nil {
			var data []byte
			// Keep even legacy request/response text byte-for-byte.
			data, err = json.Marshal(map[string]string{"request": request, "response": response})
			if err == nil {
				err = save("action", id, data)
			}
		}
		if err != nil {
			rows.Close()
			return err
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	rows, err = t.Query("SELECT "+executionColumns+" FROM xloom_executions WHERE project_id=? ORDER BY rowid", project)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		execution, err := scanExecution(rows)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(execution)
		if err != nil {
			return err
		}
		if err = save("execution", execution.ID, raw); err != nil {
			return err
		}
	}
	return rows.Err()
}

type RoundSummary struct {
	Generation int64  `json:"generation"`
	ArchivedAt string `json:"archived_at"`
}

type RoundPage struct {
	Items      []RoundSummary `json:"items"`
	NextCursor *int64         `json:"next_cursor,omitempty"`
}

func (t *Tx) RoundHistory(project string, after int64, limit int) (RoundPage, error) {
	out := RoundPage{Items: []RoundSummary{}}
	if after < -1 || limit < 1 || limit > 100 {
		return out, Err(422, "round history requires cursor >= -1 and limit between 1 and 100")
	}
	var exists bool
	if err := t.QueryRow("SELECT EXISTS(SELECT 1 FROM projects WHERE id=?)", project).Scan(&exists); err != nil {
		return out, err
	}
	if !exists {
		return out, Err(404, "Project not found")
	}
	rows, err := t.Query("SELECT generation,archived_at FROM xloom_round_history WHERE project_id=? AND generation>? ORDER BY generation LIMIT ?", project, after, limit+1)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var entry RoundSummary
		if err = rows.Scan(&entry.Generation, &entry.ArchivedAt); err != nil {
			return out, err
		}
		if len(out.Items) == limit {
			cursor := out.Items[len(out.Items)-1].Generation
			out.NextCursor = &cursor
			break
		}
		out.Items = append(out.Items, entry)
	}
	return out, rows.Err()
}

type RoundEntry struct {
	Cursor int64  `json:"cursor"`
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type RoundEntryPage struct {
	Items      []RoundEntry `json:"items"`
	NextCursor *int64       `json:"next_cursor,omitempty"`
}

func (t *Tx) requireRound(project string, generation int64) error {
	var exists bool
	if err := t.QueryRow("SELECT EXISTS(SELECT 1 FROM xloom_round_history WHERE project_id=? AND generation=?)", project, generation).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return Err(404, "Archived round not found")
	}
	return nil
}

func (t *Tx) RoundEntries(project string, generation, after int64, limit int) (RoundEntryPage, error) {
	out := RoundEntryPage{Items: []RoundEntry{}}
	if generation < 0 || after < 0 || limit < 1 || limit > 100 {
		return out, Err(422, "invalid archived round cursor or limit")
	}
	if err := t.requireRound(project, generation); err != nil {
		return out, err
	}
	rows, err := t.Query("SELECT rowid,kind,id,length(data),sha256 FROM xloom_round_entries WHERE project_id=? AND generation=? AND rowid>? ORDER BY rowid LIMIT ?", project, generation, after, limit+1)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var entry RoundEntry
		if err = rows.Scan(&entry.Cursor, &entry.Kind, &entry.ID, &entry.Bytes, &entry.SHA256); err != nil {
			return out, err
		}
		if len(out.Items) == limit {
			cursor := out.Items[len(out.Items)-1].Cursor
			out.NextCursor = &cursor
			break
		}
		out.Items = append(out.Items, entry)
	}
	return out, rows.Err()
}

type RoundEntryChunk struct {
	Data       []byte `json:"data"` // JSON encodes byte chunks as base64, including split UTF-8.
	Encoding   string `json:"encoding"`
	Offset     int64  `json:"offset"`
	TotalBytes int64  `json:"total_bytes"`
	NextOffset *int64 `json:"next_offset,omitempty"`
	SHA256     string `json:"sha256"`
}

func (t *Tx) ReadRoundEntry(project string, generation, entry, offset int64, limit int) (RoundEntryChunk, error) {
	out := RoundEntryChunk{Encoding: "base64", Offset: offset}
	if generation < 0 || entry < 1 || offset < 0 || limit < 1 || limit > 32<<10 {
		return out, Err(422, "invalid archive entry; byte limit must be between 1 and 32768")
	}
	err := t.QueryRow("SELECT substr(data,?,?),length(data),sha256 FROM xloom_round_entries WHERE project_id=? AND generation=? AND rowid=?", offset+1, limit, project, generation, entry).Scan(&out.Data, &out.TotalBytes, &out.SHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return out, Err(404, "Archived entry not found")
	}
	if err != nil {
		return out, err
	}
	if offset > out.TotalBytes {
		return out, Err(422, "archive offset exceeds entry size")
	}
	if next := offset + int64(len(out.Data)); next < out.TotalBytes {
		out.NextOffset = &next
	}
	return out, nil
}
