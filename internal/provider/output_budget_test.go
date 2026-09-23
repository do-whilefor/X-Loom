package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"xloom/internal/agent"
)

func TestGenerateOutputBudgetDefaultsAndOverrides(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured int
		want       int
	}{
		{"default", 0, 32768},
		{"bounded evaluation", 8192, 8192},
		{"larger explicit allowance", 65536, 65536},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			p := testProvider(func(req *http.Request) (*http.Response, error) {
				calls++
				var payload struct {
					MaxTokens    int `json:"max_tokens"`
					OutputConfig struct {
						Effort string `json:"effort"`
					} `json:"output_config"`
				}
				if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
					t.Fatal(err)
				}
				if payload.MaxTokens != tc.want || payload.OutputConfig.Effort != "max" {
					t.Fatalf("request budget=%d effort=%q, want %d/max", payload.MaxTokens, payload.OutputConfig.Effort, tc.want)
				}
				return testResponse(req, http.StatusOK, "application/json", `{"role":"assistant","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn"}`), nil
			})
			p.MaxTokens = tc.configured
			message, err := p.Generate(context.Background(), []agent.Message{agent.Text("user", "Do the task")}, nil, nil)
			if err != nil || message.Text() != "done" || calls != 1 {
				t.Fatalf("response=%q requests=%d error=%v", message.Text(), calls, err)
			}
		})
	}
}

type streamedToolCall struct {
	id, input string
}

func toolResponseStream(t *testing.T, stop string, calls ...streamedToolCall) string {
	t.Helper()
	var stream strings.Builder
	write := func(event any) {
		raw, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		stream.WriteString("data: ")
		stream.Write(raw)
		stream.WriteString("\n\n")
	}
	write(map[string]any{"type": "message_start"})
	for index, call := range calls {
		write(map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "tool_use", "id": call.id, "name": "record", "input": map[string]any{}}})
		write(map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "input_json_delta", "partial_json": call.input}})
		write(map[string]any{"type": "content_block_stop", "index": index})
	}
	write(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop}})
	write(map[string]any{"type": "message_stop"})
	return stream.String()
}

func TestStreamedTruncationSettlesToolsAndAllowsCompleteReissue(t *testing.T) {
	for _, stop := range []string{"max_tokens", "length"} {
		t.Run(stop, func(t *testing.T) {
			requests, effects := 0, 0
			p := testProvider(func(req *http.Request) (*http.Response, error) {
				requests++
				switch requests {
				case 1:
					stream := toolResponseStream(t, stop,
						streamedToolCall{"complete-before-truncation", `{"value":"must not execute"}`},
						streamedToolCall{"partial", `{"value":"unfinished`})
					return testResponse(req, http.StatusOK, "text/event-stream", stream), nil
				case 2:
					if effects != 0 {
						t.Fatal("a tool from the truncated response executed")
					}
					var payload struct {
						Messages []agent.Message `json:"messages"`
					}
					if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
						t.Fatal(err)
					}
					if len(payload.Messages) != 3 || len(payload.Messages[2].Content) != 2 {
						t.Fatalf("truncated tool group was not settled: %+v", payload.Messages)
					}
					for index, id := range []string{"complete-before-truncation", "partial"} {
						result := payload.Messages[2].Content[index]
						if result.Type != "tool_result" || result.ToolUseID != id || !result.IsError || !strings.Contains(string(result.Content), "truncated") {
							t.Fatalf("missing explicit truncation result for %s: %+v", id, result)
						}
					}
					return testResponse(req, http.StatusOK, "text/event-stream", toolResponseStream(t, "tool_use", streamedToolCall{"reissued", `{"value":"complete"}`})), nil
				case 3:
					return testResponse(req, http.StatusOK, "application/json", `{"role":"assistant","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn"}`), nil
				default:
					t.Fatal("unexpected extra model request")
					return nil, errors.New("unexpected request")
				}
			})
			loop := &agent.Loop{Provider: p, Tools: []agent.Tool{{
				Definition: agent.Definition{Name: "record", Schema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}`)},
				Execute: func(_ context.Context, input json.RawMessage) (string, error) {
					effects++
					if string(input) != `{"value":"complete"}` {
						t.Fatalf("executed unintended arguments: %s", input)
					}
					return "recorded", nil
				},
			}}}
			result, err := loop.Run(context.Background(), "Record the value")
			if err != nil || result != "done" || requests != 3 || effects != 1 {
				t.Fatalf("result=%q requests=%d effects=%d error=%v", result, requests, effects, err)
			}
			if loop.History[1].StopReason != stop || !json.Valid(loop.History[1].Content[1].Input) {
				t.Fatal("truncation reason or serializable call identity was lost")
			}
			if err := loop.RepairHistory(); err != nil {
				t.Fatalf("reissue left an invalid transcript: %v", err)
			}
		})
	}
}

func TestStreamedInvalidArgumentsWithNormalStopAreRejected(t *testing.T) {
	for _, stop := range []string{"end_turn", "tool_use"} {
		t.Run(stop, func(t *testing.T) {
			requests, effects := 0, 0
			p := testProvider(func(req *http.Request) (*http.Response, error) {
				requests++
				stream := toolResponseStream(t, stop, streamedToolCall{"invalid", `{"value":"unfinished`})
				return testResponse(req, http.StatusOK, "text/event-stream", stream), nil
			})
			loop := &agent.Loop{Provider: p, Tools: []agent.Tool{{
				Definition: agent.Definition{Name: "record", Schema: json.RawMessage(`{"type":"object"}`)},
				Execute: func(context.Context, json.RawMessage) (string, error) {
					effects++
					return "must not execute", nil
				},
			}}}
			_, err := loop.Run(context.Background(), "Record the value")
			var modelErr *agent.ModelError
			if !errors.As(err, &modelErr) || modelErr.Kind != agent.ErrorProvider || !strings.Contains(err.Error(), "invalid streamed tool arguments") || requests != 1 || effects != 0 {
				t.Fatalf("malformed arguments were retried or executed: requests=%d effects=%d error=%v", requests, effects, err)
			}
			if len(loop.History) != 1 {
				t.Fatal("malformed provider response entered the durable conversation")
			}
		})
	}
}
