package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"xloom/internal/agent"
)

func TestUsagePersistsButNeverLeaksToRequest(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []map[string]json.RawMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		for _, m := range body.Messages {
			for _, key := range []string{"sequence", "usage", "stop_reason"} {
				if _, ok := m[key]; ok {
					t.Error("local metadata leaked", key)
				}
			}
		}
		fmt.Fprint(w, `{"role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":300,"cache_creation_input_tokens":40}}`)
	}))
	defer s.Close()
	p := Anthropic{BaseURL: s.URL, Token: "test"}
	m, err := p.Generate(context.Background(), []agent.Message{{Role: "assistant", Content: []agent.Block{{Type: "text", Text: "previous"}}, Sequence: 7, Usage: &agent.Usage{InputTokens: 500}, StopReason: "end_turn"}}, nil, nil)
	if err != nil || m.Usage == nil || m.Usage.Input() != 440 || m.Usage.OutputTokens != 20 {
		t.Fatal(m, err)
	}
	raw, _ := json.Marshal(m)
	if !strings.Contains(string(raw), `"usage"`) {
		t.Fatal("usage was not persisted")
	}
}

func TestStreamingUsageMergesStartAndDelta(t *testing.T) {
	data := `data: {"type":"message_start","message":{"usage":{"input_tokens":12,"output_tokens":0,"cache_read_input_tokens":30}}}

data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"ok"}}

data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}

data: {"type":"message_stop"}

`
	m, err := consumeSSE(context.Background(), strings.NewReader(data), nil)
	if err != nil || m.Usage == nil || m.Usage.Input() != 42 || m.Usage.OutputTokens != 9 {
		t.Fatal(m, err)
	}
}

func TestIndependentSummaryOutputLimitPreservesMaxReasoning(t *testing.T) {
	var limits []int
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		var limit int
		_ = json.Unmarshal(body["max_tokens"], &limit)
		limits = append(limits, limit)
		if string(body["output_config"]) != `{"effort":"max"}` || string(body["thinking"]) != `{"type":"enabled"}` {
			t.Error("summary weakened reasoning")
		}
		if _, ok := body["tools"]; ok {
			t.Error("summary enabled tools")
		}
		fmt.Fprint(w, `{"role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
	}))
	defer s.Close()
	p := Anthropic{BaseURL: s.URL, Token: "test", MaxTokens: 32768}
	msg := []agent.Message{agent.Text("user", "task")}
	if _, err := p.GenerateSummary(context.Background(), msg, 2048, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Generate(context.Background(), msg, nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(limits) != 2 || limits[0] != 2048 || limits[1] != 32768 {
		t.Fatal(limits)
	}
}

func TestEndpointErrorClassificationDoesNotExposeBody(t *testing.T) {
	for _, test := range []struct {
		status int
		body   string
		kind   agent.ErrorKind
	}{
		{400, `{"error":{"type":"invalid_request_error","message":"prompt is too long: sensitive content"}}`, agent.ErrorContextOverflow},
		{400, `{"error":{"type":"invalid_request_error","message":"unknown field"}}`, agent.ErrorProvider},
		{429, `secret`, agent.ErrorRateLimit},
		{503, `secret`, agent.ErrorUnavailable},
	} {
		err := classifyEndpointError(test.status, []byte(test.body))
		var httpErr *HTTPError
		if err.Kind != test.kind || !errors.As(err, &httpErr) || httpErr.Status != test.status || strings.Contains(err.Error(), "sensitive") || strings.Contains(err.Error(), "secret") {
			t.Fatal(err)
		}
	}
	_, err := consumeSSE(context.Background(), strings.NewReader("data: {\"type\":\"message_start\"}\n\n"), nil)
	var modelErr *agent.ModelError
	if !errors.As(err, &modelErr) || modelErr.Kind != agent.ErrorTransport {
		t.Fatal(err)
	}
}

func TestInputBudgetIncludesSystemAndToolSchema(t *testing.T) {
	p := Anthropic{}
	msg := []agent.Message{agent.Text("user", "task")}
	n, err := p.InputBytes(msg, nil)
	if err != nil || n <= len(System) {
		t.Fatal(n, err)
	}
	more, err := p.InputBytes(msg, []agent.Definition{{Name: "read", Schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)}})
	if err != nil || more <= n {
		t.Fatal(n, more, err)
	}
}
