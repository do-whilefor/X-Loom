//go:build linux

package tools

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
)

const maxCopyBytes = 32 << 20

type writeArguments struct {
	Path            string  `json:"path"`
	Content         *string `json:"content"`
	SourcePath      *string `json:"source_path"`
	SourceStartLine *int    `json:"source_start_line"`
	SourceEndLine   *int    `json:"source_end_line"`
	SourceSHA256    *string `json:"source_sha256"`
}

type writeReceipt struct {
	Path         string `json:"path"`
	Mode         string `json:"mode"`
	Bytes        int    `json:"bytes"`
	SHA256       string `json:"sha256"`
	SourceSHA256 string `json:"source_sha256,omitempty"`
}

func (s *Set) write(ctx context.Context, raw json.RawMessage) (string, error) {
	var a writeArguments
	if err := decode(raw, &a, "path"); err != nil {
		return "", err
	}
	if a.Path == "" {
		return "", errors.New("path must not be empty")
	}
	if (a.Content == nil) == (a.SourcePath == nil) {
		return "", errors.New("provide exactly one of content or source_path")
	}
	if a.SourcePath == nil && (a.SourceStartLine != nil || a.SourceEndLine != nil || a.SourceSHA256 != nil) {
		return "", errors.New("source line range and source_sha256 require source_path")
	}
	if (a.SourceStartLine == nil) != (a.SourceEndLine == nil) {
		return "", errors.New("source_start_line and source_end_line must be provided together")
	}
	if a.SourceStartLine != nil && (*a.SourceStartLine < 1 || *a.SourceEndLine < *a.SourceStartLine) {
		return "", errors.New("source line range must be 1-based and inclusive")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	result := writeReceipt{Path: a.Path, Mode: "generated"}
	var data []byte
	if a.Content != nil {
		data = []byte(*a.Content)
	} else {
		if *a.SourcePath == "" {
			return "", errors.New("source_path must not be empty")
		}
		if a.SourceSHA256 != nil {
			decoded, err := hex.DecodeString(*a.SourceSHA256)
			if err != nil || len(decoded) != sha256.Size {
				return "", errors.New("source_sha256 must be a 64-character hex SHA-256")
			}
		}
		var err error
		data, err = readCopySource(ctx, s.path(*a.SourcePath))
		if err != nil {
			return "", err
		}
		result.Mode = "copied"
		result.SourceSHA256 = hashBytes(data)
		if a.SourceSHA256 != nil && !strings.EqualFold(*a.SourceSHA256, result.SourceSHA256) {
			return "", errors.New("source_sha256 mismatch; destination was not written")
		}
		if a.SourceStartLine != nil {
			data, err = copyLineRange(ctx, data, *a.SourceStartLine, *a.SourceEndLine)
			if err != nil {
				return "", err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	path := s.path(a.Path)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return "", err
	}
	var err error
	if a.SourcePath != nil {
		// Publish only after the complete copy has been written. Invalid sources,
		// failed writes, and cancellation leave an existing destination intact.
		err = atomicCopyWrite(ctx, path, data)
	} else {
		// Keep the existing generated-text write behavior, including symlinks.
		err = writeRegular(path, data, 0644)
	}
	if err != nil {
		return "", err
	}
	result.Bytes = len(data)
	result.SHA256 = hashBytes(data)
	receipt, err := json.Marshal(result)
	return string(receipt), err
}

func hashBytes(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

// Checking the open file, rather than its path, also rejects symlinks to FIFOs
// and devices without blocking. LimitReader bounds growing files as well.
func readCopySource(ctx context.Context, path string) ([]byte, error) {
	f, err := openRegular(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > maxCopyBytes {
		return nil, fmt.Errorf("source file exceeds %d bytes", maxCopyBytes)
	}
	data, err := io.ReadAll(io.LimitReader(copyContextReader{ctx, f}, maxCopyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxCopyBytes {
		return nil, fmt.Errorf("source file exceeds %d bytes", maxCopyBytes)
	}
	return data, ctx.Err()
}

type copyContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r copyContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// A final newline terminates the last line; it does not create an empty line.
// Slicing preserves CRLF, escapes, Unicode spelling, and the final byte.
func copyLineRange(ctx context.Context, data []byte, start, end int) ([]byte, error) {
	selected := -1
	for line, offset := 1, 0; offset < len(data); line++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if line == start {
			selected = offset
		}
		next := len(data)
		if newline := bytes.IndexByte(data[offset:], '\n'); newline >= 0 {
			next = offset + newline + 1
		}
		if line == end {
			return data[selected:next], nil
		}
		offset = next
	}
	return nil, errors.New("source line range exceeds the file")
}

func atomicCopyWrite(ctx context.Context, path string, data []byte) error {
	mode := os.FileMode(0644)
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("copy destination must be a regular file, not a symlink or special file")
		}
		mode = info.Mode().Perm()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".xloom-copy-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := f.Chmod(mode); err != nil {
		return err
	}
	if _, err := io.Copy(f, copyContextReader{ctx, bytes.NewReader(data)}); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
