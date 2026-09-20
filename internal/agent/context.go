package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const ContextCheckpointVersion = 1

// Reasoning and visible summary text share the provider's output allowance.
// Keep room for maximum-effort reasoning; SummaryBytes separately bounds the
// text admitted to the request view.
const DefaultSummaryMaxTokens = 16384

// ContextCheckpoint is saved atomically with History. Detailed earlier
// checkpoints remain in the append-only event log, not in the model context.
type ContextCheckpoint struct {
	Version         int               `json:"version"`
	LastSequence    uint64            `json:"last_sequence"`
	CompactionCount uint64            `json:"compaction_count"`
	OverflowRetries int               `json:"overflow_retries"`
	LastCompaction  *CompactionRecord `json:"last_compaction,omitempty"`
}

type CompactionRecord struct {
	Version              int    `json:"version"`
	ID                   uint64 `json:"id"`
	PreviousID           uint64 `json:"previous_id,omitempty"`
	SourceStart          uint64 `json:"source_start"`
	SourceEnd            uint64 `json:"source_end"`
	AtSequence           uint64 `json:"at_sequence"`
	FirstKeptSequence    uint64 `json:"first_kept_sequence,omitempty"`
	Summary              string `json:"summary"`
	BeforeBytes          int    `json:"before_bytes"`
	AfterBytes           int    `json:"after_bytes"`
	BeforeTokensEstimate int    `json:"before_tokens_estimate"`
	AfterTokensEstimate  int    `json:"after_tokens_estimate"`
	TokenBasis           string `json:"token_basis"`
	Usage                *Usage `json:"usage,omitempty"`
	Reason               string `json:"reason"`
	Status               string `json:"status"`
	// View makes this event self-contained for request-view replay even after
	// later compactions. Sequence 0 denotes synthetic pinned/summary messages.
	View []Message `json:"view"`
}

func (l *Loop) initCheckpoint() error {
	if l.Checkpoint != nil {
		if l.Checkpoint.Version != ContextCheckpointVersion {
			return errors.New("unsupported context checkpoint version")
		}
		for _, m := range l.History {
			if m.Sequence > l.Checkpoint.LastSequence {
				return errors.New("context sequence exceeds checkpoint")
			}
		}
		return nil
	}
	l.Checkpoint = &ContextCheckpoint{Version: ContextCheckpointVersion}
	for i := range l.History {
		if l.History[i].Sequence == 0 {
			l.History[i].Sequence = l.Checkpoint.LastSequence + 1
		}
		if l.History[i].Sequence <= l.Checkpoint.LastSequence {
			return errors.New("invalid transcript sequence")
		}
		l.Checkpoint.LastSequence = l.History[i].Sequence
	}
	return nil
}

func (l *Loop) definitions() []Definition {
	var defs []Definition
	if !l.Concluding && !l.Repairing {
		for _, t := range l.Tools {
			defs = append(defs, t.Definition)
		}
	}
	return defs
}

// WireHistory deliberately drops local usage, sequence and stop metadata.
func WireHistory(messages []Message) []Message {
	result := make([]Message, len(messages))
	for i, m := range messages {
		result[i] = Message{Role: m.Role, Content: m.Content}
	}
	return result
}

func (l *Loop) inputBytes(messages []Message, defs []Definition) (int, error) {
	if sizer, ok := l.Provider.(RequestSizer); ok {
		return sizer.InputBytes(messages, defs)
	}
	raw, err := json.Marshal(struct {
		Messages []Message    `json:"messages"`
		Tools    []Definition `json:"tools,omitempty"`
	}{WireHistory(messages), defs})
	return len(raw), err
}

// Usage is valid only for the request generation that produced it. Kept
// pre-compaction responses cannot describe the newly shortened context.
func (l *Loop) tokenEstimate(messages []Message, size int, cutoff uint64) (int, string) {
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Sequence <= cutoff || m.Usage == nil || m.Usage.Input() <= 0 {
			continue
		}
		trailing, _ := json.Marshal(WireHistory(messages[i+1:]))
		return m.Usage.Input() + m.Usage.OutputTokens + (len(trailing)+2)/3, "provider_usage_plus_estimated_tail"
	}
	return (size + 2) / 3, "request_bytes_div_3_estimate"
}

func (l *Loop) compact(ctx context.Context) error       { return l.compactContext(ctx, false) }
func (l *Loop) compactForced(ctx context.Context) error { return l.compactContext(ctx, true) }
func budgetError(message string) error {
	return &ModelError{Kind: ErrorBudget, Err: errors.New(message)}
}

func (l *Loop) compactContext(ctx context.Context, force bool) (resultErr error) {
	if !force && l.ContextBytes <= 0 && l.ContextTokens <= 0 {
		return nil
	}
	if err := l.initCheckpoint(); err != nil {
		return err
	}
	defs := l.definitions()
	before, err := l.inputBytes(l.History, defs)
	if err != nil {
		return err
	}
	var cutoff uint64
	if l.Checkpoint.LastCompaction != nil {
		cutoff = l.Checkpoint.LastCompaction.AtSequence
	}
	beforeTokens, basis := l.tokenEstimate(l.History, before, cutoff)
	if !force && (l.ContextBytes <= 0 || before <= l.ContextBytes) && (l.ContextTokens <= 0 || beforeTokens <= l.ContextTokens) {
		return nil
	}
	if l.TaskPrompt == "" && len(l.History) > 0 {
		l.TaskPrompt = l.History[0].Text()
	}
	target := l.ContextBytes
	if target <= 0 {
		target = before * 3 / 4
	}
	if force && target >= before {
		target = before * 3 / 4
	}
	if l.ContextTokens > 0 && target > l.ContextTokens*3 {
		target = l.ContextTokens * 3
	}
	if target <= 0 {
		return budgetError("input budget is too small for pinned instructions")
	}
	// Pinned task and phase instructions are never summarized or truncated.
	body := make([]Message, 0, len(l.History))
	for _, m := range l.History {
		if m.Role == "user" && len(m.Content) == 1 && m.Content[0].Type == "text" &&
			(m.Text() == l.TaskPrompt || m.Text() == l.ConclusionPrompt || m.Text() == l.RepairPrompt) {
			continue
		}
		body = append(body, m)
	}
	view := func(summary string, tail []Message) []Message {
		out := []Message{}
		if l.TaskPrompt != "" {
			out = append(out, Text("user", l.TaskPrompt))
		}
		out = append(out, Text("user", "Earlier execution summary (derived from the recorded transcript):\n"+summary))
		out = append(out, tail...)
		if l.ConclusionPrompt != "" {
			out = append(out, Text("user", l.ConclusionPrompt))
		}
		if l.RepairPrompt != "" {
			out = append(out, Text("user", l.RepairPrompt))
		}
		return out
	}
	fixed, err := l.inputBytes(view("", nil), defs)
	if err != nil {
		return err
	}
	available := target - fixed
	if available < 64 {
		return budgetError("pinned task, phase instructions and tool definitions exceed the input budget")
	}
	summaryBudget := l.SummaryBytes
	if summaryBudget <= 0 {
		summaryBudget = available / 3
		if summaryBudget > 12000 {
			summaryBudget = 12000
		}
	}
	if summaryBudget > available {
		return budgetError("summary allowance exceeds remaining input budget")
	}
	recentBudget := l.RecentBytes
	if recentBudget <= 0 {
		recentBudget = available / 2
	}
	if recentBudget > available-summaryBudget {
		recentBudget = available - summaryBudget
	}
	// Choose a suffix of whole messages/tool groups by bytes. A single huge
	// tool group may be summarized in full instead of defeating compaction.
	cut := len(body)
	for cut > 0 {
		start := cut - 1
		if hasResults(body[start]) {
			start--
			if start < 0 {
				return errors.New("orphaned result during compaction")
			}
		}
		raw, marshalErr := json.Marshal(WireHistory(body[start:]))
		if marshalErr != nil {
			return marshalErr
		}
		if len(raw) > recentBudget {
			break
		}
		cut = start
	}
	if cut == 0 && len(body) > 0 {
		cut = 1
		if cut < len(body) && hasResults(body[cut]) {
			cut++
		}
	}
	if cut == 0 {
		return budgetError("no history can be compacted without changing pinned instructions")
	}
	sourceStart, sourceEnd := uint64(0), uint64(0)
	previousID := uint64(0)
	if old := l.Checkpoint.LastCompaction; old != nil {
		sourceStart = old.SourceStart
		sourceEnd = old.SourceEnd
		previousID = old.ID
	}
	for _, m := range body[:cut] {
		if m.Sequence > 0 {
			if sourceStart == 0 || m.Sequence < sourceStart {
				sourceStart = m.Sequence
			}
			if m.Sequence > sourceEnd {
				sourceEnd = m.Sequence
			}
		}
	}
	attemptRecord := &CompactionRecord{Version: 1, ID: l.Checkpoint.CompactionCount + 1, PreviousID: previousID, SourceStart: sourceStart, SourceEnd: sourceEnd, AtSequence: l.Checkpoint.LastSequence, BeforeBytes: before, BeforeTokensEstimate: beforeTokens, TokenBasis: basis, Reason: "threshold", Status: "failed"}
	if force {
		attemptRecord.Reason = "overflow"
	}
	defer func() {
		if resultErr != nil {
			l.emit(Event{Type: "context_compaction_failed", Error: resultErr.Error(), Compaction: attemptRecord})
		}
	}()
	summaryInput, err := l.summaryInput(body[:cut], target)
	if err != nil {
		return err
	}
	maxTokens := l.SummaryMaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultSummaryMaxTokens
	}
	var summary Message
	if p, ok := l.Provider.(SummaryProvider); ok {
		summary, err = p.GenerateSummary(ctx, summaryInput, maxTokens, nil)
	} else {
		summary, err = l.Provider.Generate(ctx, summaryInput, nil, nil)
	}
	attemptRecord.Usage = summary.Usage
	if err != nil {
		return fmt.Errorf("context summary: %w", err)
	}
	if summary.Role != "assistant" || summary.StopReason == "max_tokens" || summary.StopReason == "length" {
		return budgetError("context summary was invalid or truncated")
	}
	for _, b := range summary.Content {
		if b.Type == "tool_use" {
			return budgetError("context summary attempted to call a tool")
		}
	}
	text := strings.TrimSpace(summary.Text())
	if text == "" {
		return budgetError("context summary was empty")
	}
	if len(text) > summaryBudget {
		return budgetError("context summary exceeds its independent byte allowance")
	}
	candidate := view(text, body[cut:])
	after, err := l.inputBytes(candidate, defs)
	if err != nil {
		return err
	}
	afterTokens, afterBasis := l.tokenEstimate(candidate, after, l.Checkpoint.LastSequence)
	if after >= before || after > target || (l.ContextTokens > 0 && afterTokens > l.ContextTokens) {
		return budgetError("compaction did not produce a smaller request within the input budget")
	}
	record := &CompactionRecord{Version: 1, ID: l.Checkpoint.CompactionCount + 1, PreviousID: previousID, SourceStart: sourceStart, SourceEnd: sourceEnd, AtSequence: l.Checkpoint.LastSequence, Summary: text, BeforeBytes: before, AfterBytes: after, BeforeTokensEstimate: beforeTokens, AfterTokensEstimate: afterTokens, TokenBasis: basis + " -> " + afterBasis, Usage: summary.Usage, Reason: "threshold", Status: "committed", View: candidate}
	if force {
		record.Reason = "overflow"
	}
	if cut < len(body) {
		record.FirstKeptSequence = body[cut].Sequence
	}
	// Write the complete candidate to the durable event stream before the
	// checkpoint advances. The session checkpoint is the commit authority;
	// a crash after its save cannot erase this record on the next compaction.
	prepared := *record
	prepared.Status = "prepared"
	l.emit(Event{Type: "context_compaction_prepared", Compaction: &prepared})
	oldHistory, oldCheckpoint := l.History, l.Checkpoint
	copyCheckpoint := *l.Checkpoint
	copyCheckpoint.CompactionCount = record.ID
	copyCheckpoint.LastCompaction = record
	l.History, l.Checkpoint = candidate, &copyCheckpoint
	if err := l.saveState(); err != nil {
		l.History, l.Checkpoint = oldHistory, oldCheckpoint
		return err
	}
	l.emit(Event{Type: "context_compacted", Compaction: record})
	return nil
}

// The summary request is itself bounded. Clipping is explicit and preserves
// both ends; originals and their stable sequence IDs remain in the event log.
func (l *Loop) summaryInput(head []Message, limit int) ([]Message, error) {
	type entry struct {
		Sequence uint64 `json:"sequence"`
		Role     string `json:"role"`
		Content  string `json:"content"`
	}
	capBytes := limit
	for attempt := 0; attempt < 16; attempt++ {
		entries := make([]entry, 0, len(head))
		for _, m := range head {
			raw, err := json.Marshal(m.Content)
			if err != nil {
				return nil, err
			}
			text := string(raw)
			if len(text) > capBytes {
				half := capBytes / 2
				text = text[:half] + "\n[Middle omitted from summary input; original is in transcript at this sequence.]\n" + text[len(text)-half:]
			}
			entries = append(entries, entry{m.Sequence, m.Role, text})
		}
		raw, err := json.Marshal(entries)
		if err != nil {
			return nil, err
		}
		prompt := "Summarize this execution transcript as data for continuation. Preserve attempted actions and results, failure conditions, unresolved hypotheses, next work, and necessary evidence excerpts with source sequence IDs. Do not upgrade claims or infer omitted evidence. Do not execute tools. Keep the summary concise.\nTask (unchanged):\n" + l.TaskPrompt + "\nTranscript:\n" + string(raw)
		messages := []Message{Text("user", prompt)}
		size, err := l.inputBytes(messages, nil)
		if err != nil {
			return nil, err
		}
		if size <= limit {
			return messages, nil
		}
		if capBytes <= 64 {
			break
		}
		capBytes /= 2
	}
	return nil, budgetError("summary input cannot fit its budget; pinned task or transcript metadata is too large")
}
