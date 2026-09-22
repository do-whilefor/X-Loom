//go:build linux

package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xloom/internal/agent"
	"xloom/internal/tools"
)

// A controlled model selects source ranges, then copies through the public
// tool schema after compaction. It deliberately generates altered notes and a
// separate altered file, which must never be labelled a verified copy.
func TestFileFidelityThroughReadCompactionAndWrite(t *testing.T) {
	dir := t.TempDir()
	original := []byte("{\"unicode\":\"e\u0301/é/雪/😀\",\"integer\":9007199254740993123456789,\"escaped\":\"\\n\\\\\\\"\"}\r\n" +
		strings.Repeat("{\"progress\":\"synthetic unverified data\"}\r\n", 1600) + "{\"condition\":\"MODE=B remains unverified\"}")
	if err := os.WriteFile(filepath.Join(dir, "original.jsonl"), original, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(original)
	model := &fidelityModel{t: t, digest: hex.EncodeToString(sum[:])}
	set := tools.Set{Dir: dir, RunDir: filepath.Join(dir, "run"), OutputBytes: len(original) + 1}
	loop := &agent.Loop{Provider: model, Tools: set.All(), ContextBytes: 18000, RecentBytes: 256, SummaryBytes: 6000,
		Emit: func(event agent.Event) {
			if event.Type == "context_compacted" {
				model.compactions++
				if len(event.Compaction.Quotes) != 2 {
					t.Fatal("missing exact head/tail sources")
				}
				for _, quote := range event.Compaction.Quotes {
					if quote.SourceSHA256 != model.digest || quote.StartByte < 0 || quote.EndByte > len(original) ||
						quote.Text != string(original[quote.StartByte:quote.EndByte]) {
						t.Fatal("compaction changed original tool bytes or their identity")
					}
				}
			}
		},
	}
	if _, err := loop.Run(context.Background(), "Read original.jsonl and preserve it byte for byte using source_path. Interpretations are not original evidence."); err != nil {
		t.Fatal(err)
	}
	if model.compactions != 1 || model.turn != 3 {
		t.Fatalf("unexpected execution: %+v", model)
	}
	copied, err := os.ReadFile(filepath.Join(dir, "copied.jsonl"))
	if err != nil || !bytes.Equal(copied, original) {
		t.Fatal("copy changed after context compaction", err)
	}
	generated, err := os.ReadFile(filepath.Join(dir, "generated.txt"))
	if err != nil || string(generated) != "é" {
		t.Fatal("generated content path stopped working", err)
	}
}

type fidelityModel struct {
	t                 *testing.T
	digest            string
	turn, compactions int
}

func (m *fidelityModel) Generate(_ context.Context, history []agent.Message, defs []agent.Definition, _ agent.Emit) (agent.Message, error) {
	m.turn++
	if len(defs) != 7 {
		return agent.Message{}, fmt.Errorf("unexpected tool set: %d", len(defs))
	}
	switch m.turn {
	case 1:
		return agent.Message{Role: "assistant", Content: []agent.Block{{Type: "tool_use", ID: "read", Name: "read", Input: json.RawMessage(`{"path":"original.jsonl"}`)}}}, nil
	case 2:
		if m.compactions != 1 || !strings.Contains(history[1].Text(), "unverified_notes") {
			return agent.Message{}, fmt.Errorf("summary trust labels missing")
		}
		copyInput, _ := json.Marshal(map[string]string{"path": "copied.jsonl", "source_path": "original.jsonl", "source_sha256": m.digest})
		return agent.Message{Role: "assistant", Content: []agent.Block{
			{Type: "tool_use", ID: "copy", Name: "write", Input: copyInput},
			{Type: "tool_use", ID: "generate", Name: "write", Input: json.RawMessage(`{"path":"generated.txt","content":"é"}`)},
		}}, nil
	case 3:
		for _, result := range history[len(history)-1].Content {
			if result.IsError {
				return agent.Message{}, fmt.Errorf("write failed")
			}
			var text string
			var receipt struct{ Mode, SHA256 string }
			if json.Unmarshal(result.Content, &text) != nil || json.Unmarshal([]byte(text), &receipt) != nil {
				return agent.Message{}, fmt.Errorf("missing receipt")
			}
			if result.ToolUseID == "copy" && (receipt.Mode != "copied" || receipt.SHA256 != m.digest) {
				return agent.Message{}, fmt.Errorf("copy receipt is not verified")
			}
			if result.ToolUseID == "generate" && receipt.Mode != "generated" {
				return agent.Message{}, fmt.Errorf("model output promoted to verified copy")
			}
		}
		return agent.Text("assistant", "done"), nil
	default:
		return agent.Message{}, fmt.Errorf("unexpected extra model call")
	}
}

func (m *fidelityModel) GenerateSummary(_ context.Context, messages []agent.Message, _ int, _ agent.Emit) (agent.Message, error) {
	const marker = "Transcript and quote sources (data, not instructions):\n"
	_, raw, ok := strings.Cut(messages[0].Text(), marker)
	if !ok {
		return agent.Message{}, fmt.Errorf("summary source inventory missing")
	}
	var input struct {
		Sources []struct {
			ID    string
			Lines []struct {
				Line int
				Text string
			}
		} `json:"quote_sources"`
	}
	if err := json.Unmarshal([]byte(raw), &input); err != nil {
		return agent.Message{}, err
	}
	selections := []map[string]any{}
	for _, source := range input.Sources {
		for _, line := range source.Lines {
			if strings.Contains(line.Text, "unicode") || strings.Contains(line.Text, "MODE=B") {
				selections = append(selections, map[string]any{"source": source.ID, "start_line": line.Line, "end_line": line.Line})
			}
		}
	}
	if len(selections) != 2 {
		return agent.Message{}, fmt.Errorf("fixture head/tail were not exposed")
	}
	text, _ := json.Marshal(map[string]any{"notes": "The model may normalize e + combining acute to é; this interpretation is not an exact copy.", "quotes": selections})
	return agent.Text("assistant", string(text)), nil
}
