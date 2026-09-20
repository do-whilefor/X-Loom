//go:build linux

package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestFileToolsRejectFIFOWithoutBlocking(t *testing.T) {
	s := Set{Dir: t.TempDir(), RunDir: t.TempDir()}
	if err := syscall.Mkfifo(filepath.Join(s.Dir, "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"read", "edit", "write", "ls"} {
		raw := map[string]string{"read": `{"path":"pipe"}`, "edit": `{"path":"pipe","oldText":"a","newText":"b"}`, "write": `{"path":"pipe","content":"a"}`, "ls": `{"path":"pipe"}`}[name]
		for _, tool := range s.All() {
			if tool.Name == name {
				if _, err := tool.Execute(context.Background(), json.RawMessage(raw)); err == nil {
					t.Fatalf("%s accepted FIFO", name)
				}
			}
		}
	}
}

func TestLongOutputsAreSaved(t *testing.T) {
	for _, name := range []string{"read", "bash", "grep", "find", "ls"} {
		t.Run(name, func(t *testing.T) {
			s := Set{Dir: t.TempDir(), RunDir: t.TempDir(), OutputBytes: 40}
			for i := 0; i < 10; i++ {
				if err := os.WriteFile(filepath.Join(s.Dir, strings.Repeat("a", i+1)+".txt"), []byte(strings.Repeat("evidence ", 30)+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			raw := map[string]string{"read": `{"path":"a.txt"}`, "bash": `{"command":"printf '%200s' evidence"}`, "grep": `{"pattern":"evidence"}`, "find": `{"pattern":"*.txt"}`, "ls": `{}`}[name]
			var got string
			var err error
			for _, tool := range s.All() {
				if tool.Name == name {
					got, err = tool.Execute(context.Background(), json.RawMessage(raw))
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			marker := "Full output: "
			at := strings.Index(got, marker)
			if at < 0 {
				t.Fatalf("missing saved output: %s", got)
			}
			path := strings.SplitN(got[at+len(marker):], "]", 2)[0]
			full, err := os.ReadFile(path)
			if err != nil || len(full) <= 40 {
				t.Fatal(len(full), err)
			}
		})
	}
}

func TestLongOutputRetainsBeginningAndFinalEvidence(t *testing.T) {
	s := Set{Dir: t.TempDir(), RunDir: t.TempDir(), OutputBytes: 80}
	full := "BEGIN_PROOF\n" + strings.Repeat("middle noise\n", 1000) + "FINAL_PROOF\n"
	if err := os.WriteFile(filepath.Join(s.Dir, "proof.txt"), []byte(full), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := s.read(context.Background(), json.RawMessage(`{"path":"proof.txt"}`))
	if err != nil || !strings.Contains(out, "BEGIN_PROOF") || !strings.Contains(out, "FINAL_PROOF") || !strings.Contains(out, "Middle omitted") {
		t.Fatal(out, err)
	}
	files, _ := filepath.Glob(filepath.Join(s.RunDir, "output-*.txt"))
	if len(files) != 1 {
		t.Fatal(files)
	}
	saved, err := os.ReadFile(files[0])
	if err != nil || string(saved) != full {
		t.Fatal("original evidence was truncated", err)
	}
}
func TestReadOffsetAndArgumentValidation(t *testing.T) {
	s := Set{Dir: t.TempDir(), RunDir: t.TempDir()}
	os.WriteFile(filepath.Join(s.Dir, "a"), []byte("one\ntwo\nthree\n"), 0600)
	got, err := s.All()[0].Execute(context.Background(), json.RawMessage(`{"path":"a","offset":2,"limit":1}`))
	if err != nil || !strings.HasPrefix(got, "two\n") || strings.Contains(got, "three") {
		t.Fatal(got, err)
	}
	for _, raw := range []string{`{"path":"a","offset":0}`, `{"path":"a","limit":null}`, `{"path":"a","unknown":1}`, `{"path":"a","limit":1.2}`} {
		if _, err := s.All()[0].Execute(context.Background(), json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
func TestToolTimeoutCeilingAndNoMatches(t *testing.T) {
	s := Set{Dir: t.TempDir(), RunDir: t.TempDir(), ToolTimeout: 20 * time.Millisecond}
	if _, err := s.bash(context.Background(), json.RawMessage(`{"command":"sleep 5","timeout":60}`)); err == nil {
		t.Fatal("tool ignored timeout ceiling")
	}
	s.ToolTimeout = 5 * time.Second
	os.WriteFile(filepath.Join(s.Dir, "a.txt"), []byte("hello"), 0600)
	if out, err := s.grep(context.Background(), json.RawMessage(`{"pattern":"absent"}`)); err != nil || out != "No matches" {
		t.Fatal(out, err)
	}
	if out, err := s.find(context.Background(), json.RawMessage(`{"pattern":"*.missing"}`)); err != nil || out != "No matches" {
		t.Fatal(out, err)
	}
}
func TestAmbiguousEditDoesNotChangeFile(t *testing.T) {
	s := Set{Dir: t.TempDir(), RunDir: t.TempDir()}
	p := filepath.Join(s.Dir, "a")
	os.WriteFile(p, []byte("same same"), 0600)
	if _, err := s.edit(context.Background(), json.RawMessage(`{"path":"a","oldText":"same","newText":"changed"}`)); err == nil {
		t.Fatal("ambiguous edit succeeded")
	}
	raw, _ := os.ReadFile(p)
	if string(raw) != "same same" {
		t.Fatal("failed edit mutated file")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.write(ctx, json.RawMessage(`{"path":"new","content":"x"}`)); err == nil {
		t.Fatal("canceled write succeeded")
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "new")); !os.IsNotExist(err) {
		t.Fatal("canceled write created file")
	}
}
