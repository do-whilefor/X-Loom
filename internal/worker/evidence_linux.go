//go:build linux

package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
	"xloom/internal/board"
	"xloom/internal/contract"
)

const maxEvidenceFileBytes = 32 << 20
const maxEvidenceExcerptBytes = 8192

func retainEvidenceBytes(ctx context.Context, run, dir string, raw []byte) (board.EvidenceRef, error) {
	digest := sha256.Sum256(raw)
	ref := board.EvidenceRef{RunID: run, Path: filepath.Join(dir, hex.EncodeToString(digest[:])+".raw"), Excerpt: string(raw)}
	if err := retainEvidenceFile(ctx, ref.Path, raw); err != nil {
		return board.EvidenceRef{}, err
	}
	return ref, nil
}

// prepareFinalEvidence runs before the result is durable. Normal execution
// freezes selected files once; conclusion selects only its persisted boundary
// fragments and never follows a newly supplied workspace path.
func prepareFinalEvidence(ctx context.Context, j Job, runDir string, r Result, frozen []board.EvidenceRef) (Result, error) {
	if j.ResultContractVersion < 2 || j.Kind == "reason" || r.Status != "success" {
		return r, nil
	}
	parsed, err := parseOutput(j, r.Conclude, r.Text)
	if err != nil || len(parsed.FactPayload) == 0 {
		return r, err
	}
	var payload map[string]json.RawMessage
	if err = json.Unmarshal(parsed.FactPayload, &payload); err != nil {
		return r, err
	}
	action := board.StateAction{Op: "fact", IdempotencyKey: j.RunID + ":final-result", Payload: parsed.FactPayload}
	if r.Conclude {
		var selections []board.EvidenceRef
		if err = json.Unmarshal(payload["evidence"], &selections); err != nil {
			return r, err
		}
		var refs []board.EvidenceRef
		for _, selection := range selections {
			var found *board.EvidenceRef
			for n := range frozen {
				if selection.Path == frozen[n].Path {
					found = &frozen[n]
					break
				}
			}
			if found == nil || !currentEvidence(selection.RunID, j.RunID) || !currentEvidence(found.RunID, j.RunID) {
				return r, errors.New("conclusion evidence must select a frozen boundary fragment")
			}
			// Reuse the immutable bytes saved at the boundary, never reopen
			// the mutable source. Restore a missing retained fragment exactly;
			// conflicting bytes at that address fail instead of being replaced.
			retained, err := freezeEvidenceBytes(ctx, j.RunID, runDir, []byte(found.Excerpt))
			if err != nil {
				return r, err
			}
			if retained.Path != found.Path {
				return r, errors.New("saved conclusion fragment has an invalid identity")
			}
			excerpt, err := selectedEvidence([]byte(found.Excerpt), selection)
			if err != nil {
				return r, err
			}
			ref := *found
			ref.Excerpt, ref.StartLine, ref.EndLine = excerpt, selection.StartLine, selection.EndLine
			refs = append(refs, ref)
		}
		action, err = setActionEvidence(action, payload, refs)
	} else {
		action, err = prepareEvidence(ctx, j, runDir, action)
	}
	if err != nil {
		return r, err
	}
	m, err := contract.Extract(r.Text)
	if err != nil {
		return r, err
	}
	var data map[string]json.RawMessage
	if err = json.Unmarshal(m["data"], &data); err != nil {
		return r, err
	}
	data["fact"] = action.Payload
	m["data"], err = json.Marshal(data)
	if err != nil {
		return r, err
	}
	raw, err := json.Marshal(m)
	r.Text = string(raw)
	return r, err
}

// The manifest freezes a tool action before it can reach the graph bridge.
// Retrying after a lost receipt must not read a newer version of the source.
type evidenceAction struct {
	Input    json.RawMessage     `json:"input"`
	Evidence []board.EvidenceRef `json:"evidence"`
}

func prepareEvidence(ctx context.Context, j Job, runDir string, action board.StateAction) (board.StateAction, error) {
	if err := ctx.Err(); err != nil {
		return action, err
	}
	if action.Op != "fact" && action.Op != "finding" {
		return action, nil
	}
	if len(action.Payload) > MaxGraphRPCBytes {
		return action, errors.New("evidence action payload is too large")
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(action.Payload, &payload); err != nil || payload == nil {
		return action, errors.New("evidence action payload must be an object")
	}
	var refs []board.EvidenceRef
	if raw, ok := payload["evidence"]; ok {
		var selections []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &selections); err != nil || selections == nil {
			return action, errors.New("evidence must be an array of selections, not null")
		}
		for _, selection := range selections {
			if selection == nil {
				return action, errors.New("evidence selection cannot be null")
			}
			for _, value := range selection {
				if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
					return action, errors.New("evidence selection fields cannot be null")
				}
			}
		}
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if err := d.Decode(&refs); err != nil {
			return action, fmt.Errorf("invalid evidence selection: %w", err)
		}
	}
	if len(refs) == 0 {
		return action, nil // Sources-only Findings retain the existing server rules.
	}
	if len(refs) > 32 || j.RunID == "" || runDir == "" {
		return action, errors.New("evidence requires a run directory and at most 32 selections")
	}
	dir, err := filepath.Abs(filepath.Join(runDir, "evidence"))
	if err != nil {
		return action, err
	}
	if err = os.MkdirAll(runDir, 0700); err != nil {
		return action, err
	}
	if err = os.Mkdir(dir, 0700); err == nil {
		if err = syncDirectory(runDir); err != nil {
			return action, err
		}
	} else if !os.IsExist(err) {
		return action, err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return action, errors.New("evidence directory must be a directory, not a symlink")
	}
	encoded, err := json.Marshal(action)
	if err != nil {
		return action, err
	}
	// Object ordering/whitespace are immaterial to a retry. UseNumber keeps
	// integer values intact; the actual payload remains RawMessage throughout.
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var canonical any
	if err = decoder.Decode(&canonical); err != nil {
		return action, err
	}
	input, err := json.Marshal(canonical)
	if err != nil {
		return action, err
	}
	key := sha256.Sum256([]byte(action.IdempotencyKey))
	manifest := filepath.Join(dir, hex.EncodeToString(key[:])+".action.json")
	if raw, err := readEvidenceFile(ctx, manifest, MaxGraphRPCBytes*4); err == nil {
		var saved evidenceAction
		if err = json.Unmarshal(raw, &saved); err != nil || !bytes.Equal(saved.Input, input) || len(saved.Evidence) != len(refs) {
			return action, errors.New("evidence idempotency key already prepared with different arguments; use a new key for a changed action")
		}
		for i, ref := range saved.Evidence {
			if !currentEvidence(refs[i].RunID, j.RunID) {
				if ref != refs[i] {
					return action, errors.New("prepared historical evidence changed")
				}
				continue
			}
			if err := verifyEvidenceSnapshot(ctx, dir, j.RunID, refs[i], ref); err != nil {
				return action, err
			}
		}
		return setActionEvidence(action, payload, saved.Evidence)
	} else if !os.IsNotExist(err) {
		return action, err
	}
	prepared := evidenceAction{Input: input}
	for i, ref := range refs {
		if !currentEvidence(ref.RunID, j.RunID) {
			if action.Op != "finding" {
				return action, errors.New("new fact evidence must belong to this run")
			}
			// Existing references must match a cited Fact at the server. Never
			// relabel another run's observation as freshly captured evidence.
			continue
		}
		if strings.TrimSpace(ref.Path) == "" || strings.ContainsAny(ref.Path, "\x00\r\n") || strings.Contains(ref.Path, "://") {
			return action, errors.New("evidence requires a local file path")
		}
		source := ref.Path
		if !filepath.IsAbs(source) {
			source = filepath.Join(j.Workspace, source)
		}
		raw, err := readEvidenceFile(ctx, source, maxEvidenceFileBytes)
		if err != nil {
			return action, fmt.Errorf("read selected evidence: %w", err)
		}
		if !utf8.Valid(raw) {
			return action, errors.New("evidence original must be UTF-8 text")
		}
		excerpt, err := selectedEvidence(raw, ref)
		if err != nil {
			return action, err
		}
		digest := sha256.Sum256(raw)
		ref.Path = filepath.Join(dir, hex.EncodeToString(digest[:])+".raw")
		ref.RunID, ref.Excerpt = j.RunID, excerpt
		if err = retainEvidenceFile(ctx, ref.Path, raw); err != nil {
			return action, err
		}
		refs[i] = ref
	}
	action, err = setActionEvidence(action, payload, refs)
	if err != nil {
		return action, err
	}
	prepared.Evidence = refs
	raw, err := json.Marshal(prepared)
	if err != nil {
		return action, err
	}
	if len(raw) > MaxGraphRPCBytes*4 {
		return action, errors.New("prepared evidence action is too large; select fewer or shorter excerpts")
	}
	if err = retainEvidenceFile(ctx, manifest, raw); err != nil {
		return action, err
	}
	return action, nil
}

func currentEvidence(identity, run string) bool {
	return identity == "" || identity == run || strings.HasSuffix(identity, "@"+run)
}

func setActionEvidence(action board.StateAction, payload map[string]json.RawMessage, refs []board.EvidenceRef) (board.StateAction, error) {
	raw, err := json.Marshal(refs)
	if err != nil {
		return action, err
	}
	payload["evidence"] = raw
	action.Payload, err = json.Marshal(payload)
	return action, err
}

func selectedEvidence(raw []byte, ref board.EvidenceRef) (string, error) {
	selection, err := evidenceRange(raw, ref.StartLine, ref.EndLine)
	if err != nil {
		return "", err
	}
	if ref.Excerpt != "" {
		// Legacy callers may have selected a shorter excerpt inside these
		// lines. Verify the exact bytes, without trimming or normalization.
		if !bytes.Contains(selection, []byte(ref.Excerpt)) {
			return "", errors.New("evidence excerpt differs from the selected original bytes; omit excerpt and select path/lines")
		}
		selection = []byte(ref.Excerpt)
	}
	if len(selection) > maxEvidenceExcerptBytes {
		return "", errors.New("evidence excerpt exceeds 8192 bytes; select a smaller line range")
	}
	if !utf8.Valid(selection) || strings.TrimSpace(string(selection)) == "" {
		return "", errors.New("evidence excerpt must be nonempty UTF-8 text")
	}
	return string(selection), nil
}

func evidenceRange(raw []byte, start, end int) ([]byte, error) {
	if start < 0 || end < 0 || start == 0 && end != 0 || start > 0 && end < start {
		return nil, errors.New("evidence lines require both start_line and end_line, inclusive and 1-based")
	}
	selected := raw
	if start > 0 {
		line, begin, finish := 1, -1, -1
		for offset := 0; offset < len(raw); {
			if line == start {
				begin = offset
			}
			next := bytes.IndexByte(raw[offset:], '\n')
			if next < 0 {
				offset = len(raw)
			} else {
				offset += next + 1
			}
			if line == end {
				finish = offset
				break
			}
			line++
		}
		if begin < 0 || finish < 0 {
			return nil, errors.New("evidence line range is outside the original file")
		}
		selected = raw[begin:finish]
	}
	return selected, nil
}

func readEvidenceFile(ctx context.Context, path string, limit int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() > int64(limit) {
		return nil, errors.New("evidence must be a regular file within its size limit")
	}
	raw, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if len(raw) > limit || before.Size() != int64(len(raw)) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, errors.New("evidence file changed while reading or exceeded its size limit")
	}
	return raw, ctx.Err()
}

func retainEvidenceFile(ctx context.Context, path string, raw []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if old, err := readEvidenceFile(ctx, path, maxEvidenceFileBytes); err == nil {
		if !bytes.Equal(old, raw) {
			return errors.New("retained evidence changed; refusing to overwrite it")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".capture-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err = f.Write(raw); err == nil {
		err = f.Chmod(0400)
	}
	if err == nil {
		err = f.Sync()
	}
	if err == nil {
		err = f.Close()
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = os.Link(f.Name(), path) // Publish atomically without replacing a snapshot.
	}
	if os.IsExist(err) {
		old, readErr := readEvidenceFile(ctx, path, maxEvidenceFileBytes)
		if readErr != nil || !bytes.Equal(old, raw) {
			return errors.New("conflicting retained evidence")
		}
		err = nil
	}
	if err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func verifyEvidenceSnapshot(ctx context.Context, dir, run string, original, ref board.EvidenceRef) error {
	if filepath.Dir(ref.Path) != dir || ref.RunID != run || ref.StartLine != original.StartLine || ref.EndLine != original.EndLine {
		return errors.New("prepared evidence has invalid snapshot identity")
	}
	raw, err := readEvidenceFile(ctx, ref.Path, maxEvidenceFileBytes)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(raw)
	excerpt, err := selectedEvidence(raw, original)
	if err != nil || !utf8.Valid(raw) || filepath.Base(ref.Path) != hex.EncodeToString(digest[:])+".raw" || excerpt != ref.Excerpt {
		return errors.New("retained evidence failed its content hash or excerpt check")
	}
	return nil
}
