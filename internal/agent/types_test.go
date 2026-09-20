package agent

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestBlockJSONPreservesRequiredEmptyFields(t *testing.T) {
	for _, tc := range []struct {
		name  string
		block Block
		want  string
	}{
		{"text", Block{Type: "text"}, `{"type":"text","text":""}`},
		{"thinking", Block{Type: "thinking", Signature: "opaque"}, `{"type":"thinking","thinking":"","signature":"opaque"}`},
		{"empty signature", Block{Type: "thinking", Thinking: "reason"}, `{"type":"thinking","thinking":"reason","signature":""}`},
		{"redacted thinking", Block{Type: "redacted_thinking"}, `{"type":"redacted_thinking","data":""}`},
		{"empty tool result", Block{Type: "tool_result", ToolUseID: "call", Content: json.RawMessage(`""`)}, `{"type":"tool_result","tool_use_id":"call","content":""}`},
		{"empty tool arguments", Block{Type: "tool_use", ID: "call", Name: "read", Input: json.RawMessage(`{}`)}, `{"type":"tool_use","id":"call","name":"read","input":{}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.block)
			if err != nil {
				t.Fatal(err)
			}
			var got, want map[string]any
			json.Unmarshal(raw, &got)
			json.Unmarshal([]byte(tc.want), &want)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %s want %s", raw, tc.want)
			}
		})
	}
}
