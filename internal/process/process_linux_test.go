//go:build linux

package process

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestInterruptWorkerHelper(t *testing.T) {
	dir := os.Getenv("XLOOM_TEST_INTERRUPT_DIR")
	if dir == "" {
		return
	}
	unlock, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := RegisterWorker(dir); err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(filepath.Join(dir, "output.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if err := Run(context.Background(), dir, dir, out, "bash", "-c", "sleep 60 & echo $! > child.pid; wait"); err != nil {
		t.Fatal(err)
	}
}

func TestInterruptStopsWorkerAndChildrenWithoutPermanentlyCancelling(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestInterruptWorkerHelper$")
	cmd.Env = append(os.Environ(), "XLOOM_TEST_INTERRUPT_DIR="+dir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	child := awaitPID(t, filepath.Join(dir, "child.pid"))
	if err := Interrupt(dir); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	awaitGone(t, child)
	if Cancelled(dir) {
		t.Fatal("infrastructure interruption became a hard stop")
	}
	unlock, err := Lock(dir)
	if err != nil {
		t.Fatal("resume lock unavailable", err)
	}
	unlock()
	if err := Cancel(dir, false); err != nil {
		t.Fatal(err)
	}
	if err := Interrupt(dir); err != nil {
		t.Fatal(err)
	}
	if !Cancelled(dir) {
		t.Fatal("interrupt cleared a previous hard stop")
	}
}

func TestLaunchTokenRejectsLateInterruptedProcess(t *testing.T) {
	dir := t.TempDir()
	old := strings.Repeat("a", 32)
	next := strings.Repeat("b", 32)
	t.Setenv("XLOOM_LAUNCH_TOKEN", old)
	if err := os.WriteFile(filepath.Join(dir, "launch-token"), []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	if err := CheckLaunch(dir); err != nil {
		t.Fatal(err)
	}
	if err := Interrupt(dir); err != nil {
		t.Fatal(err)
	}
	if err := CheckLaunch(dir); err == nil {
		t.Fatal("late old exec accepted after interruption without PID")
	}
	if err := os.WriteFile(filepath.Join(dir, "launch-token"), []byte(next), 0600); err != nil {
		t.Fatal(err)
	}
	if err := CheckLaunch(dir); err == nil {
		t.Fatal("old exec accepted after next launch")
	}
	t.Setenv("XLOOM_LAUNCH_TOKEN", next)
	if err := CheckLaunch(dir); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XLOOM_LAUNCH_TOKEN", "")
	if err := CheckLaunch(dir); err != nil {
		t.Fatal("manual worker compatibility broken", err)
	}
}

func awaitPID(t *testing.T, path string) int {
	t.Helper()
	end := time.Now().Add(3 * time.Second)
	for time.Now().Before(end) {
		if raw, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid > 1 {
				return pid
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("subprocess never wrote PID")
	return 0
}
func running(pid int) bool {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	end := strings.LastIndexByte(string(raw), ')')
	fields := strings.Fields(string(raw)[end+1:])
	return len(fields) > 0 && fields[0] != "Z"
}
func awaitGone(t *testing.T, pid int) {
	t.Helper()
	end := time.Now().Add(time.Second)
	for time.Now().Before(end) {
		if !running(pid) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("process %d survived cancellation", pid)
}
func TestCancellationKillsOnlyOwnSubprocessTree(t *testing.T) {
	dir1, dir2 := t.TempDir(), t.TempDir()
	out1, _ := os.Create(filepath.Join(dir1, "out"))
	defer out1.Close()
	out2, _ := os.Create(filepath.Join(dir2, "out"))
	defer out2.Close()
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	done1, done2 := make(chan error, 1), make(chan error, 1)
	go func() { done1 <- Run(ctx1, dir1, dir1, out1, "bash", "-c", "sleep 60 & echo $! > child.pid; wait") }()
	go func() { done2 <- Run(ctx2, dir2, dir2, out2, "bash", "-c", "sleep 60 & echo $! > child.pid; wait") }()
	pid1, pid2 := awaitPID(t, filepath.Join(dir1, "child.pid")), awaitPID(t, filepath.Join(dir2, "child.pid"))
	cancel1()
	if err := <-done1; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	awaitGone(t, pid1)
	if !running(pid2) {
		t.Fatal("canceling first task killed second task")
	}
	cancel2()
	if err := <-done2; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	awaitGone(t, pid2)
}
func TestSuccessfulCommandCleansBackgroundChildren(t *testing.T) {
	dir := t.TempDir()
	out, _ := os.Create(filepath.Join(dir, "out"))
	defer out.Close()
	if err := Run(context.Background(), dir, dir, out, "bash", "-c", "sleep 60 & echo $! > child.pid"); err != nil {
		t.Fatal(err)
	}
	awaitGone(t, awaitPID(t, filepath.Join(dir, "child.pid")))
	entries, _ := filepath.Glob(filepath.Join(dir, "group-*"))
	if len(entries) != 0 {
		t.Fatal("stale process group markers", entries)
	}
}

func TestNewSessionChildIsStillCleaned(t *testing.T) {
	dir := t.TempDir()
	out, _ := os.Create(filepath.Join(dir, "out"))
	defer out.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, dir, dir, out, "bash", "-c", `setsid bash -c 'echo $$ > child.pid; exec sleep 60' & wait`)
	}()
	pid := awaitPID(t, filepath.Join(dir, "child.pid"))
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	awaitGone(t, pid)
}
func TestStaleProcessIdentityNeverSignalsReusedPID(t *testing.T) {
	dir := t.TempDir()
	raw, _ := json.Marshal(identity{PID: os.Getpid(), Start: "incorrect-starttime"})
	os.WriteFile(filepath.Join(dir, "worker.pid"), raw, 0600)
	if err := Cancel(dir, true); err != nil {
		t.Fatal(err)
	}
	if !Cancelled(dir) {
		t.Fatal("cancel before startup was not persisted")
	}
	if err := syscall.Kill(os.Getpid(), 0); err != nil {
		t.Fatal(err)
	}
}
func TestExecutionLockAndEarlyCancellation(t *testing.T) {
	dir := t.TempDir()
	unlock, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := Lock(dir); err == nil {
		other()
		t.Fatal("duplicate execution acquired lock")
	}
	unlock()
	unlock, err = Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	if err := Cancel(filepath.Join(dir, "not-started"), false); err != nil {
		t.Fatal(err)
	}
	if !Cancelled(filepath.Join(dir, "not-started")) {
		t.Fatal("no early-cancel marker")
	}
}
