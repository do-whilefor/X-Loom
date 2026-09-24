//go:build linux

package integration

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"xloom/internal/board"
)

// Acceptance diagnostics read the test-owned store, not a production endpoint
// that exports every immutable job and result in the namespace.
func testExecutions(ctx context.Context, store *board.Store, namespace string) ([]board.Execution, error) {
	out := []board.Execution{}
	err := store.Do(ctx, func(tx *board.Tx) error {
		rows, err := tx.Query("SELECT project_id,id FROM xloom_executions WHERE namespace=? ORDER BY created_at,rowid", namespace)
		if err != nil {
			return err
		}
		var ids [][2]string
		for rows.Next() {
			var id [2]string
			if err := rows.Scan(&id[0], &id[1]); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, id := range ids {
			run, err := tx.Execution(id[0], id[1])
			if err != nil {
				return err
			}
			out = append(out, run)
		}
		return nil
	})
	return out, err
}

// Share the test server's network namespace when acceptance tests themselves
// run in Docker. Engine inspect normalizes container network targets to their
// full ID, which must also be used when a later run checks the saved config.
func testContainerNetwork(t *testing.T) string {
	t.Helper()
	if _, err := os.Stat("/.dockerenv"); os.IsNotExist(err) {
		return "host"
	} else if err != nil {
		t.Fatal(err)
	}
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", "/var/run/docker.sock")
	}}
	defer transport.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/containers/"+url.PathEscape(hostname)+"/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		t.Fatalf("inspect test container network: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("inspect test container network returned HTTP %d", response.StatusCode)
	}
	var inspected struct {
		ID string `json:"Id"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&inspected); err != nil {
		t.Fatalf("decode test container inspection: %v", err)
	}
	if id, err := hex.DecodeString(inspected.ID); err != nil || len(id) != 32 {
		t.Fatalf("test container inspection returned invalid full ID %q", inspected.ID)
	}
	return "container:" + inspected.ID
}

type executionProcess struct {
	name  string
	pid   int
	state string
}

func (p executionProcess) dead() bool { return p.state == "gone" || p.state == "Z" || p.state == "X" }

// Inspect through a separate Engine exec, not the cancelled worker's stdout.
// Bash builtins read /proc so the runtime image needs no ps or additional tools.
func inspectExecutionProcesses(ctx context.Context, container, runDir string) ([]executionProcess, error) {
	const script = `set -eu
for name in shell child; do
  IFS= read -r pid < "$1/$name.pid"
  case "$pid" in ''|*[!0-9]*) exit 2;; esac
  if { IFS= read -r stat < "/proc/$pid/stat"; } 2>/dev/null; then
    stat=${stat##*) }
    state=${stat%% *}
  else
    state=gone
  fi
  printf '%s %s %s\n' "$name" "$pid" "$state"
done`
	text, err := dockerExec(ctx, container, []string{"bash", "-c", script, "xloom-cancellation-check", runDir})
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) != 2 {
		return nil, fmt.Errorf("expected shell and child process states, got %q", text)
	}
	out := make([]executionProcess, 2)
	for i, name := range []string{"shell", "child"} {
		fields := strings.Fields(lines[i])
		if len(fields) != 3 || fields[0] != name {
			return nil, fmt.Errorf("invalid %s process state: %q", name, lines[i])
		}
		pid, err := strconv.Atoi(fields[1])
		if err != nil || pid <= 1 || (fields[2] != "gone" && len(fields[2]) != 1) {
			return nil, fmt.Errorf("invalid process identity: %q", lines[i])
		}
		out[i] = executionProcess{name: name, pid: pid, state: fields[2]}
	}
	if out[0].pid == out[1].pid {
		return nil, fmt.Errorf("shell and child unexpectedly share PID %d", out[0].pid)
	}
	return out, nil
}

func dockerExec(ctx context.Context, container string, argv []string) (string, error) {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", "/var/run/docker.sock")
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	request := func(method, path string, input any) (*http.Response, error) {
		var body io.Reader
		if input != nil {
			raw, err := json.Marshal(input)
			if err != nil {
				return nil, err
			}
			body = bytes.NewReader(raw)
		}
		req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, body)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		response, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		if response.StatusCode >= 300 {
			response.Body.Close()
			return nil, fmt.Errorf("Docker process inspection returned HTTP %d", response.StatusCode)
		}
		return response, nil
	}
	response, err := request("POST", "/containers/"+url.PathEscape(container)+"/exec", map[string]any{"Cmd": argv, "AttachStdout": true, "AttachStderr": true, "Tty": false})
	if err != nil {
		return "", err
	}
	var created struct {
		ID string `json:"Id"`
	}
	err = json.NewDecoder(response.Body).Decode(&created)
	response.Body.Close()
	if err != nil || created.ID == "" {
		return "", fmt.Errorf("Docker process inspection could not create exec: %v", err)
	}
	response, err = request("POST", "/exec/"+url.PathEscape(created.ID)+"/start", map[string]bool{"Detach": false, "Tty": false})
	if err != nil {
		return "", err
	}
	var stdout, stderr bytes.Buffer
	for {
		var header [8]byte
		_, err = io.ReadFull(response.Body, header[:])
		if err == io.EOF {
			err = nil
			break
		}
		if err != nil {
			break
		}
		size := binary.BigEndian.Uint32(header[4:])
		if size > 1<<20 {
			err = fmt.Errorf("oversized Docker inspection output")
			break
		}
		var destination io.Writer = &stdout
		if header[0] != 1 {
			destination = &stderr
		}
		if _, err = io.CopyN(destination, response.Body, int64(size)); err != nil {
			break
		}
	}
	response.Body.Close()
	if err != nil {
		return "", err
	}
	for {
		response, err = request("GET", "/exec/"+url.PathEscape(created.ID)+"/json", nil)
		if err != nil {
			return "", err
		}
		var status struct {
			Running bool `json:"Running"`
			Exit    int  `json:"ExitCode"`
		}
		err = json.NewDecoder(response.Body).Decode(&status)
		response.Body.Close()
		if err != nil {
			return "", err
		}
		if !status.Running {
			if status.Exit != 0 {
				return "", fmt.Errorf("Docker process inspection exited %d: %s", status.Exit, strings.TrimSpace(stderr.String()))
			}
			return stdout.String(), nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}
