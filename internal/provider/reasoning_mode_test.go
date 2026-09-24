package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"xloom/internal/agent"
)

// OpenRouter's public MessagesRequest schema requires budget_tokens for enabled
// thinking, while adaptive accepts output_config.effort without a token budget:
// https://openrouter.ai/docs/api/api-reference/anthropic-messages/create-a-message
func TestThinkingModeMatchesEndpointForTurnsAndSummaries(t *testing.T) {
	for _, endpoint := range []struct {
		name, base, thinking string
	}{
		{"default gateway", "", "enabled"},
		{"explicit default gateway", DefaultBaseURL, "enabled"},
		{"OpenRouter base", "https://openrouter.ai/api", "adaptive"},
		{"OpenRouter version", "https://openrouter.ai/api/v1", "adaptive"},
		{"OpenRouter messages", "https://openrouter.ai/api/v1/messages", "adaptive"},
		{"OpenRouter hostname casing and port", "https://OPENROUTER.AI:443/api/", "adaptive"},
		{"other gateway", "https://example.invalid/api", "enabled"},
		{"hostname suffix", "https://openrouter.ai.example.invalid/api", "enabled"},
		{"path only", "https://example.invalid/openrouter.ai/api", "enabled"},
	} {
		for _, effort := range []string{"low", "high", "max"} {
			t.Run(endpoint.name+"/"+effort, func(t *testing.T) {
				messages := []agent.Message{agent.Text("user", "Inspect the synthetic fixture")}
				calls, wireBytes := 0, 0
				p := testProvider(func(req *http.Request) (*http.Response, error) {
					calls++
					raw, err := io.ReadAll(req.Body)
					if err != nil {
						t.Fatal(err)
					}
					var payload struct {
						MaxTokens    int               `json:"max_tokens"`
						Thinking     map[string]string `json:"thinking"`
						OutputConfig struct {
							Effort string `json:"effort"`
						} `json:"output_config"`
					}
					if err := json.Unmarshal(raw, &payload); err != nil {
						t.Fatal(err)
					}
					budget := 65536
					if calls == 1 {
						wireBytes = len(raw)
					} else {
						budget = 16384
					}
					if len(payload.Thinking) != 1 || payload.Thinking["type"] != endpoint.thinking || payload.OutputConfig.Effort != effort || payload.MaxTokens != budget {
						t.Fatalf("request %d changed thinking mode, effort or output allowance: %+v", calls, payload)
					}
					return testResponse(req, http.StatusOK, "application/json", `{"role":"assistant","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn"}`), nil
				})
				p.BaseURL, p.ReasoningEffort, p.MaxTokens = endpoint.base, effort, 65536
				if _, err := p.Generate(context.Background(), messages, nil, nil); err != nil {
					t.Fatal(err)
				}
				if size, err := p.InputBytes(messages, nil); err != nil || size != wireBytes {
					t.Fatalf("request sizing differs from the emitted request: size=%d wire=%d err=%v", size, wireBytes, err)
				}
				if _, err := p.GenerateSummary(context.Background(), messages, 16384, nil); err != nil {
					t.Fatal(err)
				}
				if calls != 2 || p.MaxTokens != 65536 {
					t.Fatalf("unexpected retry or summary changed normal allowance: calls=%d max_tokens=%d", calls, p.MaxTokens)
				}
			})
		}
	}
}
