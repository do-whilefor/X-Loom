package agent

import (
	"context"
	"time"
)

// RequestObservation describes a logical provider call. HTTP retries inside a
// provider and usage not returned by that provider are not observable here.
type RequestObservation struct {
	Kind       string `json:"kind"`
	InputBytes int    `json:"input_bytes,omitempty"`
	InputBasis string `json:"input_basis,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	Usage      *Usage `json:"usage,omitempty"`
	Failed     bool   `json:"failed,omitempty"`
}

func (l *Loop) generate(ctx context.Context, messages []Message, defs []Definition, summaryTokens int) (Message, error) {
	observation := RequestObservation{Kind: "turn"}
	if summaryTokens > 0 {
		observation.Kind = "summary"
	}
	if l.ObserveRequests {
		observation.InputBytes, _ = l.inputBytes(messages, defs)
		observation.InputBasis = "wire_history_and_tools"
		if _, ok := l.Provider.(RequestSizer); ok {
			observation.InputBasis = "provider_request"
		}
		l.emit(Event{Type: "model_call_start", Request: &observation})
	}
	started := time.Now()
	var message Message
	var err error
	if summaryTokens > 0 {
		if provider, ok := l.Provider.(SummaryProvider); ok {
			message, err = provider.GenerateSummary(ctx, messages, summaryTokens, nil)
		} else {
			message, err = l.Provider.Generate(ctx, messages, nil, nil)
		}
	} else {
		message, err = l.Provider.Generate(ctx, messages, defs, l.emit)
	}
	if l.ObserveRequests {
		completed := observation
		completed.DurationMS, completed.Usage, completed.Failed = time.Since(started).Milliseconds(), message.Usage, err != nil
		l.emit(Event{Type: "model_call_end", Request: &completed})
	}
	return message, err
}
