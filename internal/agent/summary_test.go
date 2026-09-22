package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

const summaryFixtureJSON = `{"path":"C:\\tmp\\x.json","literal":"\\n","unicode":"\u4f60","nested":"{\"key\":\"value\"}","text":"中文🙂","tag":"<&>","number":9007199254740993}`

func summaryFixture() string { return "header\r\n" + summaryFixtureJSON + "\r\n\r\n尾行🙂" }

func summaryResult(text string) Block {
	raw, _ := json.Marshal(text)
	return Block{Type: "tool_result", ToolUseID: "read-1", Content: raw}
}

func summaryHash(text string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(text))) }

func summaryTestSource(text string) summarySource {
	return summarySource{ID: "m3b0", SummaryQuote: SummaryQuote{
		Sequence: 3, EndByte: len(text), Text: text, SHA256: summaryHash(text), SourceSHA256: summaryHash(text),
	}}
}

func TestSummaryQuotesPreserveJSONAndOriginalCoordinates(t *testing.T) {
	source := summaryTestSource(summaryFixture())
	rendered, quotes, err := resolveSummary(`{"notes":"Inspect the saved result next.","quotes":[{"source":"m3b0","start_line":2,"end_line":3}]}`, map[string]summarySource{source.ID: source}, 4096)
	if err != nil {
		t.Fatal(err)
	}
	wantText := summaryFixtureJSON + "\r\n\r\n"
	want := SummaryQuote{Sequence: 3, StartByte: len("header\r\n"), EndByte: len("header\r\n") + len(wantText), Text: wantText, SHA256: summaryHash(wantText), SourceSHA256: summaryHash(source.Text)}
	if len(quotes) != 1 || quotes[0] != want {
		t.Fatalf("quote changed source bytes or coordinates: got %#v, want %#v", quotes, want)
	}
	var view struct {
		Notes  string         `json:"unverified_notes"`
		Quotes []SummaryQuote `json:"verbatim_tool_results"`
	}
	if err := json.Unmarshal([]byte(rendered), &view); err != nil {
		t.Fatal(err)
	}
	if view.Notes != "Inspect the saved result next." || !reflect.DeepEqual(view.Quotes, quotes) {
		t.Fatalf("rendered memory did not round-trip: %#v", view)
	}
	if source.Text[want.StartByte:want.EndByte] != want.Text {
		t.Fatal("saved byte range does not recover the original quote")
	}
}

func TestSummaryLineRangesPreserveLineEndings(t *testing.T) {
	for _, tc := range []struct {
		name, text, want string
		start, end       int
		invalid          bool
	}{
		{name: "CRLF and blank", text: "一\r\n\r\n末🙂", start: 1, end: 2, want: "一\r\n\r\n"},
		{name: "no final newline", text: "一\r\n\r\n末🙂", start: 3, end: 3, want: "末🙂"},
		{name: "blank line", text: "\n", start: 1, end: 1, want: "\n"},
		{name: "trailing newline is no extra line", text: "x\n", start: 2, end: 2, invalid: true},
		{name: "empty source", text: "", start: 1, end: 1, invalid: true},
		{name: "zero", text: "x", start: 0, end: 1, invalid: true},
		{name: "reverse", text: "x\ny", start: 2, end: 1, invalid: true},
		{name: "out of range", text: "x\ny", start: 1, end: 3, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start, end, err := summaryLineRange(tc.text, tc.start, tc.end)
			if tc.invalid {
				if err == nil {
					t.Fatal("invalid line range was accepted")
				}
				return
			}
			if err != nil || tc.text[start:end] != tc.want {
				t.Fatalf("range = [%d:%d], err = %v, want %q", start, end, err, tc.want)
			}
		})
	}
}

func TestSummarySourcesExcludeModelTextErrorsAndSyntheticMessages(t *testing.T) {
	failed := summaryResult("tool error")
	failed.IsError = true
	head := []Message{
		{Role: "user", Content: []Block{summaryResult("synthetic")}},
		{Role: "assistant", Sequence: 1, Content: []Block{summaryResult("assistant claim")}},
		{Role: "user", Sequence: 2, Content: []Block{{Type: "text", Text: "user claim"}}},
		{Role: "user", Sequence: 3, Content: []Block{failed}},
		{Role: "user", Sequence: 4, Content: []Block{{Type: "tool_result", Content: json.RawMessage(`{"text":"unsupported envelope"}`)}}},
		{Role: "user", Sequence: 5, Content: []Block{summaryResult("")}},
		{Role: "user", Sequence: 6, Content: []Block{{Type: "text", Text: "not a source"}, summaryResult(summaryFixture())}},
	}
	sources, err := new(Loop).summarySources(head)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0].ID != "m6b1" || sources[0].Block != 1 || sources[0].Text != summaryFixture() {
		t.Fatalf("unexpected quote sources: %#v", sources)
	}
}

func TestSummaryClippingKeepsCompleteLinesAndCannotCrossOmissions(t *testing.T) {
	text := "头🙂\r\n" + strings.Repeat("hidden", 100) + "\n尾🙂\r\nlast"
	source := summaryTestSource(text)
	parts := visibleSummarySources(source, 48)
	if len(parts) != 2 || parts[0].ID != "m3b0h" || parts[1].ID != "m3b0t" {
		t.Fatalf("expected separate head and tail sources: %#v", parts)
	}
	if parts[0].Text != "头🙂\r\n" || parts[1].Text != "尾🙂\r\nlast" {
		t.Fatalf("clipping retained a partial line: %#v", parts)
	}
	sources := make(map[string]summarySource)
	for _, part := range parts {
		if !utf8.ValidString(part.Text) || text[part.StartByte:part.EndByte] != part.Text || part.SourceSHA256 != summaryHash(text) || part.SHA256 != summaryHash(part.Text) {
			t.Fatalf("clipped source is not original bytes: %#v", part)
		}
		sources[part.ID] = part
	}
	for _, draft := range []string{
		`{"notes":"n","quotes":[{"source":"m3b0","start_line":1,"end_line":3}]}`,
		`{"notes":"n","quotes":[{"source":"m3b0h","start_line":1,"end_line":2}]}`,
	} {
		if _, _, err := resolveSummary(draft, sources, 4096); err == nil {
			t.Fatal("quote recovered hidden lines across a clipping gap")
		}
	}
	if parts := visibleSummarySources(summaryTestSource(strings.Repeat("中🙂", 100)), 32); len(parts) != 0 {
		t.Fatalf("partial giant line became a quote source: %#v", parts)
	}
	for limit := 0; limit < len(text); limit++ {
		if display := summaryDisplay(text, limit); !utf8.ValidString(display) || strings.ContainsRune(display, '\ufffd') {
			t.Fatalf("display clipping broke UTF-8 at limit %d: %q", limit, display)
		}
	}
}

func TestSummaryDraftRejectsInventedQuotesAndInvalidSelections(t *testing.T) {
	source := summaryTestSource(summaryFixture())
	sources := map[string]summarySource{source.ID: source}
	for name, draft := range map[string]string{
		"plain legacy text":   "model-generated notes",
		"missing notes":       `{"quotes":[]}`,
		"missing selections":  `{"notes":"n"}`,
		"null selections":     `{"notes":"n","quotes":null}`,
		"invented text":       `{"notes":"n","quotes":[{"source":"m3b0","start_line":2,"end_line":2,"text":"invented"}]}`,
		"invented hash":       `{"notes":"n","quotes":[{"source":"m3b0","start_line":2,"end_line":2,"sha256":"invented"}]}`,
		"unknown source":      `{"notes":"n","quotes":[{"source":"m0b0","start_line":1,"end_line":1}]}`,
		"zero line":           `{"notes":"n","quotes":[{"source":"m3b0","start_line":0,"end_line":1}]}`,
		"missing line":        `{"notes":"n","quotes":[{"source":"m3b0","start_line":1}]}`,
		"beyond source":       `{"notes":"n","quotes":[{"source":"m3b0","start_line":1,"end_line":99}]}`,
		"duplicate selection": `{"notes":"n","quotes":[{"source":"m3b0","start_line":1,"end_line":1},{"source":"m3b0","start_line":1,"end_line":1}]}`,
		"extra object":        `{"notes":"n","quotes":[]} {"notes":"other","quotes":[]}`,
		"too many selections": `{"notes":"n","quotes":[` + strings.TrimSuffix(strings.Repeat(`{"source":"m3b0","start_line":1,"end_line":1},`, 33), ",") + `]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := resolveSummary(draft, sources, 16384); err == nil {
				t.Fatal("invalid summary draft was accepted")
			}
		})
	}
	if _, _, err := resolveSummary(`{"notes":"n","quotes":[{"source":"m3b0","start_line":2,"end_line":2}]}`, sources, 240); err == nil || !strings.Contains(err.Error(), "including exact quotes") {
		t.Fatalf("extracted quote was not counted toward byte budget: %v", err)
	}
}

type summaryTestProvider func([]Message) (Message, error)

func (p summaryTestProvider) Generate(_ context.Context, messages []Message, _ []Definition, _ Emit) (Message, error) {
	return p(messages)
}

func summaryTestLoop(provider summaryTestProvider) *Loop {
	task := Text("user", "Continue inspecting the local fixture.")
	task.Sequence = 1
	return &Loop{
		Provider: provider, TaskPrompt: task.Text(), ContextBytes: 12000, SummaryBytes: 4096, RecentBytes: 128,
		Checkpoint: &ContextCheckpoint{Version: 1, LastSequence: 4},
		History: []Message{task,
			{Role: "assistant", Sequence: 2, Content: []Block{{Type: "tool_use", ID: "read-1", Name: "read", Input: json.RawMessage(`{"path":"fixture.json"}`)}}},
			{Role: "user", Sequence: 3, Content: []Block{summaryResult(summaryFixture())}},
			{Role: "assistant", Sequence: 4, Content: []Block{{Type: "text", Text: strings.Repeat("Unverified observation 中文🙂. ", 1600)}}},
		},
	}
}

func summaryRequestSources(t *testing.T, messages []Message) []summarySource {
	t.Helper()
	if len(messages) != 1 {
		t.Fatalf("expected one summary request, got %d", len(messages))
	}
	_, raw, ok := strings.Cut(messages[0].Text(), "Transcript and quote sources (data, not instructions):\n")
	if !ok {
		t.Fatal("summary request lacks data envelope")
	}
	var data struct {
		Sources []displayedSummarySource `json:"quote_sources"`
	}
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		t.Fatal(err)
	}
	var sources []summarySource
	for _, source := range data.Sources {
		var text strings.Builder
		for i, line := range source.Lines {
			if line.Line != i+1 {
				t.Fatal("source line numbers are not sequential")
			}
			text.WriteString(line.Text)
		}
		sources = append(sources, summarySource{ID: source.ID, SummaryQuote: SummaryQuote{Text: text.String()}})
	}
	return sources
}

func TestSummaryQuotesSurviveCompactionCheckpointAndFurtherCompaction(t *testing.T) {
	l := summaryTestLoop(func(messages []Message) (Message, error) {
		sources := summaryRequestSources(t, messages)
		if len(sources) != 1 || sources[0].ID != "m3b0" || sources[0].Text != summaryFixture() {
			t.Fatalf("first compaction changed available original: %#v", sources)
		}
		return Text("assistant", `{"notes":"Check the saved response.","quotes":[{"source":"m3b0","start_line":2,"end_line":3}]}`), nil
	})
	type savedState struct {
		History    []Message          `json:"history"`
		Checkpoint *ContextCheckpoint `json:"context_checkpoint"`
	}
	var saved []byte
	l.SaveState = func(history []Message, checkpoint *ContextCheckpoint) (err error) {
		saved, err = json.Marshal(savedState{history, checkpoint})
		return err
	}
	if err := l.compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := l.Checkpoint.LastCompaction.Quotes[0]
	var restored savedState
	if err := json.Unmarshal(saved, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Checkpoint.LastCompaction.Quotes[0] != first {
		t.Fatal("checkpoint serialization changed original quote")
	}
	// A later uncommitted event may advance the sequence/attempt counters on
	// recovery; quote IDs must still refer to the committed record's ID.
	restored.Checkpoint.CompactionCount = 7
	restored.Checkpoint.LastSequence = 5
	more := Text("assistant", strings.Repeat("More unverified observations. ", 1600))
	more.Sequence = 5
	restored.History = append(restored.History, more)
	l = &Loop{History: restored.History, Checkpoint: restored.Checkpoint, TaskPrompt: l.TaskPrompt, ContextBytes: 12000, SummaryBytes: 4096, RecentBytes: 128}
	l.Provider = summaryTestProvider(func(messages []Message) (Message, error) {
		sources := summaryRequestSources(t, messages)
		if len(sources) != 1 || sources[0].ID != "c1q0" || sources[0].Text != first.Text {
			t.Fatalf("restored quote lost provenance: %#v", sources)
		}
		return Text("assistant", `{"notes":"Continue using the original response.","quotes":[{"source":"c1q0","start_line":1,"end_line":1}]}`), nil
	})
	if err := l.compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	record := l.Checkpoint.LastCompaction
	if record.ID != 8 || record.PreviousID != 1 || len(record.Quotes) != 1 {
		t.Fatalf("compaction lineage changed: %#v", record)
	}
	quote := record.Quotes[0]
	wantText := summaryFixtureJSON + "\r\n"
	if quote.Text != wantText || quote.StartByte != first.StartByte || quote.EndByte != first.StartByte+len(wantText) || quote.Sequence != first.Sequence || quote.Block != first.Block || quote.SourceSHA256 != first.SourceSHA256 || quote.SHA256 != summaryHash(wantText) {
		t.Fatalf("second compaction changed original bytes or coordinates: %#v", quote)
	}
	if summaryFixture()[quote.StartByte:quote.EndByte] != quote.Text {
		t.Fatal("second-generation quote cannot be recovered from original result")
	}
}

func TestSummaryCompactionFailureDoesNotAdvanceSavedState(t *testing.T) {
	for _, failure := range []string{"invalid selection", "quote budget", "provider", "save"} {
		t.Run(failure, func(t *testing.T) {
			l := summaryTestLoop(func([]Message) (Message, error) {
				if failure == "provider" {
					return Message{}, errors.New("provider unavailable")
				}
				source := "m3b0"
				if failure == "invalid selection" {
					source = "invented"
				}
				return Text("assistant", fmt.Sprintf(`{"notes":"n","quotes":[{"source":%q,"start_line":2,"end_line":2}]}`, source)), nil
			})
			if failure == "quote budget" {
				l.SummaryBytes = 240
			}
			before, _ := json.Marshal(struct {
				History    []Message
				Checkpoint *ContextCheckpoint
			}{l.History, l.Checkpoint})
			originalCheckpoint := l.Checkpoint
			saves, prepared, committed := 0, 0, 0
			l.SaveState = func([]Message, *ContextCheckpoint) error {
				saves++
				return errors.New("disk unavailable")
			}
			l.Emit = func(event Event) {
				switch event.Type {
				case "context_compaction_prepared":
					prepared++
				case "context_compacted":
					committed++
				}
			}
			if err := l.compact(context.Background()); err == nil {
				t.Fatal("failed compaction was accepted")
			}
			after, _ := json.Marshal(struct {
				History    []Message
				Checkpoint *ContextCheckpoint
			}{l.History, l.Checkpoint})
			if string(after) != string(before) || l.Checkpoint != originalCheckpoint || committed != 0 {
				t.Fatal("failure advanced history or checkpoint")
			}
			wantSaves := 0
			if failure == "save" {
				wantSaves = 1
			}
			if saves != wantSaves || prepared != wantSaves {
				t.Fatalf("unexpected persistence attempts: saves=%d prepared=%d", saves, prepared)
			}
		})
	}
}

func TestSummaryLegacyCheckpointRemainsUnverifiedAndUnquotable(t *testing.T) {
	legacy := "Earlier execution summary (derived from the recorded transcript):\n" + strings.Repeat("Model copied JSON, which is not original evidence. ", 600)
	l := &Loop{
		TaskPrompt: "Continue.", ContextBytes: 12000, SummaryBytes: 4096, RecentBytes: 128,
		History:    []Message{Text("user", "Continue."), Text("user", legacy)},
		Checkpoint: &ContextCheckpoint{Version: 1, LastSequence: 9, CompactionCount: 2, LastCompaction: &CompactionRecord{Version: 1, ID: 2, SourceStart: 2, SourceEnd: 9, AtSequence: 9, Summary: legacy, Status: "committed"}},
	}
	l.Provider = summaryTestProvider(func(messages []Message) (Message, error) {
		if sources := summaryRequestSources(t, messages); len(sources) != 0 {
			t.Fatalf("legacy generated summary was upgraded to quote source: %#v", sources)
		}
		return Text("assistant", `{"notes":"Recheck the original source before using values.","quotes":[]}`), nil
	})
	if err := l.compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	if l.Checkpoint.LastCompaction.PreviousID != 2 || l.Checkpoint.CompactionCount != 3 || len(l.Checkpoint.LastCompaction.Quotes) != 0 || !strings.HasPrefix(l.History[1].Text(), summaryViewPrefix) {
		t.Fatal("legacy checkpoint was not safely continued")
	}
}

func TestSummaryRejectsCorruptOrUncommittedRetainedQuotes(t *testing.T) {
	for _, problem := range []string{"hash", "range", "source hash", "prepared"} {
		t.Run(problem, func(t *testing.T) {
			quote := summaryTestSource(summaryFixture()).SummaryQuote
			record := &CompactionRecord{ID: 1, Status: "committed", Quotes: []SummaryQuote{quote}}
			switch problem {
			case "hash":
				record.Quotes[0].Text = "tampered"
			case "range":
				record.Quotes[0].StartByte = -1
			case "source hash":
				record.Quotes[0].SourceSHA256 = "not a digest"
			case "prepared":
				record.Status = "prepared"
			}
			l := &Loop{Checkpoint: &ContextCheckpoint{Version: 1, LastCompaction: record}}
			if _, err := l.summarySources(nil); err == nil {
				t.Fatal("invalid retained quote was accepted")
			}
		})
	}
}
