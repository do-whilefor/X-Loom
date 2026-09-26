package report

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func put(t *testing.T, root, path, text string) string {
	t.Helper()
	path = filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRenderSingleSourceAndDeduplicatedEvidence(t *testing.T) {
	root := t.TempDir()
	path := put(t, root, "evidence/http.txt", "HTTP/1.1 200 OK\n\n{\"amount\":9007199254740993}")
	hash := sha256.Sum256([]byte("HTTP/1.1 200 OK\n\n{\"amount\":9007199254740993}"))
	source := fmt.Sprintf(`{
		"title": "Example report",
		"findings": [{"title":"Observed response", "amount":9007199254740993,
			"evidence_paths":["evidence/http.txt", %q],
			"details":{"evidence":[{"path":"evidence/http.txt","start_line":1,"end_line":3,"sha256":%q},
				{"path":"evidence/http.txt","start_line":1,"end_line":3}]}}],
		"not_evidence":{"path":"need-not-exist"}, "a/b~c": []
	}`, filepath.ToSlash(path), strings.ToUpper(hex.EncodeToString(hash[:])))
	got, err := Render(root, strings.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got.JSON, []byte("9007199254740993")) || !bytes.Contains(got.Markdown, []byte("9007199254740993")) {
		t.Fatal("large integer lost precision")
	}
	if !bytes.Contains(got.JSON, []byte(filepath.ToSlash(path))) {
		t.Fatal("source absolute path changed")
	}
	decoder := json.NewDecoder(bytes.NewReader(got.JSON))
	decoder.UseNumber()
	var decoded, original any
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	originalDecoder := json.NewDecoder(strings.NewReader(source))
	originalDecoder.UseNumber()
	if err := originalDecoder.Decode(&original); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, original) {
		t.Fatal("report JSON changed source fields")
	}
	wantCounts := Counts{Arrays: map[string]int{"/findings": 1, "/findings/0/evidence_paths": 2, "/findings/0/details/evidence": 2, "/a~1b~0c": 0}, EvidenceReferences: 4, EvidenceFiles: 1}
	if !reflect.DeepEqual(got.Summary.Counts, wantCounts) {
		t.Fatalf("counts = %#v", got.Summary.Counts)
	}
	file := got.Summary.Evidence[0]
	if file.Path != "evidence/http.txt" || file.Lines != 3 || file.SHA256 != hex.EncodeToString(hash[:]) || len(file.References) != 2 {
		t.Fatalf("evidence = %#v", file)
	}
	for _, ref := range file.References {
		if len(ref.Sources) != 2 {
			t.Fatalf("duplicate ranges not merged: %#v", ref)
		}
	}
	if !strings.Contains(got.Summary.ValidationScope, "factual support and numeric claims require separate review") {
		t.Fatal("mechanical checks must not claim factual verification")
	}
	second, err := Render(root, strings.NewReader(source))
	if err != nil || !reflect.DeepEqual(got, second) {
		t.Fatalf("render was not deterministic: %v", err)
	}
}

func TestRenderRejectsInvalidSourceOrEvidence(t *testing.T) {
	root := t.TempDir()
	put(t, root, "e.txt", "one\ntwo\n")
	outside := put(t, t.TempDir(), "outside.txt", "outside")
	cases := map[string]string{
		"null source": "null", "array source": "[]", "trailing JSON": `{} {}`,
		"trailing junk": `{} x`, "missing": `{"evidence":"missing"}`,
		"empty": `{"evidence":""}`, "null reference": `{"evidence":null}`,
		"wrong object": `{"evidence":{"text":"e.txt"}}`, "invalid path": `{"evidence":17}`,
		"escape": `{"evidence":"../outside.txt"}`, "absolute escape": fmt.Sprintf(`{"evidence":%q}`, filepath.ToSlash(outside)),
		"directory": `{"evidence":"."}`, "backslash": `{"evidence":"a\\b"}`,
		"range start only": `{"evidence":{"path":"e.txt","start_line":1}}`,
		"range end only":   `{"evidence":{"path":"e.txt","end_line":1}}`,
		"range zero":       `{"evidence":{"path":"e.txt","start_line":0,"end_line":1}}`,
		"range fractional": `{"evidence":{"path":"e.txt","start_line":1.5,"end_line":2}}`,
		"range overflow":   `{"evidence":{"path":"e.txt","start_line":9223372036854775808,"end_line":2}}`,
		"range string":     `{"evidence":{"path":"e.txt","start_line":"1","end_line":2}}`,
		"range reversed":   `{"evidence":{"path":"e.txt","start_line":2,"end_line":1}}`,
		"range too long":   `{"evidence":{"path":"e.txt","start_line":1,"end_line":3}}`,
		"invalid hash":     `{"evidence":{"path":"e.txt","sha256":"not-a-hash"}}`,
		"wrong hash":       `{"evidence":{"path":"e.txt","sha256":"0000000000000000000000000000000000000000000000000000000000000000"}}`,
	}
	for name, source := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Render(root, strings.NewReader(source)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestLineCountingAndMarkdown(t *testing.T) {
	root := t.TempDir()
	for _, content := range []string{"", "one", "one\n", "one\ntwo", "\n\n", strings.Repeat("x", 65536), strings.Repeat("x", 65536) + "\n"} {
		path := put(t, root, "e.txt", content)
		file, err := inspectFile(path)
		if err != nil {
			t.Fatal(err)
		}
		want := strings.Count(content, "\n")
		if content != "" && !strings.HasSuffix(content, "\n") {
			want++
		}
		if file.Lines != want || file.Bytes != int64(len(content)) {
			t.Fatalf("size %d: file = %#v", len(content), file)
		}
	}
	got, err := Render(root, strings.NewReader(`{"text":"<b>[link](url)\n# heading","value":false,"nothing":null,"nested":{"items":[1,"two",{}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"&lt;b&gt;\\[link\\](url)", "\\# heading", "false", "null", "two", "{}"} {
		if !bytes.Contains(got.Markdown, []byte(text)) {
			t.Errorf("markdown missing %q: %s", text, got.Markdown)
		}
	}
	want := "# Report\n\n- **findings**:\n  -\n    - **title**: one  \n      continued\n  -\n    - **title**: two\n"
	got, err = Render(root, strings.NewReader(`{"findings":[{"title":"one\ncontinued"},{"title":"two"}]}`))
	if err != nil || string(got.Markdown) != want {
		t.Fatalf("nested multiline markdown = %s, err = %v", got.Markdown, err)
	}
}

func TestGenerateProtectsSourceEvidenceAndOutputs(t *testing.T) {
	root := t.TempDir()
	put(t, root, "source.json", `{"findings":[{"evidence_paths":["e.txt"]}]}`)
	put(t, root, "e.txt", "evidence")
	index, err := Generate(root, "source.json", "report")
	if err != nil {
		t.Fatal(err)
	}
	if index.Counts.EvidenceFiles != 1 {
		t.Fatalf("index = %#v", index)
	}
	for _, suffix := range []string{".json", ".md", ".index.json"} {
		if _, err := os.Stat(filepath.Join(root, "report"+suffix)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Generate(root, "source.json", "report"); err != nil {
		t.Fatalf("replace existing report: %v", err)
	}
	leftovers, err := filepath.Glob(filepath.Join(root, ".xloom-report-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("temporary files not removed: %v, err = %v", leftovers, err)
	}
	for _, output := range []string{"source", "../escaped", "missing/report", ""} {
		if _, err := Generate(root, "source.json", output); err == nil {
			t.Fatalf("unsafe output accepted: %q", output)
		}
	}
	for _, suffix := range []string{".json", ".md", ".index.json"} {
		t.Run(suffix, func(t *testing.T) {
			isolated := t.TempDir()
			put(t, isolated, "source.json", fmt.Sprintf(`{"evidence":%q}`, "report"+suffix))
			put(t, isolated, "report"+suffix, "original evidence")
			if _, err := Generate(isolated, "source.json", "report"); err == nil {
				t.Fatal("evidence overwrite accepted")
			}
			data, err := os.ReadFile(filepath.Join(isolated, "report"+suffix))
			if err != nil || string(data) != "original evidence" {
				t.Fatal("evidence changed")
			}
		})
	}
	put(t, root, "invalid.json", `{"evidence":"missing.txt"}`)
	if _, err := Generate(root, "invalid.json", "failed"); err == nil {
		t.Fatal("invalid report accepted")
	}
	if _, err := os.Stat(filepath.Join(root, "failed.json")); !os.IsNotExist(err) {
		t.Fatal("output written before validation")
	}
}

func TestSymlinksAndHardlinks(t *testing.T) {
	root := t.TempDir()
	outside := put(t, t.TempDir(), "outside.txt", "outside")
	inside := put(t, root, "inside.txt", "inside")
	if err := os.Symlink(outside, filepath.Join(root, "escape.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := Render(root, strings.NewReader(`{"evidence":"escape.txt"}`)); err == nil {
		t.Fatal("symlink escape accepted")
	}
	if err := os.Symlink(inside, filepath.Join(root, "alias.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := Render(root, strings.NewReader(`{"evidence":["alias.txt","inside.txt"]}`)); err != nil {
		t.Fatal(err)
	}
	put(t, root, "source.json", `{"evidence":"inside.txt"}`)
	if err := os.Symlink(outside, filepath.Join(root, "report.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(root, "source.json", "report"); err == nil {
		t.Fatal("symlink output accepted")
	}
	if _, err := os.Stat(filepath.Join(root, "report.json")); !os.IsNotExist(err) {
		t.Fatal("earlier sibling written before checking output symlink")
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(root, "outside")); err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(root, "source.json", "outside/report"); err == nil {
		t.Fatal("output parent symlink escape accepted")
	}
	if err := os.Link(inside, filepath.Join(root, "hardlink.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(root, "source.json", "hardlink"); err == nil {
		t.Fatal("evidence hardlink overwrite accepted")
	}
}
