package docker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"path"
	"time"

	"xloom/internal/worker"
)

func (c *Client) SetGraphHandler(handler func(context.Context, worker.Job, worker.GraphRequest) (any, error)) {
	c.graphMu.Lock()
	defer c.graphMu.Unlock()
	c.graphHandler = handler
}

func (c *Client) graphBridge(parent context.Context, name, runDir string, j worker.Job) func(worker.GraphRequest) error {
	seen := map[string][32]byte{}
	return func(request worker.GraphRequest) error {
		if !j.GraphRPC {
			return errors.New("graph bridge is disabled for this job")
		}
		if !worker.ValidGraphRequestID(request.RequestID) {
			return errors.New("invalid graph request_id")
		}
		ctx, cancel := context.WithTimeout(parent, 20*time.Second)
		defer cancel()
		if err := ctx.Err(); err != nil {
			return err
		}
		response := worker.GraphResponse{RequestID: request.RequestID}
		raw, err := json.Marshal(request)
		digest := sha256.Sum256(raw)
		if err == nil {
			err = worker.ValidateGraphRequest(j, request)
		}
		if previous, ok := seen[request.RequestID]; ok && previous != digest {
			err = errors.New("graph request_id reused for different input")
		}
		if len(seen) >= 4096 {
			err = errors.New("graph bridge request limit reached")
		}
		if err == nil {
			seen[request.RequestID] = digest
			c.graphMu.RLock()
			handler := c.graphHandler
			c.graphMu.RUnlock()
			if handler == nil {
				err = errors.New("dispatcher has no graph handler")
			} else {
				var result any
				result, err = handler(ctx, j, request)
				if err == nil {
					response.Result, err = json.Marshal(result)
					if err == nil && request.Op == "graph_action" {
						response.Result, err = worker.CompactGraphActionResult(response.Result)
					}
				}
			}
		}
		if err != nil {
			response.Error = err.Error()
			response.Result = nil
		}
		raw, err = json.Marshal(response)
		if err != nil {
			return err
		}
		if len(raw) > worker.MaxGraphRPCBytes {
			response.Result = nil
			response.Error = "graph response exceeds 128 KiB; request a smaller page"
			raw, _ = json.Marshal(response)
		}
		// A complete temporary file is uploaded before publishing the response.
		// Every path component comes from the verified run or a 32-hex request ID.
		target := path.Join(runDir, "graph-response-"+request.RequestID+".json")
		tmp := target + ".tmp"
		if err = c.archive(ctx, name, tmp, raw); err != nil {
			return err
		}
		_, err = c.exec(ctx, name, []string{"/bin/mv", "-T", "--", tmp, target}, nil, io.Discard)
		return err
	}
}
