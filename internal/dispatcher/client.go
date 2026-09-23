package dispatcher

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"xloom/internal/board"
)

type Client struct {
	Base string
	HTTP *http.Client
}
type ProtocolError struct {
	Status int
	Detail string
}

func (e *ProtocolError) Error() string { return fmt.Sprintf("board HTTP %d: %s", e.Status, e.Detail) }

type Lease struct {
	Run    string
	Kind   string
	Intent string
}

func (c *Client) Do(ctx context.Context, method, path string, input, output any, lease *Lease) error {
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.Base, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if lease != nil {
		req.Header.Set("X-Xloom-Run", lease.Run)
		req.Header.Set("X-Xloom-Lease", lease.Kind)
		req.Header.Set("X-Xloom-Intent", lease.Intent)
	}
	h := c.HTTP
	if h == nil {
		h = &http.Client{Timeout: 10 * time.Second}
	}
	res, err := h.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return &ProtocolError{res.StatusCode, string(data)}
	}
	if output == nil {
		_, err = io.Copy(io.Discard, res.Body)
		return err
	}
	const maxResponseBytes = 32 << 20
	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxResponseBytes {
		return fmt.Errorf("board response exceeds %d-byte limit", maxResponseBytes)
	}
	return json.Unmarshal(data, output)
}
func (c *Client) List(ctx context.Context) ([]board.Summary, error) {
	var p []board.Summary
	err := c.Do(ctx, "GET", "/projects", nil, &p, nil)
	return p, err
}
func (c *Client) Get(ctx context.Context, id string) (board.Graph, error) {
	var g board.Graph
	err := c.Do(ctx, "GET", projectPath(id), nil, &g, nil)
	return g, err
}
func projectPath(id string) string { return "/projects/" + url.PathEscape(id) }
