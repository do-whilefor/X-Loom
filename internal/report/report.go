// Package report renders a single JSON source and checks its evidence references.
// These checks establish file integrity, not the truth of claims in the source.
package report

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const validationScope = "File existence, SHA-256 and optional line ranges only; factual support and numeric claims require separate review."

type Counts struct {
	Arrays             map[string]int `json:"arrays"`
	EvidenceReferences int            `json:"evidence_references"`
	EvidenceFiles      int            `json:"evidence_files"`
}

type Reference struct {
	Sources   []string `json:"sources"`
	StartLine int      `json:"start_line,omitempty"`
	EndLine   int      `json:"end_line,omitempty"`
}

type Evidence struct {
	Path       string      `json:"path"`
	Bytes      int64       `json:"bytes"`
	Lines      int         `json:"lines"`
	SHA256     string      `json:"sha256"`
	References []Reference `json:"references"`
}

type Index struct {
	ValidationScope string     `json:"validation_scope"`
	Counts          Counts     `json:"counts"`
	Evidence        []Evidence `json:"evidence"`
}

type Artifacts struct {
	JSON     []byte
	Markdown []byte
	Index    []byte
	Summary  Index
}

// Render accepts a JSON object. An "evidence" or "evidence_paths" field anywhere in that object
// contains a path, {path, start_line?, end_line?, sha256?}, or a list of these.
// Paths must stay inside root; relative paths resolve against root. Line bounds
// must be paired, one-based and inclusive. JSON Pointer source locations
// in the index identify every occurrence without maintaining a second path list.
func Render(root string, source io.Reader) (Artifacts, error) {
	root, err := canonicalRoot(root)
	if err != nil {
		return Artifacts{}, err
	}
	decoder := json.NewDecoder(source)
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		return Artifacts{}, fmt.Errorf("decode report source: %w", err)
	}
	if value == nil {
		return Artifacts{}, errors.New("report source must be a JSON object")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return Artifacts{}, errors.New("report source must contain exactly one JSON object")
	}
	index := Index{ValidationScope: validationScope, Counts: Counts{Arrays: map[string]int{}}, Evidence: []Evidence{}}
	files := map[string]*Evidence{}
	// Cache by resolved file so aliases do not re-read the same evidence.
	checked := map[string]Evidence{}
	var walk func(any, string) error
	walk = func(v any, pointer string) error {
		switch v := v.(type) {
		case map[string]any:
			for _, key := range sortedKeys(v) {
				child := pointer + "/" + pointerEscape(key)
				if key == "evidence" || key == "evidence_paths" {
					if err := collectEvidence(root, v[key], child, files, checked, &index.Counts); err != nil {
						return err
					}
				}
				if err := walk(v[key], child); err != nil {
					return err
				}
			}
		case []any:
			index.Counts.Arrays[pointer] = len(v)
			for i, item := range v {
				if err := walk(item, fmt.Sprintf("%s/%d", pointer, i)); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(value, ""); err != nil {
		return Artifacts{}, err
	}
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		index.Evidence = append(index.Evidence, *files[path])
	}
	index.Counts.EvidenceFiles = len(files)
	jsonData, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return Artifacts{}, err
	}
	indexData, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return Artifacts{}, err
	}
	var markdown strings.Builder
	markdown.WriteString("# Report\n\n")
	writeMarkdown(&markdown, value, 0)
	return Artifacts{JSON: append(jsonData, '\n'), Markdown: []byte(markdown.String()), Index: append(indexData, '\n'), Summary: index}, nil
}

func collectEvidence(root string, value any, pointer string, files map[string]*Evidence, checked map[string]Evidence, counts *Counts) error {
	if list, ok := value.([]any); ok {
		for i, item := range list {
			if err := collectEvidence(root, item, fmt.Sprintf("%s/%d", pointer, i), files, checked, counts); err != nil {
				return err
			}
		}
		return nil
	}
	var path, expectedHash string
	ref := Reference{Sources: []string{pointer}}
	switch v := value.(type) {
	case string:
		path = v
	case map[string]any:
		path, _ = v["path"].(string)
		if raw, ok := v["sha256"]; ok {
			var valid bool
			expectedHash, valid = raw.(string)
			decoded, err := hex.DecodeString(expectedHash)
			if !valid || err != nil || len(decoded) != sha256.Size {
				return fmt.Errorf("%s: sha256 must contain 64 hexadecimal characters", pointer)
			}
		}
		for key, target := range map[string]*int{"start_line": &ref.StartLine, "end_line": &ref.EndLine} {
			if raw, ok := v[key]; ok {
				number, valid := raw.(json.Number)
				n, err := number.Int64()
				if !valid || err != nil || n < 1 || int64(int(n)) != n {
					return fmt.Errorf("%s: %s must be a positive integer", pointer, key)
				}
				*target = int(n)
			}
		}
	default:
		return fmt.Errorf("%s: evidence must be a path, reference object, or list", pointer)
	}
	if path == "" || strings.Contains(path, "\\") {
		return fmt.Errorf("%s: evidence path must be a nonempty slash-separated path", pointer)
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	resolved, err := resolveExisting(root, path)
	if err != nil {
		return fmt.Errorf("%s: %w", pointer, err)
	}
	path, err = filepath.Rel(root, path)
	if err != nil {
		return fmt.Errorf("%s: %w", pointer, err)
	}
	path = filepath.ToSlash(path)
	if (ref.StartLine == 0) != (ref.EndLine == 0) {
		return fmt.Errorf("%s: start_line and end_line must be paired", pointer)
	}
	if ref.EndLine < ref.StartLine {
		return fmt.Errorf("%s: end_line precedes start_line", pointer)
	}
	file, ok := checked[resolved]
	if !ok {
		file, err = inspectFile(resolved)
		if err != nil {
			return fmt.Errorf("%s: %w", pointer, err)
		}
		checked[resolved] = file
	}
	if ref.EndLine > file.Lines {
		return fmt.Errorf("%s: line %d exceeds %s (%d lines)", pointer, ref.EndLine, path, file.Lines)
	}
	if expectedHash != "" && !strings.EqualFold(expectedHash, file.SHA256) {
		return fmt.Errorf("%s: SHA-256 mismatch for %s", pointer, path)
	}
	entry := files[path]
	if entry == nil {
		file.Path = path
		entry = &file
		files[path] = entry
	}
	merged := false
	for i := range entry.References {
		if entry.References[i].StartLine == ref.StartLine && entry.References[i].EndLine == ref.EndLine {
			entry.References[i].Sources = append(entry.References[i].Sources, pointer)
			merged = true
			break
		}
	}
	if !merged {
		entry.References = append(entry.References, ref)
	}
	counts.EvidenceReferences++
	return nil
}

func inspectFile(path string) (Evidence, error) {
	stat, err := os.Stat(path)
	if err != nil {
		return Evidence{}, err
	}
	if !stat.Mode().IsRegular() {
		return Evidence{}, fmt.Errorf("evidence is not a regular file: %s", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return Evidence{}, err
	}
	defer f.Close()
	stat, err = f.Stat()
	if err != nil {
		return Evidence{}, err
	}
	if !stat.Mode().IsRegular() {
		return Evidence{}, fmt.Errorf("evidence is not a regular file: %s", path)
	}
	hash := sha256.New()
	reader := io.TeeReader(f, hash)
	buffer := make([]byte, 32*1024)
	var size int64
	var lines int
	var last byte
	for {
		n, err := reader.Read(buffer)
		size += int64(n)
		lines += bytes.Count(buffer[:n], []byte{'\n'})
		if n > 0 {
			last = buffer[n-1]
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return Evidence{}, err
		}
	}
	if size > 0 && last != '\n' {
		lines++
	}
	return Evidence{Bytes: size, Lines: lines, SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}

func canonicalRoot(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	stat, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !stat.IsDir() {
		return "", errors.New("report root must be a directory")
	}
	return resolved, nil
}

func contained(root, path string) error {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("path escapes report root: %s", path)
	}
	return nil
}

func resolveExisting(root, path string) (string, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	if err := contained(root, path); err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if err := contained(root, resolved); err != nil {
		return "", err
	}
	return resolved, nil
}

func sortedKeys(v map[string]any) []string {
	keys := make([]string, 0, len(v))
	for key := range v {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func pointerEscape(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}

var markdownEscaper = strings.NewReplacer("\\", "\\\\", "`", "\\`", "*", "\\*", "_", "\\_", "[", "\\[", "]", "\\]", "<", "&lt;", ">", "&gt;", "#", "\\#", "|", "\\|")

func scalar(value any) string {
	if s, ok := value.(string); ok {
		return strings.ReplaceAll(markdownEscaper.Replace(s), "\n", "  \n")
	}
	data, _ := json.Marshal(value)
	return string(data)
}

func writeMarkdown(out *strings.Builder, value any, depth int) {
	indent := strings.Repeat("  ", depth)
	switch v := value.(type) {
	case map[string]any:
		if len(v) == 0 {
			out.WriteString(indent + "{}\n")
		}
		for _, key := range sortedKeys(v) {
			out.WriteString(indent + "- **" + markdownEscaper.Replace(key) + "**:")
			writeItem(out, v[key], depth)
		}
	case []any:
		if len(v) == 0 {
			out.WriteString(indent + "[]\n")
		}
		for _, item := range v {
			out.WriteString(indent + "-")
			writeItem(out, item, depth)
		}
	default:
		out.WriteString(indent + scalar(v) + "\n")
	}
}

func writeItem(out *strings.Builder, value any, depth int) {
	switch value.(type) {
	case map[string]any, []any:
		out.WriteByte('\n')
		writeMarkdown(out, value, depth+1)
	default:
		text := strings.ReplaceAll(scalar(value), "\n", "\n"+strings.Repeat("  ", depth+1))
		out.WriteString(" " + text + "\n")
	}
}

// Generate validates before writing. The output prefix names three siblings:
// .json, .md and .index.json. Their parent must already exist inside root.
// Existing regular report files may be replaced; source/evidence may not.
func Generate(root, sourcePath, outputPrefix string) (Index, error) {
	root, err := canonicalRoot(root)
	if err != nil {
		return Index{}, err
	}
	sourcePath, err = resolveExisting(root, sourcePath)
	if err != nil {
		return Index{}, err
	}
	stat, err := os.Stat(sourcePath)
	if err != nil {
		return Index{}, err
	}
	if !stat.Mode().IsRegular() {
		return Index{}, errors.New("report source must be a regular file")
	}
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		return Index{}, err
	}
	artifacts, err := Render(root, bytes.NewReader(data))
	if err != nil {
		return Index{}, err
	}
	if outputPrefix == "" {
		return Index{}, errors.New("output prefix is required")
	}
	if !filepath.IsAbs(outputPrefix) {
		outputPrefix = filepath.Join(root, outputPrefix)
	}
	parent, err := resolveExisting(root, filepath.Dir(outputPrefix))
	if err != nil {
		return Index{}, err
	}
	outputPrefix = filepath.Join(parent, filepath.Base(outputPrefix))
	protected := []string{sourcePath}
	for _, file := range artifacts.Summary.Evidence {
		path, err := resolveExisting(root, file.Path)
		if err != nil {
			return Index{}, err
		}
		protected = append(protected, path)
	}
	outputs := []struct {
		path string
		data []byte
	}{{outputPrefix + ".json", artifacts.JSON}, {outputPrefix + ".md", artifacts.Markdown}, {outputPrefix + ".index.json", artifacts.Index}}
	for _, output := range outputs {
		stat, err := os.Lstat(output.path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return Index{}, err
		}
		if err == nil && !stat.Mode().IsRegular() {
			return Index{}, fmt.Errorf("output must be a regular file, not a symlink or directory: %s", output.path)
		}
		for _, input := range protected {
			inputStat, err := os.Stat(input)
			if err != nil {
				return Index{}, err
			}
			if input == output.path || (stat != nil && os.SameFile(inputStat, stat)) {
				return Index{}, fmt.Errorf("output would overwrite source or evidence: %s", output.path)
			}
		}
	}
	for _, output := range outputs {
		if err := replaceFile(output.path, output.data); err != nil {
			return Index{}, err
		}
	}
	return artifacts.Summary, nil
}

func replaceFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".xloom-report-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
