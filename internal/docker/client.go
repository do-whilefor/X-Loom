// Package docker uses the Docker Engine HTTP API through its Unix socket.
package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"xloom/internal/config"
	"xloom/internal/worker"
)

type Client struct {
	Config config.Container
	http   *http.Client
	locks  sync.Map
}
type APIError struct{ Status int }

func (e *APIError) Error() string { return fmt.Sprintf("Docker Engine returned HTTP %d", e.Status) }
func New(c config.Container) *Client {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", c.Socket)
	}, ResponseHeaderTimeout: 30 * time.Second}
	return &Client{Config: c, http: &http.Client{Transport: transport}}
}
func (c *Client) Close()                { c.http.CloseIdleConnections() }
func (c *Client) name(id string) string { return c.Config.Namespace + "-dispatch-" + id }
func (c *Client) lock(id string) func() {
	value, _ := c.locks.LoadOrStore(id, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}
func (c *Client) request(ctx context.Context, method, route string, body io.Reader, content string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+route, body)
	if err != nil {
		return nil, err
	}
	if content != "" {
		req.Header.Set("Content-Type", content)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 300 {
		res.Body.Close()
		return nil, &APIError{res.StatusCode}
	}
	return res, nil
}
func (c *Client) json(ctx context.Context, method, route string, input, output any) error {
	var body io.Reader
	if input != nil {
		raw, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	res, err := c.request(ctx, method, route, body, "application/json")
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if output != nil {
		return json.NewDecoder(io.LimitReader(res.Body, 16<<20)).Decode(output)
	}
	_, err = io.Copy(io.Discard, res.Body)
	return err
}
func status(err error, code int) bool { var e *APIError; return errors.As(err, &e) && e.Status == code }
func (c *Client) ensure(ctx context.Context, id string) (string, error) {
	unlock := c.lock(id)
	defer unlock()
	name := c.name(id)
	var info struct {
		Image  string `json:"Image"`
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
		State struct {
			Running bool `json:"Running"`
		} `json:"State"`
	}
	err := c.json(ctx, "GET", "/containers/"+url.PathEscape(name)+"/json", nil, &info)
	if err == nil && (info.Config.Labels["xloom.namespace"] != c.Config.Namespace || info.Config.Labels["xloom.project"] != id) {
		return "", errors.New("container name belongs to a different project or dispatcher namespace")
	}
	missing := status(err, 404)
	if err != nil && !missing {
		return "", err
	}
	// A mutable tag may now name a different binary or runtime. Resolve it on
	// every launch and compare immutable IDs before touching a saved workspace.
	var desired struct {
		ID string `json:"Id"`
	}
	if err = c.json(ctx, "GET", "/images/"+url.PathEscape(c.Config.Image)+"/json", nil, &desired); err != nil {
		return "", fmt.Errorf("resolve configured worker image %q locally: %w", c.Config.Image, err)
	}
	if desired.ID == "" {
		return "", errors.New("Docker returned an empty ID for the configured worker image")
	}
	if missing {
		input := map[string]any{"Image": desired.ID, "Entrypoint": []string{"/bin/sh", "-c"}, "Cmd": []string{"exec sleep infinity"}, "WorkingDir": "/workspace", "Labels": map[string]string{"xloom.namespace": c.Config.Namespace, "xloom.project": id}, "HostConfig": map[string]any{"NetworkMode": c.Config.Network, "CapAdd": c.Config.CapAdd, "Init": true}}
		err = c.json(ctx, "POST", "/containers/create?name="+url.QueryEscape(name), input, nil)
		if status(err, 409) {
			missing = false
			err = c.json(ctx, "GET", "/containers/"+url.PathEscape(name)+"/json", nil, &info)
			if err == nil && (info.Config.Labels["xloom.namespace"] != c.Config.Namespace || info.Config.Labels["xloom.project"] != id) {
				return "", errors.New("container creation raced with a different project or dispatcher namespace")
			}
		}
	}
	if err != nil {
		return "", err
	}
	if !missing && info.Image != desired.ID {
		return "", fmt.Errorf("project %s container uses image %q but configured worker image %q resolves to %q; preserve and migrate its workspace before recreating the container", id, info.Image, c.Config.Image, desired.ID)
	}
	if !info.State.Running {
		err = c.json(ctx, "POST", "/containers/"+url.PathEscape(name)+"/start", nil, nil)
		if status(err, 304) {
			err = nil
		}
	}
	return name, err
}
func (c *Client) archive(ctx context.Context, name, target string, data []byte) error {
	if !strings.HasPrefix(target, "/workspace/.xloom/runs/") || path.Clean(target) != target {
		return errors.New("invalid run archive path")
	}
	var buffer bytes.Buffer
	tw := tar.NewWriter(&buffer)
	rel := strings.TrimPrefix(target, "/workspace/")
	parts := strings.Split(rel, "/")
	for n := 1; n < len(parts); n++ {
		if err := tw.WriteHeader(&tar.Header{Name: strings.Join(parts[:n], "/"), Typeflag: tar.TypeDir, Mode: 0700}); err != nil {
			return err
		}
	}
	if err := tw.WriteHeader(&tar.Header{Name: rel, Size: int64(len(data)), Mode: 0600}); err != nil {
		return err
	}
	if _, err := tw.Write(data); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	res, err := c.request(ctx, "PUT", "/containers/"+url.PathEscape(name)+"/archive?path=%2Fworkspace", &buffer, "application/x-tar")
	if err != nil {
		return err
	}
	res.Body.Close()
	return nil
}

// exec starts without TTY; Docker frames stdout/stderr with an 8-byte header.
func (c *Client) exec(ctx context.Context, name string, argv, env []string, out io.Writer) (string, error) {
	var created struct {
		ID string `json:"Id"`
	}
	err := c.json(ctx, "POST", "/containers/"+url.PathEscape(name)+"/exec", map[string]any{"AttachStdout": true, "AttachStderr": true, "Tty": false, "Cmd": argv, "Env": env, "WorkingDir": "/workspace"}, &created)
	if err != nil {
		return "", err
	}
	if created.ID == "" {
		return "", errors.New("Docker returned an empty exec ID")
	}
	data := strings.NewReader(`{"Detach":false,"Tty":false}`)
	res, err := c.request(ctx, "POST", "/exec/"+created.ID+"/start", data, "application/json")
	if err != nil {
		return created.ID, err
	}
	defer res.Body.Close()
	for {
		var header [8]byte
		_, err = io.ReadFull(res.Body, header[:])
		if err == io.EOF {
			break
		}
		if err != nil {
			return created.ID, err
		}
		size := binary.BigEndian.Uint32(header[4:])
		if size > 32<<20 {
			return created.ID, errors.New("oversized Docker stream frame")
		}
		destination := out
		if header[0] != 1 {
			destination = io.Discard
		}
		if _, err = io.CopyN(destination, res.Body, int64(size)); err != nil {
			return created.ID, err
		}
	}
	var info struct {
		Running bool `json:"Running"`
		Exit    int  `json:"ExitCode"`
	}
	for {
		if err = c.json(ctx, "GET", "/exec/"+created.ID+"/json", nil, &info); err != nil {
			return created.ID, err
		}
		if !info.Running {
			if info.Exit != 0 {
				return created.ID, fmt.Errorf("worker process exited with code %d", info.Exit)
			}
			return created.ID, nil
		}
		select {
		case <-ctx.Done():
			return created.ID, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

type resultSink struct {
	pending []byte
	result  worker.Result
	found   bool
}

func (s *resultSink) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		end := bytes.IndexByte(p, '\n')
		if end < 0 {
			s.pending = append(s.pending, p...)
			break
		}
		s.pending = append(s.pending, p[:end]...)
		if len(s.pending) > 32<<20 {
			return 0, errors.New("worker event too large")
		}
		var result worker.Result
		if json.Unmarshal(s.pending, &result) == nil && result.Type == "result" {
			if s.found {
				return 0, errors.New("worker emitted multiple results")
			}
			s.result = result
			s.found = true
		}
		s.pending = nil
		p = p[end+1:]
	}
	if len(s.pending) > 32<<20 {
		return 0, errors.New("worker event too large")
	}
	return n, nil
}
func (c *Client) Run(ctx context.Context, w config.Worker, j worker.Job) (worker.Result, error) {
	if j.RunID == "" || strings.ContainsAny(j.RunID, "/\\.") {
		return worker.Result{}, errors.New("invalid run ID")
	}
	for _, ch := range j.RunID {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_') {
			return worker.Result{}, errors.New("invalid run ID")
		}
	}
	name, err := c.ensure(ctx, j.Graph.Project.ID)
	if err != nil {
		return worker.Result{}, err
	}
	target := "/workspace/.xloom/runs/" + j.RunID + "/job.json"
	raw, err := json.Marshal(j)
	if err != nil {
		return worker.Result{}, err
	}
	if err = c.archive(ctx, name, target, raw); err != nil {
		return worker.Result{}, err
	}
	env := []string{}
	for k, v := range w.Env {
		env = append(env, k+"="+v)
	}
	sort.Strings(env)
	sink := &resultSink{}
	_, err = c.exec(ctx, name, []string{"/usr/local/bin/xloom", "worker", "--job", target}, env, sink)
	if err != nil || ctx.Err() != nil {
		c.cancel(name, path.Dir(target))
		if ctx.Err() != nil {
			return worker.Result{}, ctx.Err()
		}
		return worker.Result{}, err
	}
	if len(sink.pending) > 0 {
		if _, err = sink.Write([]byte{'\n'}); err != nil {
			return worker.Result{}, err
		}
	}
	if !sink.found {
		return worker.Result{}, errors.New("worker exited without a result")
	}
	return sink.result, nil
}
func (c *Client) cancel(name, runDir string) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	c.exec(ctx, name, []string{"/usr/local/bin/xloom", "worker", "--cancel", runDir}, nil, io.Discard)
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	c.exec(ctx, name, []string{"/usr/local/bin/xloom", "worker", "--cancel", runDir, "--force"}, nil, io.Discard)
}
func (c *Client) Cleanup(ctx context.Context, id, state string) error {
	unlock := c.lock(id)
	defer unlock()
	var info struct {
		ID     string `json:"Id"`
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
	}
	err := c.json(ctx, "GET", "/containers/"+url.PathEscape(c.name(id))+"/json", nil, &info)
	if status(err, 404) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Config.Labels["xloom.namespace"] != c.Config.Namespace || info.Config.Labels["xloom.project"] != id {
		return errors.New("refusing to clean up a container belonging to a different project or dispatcher namespace")
	}
	if info.ID == "" {
		return errors.New("Docker returned an empty container ID during cleanup")
	}
	// Names can be reassigned after inspection. Target the verified, immutable
	// container ID so a replacement with the same name is never stopped/removed.
	route := "/containers/" + url.PathEscape(info.ID)
	if state == "deleted" || (state == "completed" && c.Config.CompletedAction == "remove") {
		err = c.json(ctx, "DELETE", route+"?force=true", nil, nil)
	} else {
		err = c.json(ctx, "POST", route+"/stop?t=1", nil, nil)
	}
	if status(err, 404) || status(err, 304) {
		return nil
	}
	return err
}
func (c *Client) Projects(ctx context.Context) ([]string, error) {
	filters, _ := json.Marshal(map[string][]string{"label": {"xloom.namespace=" + c.Config.Namespace}})
	var containers []struct {
		Labels map[string]string `json:"Labels"`
	}
	if err := c.json(ctx, "GET", "/containers/json?all=true&filters="+url.QueryEscape(string(filters)), nil, &containers); err != nil {
		return nil, err
	}
	ids := []string{}
	for _, item := range containers {
		if id := item.Labels["xloom.project"]; id != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, nil
}
