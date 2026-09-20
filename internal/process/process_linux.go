//go:build linux

// Package process scopes shell cancellation to a task's process groups.
package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func Run(ctx context.Context, dir, runDir string, output *os.File, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdout = output
	cmd.Stderr = output
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 2 * time.Second
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	marker := filepath.Join(runDir, fmt.Sprintf("group-%d", pid))
	if err := os.WriteFile(marker, []byte(strconv.Itoa(pid)), 0600); err != nil {
		syscall.Kill(-pid, syscall.SIGKILL)
		cmd.Wait()
		return err
	}
	err := cmd.Wait()
	if ctx.Err() != nil {
		syscall.Kill(-pid, syscall.SIGKILL)
		return ctx.Err()
	}
	if errors.Is(syscall.Kill(-pid, 0), syscall.ESRCH) {
		os.Remove(marker)
	}
	return err
}
func KillGroups(runDir string) error {
	entries, err := os.ReadDir(runDir)
	if err != nil {
		return err
	}
	var result error
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "group-") {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimPrefix(entry.Name(), "group-"))
		if err != nil || pid <= 1 {
			continue
		}
		if err = syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			result = errors.Join(result, err)
		}
	}
	return result
}
func Cancel(runDir string, force bool) error {
	data, err := os.ReadFile(filepath.Join(runDir, "worker.pid"))
	if os.IsNotExist(err) {
		return KillGroups(runDir)
	}
	if err != nil {
		return err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 {
		return errors.New("invalid worker pid")
	}
	signal := syscall.SIGTERM
	if force {
		signal = syscall.SIGKILL
	}
	err = syscall.Kill(pid, signal)
	if errors.Is(err, syscall.ESRCH) {
		err = nil
	}
	return errors.Join(err, KillGroups(runDir))
}
