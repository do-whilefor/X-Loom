//go:build linux

package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func fidelityFixture(t *testing.T) []byte {
	t.Helper()
	data := []byte(strings.Join([]string{
		`{`,
		`  "escaped": "quote: \" slash: \\ newline: \n tab: \t literal: \\n",`,
		"  \"unicode\": \"é/e\u0301/雪/😀\",",
		`  "integer": 900719925474099312345678901,`,
		`  "surrogates": "\uD83D\uDE00",`,
		`  "nested": "{\"path\":\"C:\\\\temp\"}"`,
		`}`,
	}, "\r\n"))
	if !json.Valid(data) {
		t.Fatal("invalid JSON test fixture")
	}
	return data
}

func fidelitySet(t *testing.T) *Set {
	t.Helper()
	dir := t.TempDir()
	return &Set{Dir: dir, RunDir: filepath.Join(dir, "run"), OutputBytes: 1 << 20}
}

func callFidelityTool(t *testing.T, s *Set, ctx context.Context, name string, args map[string]any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range s.All() {
		if tool.Definition.Name == name {
			return tool.Execute(ctx, raw)
		}
	}
	t.Fatalf("missing tool %q", name)
	return "", nil
}

func putFidelityFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0640); err != nil {
		t.Fatal(err)
	}
}

func assertFidelityFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("file bytes changed: got %q, want %q", got, want)
	}
}

func fixtureHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestReadPreservesOriginalTextBytes(t *testing.T) {
	s := fidelitySet(t)
	for _, tc := range []struct {
		name   string
		data   []byte
		offset int
		limit  int
		want   string
	}{
		{"complex_json", fidelityFixture(t), 1, 7, string(fidelityFixture(t))},
		{"crlf_at_limit", []byte("one\r\ntwo\r\n"), 1, 2, "one\r\ntwo\r\n"},
		{"lf_at_limit", []byte("one\ntwo\n"), 1, 2, "one\ntwo\n"},
		{"no_final_newline", []byte("one\r\ntwo"), 2, 1, "two"},
		{"empty", nil, 1, 1, ""},
		{"offset_past_end", []byte("one\n"), 2, 1, ""},
		{"long_line", []byte(strings.Repeat("é", 9000) + "\r\n"), 1, 1, strings.Repeat("é", 9000) + "\r\n"},
		{"long_final_line", []byte(strings.Repeat("e\u0301", 9000)), 1, 1, strings.Repeat("e\u0301", 9000)},
		{"truncated", []byte("one\r\ntwo"), 1, 1, "one\r\n\n[Read limited; continue reading source with offset/limit.]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			putFidelityFile(t, filepath.Join(s.Dir, "source"), tc.data)
			got, err := callFidelityTool(t, s, context.Background(), "read", map[string]any{"path": "source", "offset": tc.offset, "limit": tc.limit})
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("read changed text: got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReadTruncationDoesNotInventReplacementCharacters(t *testing.T) {
	s := fidelitySet(t)
	s.OutputBytes = 11
	original := []byte(strings.Repeat("😀", 10))
	putFidelityFile(t, filepath.Join(s.Dir, "source"), original)
	got, err := callFidelityTool(t, s, context.Background(), "read", map[string]any{"path": "source"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "�") || !strings.HasPrefix(got, "😀\n[Middle omitted. Full output: ") || !strings.HasSuffix(got, "]\n😀") {
		t.Fatalf("UTF-8 preview changed source characters: %q", got)
	}
	files, err := filepath.Glob(filepath.Join(s.RunDir, "output-*.txt"))
	if err != nil || len(files) != 1 {
		t.Fatal("full output not retained", err)
	}
	assertFidelityFile(t, files[0], original)
}

func TestWriteCopiesBytesAndReportsProvenance(t *testing.T) {
	s := fidelitySet(t)
	source := fidelityFixture(t)
	putFidelityFile(t, filepath.Join(s.Dir, "source.json"), source)
	lines := bytes.SplitAfter(source, []byte("\n"))
	selected := bytes.Join(lines[1:3], nil)
	for _, tc := range []struct {
		name string
		args map[string]any
		want []byte
	}{
		{"whole", map[string]any{}, source},
		{"hash_checked", map[string]any{"source_sha256": strings.ToUpper(fixtureHash(source))}, source},
		{"line_range", map[string]any{"source_start_line": 2, "source_end_line": 3, "source_sha256": fixtureHash(source)}, selected},
		{"last_line", map[string]any{"source_start_line": 7, "source_end_line": 7}, []byte("}")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.args["path"] = "nested/copy.json"
			tc.args["source_path"] = "source.json"
			out, err := callFidelityTool(t, s, context.Background(), "write", tc.args)
			if err != nil {
				t.Fatal(err)
			}
			assertFidelityFile(t, filepath.Join(s.Dir, "nested/copy.json"), tc.want)
			var receipt writeReceipt
			if err := json.Unmarshal([]byte(out), &receipt); err != nil {
				t.Fatal(err)
			}
			if receipt.Mode != "copied" || receipt.Bytes != len(tc.want) || receipt.SHA256 != fixtureHash(tc.want) || receipt.SourceSHA256 != fixtureHash(source) || receipt.Path != "nested/copy.json" {
				t.Fatalf("incorrect copy receipt: %s", out)
			}
		})
	}
}

func TestWriteGeneratedTextReportsGeneratedMode(t *testing.T) {
	s := fidelitySet(t)
	for _, data := range [][]byte{nil, fidelityFixture(t)} {
		out, err := callFidelityTool(t, s, context.Background(), "write", map[string]any{"path": "generated", "content": string(data)})
		if err != nil {
			t.Fatal(err)
		}
		assertFidelityFile(t, filepath.Join(s.Dir, "generated"), data)
		var receipt writeReceipt
		if err := json.Unmarshal([]byte(out), &receipt); err != nil {
			t.Fatal(err)
		}
		if receipt.Mode != "generated" || receipt.Bytes != len(data) || receipt.SHA256 != fixtureHash(data) || receipt.SourceSHA256 != "" {
			t.Fatalf("incorrect generated receipt: %s", out)
		}
	}
}

func TestWriteCopyRejectsInvalidInputsWithoutChangingDestination(t *testing.T) {
	s := fidelitySet(t)
	putFidelityFile(t, filepath.Join(s.Dir, "source"), []byte("first\r\nsecond\n"))
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"neither", map[string]any{}},
		{"both", map[string]any{"source_path": "source", "content": ""}},
		{"empty_source", map[string]any{"source_path": ""}},
		{"missing_source", map[string]any{"source_path": "missing"}},
		{"null_source", map[string]any{"source_path": nil}},
		{"null_content", map[string]any{"content": nil}},
		{"hash_mismatch", map[string]any{"source_path": "source", "source_sha256": strings.Repeat("0", 64)}},
		{"hash_invalid", map[string]any{"source_path": "source", "source_sha256": strings.Repeat("z", 64)}},
		{"hash_short", map[string]any{"source_path": "source", "source_sha256": "abc"}},
		{"hash_with_content", map[string]any{"content": "created", "source_sha256": strings.Repeat("0", 64)}},
		{"range_with_content", map[string]any{"content": "created", "source_start_line": 1, "source_end_line": 1}},
		{"missing_start", map[string]any{"source_path": "source", "source_end_line": 1}},
		{"missing_end", map[string]any{"source_path": "source", "source_start_line": 1}},
		{"zero_start", map[string]any{"source_path": "source", "source_start_line": 0, "source_end_line": 1}},
		{"reverse_range", map[string]any{"source_path": "source", "source_start_line": 2, "source_end_line": 1}},
		{"range_after_eof", map[string]any{"source_path": "source", "source_start_line": 3, "source_end_line": 3}},
		{"end_after_eof", map[string]any{"source_path": "source", "source_start_line": 1, "source_end_line": 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			putFidelityFile(t, filepath.Join(s.Dir, "target"), []byte("original"))
			tc.args["path"] = "target"
			if _, err := callFidelityTool(t, s, context.Background(), "write", tc.args); err == nil {
				t.Fatal("invalid copy accepted")
			}
			assertFidelityFile(t, filepath.Join(s.Dir, "target"), []byte("original"))
		})
	}
}

func TestWriteCopyEmptyAndSameFile(t *testing.T) {
	s := fidelitySet(t)
	path := filepath.Join(s.Dir, "source")
	for _, data := range [][]byte{nil, {0, 0xff, '\r', '\n', 0xfe}, fidelityFixture(t)} {
		putFidelityFile(t, path, data)
		_, err := callFidelityTool(t, s, context.Background(), "write", map[string]any{"path": "source", "source_path": "source", "source_sha256": fixtureHash(data)})
		if err != nil {
			t.Fatal(err)
		}
		assertFidelityFile(t, path, data)
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0640 {
			t.Fatalf("copy did not preserve target permissions: %v, %v", info, err)
		}
	}
	_, err := callFidelityTool(t, s, context.Background(), "write", map[string]any{"path": "source", "source_path": "source", "source_start_line": 7, "source_end_line": 7})
	if err != nil {
		t.Fatal(err)
	}
	assertFidelityFile(t, path, []byte("}"))
	putFidelityFile(t, path, nil)
	if _, err := callFidelityTool(t, s, context.Background(), "write", map[string]any{"path": "source", "source_path": "source", "source_start_line": 1, "source_end_line": 1}); err == nil {
		t.Fatal("empty file should have no selectable lines")
	}
}

func TestCanceledCopyStagingPreservesDestination(t *testing.T) {
	s := fidelitySet(t)
	path := filepath.Join(s.Dir, "target")
	putFidelityFile(t, path, []byte("original"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := atomicCopyWrite(ctx, path, fidelityFixture(t)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled staging: %v", err)
	}
	assertFidelityFile(t, path, []byte("original"))
	entries, err := filepath.Glob(filepath.Join(s.Dir, ".xloom-copy-*"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("canceled copy left temporary files: %v, %v", entries, err)
	}
}

func TestWriteCopyRejectsOversizeSourceAndCancellation(t *testing.T) {
	s := fidelitySet(t)
	source := filepath.Join(s.Dir, "source")
	putFidelityFile(t, source, nil)
	if err := os.Truncate(source, maxCopyBytes+1); err != nil {
		t.Fatal(err)
	}
	putFidelityFile(t, filepath.Join(s.Dir, "target"), []byte("original"))
	args := map[string]any{"path": "target", "source_path": "source"}
	if _, err := callFidelityTool(t, s, context.Background(), "write", args); err == nil {
		t.Fatal("oversize source accepted")
	}
	assertFidelityFile(t, filepath.Join(s.Dir, "target"), []byte("original"))
	putFidelityFile(t, source, fidelityFixture(t))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := callFidelityTool(t, s, ctx, "write", args); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled copy: %v", err)
	}
	assertFidelityFile(t, filepath.Join(s.Dir, "target"), []byte("original"))
	entries, err := filepath.Glob(filepath.Join(s.Dir, ".xloom-copy-*"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("temporary copies remain: %v, %v", entries, err)
	}
}

func TestWriteCopyRejectsSpecialFilesWithoutBlocking(t *testing.T) {
	s := fidelitySet(t)
	putFidelityFile(t, filepath.Join(s.Dir, "source"), []byte("text"))
	putFidelityFile(t, filepath.Join(s.Dir, "target"), []byte("original"))
	if err := syscall.Mkfifo(filepath.Join(s.Dir, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("fifo", filepath.Join(s.Dir, "fifo-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(s.Dir, "target-link")); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"fifo", "fifo-link", ".", "/dev/null"} {
		if _, err := callFidelityTool(t, s, context.Background(), "write", map[string]any{"path": "target", "source_path": source}); err == nil {
			t.Fatalf("special source %s accepted", source)
		}
		assertFidelityFile(t, filepath.Join(s.Dir, "target"), []byte("original"))
	}
	for _, target := range []string{"fifo", "fifo-link", "target-link", ".", "/dev/null"} {
		if _, err := callFidelityTool(t, s, context.Background(), "write", map[string]any{"path": target, "source_path": "source"}); err == nil {
			t.Fatalf("special destination %s accepted", target)
		}
	}
	assertFidelityFile(t, filepath.Join(s.Dir, "target"), []byte("original"))
	if _, err := callFidelityTool(t, s, context.Background(), "write", map[string]any{"path": "copy", "source_path": "target-link"}); err != nil {
		t.Fatalf("symlink to regular source rejected: %v", err)
	}
	assertFidelityFile(t, filepath.Join(s.Dir, "copy"), []byte("original"))
}
