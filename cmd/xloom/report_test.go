//go:build linux

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReportCommand(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "source.json"), []byte(`{"findings":[],"summary":"No evidence collected"}`), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if err := run(context.Background(), []string{"report", "--root", root, "--source", "source.json", "--output", "report"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"evidence_files":0`) || !strings.Contains(out.String(), "separate review") {
		t.Fatalf("stdout = %s", &out)
	}
	for _, suffix := range []string{".json", ".md", ".index.json"} {
		if _, err := os.Stat(filepath.Join(root, "report"+suffix)); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"report"}, {"report", "--source", "source.json"}, {"report", "extra"}, {"report", "--unknown"}} {
		if err := run(context.Background(), args, &out, &errOut); err == nil {
			t.Fatalf("accepted invalid args: %v", args)
		}
	}
	out.Reset()
	if err := run(context.Background(), []string{"--help"}, &out, &errOut); err != nil || !strings.Contains(out.String(), "report") {
		t.Fatalf("usage = %s, err = %v", &out, err)
	}
	if err := run(context.Background(), []string{"report", "--help"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
}
