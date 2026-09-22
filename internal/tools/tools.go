//go:build linux

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
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"xloom/internal/agent"
	"xloom/internal/process"
)

type Set struct {
	Dir         string
	RunDir      string
	OutputBytes int
	ToolTimeout time.Duration
}

func (s *Set) All() []agent.Tool {
	def := func(name, desc, schema string, parallel, conclude bool, fn func(context.Context, json.RawMessage) (string, error)) agent.Tool {
		return agent.Tool{Definition: agent.Definition{Name: name, Description: desc, Schema: json.RawMessage(schema)}, Parallel: parallel, Conclude: conclude, Execute: func(ctx context.Context, raw json.RawMessage) (string, error) {
			if err := agent.ValidateArguments(json.RawMessage(schema), raw); err != nil {
				return "", err
			}
			return fn(ctx, raw)
		}}
	}
	return []agent.Tool{
		def("read", "Read a text file with optional 1-based offset and line limit.", `{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer","minimum":1},"limit":{"type":"integer","minimum":1}},"required":["path"],"additionalProperties":false}`, true, true, s.read),
		def("bash", "Run a bash command in the project workspace. Long output is saved to a file.", `{"type":"object","properties":{"command":{"type":"string"},"timeout":{"type":"integer","minimum":1}},"required":["command"],"additionalProperties":false}`, false, false, s.bash),
		def("edit", "Replace exactly one occurrence of oldText in a UTF-8 file.", `{"type":"object","properties":{"path":{"type":"string"},"oldText":{"type":"string"},"newText":{"type":"string"}},"required":["path","oldText","newText"],"additionalProperties":false}`, false, false, s.edit),
		def("write", "Write a file using exactly one of content (generated text) or source_path (byte-exact copy). Optional source_start_line/source_end_line must be paired, 1-based inclusive. source_sha256 verifies the entire source file before copying. Creates parent directories; returns byte count and SHA-256.", `{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"},"source_path":{"type":"string"},"source_start_line":{"type":"integer","minimum":1},"source_end_line":{"type":"integer","minimum":1},"source_sha256":{"type":"string"}},"required":["path"],"additionalProperties":false}`, false, false, s.write),
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
	for k, v := range fields {
		if string(v) == "null" {
			return fmt.Errorf("argument %s cannot be null", k)
		}
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
		head := s.limit() / 2
		tailBytes := s.limit() - head
		if _, seekErr := f.Seek(-int64(tailBytes), io.SeekEnd); seekErr != nil {
			return "", errors.Join(err, readErr, seekErr)
		}
		tail, tailErr := io.ReadAll(io.LimitReader(f, int64(tailBytes)))
		readErr = errors.Join(readErr, tailErr)
		// Omit complete runes at the clipping boundaries instead of turning
		// otherwise valid UTF-8 into replacement characters in the preview.
		for head > 0 && !utf8.RuneStart(data[head]) {
			head--
		}
		start := 0
		for start < len(tail) && !utf8.RuneStart(tail[start]) {
			start++
		}
		text = string(data[:head]) + "\n[Middle omitted. Full output: " + f.Name() + "]\n" + string(tail[start:])
	}
	return strings.ToValidUTF8(text, "�"), errors.Join(err, readErr)
}
func (s *Set) run(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	ceiling := s.ToolTimeout
	if ceiling <= 0 {
		ceiling = 2 * time.Minute
	}
	if timeout <= 0 || timeout > ceiling {
		timeout = ceiling
	}
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
func (s *Set) capture(ctx context.Context, write func(io.Writer) error) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := os.MkdirAll(s.RunDir, 0700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(s.RunDir, "output-*.txt")
	if err != nil {
		return "", err
	}
	defer f.Close()
	err = write(f)
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
	f, err := openRegular(s.path(a.Path))
	if err != nil {
		return "", err
	}
	defer f.Close()
	return s.capture(ctx, func(out io.Writer) error {
		reader := bufio.NewReader(f)
		line := 1
		read := 0
		truncated := false
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			piece, err := reader.ReadSlice('\n')
			if err != nil && err != io.EOF && err != bufio.ErrBufferFull {
				return err
			}
			if line >= a.Offset {
				if _, err := out.Write(piece); err != nil {
					return err
				}
			}
			if err == io.EOF {
				break
			}
			if err != bufio.ErrBufferFull {
				if line >= a.Offset {
					read++
					if read >= a.Limit {
						_, nextErr := reader.Peek(1)
						if nextErr != nil && nextErr != io.EOF {
							return nextErr
						}
						truncated = nextErr == nil
						break
					}
				}
				line++
			}
		}
		if truncated {
			_, err = fmt.Fprintf(out, "\n[Read limited; continue reading %s with offset/limit.]", a.Path)
		}
		return err
	})
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
	if a.Timeout > 86400 {
		return "", errors.New("timeout must not exceed 86400 seconds")
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
	f, err := openRegular(path)
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
	err = writeRegular(path, []byte(strings.Replace(string(data), a.Old, a.New, 1)), info.Mode().Perm())
	return "Edited " + a.Path, err
}

// Nonblocking open followed by fstat avoids hanging on FIFOs/devices, including
// symlinks that resolve to them. Reads and edits are text-file operations.
func openRegular(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("path must be a regular file")
	}
	return f, nil
}
func writeRegular(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|syscall.O_NONBLOCK, mode)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("path must be a regular file")
	}
	if err = f.Truncate(0); err != nil {
		return err
	}
	_, err = f.Write(data)
	return err
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
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 && out == "" {
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
	out, err := s.run(ctx, 30*time.Second, "rg", "--files", "--hidden", "--glob", a.Pattern, "--", s.path(a.Path))
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 && out == "" {
		return "No matches", nil
	}
	return out, err
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
	dir, err := os.OpenFile(s.path(a.Path), os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_DIRECTORY, 0)
	if err != nil {
		return "", err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return "", err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return s.capture(ctx, func(out io.Writer) error {
		for _, e := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			name := e.Name()
			if e.IsDir() {
				name += "/"
			}
			if _, err := io.WriteString(out, name+"\n"); err != nil {
				return err
			}
		}
		return nil
	})
}
