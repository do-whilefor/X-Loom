// Package tools exposes exactly the seven initial X-Loom tools.
package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"xloom/internal/agent"
	"xloom/internal/process"
)

type Set struct {
	Dir         string
	RunDir      string
	OutputBytes int
}

func (s *Set) All() []agent.Tool {
	def := func(name, desc, schema string, parallel, conclude bool, fn func(context.Context, json.RawMessage) (string, error)) agent.Tool {
		return agent.Tool{Definition: agent.Definition{Name: name, Description: desc, Schema: json.RawMessage(schema)}, Parallel: parallel, Conclude: conclude, Execute: fn}
	}
	return []agent.Tool{
		def("read", "Read a text file with optional 1-based offset and line limit.", `{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer","minimum":1},"limit":{"type":"integer","minimum":1}},"required":["path"],"additionalProperties":false}`, true, true, s.read),
		def("bash", "Run a bash command in the project workspace. Long output is saved to a file.", `{"type":"object","properties":{"command":{"type":"string"},"timeout":{"type":"integer","minimum":1}},"required":["command"],"additionalProperties":false}`, false, false, s.bash),
		def("edit", "Replace exactly one occurrence of oldText in a UTF-8 file.", `{"type":"object","properties":{"path":{"type":"string"},"oldText":{"type":"string"},"newText":{"type":"string"}},"required":["path","oldText","newText"],"additionalProperties":false}`, false, false, s.edit),
		def("write", "Write a UTF-8 file, creating parent directories.", `{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"],"additionalProperties":false}`, false, false, s.write),
		def("grep", "Search file contents with ripgrep; regex by default.", `{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"},"glob":{"type":"string"},"ignoreCase":{"type":"boolean"},"literal":{"type":"boolean"}},"required":["pattern"],"additionalProperties":false}`, true, true, s.grep),
		def("find", "Find file paths matching a glob, including hidden files.", `{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"}},"required":["pattern"],"additionalProperties":false}`, true, true, s.find),
		def("ls", "List entries in a directory.", `{"type":"object","properties":{"path":{"type":"string"}},"additionalProperties":false}`, true, true, s.ls),
	}
}
func decode(raw json.RawMessage, dst any, required ...string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	if fields == nil {
		return errors.New("arguments must be an object")
	}
	for _, k := range required {
		v, ok := fields[k]
		if !ok || string(v) == "null" {
			return fmt.Errorf("missing argument %s", k)
		}
	}
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	return d.Decode(dst)
}
func (s *Set) path(path string) string {
	if path == "" {
		return s.Dir
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(s.Dir, path)
}
func (s *Set) limit() int {
	if s.OutputBytes > 0 {
		return s.OutputBytes
	}
	return 32000
}
func (s *Set) output(f *os.File, err error) (string, error) {
	if _, seekErr := f.Seek(0, 0); seekErr != nil {
		return "", errors.Join(err, seekErr)
	}
	data, readErr := io.ReadAll(io.LimitReader(f, int64(s.limit()+1)))
	text := string(data)
	if len(data) > s.limit() {
		text = string(data[:s.limit()]) + "\n[Output truncated. Full output: " + f.Name() + "]"
	}
	return strings.ToValidUTF8(text, "�"), errors.Join(err, readErr)
}
func (s *Set) run(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	if err := os.MkdirAll(s.RunDir, 0700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(s.RunDir, "output-*.txt")
	if err != nil {
		return "", err
	}
	defer f.Close()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err = process.Run(ctx, s.Dir, s.RunDir, f, name, args...)
	return s.output(f, err)
}
func (s *Set) read(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := decode(raw, &a, "path"); err != nil {
		return "", err
	}
	if a.Path == "" {
		return "", errors.New("path must not be empty")
	}
	if a.Offset < 0 || a.Limit < 0 {
		return "", errors.New("offset and limit must be positive")
	}
	if a.Offset == 0 {
		a.Offset = 1
	}
	if a.Limit == 0 {
		a.Limit = 2000
	}
	f, err := os.Open(s.path(a.Path))
	if err != nil {
		return "", err
	}
	defer f.Close()
	reader := bufio.NewReader(f)
	var out strings.Builder
	line := 1
	read := 0
	truncated := false
	for {
		if err := ctx.Err(); err != nil {
			return out.String(), err
		}
		piece, prefix, err := reader.ReadLine()
		if err != nil && err != io.EOF {
			return "", err
		}
		if err == io.EOF {
			break
		}
		if line >= a.Offset {
			if out.Len()+len(piece)+1 > s.limit() {
				remaining := s.limit() - out.Len()
				if remaining > 0 {
					out.Write(piece[:min(remaining, len(piece))])
				}
				truncated = true
				break
			}
			out.Write(piece)
			if !prefix {
				out.WriteByte('\n')
			}
		}
		if !prefix {
			if line >= a.Offset {
				read++
				if read >= a.Limit {
					truncated = true
					break
				}
			}
			line++
		}
	}
	if truncated {
		fmt.Fprintf(&out, "\n[Read limited; continue reading %s with offset/limit.]", a.Path)
	}
	return strings.ToValidUTF8(out.String(), "�"), nil
}
func (s *Set) bash(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}
	if err := decode(raw, &a, "command"); err != nil {
		return "", err
	}
	if strings.TrimSpace(a.Command) == "" {
		return "", errors.New("command must not be empty")
	}
	if a.Timeout < 0 {
		return "", errors.New("timeout must be positive")
	}
	if a.Timeout == 0 {
		a.Timeout = 120
	}
	return s.run(ctx, time.Duration(a.Timeout)*time.Second, "bash", "-lc", a.Command)
}
func (s *Set) edit(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Path string `json:"path"`
		Old  string `json:"oldText"`
		New  string `json:"newText"`
	}
	if err := decode(raw, &a, "path", "oldText", "newText"); err != nil {
		return "", err
	}
	if a.Path == "" || a.Old == "" {
		return "", errors.New("path and oldText must not be empty")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	path := s.path(a.Path)
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	data, err := io.ReadAll(io.LimitReader(f, 16<<20+1))
	f.Close()
	if err != nil {
		return "", err
	}
	if len(data) > 16<<20 {
		return "", errors.New("file is too large for edit; use a targeted command")
	}
	if strings.Count(string(data), a.Old) != 1 {
		return "", errors.New("oldText must match exactly once")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	err = os.WriteFile(path, []byte(strings.Replace(string(data), a.Old, a.New, 1)), info.Mode().Perm())
	return "Edited " + a.Path, err
}
func (s *Set) write(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := decode(raw, &a, "path", "content"); err != nil {
		return "", err
	}
	if a.Path == "" {
		return "", errors.New("path must not be empty")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	path := s.path(a.Path)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return "", err
	}
	err := os.WriteFile(path, []byte(a.Content), 0644)
	return "Wrote " + a.Path, err
}
func (s *Set) grep(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
		Glob    string `json:"glob"`
		Ignore  bool   `json:"ignoreCase"`
		Literal bool   `json:"literal"`
	}
	if err := decode(raw, &a, "pattern"); err != nil {
		return "", err
	}
	args := []string{"--line-number", "--hidden", "--color", "never"}
	if a.Glob != "" {
		args = append(args, "--glob", a.Glob)
	}
	if a.Ignore {
		args = append(args, "-i")
	}
	if a.Literal {
		args = append(args, "-F")
	}
	args = append(args, "--", a.Pattern, s.path(a.Path))
	out, err := s.run(ctx, 30*time.Second, "rg", args...)
	if err != nil && out == "" && err.Error() == "exit status 1" {
		return "No matches", nil
	}
	return out, err
}
func (s *Set) find(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}
	if err := decode(raw, &a, "pattern"); err != nil {
		return "", err
	}
	return s.run(ctx, 30*time.Second, "rg", "--files", "--hidden", "--glob", a.Pattern, "--", s.path(a.Path))
}
func (s *Set) ls(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := decode(raw, &a); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(s.path(a.Path))
	if err != nil {
		return "", err
	}
	var out strings.Builder
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		out.WriteString(e.Name())
		if e.IsDir() {
			out.WriteByte('/')
		}
		out.WriteByte('\n')
		if out.Len() >= s.limit() {
			out.WriteString("[Listing truncated; use find to narrow results.]\n")
			break
		}
	}
	return out.String(), nil
}
