package provider

import (
	"context"
	"strings"
	"testing"
	"xloom/internal/agent"
)

func TestFragmentedArguments(t *testing.T) {
	stream := `data: {"type":"message_start"}

data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"a","name":"read","input":{}}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"test\"}"}}

data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}

data: {"type":"message_stop"}

`
	m, err := consumeSSE(context.Background(), strings.NewReader(stream), func(agent.Event) {})
	if err != nil {
		t.Fatal(err)
	}
	if string(m.Content[0].Input) != `{"path":"test"}` {
		t.Fatal(string(m.Content[0].Input))
	}
}
func TestIncompleteStreamFails(t *testing.T) {
	_, err := consumeSSE(context.Background(), strings.NewReader("data: {\"type\":\"message_start\"}\n\n"), nil)
	if err == nil {
		t.Fatal("accepted incomplete stream")
	}
}
