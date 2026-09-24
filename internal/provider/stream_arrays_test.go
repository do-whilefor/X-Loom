package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"xloom/internal/agent"
)

func TestGeneratePreservesFragmentedGraphActionArrays(t *testing.T) {
	calls := []struct {
		id, input string
		arrays    map[string]int
	}{
		{"step", `{"op":"step","idempotency_key":"next","payload":{"action":"add","from":["origin","fact-two"],"description":"Check \"array[]\" and \u03b1"}}`, map[string]int{"from": 2}},
		{"finding", `{"op":"finding","idempotency_key":"support","payload":{"claim":"Observed","scope":"fixture","status":"candidate","sources":["fact-one","fact-two"],"evidence":[{"path":"evidence/one.txt","start_line":1,"end_line":2},{"path":"evidence/two.txt"}]}}`, map[string]int{"sources": 2, "evidence": 2}},
		{"empty-step", `{"op":"step","idempotency_key":"empty-step","payload":{"action":"add","from":[],"description":"Empty references"}}`, map[string]int{"from": 0}},
		{"empty-finding", `{"op":"finding","idempotency_key":"empty-finding","payload":{"claim":"Empty support","scope":"fixture","status":"candidate","sources":[],"evidence":[]}}`, map[string]int{"sources": 0, "evidence": 0}},
	}
	var stream strings.Builder
	writeEvent := func(event any) {
		t.Helper()
		raw, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		stream.WriteString("data: ")
		stream.Write(raw)
		stream.WriteString("\n\n")
	}
	writeEvent(map[string]any{"type": "message_start"})
	for index, call := range calls {
		writeEvent(map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "tool_use", "id": call.id, "name": "graph_action", "input": map[string]any{}}})
		// Split inside array delimiters, object fields and JSON escape sequences.
		for _, char := range call.input {
			writeEvent(map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(char)}})
		}
		writeEvent(map[string]any{"type": "content_block_stop", "index": index})
	}
	writeEvent(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}})
	writeEvent(map[string]any{"type": "message_stop"})

	requests := 0
	p := testProvider(func(req *http.Request) (*http.Response, error) {
		requests++
		return testResponse(req, http.StatusOK, "text/event-stream", stream.String()), nil
	})
	deltas := make(map[string]*strings.Builder, len(calls))
	for _, call := range calls {
		deltas[call.id] = &strings.Builder{}
	}
	message, err := p.Generate(context.Background(), []agent.Message{agent.Text("user", "Inspect the synthetic fixture")}, []agent.Definition{{Name: "graph_action", Schema: json.RawMessage(`{"type":"object"}`)}}, func(event agent.Event) {
		if event.Type != "tool_delta" || event.ToolName != "graph_action" || deltas[event.ToolID] == nil {
			t.Fatalf("unexpected stream event: %+v", event)
		}
		deltas[event.ToolID].WriteString(event.Text)
	})
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 || message.Role != "assistant" || message.StopReason != "tool_use" || len(message.Content) != len(calls) {
		t.Fatalf("unexpected response: requests=%d message=%+v", requests, message)
	}
	for index, call := range calls {
		block := message.Content[index]
		if block.Type != "tool_use" || block.ID != call.id || block.Name != "graph_action" {
			t.Fatalf("tool identity changed: %+v", block)
		}
		if string(block.Input) != call.input || deltas[call.id].String() != call.input {
			t.Fatalf("%s arguments changed: input=%s emitted=%s want=%s", call.id, block.Input, deltas[call.id].String(), call.input)
		}
		var decoded struct {
			Payload map[string]any `json:"payload"`
		}
		if err := json.Unmarshal(block.Input, &decoded); err != nil {
			t.Fatal(err)
		}
		for field, count := range call.arrays {
			items, ok := decoded.Payload[field].([]any)
			if !ok || len(items) != count {
				t.Fatalf("%s payload.%s lost array shape: %#v", call.id, field, decoded.Payload[field])
			}
			for _, item := range items {
				if field == "evidence" {
					if _, ok := item.(map[string]any); !ok {
						t.Fatalf("evidence item lost object shape: %#v", item)
					}
				} else if _, ok := item.(string); !ok {
					t.Fatalf("reference lost string shape: %#v", item)
				}
			}
		}
	}
}
