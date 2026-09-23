package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

type observedProvider struct {
	generate func(context.Context, []Message, []Definition, Emit) (Message, error)
	summary  func(context.Context, []Message, int, Emit) (Message, error)
}

func (p observedProvider) Generate(ctx context.Context, messages []Message, defs []Definition, emit Emit) (Message, error) {
	return p.generate(ctx, messages, defs, emit)
}

func (p observedProvider) GenerateSummary(ctx context.Context, messages []Message, limit int, emit Emit) (Message, error) {
	return p.summary(ctx, messages, limit, emit)
}

func TestRequestLifecycleTimestampsAndParallelTools(t *testing.T) {
	var events []Event
	turns := 0
	bothStarted := make(chan struct{})
	var toolStarts atomic.Int32
	tool := Tool{Definition: Definition{Name: "observe", Schema: json.RawMessage(`{"type":"object"}`)}, Parallel: true,
		Execute: func(ctx context.Context, _ json.RawMessage) (string, error) {
			if toolStarts.Add(1) == 2 {
				close(bothStarted)
			}
			select {
			case <-bothStarted:
				return "observed", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}}
	provider := observedProvider{generate: func(_ context.Context, _ []Message, _ []Definition, emit Emit) (Message, error) {
		turns++
		emit(Event{Type: "text_delta", Text: "visible"})
		m := Text("assistant", "done")
		m.Usage = &Usage{InputTokens: 13, OutputTokens: 7}
		if turns == 1 {
			m.Content = []Block{
				{Type: "tool_use", ID: "a", Name: "observe", Input: json.RawMessage(`{}`)},
				{Type: "tool_use", ID: "b", Name: "observe", Input: json.RawMessage(`{}`)},
			}
		}
		return m, nil
	}}
	l := Loop{Provider: provider, Tools: []Tool{tool}, ObserveRequests: true, Emit: func(e Event) { events = append(events, e) }}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	before := time.Now()
	if _, err := l.Run(ctx, "inspect"); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	var lifecycle []string
	last := before
	openTools := map[string]bool{}
	startsCount, endsCount, usageCount := 0, 0, 0
	for _, e := range events {
		if e.At == "" {
			if e.Type != "text_delta" && e.Type != "message_end" {
				t.Fatalf("lifecycle event lacks timestamp: %+v", e)
			}
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, e.At)
		if err != nil || at.Before(last) || at.After(after) || at.Location() != time.UTC {
			t.Fatalf("invalid lifecycle timestamp %q between %v and %v: %v", e.At, last, after, err)
		}
		last = at
		if e.Type == "text_delta" || e.Type == "message_end" {
			t.Fatalf("bulk transcript event unexpectedly timestamped: %s", e.Type)
		}
		switch e.Type {
		case "tool_start":
			if openTools[e.ToolID] {
				t.Fatalf("duplicate tool start: %s", e.ToolID)
			}
			openTools[e.ToolID] = true
			startsCount++
		case "tool_end":
			if !openTools[e.ToolID] {
				t.Fatalf("tool end without matching start: %s", e.ToolID)
			}
			delete(openTools, e.ToolID)
			endsCount++
		default:
			lifecycle = append(lifecycle, e.Type)
		}
		if e.Request != nil && e.Request.Usage != nil {
			if e.Type != "model_call_end" || *e.Request.Usage != (Usage{InputTokens: 13, OutputTokens: 7}) {
				t.Fatalf("usage recorded outside completed request: %+v", e)
			}
			usageCount++
		}
	}
	want := []string{"agent_start", "turn_start", "model_call_start", "model_call_end", "turn_end", "turn_start", "model_call_start", "model_call_end", "turn_end", "agent_end"}
	if !reflect.DeepEqual(lifecycle, want) || startsCount != 2 || endsCount != 2 || len(openTools) != 0 || usageCount != 2 {
		t.Fatalf("incomplete lifecycle: %v tools=%d/%d open=%v usage=%d", lifecycle, startsCount, endsCount, openTools, usageCount)
	}
}

func TestRequestObservationIncludesSummaryAndFailedUsage(t *testing.T) {
	var events []Event
	wantError := errors.New("partial stream failed")
	usage := &Usage{InputTokens: 23, OutputTokens: 11, CacheReadTokens: 5}
	p := observedProvider{summary: func(_ context.Context, _ []Message, limit int, _ Emit) (Message, error) {
		if limit != 256 {
			t.Fatalf("summary token control changed: %d", limit)
		}
		return Message{Role: "assistant", Usage: usage}, wantError
	}}
	l := Loop{Provider: p, ObserveRequests: true, Emit: func(e Event) { events = append(events, e) }}
	if _, err := l.generate(context.Background(), []Message{Text("user", "summarize")}, nil, 256); !errors.Is(err, wantError) {
		t.Fatalf("error = %v", err)
	}
	if len(events) != 2 || events[0].Type != "model_call_start" || events[1].Type != "model_call_end" {
		t.Fatalf("missing summary lifecycle: %+v", events)
	}
	for _, e := range events {
		if e.At == "" || e.Request == nil || e.Request.Kind != "summary" {
			t.Fatalf("missing summary observation: %+v", e)
		}
	}
	if events[0].Request.Usage != nil || events[0].Request.Failed || !events[1].Request.Failed || !reflect.DeepEqual(events[1].Request.Usage, usage) {
		t.Fatalf("failed request usage not observed exactly once: %+v", events)
	}
}
