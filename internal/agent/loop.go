package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

type Loop struct {
	Provider Provider
	Tools    []Tool
	History  []Message
	Emit     Emit
	Save     func([]Message) error
	// SaveState atomically persists the request view and its checkpoint. When
	// supplied it replaces Save. Original messages remain in message_end events.
	SaveState  func([]Message, *ContextCheckpoint) error
	Checkpoint *ContextCheckpoint
	Steering   <-chan string
	FollowUp   <-chan string
	Concluding bool
	Repairing  bool
	// Task and conclusion instructions remain verbatim across every compaction.
	TaskPrompt       string
	ConclusionPrompt string
	RepairPrompt     string
	// OnTurnEnd runs at a settled model/tool boundary. A returned prompt keeps
	// the same session running; a replacement context bounds subsequent turns.
	OnTurnEnd func(context.Context, *Loop, Message) (context.Context, string, error)
	// A byte budget is an approximation, not a provider token count.
	ContextBytes int
	// ContextTokens is an optional input allowance after reserving output space.
	// Token estimates are labelled estimates; ContextBytes remains a hard cap.
	ContextTokens int
	// ContextTargetTokens bounds the rebuilt request after compaction without
	// changing its trigger. Zero retains the legacy compaction allocation.
	ContextTargetTokens int
	RecentBytes         int
	SummaryBytes        int
	SummaryMaxTokens    int
	ObserveRequests     bool
	mu                  sync.Mutex
}

func (l *Loop) emit(e Event) {
	if l.Emit != nil {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.Emit(e)
	}
}
func (l *Loop) append(m Message) error {
	if err := l.initCheckpoint(); err != nil {
		return err
	}
	l.Checkpoint.LastSequence++
	m.Sequence = l.Checkpoint.LastSequence
	l.History = append(l.History, m)
	l.emit(Event{Type: "message_end", Message: &m})
	return l.saveState()
}

func (l *Loop) saveState() error {
	if l.SaveState != nil {
		return l.SaveState(l.History, l.Checkpoint)
	}
	if l.Save != nil {
		return l.Save(l.History)
	}
	return nil
}

// AppendInstruction adds a runtime instruction at a settled turn boundary.
// The caller must first settle any interrupted tool group with RepairHistory.
func (l *Loop) AppendInstruction(prompt string) error { return l.append(Text("user", prompt)) }
func queued(ch <-chan string) (string, bool) {
	if ch == nil {
		return "", false
	}
	select {
	case s, ok := <-ch:
		return s, ok
	default:
		return "", false
	}
}
func (l *Loop) Run(ctx context.Context, prompt string) (string, error) {
	if l.Provider == nil {
		return "", errors.New("missing model provider")
	}
	if err := l.initCheckpoint(); err != nil {
		return "", err
	}
	if l.TaskPrompt == "" {
		if len(l.History) > 0 {
			l.TaskPrompt = l.History[0].Text()
		} else {
			l.TaskPrompt = prompt
		}
	}
	if l.Concluding && l.ConclusionPrompt == "" && prompt != "" {
		l.ConclusionPrompt = prompt
	}
	if err := l.RepairHistory(); err != nil {
		return "", err
	}
	if prompt != "" {
		if err := l.append(Text("user", prompt)); err != nil {
			return "", err
		}
	}
	if len(l.History) == 0 {
		return "", errors.New("empty agent context")
	}
	l.emit(Event{Type: "agent_start"})
	defer l.emit(Event{Type: "agent_end"})
	last := ""
	for { // Outer loop: queued follow-up after an otherwise settled run.
		for { // Inner loop: model, tools and steering at turn boundaries.
			if err := ctx.Err(); err != nil {
				return last, err
			}
			if s, ok := queued(l.Steering); ok {
				if err := l.append(Text("user", s)); err != nil {
					return last, err
				}
			}
			if err := l.compact(ctx); err != nil {
				return last, err
			}
			var defs []Definition
			for _, t := range l.Tools {
				if !l.Concluding && !l.Repairing {
					defs = append(defs, t.Definition)
				}
			}
			l.emit(Event{Type: "turn_start"})
			m, err := l.generate(ctx, l.History, defs, 0)
			if err != nil {
				var modelErr *ModelError
				if errors.As(err, &modelErr) && modelErr.Kind == ErrorContextOverflow && l.Checkpoint.OverflowRetries < 1 {
					// Persist the allowance before the recovery attempt. Restarting
					// this run must not grant another attempt or replay any tool.
					l.Checkpoint.OverflowRetries++
					if saveErr := l.saveState(); saveErr != nil {
						return last, saveErr
					}
					if compactErr := l.compactForced(ctx); compactErr != nil {
						return last, compactErr
					}
					m, err = l.generate(ctx, l.History, defs, 0)
				}
			}
			if err != nil {
				return last, err
			}
			if m.Role != "assistant" {
				return last, errors.New("provider returned a non-assistant message")
			}
			if err := validateCalls(m); err != nil {
				return last, err
			}
			// A truncated argument fragment cannot be marshaled into a valid
			// session. Preserve the call identity but never execute any of its calls.
			if m.StopReason == "max_tokens" || m.StopReason == "length" {
				for n := range m.Content {
					if m.Content[n].Type == "tool_use" && !json.Valid(m.Content[n].Input) {
						m.Content[n].Input = json.RawMessage(`{}`)
					}
				}
			}
			if err = l.append(m); err != nil {
				return last, err
			}
			last = m.Text()
			calls := []Block{}
			for _, b := range m.Content {
				if b.Type == "tool_use" {
					calls = append(calls, b)
				}
			}
			if len(calls) > 0 {
				results := l.execute(ctx, calls, m.StopReason == "max_tokens" || m.StopReason == "length")
				// Always settle every emitted tool id, including after cancellation.
				if err = l.append(Message{Role: "user", Content: results}); err != nil {
					return last, err
				}
			}
			l.emit(Event{Type: "turn_end"})
			if err = ctx.Err(); err != nil {
				return last, err
			}
			if l.OnTurnEnd != nil {
				next, instruction, hookErr := l.OnTurnEnd(ctx, l, m)
				if hookErr != nil {
					return last, hookErr
				}
				if next != nil {
					ctx = next
				}
				if instruction != "" {
					if l.Repairing {
						l.RepairPrompt = instruction
					} else if l.Concluding {
						l.ConclusionPrompt = instruction
					}
					if err = l.append(Text("user", instruction)); err != nil {
						return last, err
					}
					continue
				}
			}
			if len(calls) > 0 {
				continue
			}
			if s, ok := queued(l.Steering); ok {
				if err = l.append(Text("user", s)); err != nil {
					return last, err
				}
				continue
			}
			break
		}
		if s, ok := queued(l.FollowUp); ok {
			if err := l.append(Text("user", s)); err != nil {
				return last, err
			}
			continue
		}
		return last, nil
	}
}
func (l *Loop) execute(ctx context.Context, calls []Block, truncated bool) []Block {
	out := make([]Block, len(calls))
	parallel := true
	find := func(name string) *Tool {
		for n := range l.Tools {
			if l.Tools[n].Name == name {
				return &l.Tools[n]
			}
		}
		return nil
	}
	for _, c := range calls {
		if t := find(c.Name); t != nil && !t.Parallel {
			parallel = false
		}
	}
	run := func(n int) {
		c := calls[n]
		l.emit(Event{Type: "tool_start", ToolID: c.ID, ToolName: c.Name})
		var text string
		var err error
		switch t := find(c.Name); {
		case truncated:
			err = errors.New("response was truncated; reissue this tool call with complete arguments")
		case ctx.Err() != nil:
			err = ctx.Err()
		case l.Concluding:
			err = errors.New("all tools are disabled during conclusion; use the supplied snapshot and session evidence")
		case l.Repairing:
			err = errors.New("all tools are disabled during result-format repair; use existing session evidence")
		case t == nil:
			err = fmt.Errorf("unknown tool %q", c.Name)
		default:
			if err = ValidateArguments(t.Schema, c.Input); err == nil {
				text, err = invoke(ctx, t, c.Input)
			}
		}
		e := Event{Type: "tool_end", ToolID: c.ID, ToolName: c.Name}
		if err != nil {
			e.Error = err.Error()
			if text != "" {
				text += "\n"
			}
			text += err.Error()
		}
		content, _ := json.Marshal(text)
		out[n] = Block{Type: "tool_result", ToolUseID: c.ID, Content: content, IsError: err != nil}
		l.emit(e)
	}
	if parallel {
		var wg sync.WaitGroup
		for n := range calls {
			wg.Add(1)
			go func(n int) { defer wg.Done(); run(n) }(n)
		}
		wg.Wait()
	} else {
		for n := range calls {
			run(n)
		}
	}
	return out
}

func invoke(ctx context.Context, t *Tool, raw json.RawMessage) (text string, err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("tool panicked: %v", v)
		}
	}()
	if t.Execute == nil {
		return "", errors.New("tool has no executor")
	}
	return t.Execute(ctx, raw)
}

func validateCalls(m Message) error {
	seen := map[string]bool{}
	for _, b := range m.Content {
		if b.Type != "tool_use" {
			continue
		}
		if b.ID == "" || b.Name == "" {
			return errors.New("tool call missing id or name")
		}
		if seen[b.ID] {
			return fmt.Errorf("duplicate tool call id %q", b.ID)
		}
		seen[b.ID] = true
	}
	return nil
}

// RepairHistory settles interrupted tool batches with errors. Replaying them
// could repeat a side effect that completed before the session was saved.
func (l *Loop) RepairHistory() error {
	var pending []Block
	for i, m := range l.History {
		if len(pending) > 0 {
			if m.Role != "user" || len(m.Content) < len(pending) {
				return errors.New("session has orphaned tool calls")
			}
			for n, c := range pending {
				if m.Content[n].Type != "tool_result" || m.Content[n].ToolUseID != c.ID {
					return errors.New("session tool result order or id mismatch")
				}
			}
			for _, b := range m.Content[len(pending):] {
				if b.Type == "tool_result" {
					return errors.New("session has extra tool results")
				}
			}
			pending = nil
		} else if hasResults(m) {
			return errors.New("session has orphaned tool results")
		}
		if m.Role == "assistant" {
			if err := validateCalls(m); err != nil {
				return err
			}
			for _, b := range m.Content {
				if b.Type == "tool_use" {
					pending = append(pending, b)
				}
			}
		}
		if m.Role != "user" && m.Role != "assistant" {
			return fmt.Errorf("invalid session role at message %d", i)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	results := make([]Block, len(pending))
	for n, c := range pending {
		results[n] = Block{Type: "tool_result", ToolUseID: c.ID, IsError: true, Content: json.RawMessage(`"Execution was interrupted; do not assume this action completed. Inspect existing evidence before deciding to retry."`)}
	}
	return l.append(Message{Role: "user", Content: results})
}
func hasResults(m Message) bool {
	for _, b := range m.Content {
		if b.Type == "tool_result" {
			return true
		}
	}
	return false
}
