//go:build linux

// Package process scopes shell cancellation to a task's process groups.
package process

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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

type identity struct {
	PID   int    `json:"pid"`
	Start string `json:"start"`
	Token string `json:"token,omitempty"`
}

// Linux starttime makes persisted PID markers safe against PID reuse.
func identityOf(pid int) (identity, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return identity{}, err
	}
	i := strings.LastIndexByte(string(raw), ')')
	if i < 0 {
		return identity{}, errors.New("invalid proc stat")
	}
	fields := strings.Fields(string(raw)[i+1:])
	if len(fields) < 20 {
		return identity{}, errors.New("short proc stat")
	}
	return identity{PID: pid, Start: fields[19]}, nil
}
func saveIdentity(path string, pid int) error {
	id, err := identityOf(pid)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(id)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0600)
}
func readIdentity(path string) (identity, bool, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return identity{}, false, nil
	}
	if err != nil {
		return identity{}, false, err
	}
	var id identity
	if err = json.Unmarshal(raw, &id); err != nil || id.PID <= 1 || id.Start == "" {
		return id, false, errors.New("invalid process identity marker")
	}
	actual, err := identityOf(id.PID)
	if os.IsNotExist(err) {
		return id, false, nil
	}
	if err != nil {
		return id, false, err
	}
	return id, id.PID == actual.PID && id.Start == actual.Start, nil
}
func RegisterWorker(runDir string) error {
	return saveIdentity(filepath.Join(runDir, "worker.pid"), os.Getpid())
}

// Docker supplies a per-launch token outside the immutable Job. A late exec
// from an interrupted HTTP start must not claim the same run after recovery.
func CheckLaunch(runDir string) error {
	token := os.Getenv("XLOOM_LAUNCH_TOKEN")
	if token == "" {
		return nil
	}
	if len(token) != 32 {
		return errors.New("invalid worker launch token")
	}
	if _, err := hex.DecodeString(token); err != nil {
		return errors.New("invalid worker launch token")
	}
	raw, err := os.ReadFile(filepath.Join(runDir, "launch-token"))
	if err != nil {
		return fmt.Errorf("read worker launch token: %w", err)
	}
	if string(raw) != token {
		return errors.New("worker launch was interrupted or superseded")
	}
	return nil
}

func invalidateLaunch(runDir string) error {
	if err := os.MkdirAll(runDir, 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(runDir, "launch-token"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	err = errors.Join(f.Sync(), f.Close())
	if err != nil {
		return err
	}
	dir, err := os.Open(runDir)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

// Lock is released by the kernel on a crash, allowing session recovery.
func Lock(runDir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(runDir, "worker.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("execution already running: %w", err)
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}
func Cancelled(runDir string) bool {
	_, err := os.Stat(filepath.Join(runDir, "cancelled"))
	return err == nil
}

func Run(ctx context.Context, dir, runDir string, output *os.File, name string, args ...string) error {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return err
	}
	token := hex.EncodeToString(random)
	cmd := exec.CommandContext(ctx, name, args...)
	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, "XLOOM_PROCESS_TOKEN=") {
			cmd.Env = append(cmd.Env, env)
		}
	}
	cmd.Env = append(cmd.Env, "XLOOM_PROCESS_TOKEN="+token)
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
	id, idErr := identityOf(pid)
	id.Token = token
	raw, marshalErr := json.Marshal(id)
	err := errors.Join(idErr, marshalErr)
	if err == nil {
		err = os.WriteFile(marker, raw, 0600)
	}
	if err != nil {
		syscall.Kill(-pid, syscall.SIGKILL)
		killToken(token)
		cmd.Wait()
		return err
	}
	err = cmd.Wait()
	// A command owns its background children as well. No command may leave
	// them running after returning success, failure or cancellation.
	// The leader is now reaped: avoid signaling a numeric group ID that may
	// have been reused. Its inherited token also identifies setsid children.
	killToken(token)
	os.Remove(marker)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// Process groups handle ordinary cancellation immediately. An inherited random
// token also finds children which opened a new session or outlived the leader.
// This is lifecycle cleanup, not a sandbox against deliberately hostile code.
func killToken(token string) error {
	if len(token) != 32 {
		return errors.New("invalid process token")
	}
	if _, err := hex.DecodeString(token); err != nil {
		return err
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return err
	}
	var result error
	needle := "\x00XLOOM_PROCESS_TOKEN=" + token + "\x00"
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 1 || pid == os.Getpid() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "environ"))
		if err != nil {
			continue
		}
		if !strings.Contains("\x00"+string(raw), needle) {
			continue
		}
		if err = syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			result = errors.Join(result, err)
		}
	}
	return result
}
func KillGroups(runDir string) error {
	entries, err := os.ReadDir(runDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var result error
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "group-") {
			continue
		}
		path := filepath.Join(runDir, entry.Name())
		id, alive, err := readIdentity(path)
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		if id.Token != "" {
			result = errors.Join(result, killToken(id.Token))
		}
		if !alive {
			os.Remove(path)
			continue
		}
		if err = syscall.Kill(-id.PID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			result = errors.Join(result, err)
		}
		os.Remove(path)
	}
	return result
}
func Cancel(runDir string, force bool) error {
	if err := os.MkdirAll(runDir, 0700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(runDir, "cancelled"), []byte("hard stop\n"), 0600); err != nil {
		return err
	}
	id, alive, err := readIdentity(filepath.Join(runDir, "worker.pid"))
	if err != nil {
		return errors.Join(err, KillGroups(runDir))
	}
	if !alive {
		return KillGroups(runDir)
	}
	// Kill owned command groups before terminating their parent worker, while
	// their leader identities can still be verified.
	groupErr := KillGroups(runDir)
	signal := syscall.SIGTERM
	if force {
		signal = syscall.SIGKILL
	}
	err = syscall.Kill(id.PID, signal)
	if errors.Is(err, syscall.ESRCH) {
		err = nil
	}
	return errors.Join(err, groupErr)
}

// Interrupt stops an execution without marking it cancelled. Stop the worker
// before its children so it cannot launch another tool while cleanup runs.
// SIGKILL deliberately bypasses signal-context handlers that persist hard stop.
func Interrupt(runDir string) error {
	// Persist invalidation before testing PID/lock: an exec may not have begun
	// yet, but its old launch token must be refused when it eventually starts.
	if err := invalidateLaunch(runDir); err != nil {
		return err
	}
	id, alive, err := readIdentity(filepath.Join(runDir, "worker.pid"))
	if err != nil {
		return err
	}
	if alive {
		if id.PID == os.Getpid() {
			return errors.New("cannot interrupt the helper itself")
		}
		if err = syscall.Kill(id.PID, syscall.SIGSTOP); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		groupErr := KillGroups(runDir)
		if err = syscall.Kill(id.PID, syscall.SIGKILL); errors.Is(err, syscall.ESRCH) {
			err = nil
		}
		if err = errors.Join(err, groupErr); err != nil {
			return err
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		unlock, lockErr := Lock(runDir)
		if lockErr == nil {
			defer unlock()
			return KillGroups(runDir)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("interrupted execution still holds its lock: %w", lockErr)
		}
		// The Worker may have acquired its lock just before publishing its PID.
		if !alive {
			id, alive, err = readIdentity(filepath.Join(runDir, "worker.pid"))
			if err != nil {
				return err
			}
			if alive {
				return Interrupt(runDir)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
}
