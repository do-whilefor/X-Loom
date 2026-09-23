package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

const ContextCheckpointVersion = 1

// Reasoning and visible summary text share the provider's output allowance.
// Keep room for maximum-effort reasoning; SummaryBytes separately bounds the
// text admitted to the request view.
const DefaultSummaryMaxTokens = 16384

// ContextCheckpoint is saved atomically with History. Detailed earlier
// checkpoints remain in the append-only event log, not in the model context.
type ContextCheckpoint struct {
	Version         int                   `json:"version"`
	LastSequence    uint64                `json:"last_sequence"`
	CompactionCount uint64                `json:"compaction_count"`
	OverflowRetries int                   `json:"overflow_retries"`
	LastCompaction  *CompactionCheckpoint `json:"last_compaction,omitempty"`
}

// CompactionCheckpoint retains the metadata needed to continue compaction.
// History already stores the current request view; older views remain in events.
// Legacy checkpoint JSON may contain a view, which decoding safely ignores.
type CompactionCheckpoint struct {
	Version           int    `json:"version"`
	ID                uint64 `json:"id"`
	PreviousID        uint64 `json:"previous_id,omitempty"`
	SourceStart       uint64 `json:"source_start"`
	SourceEnd         uint64 `json:"source_end"`
	AtSequence        uint64 `json:"at_sequence"`
	FirstKeptSequence uint64 `json:"first_kept_sequence,omitempty"`
	Summary           string `json:"summary"`
	// Quotes are copied by Go from successful tool results, never model text.
	Quotes               []SummaryQuote `json:"quotes,omitempty"`
	BeforeBytes          int            `json:"before_bytes"`
	AfterBytes           int            `json:"after_bytes"`
	BeforeTokensEstimate int            `json:"before_tokens_estimate"`
	AfterTokensEstimate  int            `json:"after_tokens_estimate"`
	TokenBasis           string         `json:"token_basis"`
	Usage                *Usage         `json:"usage,omitempty"`
	Reason               string         `json:"reason"`
	Status               string         `json:"status"`
}

type CompactionRecord struct {
	CompactionCheckpoint
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
	inputLimit := l.ContextBytes
	if inputLimit <= 0 {
		inputLimit = before * 3 / 4
	}
	if force && inputLimit >= before {
		inputLimit = before * 3 / 4
	}
	if l.ContextTokens > 0 && inputLimit > l.ContextTokens*3 {
		inputLimit = l.ContextTokens * 3
	}
	// The summary still reads under the original input allowance. A smaller
	// retained-context target must not also discard its source evidence.
	target := inputLimit
	targetTokens := l.ContextTokens
	if l.ContextTargetTokens > 0 {
		if targetTokens <= 0 || l.ContextTargetTokens < targetTokens {
			targetTokens = l.ContextTargetTokens
		}
		if target > targetTokens*3 {
			target = targetTokens * 3
		}
	}
	if target <= 0 {
		return budgetError("input budget is too small for pinned instructions")
	}
	// Runtime task data remains verbatim too. Exclude exact copies from the
	// summary and tail, and charge the retained data to the same hard budget.
	pinned := map[string]bool{l.TaskPrompt: true, l.ConclusionPrompt: true, l.RepairPrompt: true}
	var contextData []string
	for _, data := range l.ContextData {
		if data != "" && !pinned[data] {
			contextData = append(contextData, data)
			pinned[data] = true
		}
	}
	body := make([]Message, 0, len(l.History))
	for _, m := range l.History {
		if m.Role == "user" && len(m.Content) == 1 && m.Content[0].Type == "text" &&
			pinned[m.Text()] {
			continue
		}
		body = append(body, m)
	}
	view := func(summary string, tail []Message) []Message {
		out := []Message{}
		if l.TaskPrompt != "" {
			out = append(out, Text("user", l.TaskPrompt))
		}
		out = append(out, Text("user", summaryViewPrefix+summary))
		out = append(out, tail...)
		for _, data := range contextData {
			out = append(out, Text("user", data))
		}
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
		return budgetError("pinned task, runtime data, phase instructions and tool definitions exceed the input budget")
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
		if l.ContextTargetTokens > 0 {
			// Fill the target with complete recent groups instead of halving
			// it again. Reserve the summary's worst-case JSON escaping (six
			// wire bytes per source byte); short histories need no padding.
			recentBudget = max(0, available-summaryBudget*6)
		}
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
	attemptRecord := &CompactionRecord{CompactionCheckpoint: CompactionCheckpoint{Version: 1, ID: l.Checkpoint.CompactionCount + 1, PreviousID: previousID, SourceStart: sourceStart, SourceEnd: sourceEnd, AtSequence: l.Checkpoint.LastSequence, BeforeBytes: before, BeforeTokensEstimate: beforeTokens, TokenBasis: basis, Reason: "threshold", Status: "failed"}}
	if force {
		attemptRecord.Reason = "overflow"
	}
	defer func() {
		if resultErr != nil {
			l.emit(Event{Type: "context_compaction_failed", Error: resultErr.Error(), Compaction: attemptRecord})
		}
	}()
	summaryInput, sources, err := l.summaryRequest(body[:cut], inputLimit, summaryBudget)
	if err != nil {
		return err
	}
	maxTokens := l.SummaryMaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultSummaryMaxTokens
	}
	summary, err := l.generate(ctx, summaryInput, nil, maxTokens)
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
	text, quotes, err := resolveSummary(summary.Text(), sources, summaryBudget)
	if err != nil {
		return budgetError("context summary: " + err.Error())
	}
	candidate := view(text, body[cut:])
	after, err := l.inputBytes(candidate, defs)
	if err != nil {
		return err
	}
	afterTokens, afterBasis := l.tokenEstimate(candidate, after, l.Checkpoint.LastSequence)
	if after >= before || after > target || (targetTokens > 0 && afterTokens > targetTokens) {
		return budgetError("compaction did not produce a smaller request within the input budget")
	}
	record := &CompactionRecord{CompactionCheckpoint: CompactionCheckpoint{Version: 1, ID: l.Checkpoint.CompactionCount + 1, PreviousID: previousID, SourceStart: sourceStart, SourceEnd: sourceEnd, AtSequence: l.Checkpoint.LastSequence, Summary: text, BeforeBytes: before, AfterBytes: after, BeforeTokensEstimate: beforeTokens, AfterTokensEstimate: afterTokens, TokenBasis: basis + " -> " + afterBasis, Usage: summary.Usage, Reason: "threshold", Status: "committed"}, View: candidate}
	record.Quotes = quotes
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
	metadata := record.CompactionCheckpoint
	copyCheckpoint.LastCompaction = &metadata
	l.History, l.Checkpoint = candidate, &copyCheckpoint
	if err := l.saveState(); err != nil {
		l.History, l.Checkpoint = oldHistory, oldCheckpoint
		return err
	}
	l.emit(Event{Type: "context_compacted", Compaction: record})
	return nil
}
