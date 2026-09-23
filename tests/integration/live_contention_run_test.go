//go:build linux

package integration

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"xloom/internal/board"
	"xloom/internal/config"
	"xloom/internal/dispatcher"
	"xloom/internal/docker"
	"xloom/internal/server"
	"xloom/internal/worker"
)

type liveRunObservation struct {
	RunID         string    `json:"run_id"`
	Kind          string    `json:"kind"`
	StepID        string    `json:"step_id,omitempty"`
	Started       time.Time `json:"started"`
	Finished      time.Time `json:"finished,omitempty"`
	InputRevision int64     `json:"input_revision"`
	InputVersion  string    `json:"input_version"`
	Status        string    `json:"status,omitempty"`
	FailureKind   string    `json:"failure_kind,omitempty"`
	RunnerError   bool      `json:"runner_error,omitempty"`
}

type liveObservedRunner struct {
	*docker.Client
	mu   sync.Mutex
	runs []liveRunObservation
}

func (r *liveObservedRunner) Run(ctx context.Context, backend config.Worker, job worker.Job) (worker.Result, error) {
	observation := liveRunObservation{RunID: job.RunID, Kind: job.Kind, Started: time.Now().UTC()}
	if job.Intent != nil {
		observation.StepID = job.Intent.ID
	}
	if job.InputSnapshot != nil {
		observation.InputRevision, observation.InputVersion = job.InputSnapshot.Revision, job.InputSnapshot.StateVersion
	}
	r.mu.Lock()
	index := len(r.runs)
	r.runs = append(r.runs, observation)
	r.mu.Unlock()
	result, err := r.Client.Run(ctx, backend, job)
	observation.Finished, observation.Status, observation.FailureKind, observation.RunnerError = time.Now().UTC(), result.Status, result.FailureKind, err != nil
	r.mu.Lock()
	r.runs[index] = observation
	r.mu.Unlock()
	return result, err
}

func (r *liveObservedRunner) snapshot() []liveRunObservation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]liveRunObservation{}, r.runs...)
}

func saveLiveJSON(path string, data any) error {
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0600)
}

// This test runs the production Server, Scheduler, Docker bridge and Worker.
// The proxy only observes upstream bytes; every model response is real. Local
// fixture data is the only task content sent to the configured model service.
func TestLiveContentionProject(t *testing.T) {
	if os.Getenv("XLOOM_LIVE_CONTENTION_TEST") != "1" {
		t.Skip("opt in with model configuration, XLOOM_DOCKER_TEST_IMAGE and XLOOM_LIVE_OUTPUT")
	}
	image, output := os.Getenv("XLOOM_DOCKER_TEST_IMAGE"), os.Getenv("XLOOM_LIVE_OUTPUT")
	base, token, model := os.Getenv("ANTHROPIC_BASE_URL"), os.Getenv("ANTHROPIC_AUTH_TOKEN"), os.Getenv("ANTHROPIC_DEFAULT_FABLE_MODEL")
	if selected := os.Getenv("ANTHROPIC_MODEL"); selected != "" {
		model = selected
	}
	if image == "" || output == "" || base == "" || token == "" || model == "" {
		t.Fatal("live test requires explicit image, output and model configuration")
	}
	if err := os.MkdirAll(output, 0700); err != nil {
		t.Fatal(err)
	}
	proxy, observations, err := newLiveModelProxy(base, token)
	if err != nil {
		t.Fatal("invalid live proxy configuration")
	}
	defer proxy.Close()
	store, err := board.Open(filepath.Join(output, "project.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	api := httptest.NewServer(server.New(store))
	defer api.Close()
	namespace := fmt.Sprintf("xloom-live-contention-%d", time.Now().UnixNano())
	c := config.Config{
		Server:    api.URL,
		Runtime:   config.Runtime{Interval: 1, MaxWorkers: 4, MaxProjects: 1, MaxProjectWorkers: 4, HealthMode: "disabled", HealthTimeout: 30},
		Tasks:     config.Tasks{Reason: config.Task{Timeout: 300, MaxIntents: 3}, Explore: config.Task{Timeout: 0, ConcludeTimeout: 60}},
		Container: config.Container{Image: image, Network: testContainerNetwork(t), Namespace: namespace, CompletedAction: "stop"},
		Workers: []config.Worker{{Name: "live", Type: "go", TaskTypes: []string{"reason", "explore"}, MaxRunning: 4, Env: map[string]string{
			"ANTHROPIC_BASE_URL": proxy.URL, "ANTHROPIC_AUTH_TOKEN": "local-observation-proxy", "ANTHROPIC_MODEL": model,
			"XLOOM_REASONING_EFFORT": "max", "XLOOM_REQUEST_TIMEOUT": "180", "XLOOM_MAX_OUTPUT_TOKENS": "384000",
			"XLOOM_CONTEXT_TOKENS": "920000", "XLOOM_CONTEXT_TARGET_TOKENS": "250000", "XLOOM_CONTEXT_BYTES": "8388608",
		}}},
	}
	if err = c.Validate(); err != nil {
		t.Fatal(err)
	}
	runner := &liveObservedRunner{Client: docker.New(c.Container)}
	defer runner.Close()
	client := &dispatcher.Client{Base: api.URL}
	origin, goal := liveContentionTask()
	var graph board.Graph
	started := time.Now().UTC()
	if err = client.Do(context.Background(), "POST", "/projects", map[string]any{"title": "Live concurrent transaction audit", "origin": origin, "goal": goal, "bootstrap_enabled": false}, &graph, nil); err != nil {
		t.Fatal(err)
	}
	pid := graph.Project.ID
	container := namespace + "-dispatch-" + pid
	publicUpstream, _ := url.Parse(base)
	publicUpstream.User, publicUpstream.RawQuery, publicUpstream.Fragment = nil, "", ""
	manifest := map[string]any{"project_id": pid, "started": started, "model": model, "upstream": publicUpstream.String(), "reasoning_effort": "max", "request_timeout_seconds": 180, "decision_timeout_seconds": 300, "max_workers": 4, "source_commit": os.Getenv("XLOOM_SOURCE_COMMIT"), "image": image, "namespace": namespace, "healthcheck": "disabled", "cost_status": "unknown_no_verified_account_pricing", "scope": "synthetic local files; real model, scheduler and Docker workers"}
	if err = saveLiveJSON(filepath.Join(output, "manifest.json"), manifest); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	done := make(chan struct{})
	var schedulerErr error
	go func() { schedulerErr = dispatcher.New(c, runner).Run(ctx); close(done) }()
	completed := false
	defer func() {
		if !completed {
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 15*time.Second)
			_ = client.Do(stopCtx, "PUT", "/projects/"+pid+"/status", map[string]string{"status": "stopped"}, nil, nil)
			stopCancel()
		}
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("scheduler shutdown exceeded 30 seconds")
		}
		collectCtx, collectCancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer collectCancel()
		var state board.State
		if err := client.Do(collectCtx, "GET", "/projects/"+pid+"/state", nil, &state, nil); err != nil {
			t.Error(err)
		}
		_ = saveLiveJSON(filepath.Join(output, "state.json"), state)
		for filename, endpoint := range map[string]string{"state-events.json": "/projects/" + pid + "/state/events?after=0", "executions.json": "/executions?namespace=" + namespace} {
			var value any
			if err := client.Do(collectCtx, "GET", endpoint, nil, &value, nil); err != nil {
				t.Error(err)
			} else if err = saveLiveJSON(filepath.Join(output, filename), value); err != nil {
				t.Error(err)
			}
		}
		if err := saveLiveJSON(filepath.Join(output, "runs.json"), runner.snapshot()); err != nil {
			t.Error(err)
		}
		if err := saveLiveJSON(filepath.Join(output, "http-observations.json"), observations.Snapshot()); err != nil {
			t.Error(err)
		}
		files, archiveErr := collectLiveWorkspace(collectCtx, container, output)
		if archiveErr != nil {
			t.Error(archiveErr)
		}
		failures := validateLiveContention(state, files)
		if err := saveLiveJSON(filepath.Join(output, "validation.json"), map[string]any{"project_completed": completed, "failures": failures, "passed": completed && len(failures) == 0}); err != nil {
			t.Error(err)
		}
		if len(failures) > 0 {
			t.Errorf("business acceptance: %v", failures)
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cleanupCancel()
		if err := runner.Cleanup(cleanupCtx, pid, "deleted"); err != nil {
			t.Error(err)
		}
	}()
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	last := ""
	lastNotice := time.Time{}
	for {
		select {
		case <-done:
			t.Fatalf("scheduler stopped before project completion: %v", schedulerErr)
		case <-ctx.Done():
			t.Fatal("live project exceeded its 30 minute acceptance limit")
		case <-tick.C:
			var state board.State
			if err = client.Do(ctx, "GET", "/projects/"+pid+"/state", nil, &state, nil); err != nil {
				t.Fatal(err)
			}
			statuses := []string{}
			for _, step := range state.Steps {
				statuses = append(statuses, step.ID+":"+step.Status)
			}
			progress := fmt.Sprintf("status=%s revision=%d facts=%d steps=%v", state.Graph.Project.Status, state.Revision, len(state.FactRecords), statuses)
			if progress != last || time.Since(lastNotice) > 30*time.Second {
				t.Logf("elapsed=%.1fs %s", time.Since(started).Seconds(), progress)
				_ = saveLiveJSON(filepath.Join(output, "progress.json"), map[string]any{"at": time.Now().UTC(), "elapsed_seconds": time.Since(started).Seconds(), "progress": progress, "runs": runner.snapshot()})
				_ = saveLiveJSON(filepath.Join(output, "http-observations.json"), observations.Snapshot())
				last, lastNotice = progress, time.Now()
			}
			if state.Graph.Project.Status == "completed" {
				completed = true
				manifest["completed_observed"] = time.Now().UTC()
				manifest["project_wall_seconds"] = time.Since(started).Seconds()
				_ = saveLiveJSON(filepath.Join(output, "manifest.json"), manifest)
				// Allow the final receipt to settle before cancellation/export.
				for n := 0; n < 20; n++ {
					active := false
					for _, run := range runner.snapshot() {
						active = active || run.Finished.IsZero()
					}
					if !active {
						break
					}
					time.Sleep(100 * time.Millisecond)
				}
				return
			}
			if state.Graph.Project.Status != "active" {
				t.Fatalf("project stopped before completion: %s", state.Graph.Project.Status)
			}
		}
	}
}

func collectLiveWorkspace(ctx context.Context, container, output string) (map[string][]byte, error) {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", "/var/run/docker.sock")
	}}
	defer transport.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/containers/"+url.PathEscape(container)+"/archive?path=%2Fworkspace", nil)
	if err != nil {
		return nil, err
	}
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("workspace archive HTTP %d", response.StatusCode)
	}
	files := map[string][]byte{}
	reader := tar.NewReader(response.Body)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return files, err
		}
		name := filepath.Clean(header.Name)
		if !strings.HasPrefix(name, "workspace/") || strings.Contains(name, "..") || header.Typeflag != tar.TypeReg {
			continue
		}
		if header.Size > 64<<20 {
			return files, fmt.Errorf("oversized retained file %q", name)
		}
		raw, err := io.ReadAll(reader)
		if err != nil {
			return files, err
		}
		target := filepath.Join(output, name)
		if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return files, err
		}
		if err = os.WriteFile(target, raw, 0600); err != nil {
			return files, err
		}
		files["/"+name] = raw
	}
	return files, nil
}
