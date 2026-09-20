//go:build linux

package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExactToolsAndEditing(t *testing.T) {
	s := Set{Dir: t.TempDir(), RunDir: t.TempDir()}
	names := []string{}
	for _, tool := range s.All() {
		names = append(names, tool.Name)
	}
	if strings.Join(names, ",") != "read,bash,edit,write,grep,find,ls" {
		t.Fatal(names)
	}
	ctx := context.Background()
	if _, err := s.write(ctx, json.RawMessage(`{"path":"a.txt","content":"one two"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.edit(ctx, json.RawMessage(`{"path":"a.txt","oldText":"one","newText":"three"}`)); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(s.Dir, "a.txt"))
	if string(data) != "three two" {
		t.Fatal(string(data))
	}
	if _, err := s.edit(ctx, json.RawMessage(`{"path":"a.txt","oldText":"missing","newText":"x"}`)); err == nil {
		t.Fatal("accepted missing match")
	}
}
func TestShellTimeout(t *testing.T) {
	s := Set{Dir: t.TempDir(), RunDir: t.TempDir()}
	_, err := s.bash(context.Background(), json.RawMessage(`{"command":"sleep 30 & wait","timeout":1}`))
	if err == nil {
		t.Fatal("timeout did not cancel process group")
	}
}
