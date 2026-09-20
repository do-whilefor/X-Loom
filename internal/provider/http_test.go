package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"xloom/internal/agent"
)

func TestGeneratedSessionIDStableAcrossConcurrentRequests(t *testing.T) {
	var mu sync.Mutex
	ids := map[string]bool{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ids[r.Header.Get("x-opencode-session")] = true
		mu.Unlock()
		fmt.Fprint(w, `{"role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
	}))
	defer s.Close()
	p := Anthropic{BaseURL: s.URL, Token: "test"}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.Generate(context.Background(), []agent.Message{agent.Text("user", "task")}, nil, nil); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(ids) != 1 || ids[""] {
		t.Fatal("session ID changed", ids)
	}
}

func TestEndpointAndWireRequest(t *testing.T) {
	for _, base := range []string{"", "/v1", "/v1/messages"} {
		t.Run(base, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/messages" || r.Header.Get("Authorization") != "Bearer test-token" || r.Header.Get("x-api-key") != "test-token" || r.Header.Get("User-Agent") != "xloom/0.1" || r.Header.Get("x-opencode-session") != "session-test" || r.Header.Get("anthropic-version") == "" {
					t.Errorf("invalid request: %s", r.URL.Path)
				}
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if strings.Contains(string(body["messages"]), "stop_reason") {
					t.Error("session stop reason leaked into wire request")
				}
				if string(body["model"]) != `"deepseek-v4.1-flash"` || string(body["stream"]) != "true" {
					t.Error("wrong defaults")
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
			}))
			defer s.Close()
			p := Anthropic{BaseURL: s.URL + base, Token: "test-token", SessionID: "session-test"}
			m, err := p.Generate(context.Background(), []agent.Message{{Role: "assistant", Content: []agent.Block{{Type: "text", Text: "previous"}}, StopReason: "end_turn"}}, nil, nil)
			if err != nil || m.Text() != "ok" {
				t.Fatal(m, err)
			}
		})
	}
}

func TestReplayEmptyThinkingPreservesRequiredFields(t *testing.T) {
	stream := `data: {"type":"message_start"}

data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"opaque-signature"}}

data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call-1","name":"read","input":{"path":"evidence"}}}

data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}

data: {"type":"message_stop"}

`
	assistant, err := consumeSSE(context.Background(), strings.NewReader(stream), nil)
	if err != nil {
		t.Fatal(err)
	}
	// Mirror the strict schema failure observed on OpenCode's next request.
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct {
				Content []map[string]json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		block := request.Messages[1].Content[0]
		if string(block["thinking"]) != `""` || string(block["signature"]) != `"opaque-signature"` {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		fmt.Fprint(w, `{"role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
	}))
	defer s.Close()
	p := Anthropic{BaseURL: s.URL, Token: "test"}
	_, err = p.Generate(context.Background(), []agent.Message{agent.Text("user", "task"), assistant, {Role: "user", Content: []agent.Block{{Type: "tool_result", ToolUseID: "call-1", Content: json.RawMessage(`"evidence"`)}}}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Session JSON uses the same shape, so recovery does not lose the field.
	raw, err := json.Marshal(assistant)
	if err != nil || !strings.Contains(string(raw), `"thinking":""`) {
		t.Fatal(string(raw), err)
	}
}
func TestHTTPErrorRedactsBodyAndToken(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		fmt.Fprint(w, "secret response body")
	}))
	defer s.Close()
	p := Anthropic{BaseURL: s.URL, Token: "secret-token"}
	_, err := p.Generate(context.Background(), []agent.Message{agent.Text("user", "task")}, nil, nil)
	var target *HTTPError
	if !errors.As(err, &target) || target.Status != 401 || strings.Contains(err.Error(), "secret") {
		t.Fatal(err)
	}
}
func TestRetryWaitHonorsCancellation(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(429) }))
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	p := Anthropic{BaseURL: s.URL, Token: "test"}
	_, err := p.Generate(ctx, []agent.Message{agent.Text("user", "task")}, nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}
func TestStreamingTextAndToolEvents(t *testing.T) {
	data := `data: {"type":"message_start"}

data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}

data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"t","name":"read","input":{}}}

data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"a\"}"}}

data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}

data: {"type":"message_stop"}

`
	events := []string{}
	m, err := consumeSSE(context.Background(), strings.NewReader(data), func(e agent.Event) { events = append(events, e.Type) })
	if err != nil || m.Text() != "hello" || strings.Join(events, ",") != "text_delta,tool_delta" {
		t.Fatal(m, events, err)
	}
}
func TestTruncatedArgumentsAreNeverUsable(t *testing.T) {
	data := "data: {\"type\":\"message_start\"}\n\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t\",\"name\":\"write\",\"input\":{}}}\n\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"path\\\":\"}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
	m, err := consumeSSE(context.Background(), strings.NewReader(data), nil)
	if err != nil || m.StopReason != "max_tokens" || !json.Valid(m.Content[0].Input) {
		t.Fatal(m, err)
	}
	if _, err := consumeSSE(context.Background(), strings.NewReader(strings.ReplaceAll(data, "max_tokens", "tool_use")), nil); err == nil {
		t.Fatal("accepted incomplete arguments without truncation")
	}
}
func TestMalformedStreamsFail(t *testing.T) {
	for name, data := range map[string]string{
		"duplicate start": "data: {\"type\":\"message_start\"}\n\ndata: {\"type\":\"message_start\"}\n\n",
		"sparse block":    "data: {\"type\":\"message_start\"}\n\ndata: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"text\"}}\n\n",
		"orphan delta":    "data: {\"type\":\"message_start\"}\n\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"x\"}}\n\n",
		"error":           "data: {\"type\":\"error\"}\n\n",
		"bad json":        "data: nope\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := consumeSSE(context.Background(), strings.NewReader(data), nil); err == nil {
				t.Fatal("accepted malformed stream")
			}
		})
	}
}
