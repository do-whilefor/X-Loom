//go:build linux

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

type liveHTTPUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreation            struct {
		Ephemeral5mInputTokens int64 `json:"ephemeral_5m_input_tokens"`
		Ephemeral1hInputTokens int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

// Only timings, counts and explicitly selected protocol metadata are retained.
// Request/response bodies, URLs, headers and thinking text never enter a record.
type liveHTTPObservation struct {
	RequestID           int           `json:"request_id"`
	RunID               string        `json:"run_id"`
	Model               string        `json:"model"`
	MaxTokens           int           `json:"max_tokens"`
	ToolCount           int           `json:"tool_count"`
	StartedAt           time.Time     `json:"started_at"`
	FinishedAt          time.Time     `json:"finished_at"`
	HTTPStatus          int           `json:"http_status"`
	HeadersMS           *float64      `json:"headers_ms"`
	FirstEventMS        *float64      `json:"first_event_ms"`
	FirstThinkingMS     *float64      `json:"first_thinking_ms"`
	LastThinkingMS      *float64      `json:"last_thinking_ms"`
	FirstOutputMS       *float64      `json:"first_output_ms"`
	LastOutputMS        *float64      `json:"last_output_ms"`
	TerminalEvent       string        `json:"terminal_event,omitempty"`
	TerminalEventMS     *float64      `json:"terminal_event_ms,omitempty"`
	ClosedAfterTerminal bool          `json:"closed_after_terminal,omitempty"`
	ThinkingChars       int64         `json:"thinking_chars"`
	OutputChars         int64         `json:"output_chars"`
	Usage               liveHTTPUsage `json:"usage"`
	StopReason          string        `json:"stop_reason,omitempty"`
	Errors              []string      `json:"errors,omitempty"`
}

type liveProxyRecorder struct {
	mu           sync.Mutex
	observations []liveHTTPObservation
}

func (r *liveProxyRecorder) Snapshot() []liveHTTPObservation {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]liveHTTPObservation(nil), r.observations...)
	for i := range out {
		out[i].Errors = append([]string(nil), out[i].Errors...)
		for _, timing := range []**float64{&out[i].HeadersMS, &out[i].FirstEventMS, &out[i].FirstThinkingMS, &out[i].LastThinkingMS, &out[i].FirstOutputMS, &out[i].LastOutputMS, &out[i].TerminalEventMS} {
			if *timing != nil {
				value := **timing
				*timing = &value
			}
		}
	}
	return out
}

type liveProxyAttempt struct {
	recorder *liveProxyRecorder
	index    int
}

func (a *liveProxyAttempt) update(fn func(*liveHTTPObservation)) {
	a.recorder.mu.Lock()
	defer a.recorder.mu.Unlock()
	fn(&a.recorder.observations[a.index])
}

func (a *liveProxyAttempt) failure(kind string) {
	a.update(func(o *liveHTTPObservation) {
		for _, old := range o.Errors {
			if old == kind {
				return
			}
		}
		o.Errors = append(o.Errors, kind)
	})
}

// The Anthropic consumer returns at message_stop and closes its HTTP body
// without waiting for the upstream connection to reach EOF. Such a close is
// expected after an observed terminal event, not a failed model response.
// This records upstream completion only; it does not assert client delivery.
func (a *liveProxyAttempt) transportFailure(kind string) {
	a.update(func(o *liveHTTPObservation) {
		if o.TerminalEvent != "" {
			o.ClosedAfterTerminal = true
			return
		}
		for _, old := range o.Errors {
			if old == kind {
				return
			}
		}
		o.Errors = append(o.Errors, kind)
	})
}

type liveProxyContextKey struct{}

func newLiveModelProxy(upstream, token string) (*httptest.Server, *liveProxyRecorder, error) {
	target, err := url.Parse(upstream)
	if err != nil || target == nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" || target.User != nil {
		return nil, nil, errors.New("invalid live model upstream")
	}
	recorder := &liveProxyRecorder{}
	proxy := httputil.NewSingleHostReverseProxy(target)
	director := proxy.Director
	proxy.Director = func(r *http.Request) {
		director(r)
		r.Host = target.Host
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-Api-Key", token)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	proxy.Transport = transport
	proxy.FlushInterval = -1
	proxy.ErrorLog = log.New(io.Discard, "", 0)
	proxy.ModifyResponse = func(response *http.Response) error {
		attempt := response.Request.Context().Value(liveProxyContextKey{}).(*liveProxyAttempt)
		attempt.update(func(o *liveHTTPObservation) {
			o.HTTPStatus = response.StatusCode
			elapsed := float64(time.Since(o.StartedAt)) / float64(time.Millisecond)
			o.HeadersMS = &elapsed
		})
		if response.StatusCode >= 400 {
			attempt.failure("upstream_http_error")
		}
		response.Body = &liveObservedBody{ReadCloser: response.Body, attempt: attempt, sse: strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream")}
		return nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, _ error) {
		attempt := r.Context().Value(liveProxyContextKey{}).(*liveProxyAttempt)
		kind := "upstream_request_failed"
		if r.Context().Err() != nil {
			kind = "request_cancelled"
		}
		attempt.failure(kind)
		attempt.update(func(o *liveHTTPObservation) { o.HTTPStatus = http.StatusBadGateway })
		http.Error(w, "model upstream unavailable", http.StatusBadGateway)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder.mu.Lock()
		index := len(recorder.observations)
		recorder.observations = append(recorder.observations, liveHTTPObservation{RequestID: index + 1, RunID: r.Header.Get("X-Opencode-Session"), StartedAt: time.Now()})
		recorder.mu.Unlock()
		attempt := &liveProxyAttempt{recorder: recorder, index: index}
		defer func() {
			if r.Context().Err() != nil {
				attempt.transportFailure("request_cancelled")
			}
			attempt.update(func(o *liveHTTPObservation) { o.FinishedAt = time.Now() })
		}()
		// Restore every request byte, including bytes beyond the metadata limit.
		// No client or upstream deadline is introduced by the recorder.
		if r.Body != nil {
			body := r.Body
			raw, readErr := io.ReadAll(io.LimitReader(body, (32<<20)+1))
			if readErr != nil {
				attempt.failure("request_body_read_failed")
				attempt.update(func(o *liveHTTPObservation) { o.HTTPStatus = http.StatusBadRequest })
				http.Error(w, "invalid model request", http.StatusBadRequest)
				return
			}
			r.Body = struct {
				io.Reader
				io.Closer
			}{io.MultiReader(bytes.NewReader(raw), body), body}
			if len(raw) <= 32<<20 {
				var metadata struct {
					Model     string            `json:"model"`
					MaxTokens int               `json:"max_tokens"`
					Tools     []json.RawMessage `json:"tools"`
				}
				if json.Unmarshal(raw, &metadata) == nil {
					attempt.update(func(o *liveHTTPObservation) {
						o.Model, o.MaxTokens, o.ToolCount = metadata.Model, metadata.MaxTokens, len(metadata.Tools)
					})
				}
			}
		}
		proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), liveProxyContextKey{}, attempt)))
	}))
	server.Config.RegisterOnShutdown(transport.CloseIdleConnections)
	return server, recorder, nil
}

type liveObservedBody struct {
	io.ReadCloser
	attempt  *liveProxyAttempt
	sse      bool
	buffer   []byte
	dropping bool
	ended    bool
}

func (b *liveObservedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		if b.sse {
			b.observeSSE(p[:n])
		} else if !b.dropping {
			if len(b.buffer)+n > 32<<20 {
				b.buffer, b.dropping = nil, true
				b.attempt.failure("json_observation_limit")
			} else {
				b.buffer = append(b.buffer, p[:n]...)
			}
		}
	}
	if err != nil && !b.ended {
		b.ended = true
		if err == io.EOF {
			if b.sse && !b.dropping && len(b.buffer) > 0 {
				b.observeLine(b.buffer)
			} else if !b.sse && !b.dropping {
				b.observeJSON(b.buffer)
			}
		} else {
			b.attempt.transportFailure("upstream_body_read_failed")
		}
		b.buffer = nil
	}
	return n, err
}

func (b *liveObservedBody) observeSSE(raw []byte) {
	for len(raw) > 0 {
		end := bytes.IndexByte(raw, '\n')
		part := raw
		if end >= 0 {
			part = raw[:end]
		}
		if !b.dropping {
			if len(b.buffer)+len(part) > 1<<20 {
				b.buffer, b.dropping = nil, true
				b.attempt.failure("sse_line_observation_limit")
			} else {
				b.buffer = append(b.buffer, part...)
			}
		}
		if end < 0 {
			return
		}
		if !b.dropping {
			b.observeLine(b.buffer)
		}
		b.buffer, b.dropping = b.buffer[:0], false
		raw = raw[end+1:]
	}
}

func (b *liveObservedBody) observeLine(line []byte) {
	if data, ok := bytes.CutPrefix(bytes.TrimSuffix(line, []byte{'\r'}), []byte("data:")); ok {
		b.observeJSON(bytes.TrimSpace(data))
	}
}

func (b *liveObservedBody) observeJSON(raw []byte) {
	if len(raw) == 0 || bytes.Equal(raw, []byte("[DONE]")) {
		return
	}
	type block struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	}
	var event struct {
		Type         string                     `json:"type"`
		Usage        map[string]json.RawMessage `json:"usage"`
		StopReason   string                     `json:"stop_reason"`
		ContentBlock block                      `json:"content_block"`
		Delta        block                      `json:"delta"`
		Content      []block                    `json:"content"`
		Message      struct {
			Usage map[string]json.RawMessage `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &event) != nil {
		b.attempt.failure("response_event_parse_failed")
		return
	}
	b.attempt.update(func(o *liveHTTPObservation) {
		elapsed := float64(time.Since(o.StartedAt)) / float64(time.Millisecond)
		if o.FirstEventMS == nil {
			o.FirstEventMS = &elapsed
		}
		if event.Type == "message_stop" || (!b.sse && event.Type == "message" && event.StopReason != "") {
			if o.TerminalEvent == "" {
				o.TerminalEvent, o.TerminalEventMS = event.Type, &elapsed
			}
		}
		mergeLiveUsage(&o.Usage, event.Message.Usage)
		mergeLiveUsage(&o.Usage, event.Usage)
		if event.StopReason != "" {
			o.StopReason = event.StopReason
		}
		if event.Delta.StopReason != "" {
			o.StopReason = event.Delta.StopReason
		}
		if event.Type == "error" {
			o.Errors = append(o.Errors, "upstream_stream_error")
		}
		observe := func(value block) {
			thinking := value.Type == "thinking" || value.Type == "thinking_delta"
			output := value.Type == "text" || value.Type == "text_delta" || value.Type == "tool_use" || value.Type == "input_json_delta"
			if thinking {
				if o.FirstThinkingMS == nil {
					o.FirstThinkingMS = &elapsed
				}
				o.LastThinkingMS = &elapsed
				o.ThinkingChars += int64(utf8.RuneCountInString(value.Thinking))
			}
			if output {
				if o.FirstOutputMS == nil {
					o.FirstOutputMS = &elapsed
				}
				o.LastOutputMS = &elapsed
				o.OutputChars += int64(utf8.RuneCountInString(value.Text) + utf8.RuneCountInString(value.PartialJSON))
			}
		}
		observe(event.ContentBlock)
		observe(event.Delta)
		for _, value := range event.Content {
			observe(value)
		}
	})
}

func mergeLiveUsage(usage *liveHTTPUsage, values map[string]json.RawMessage) {
	// Anthropic delta usage is cumulative, not an increment. Missing fields
	// retain message_start values, including cache creation and cache reads.
	for key, destination := range map[string]*int64{"input_tokens": &usage.InputTokens, "output_tokens": &usage.OutputTokens, "cache_creation_input_tokens": &usage.CacheCreationInputTokens, "cache_read_input_tokens": &usage.CacheReadInputTokens} {
		var value *int64
		if raw, ok := values[key]; ok && json.Unmarshal(raw, &value) == nil && value != nil && *value >= 0 {
			*destination = *value
		}
	}
	var cache map[string]json.RawMessage
	if json.Unmarshal(values["cache_creation"], &cache) == nil {
		for key, destination := range map[string]*int64{"ephemeral_5m_input_tokens": &usage.CacheCreation.Ephemeral5mInputTokens, "ephemeral_1h_input_tokens": &usage.CacheCreation.Ephemeral1hInputTokens} {
			var value *int64
			if raw, ok := cache[key]; ok && json.Unmarshal(raw, &value) == nil && value != nil && *value >= 0 {
				*destination = *value
			}
		}
	}
}

func liveFinishedObservations(t *testing.T, recorder *liveProxyRecorder) []liveHTTPObservation {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		items := recorder.Snapshot()
		if len(items) > 0 && !items[len(items)-1].FinishedAt.IsZero() {
			return items
		}
		if time.Now().After(deadline) {
			t.Fatal("proxy did not archive the completed HTTP attempt")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestLiveModelProxyPreservesStreamAndRecordsOnlyMetrics(t *testing.T) {
	const secret = "upstream-secret-not-for-recording"
	const thinking = "private-thinking-证据"
	const requestBody = `{"model":"fixture-model","max_tokens":123,"tools":[{"name":"fixture"}],"messages":[{"role":"user","content":"private-request-text"}]}`
	stream := "event: message_start\r\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":1,\"cache_creation_input_tokens\":20,\"cache_read_input_tokens\":30,\"cache_creation\":{\"ephemeral_5m_input_tokens\":12,\"ephemeral_1h_input_tokens\":8}}}}\r\n\r\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"" + thinking + "\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"答复\"}}\n\n" +
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":4}}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":7,\"input_tokens\":null,\"cache_read_input_tokens\":null}}\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != requestBody || r.Header.Get("Authorization") != "Bearer "+secret || r.Header.Get("X-Api-Key") != secret || r.URL.Path != "/base/v1/messages" {
			t.Error("proxy changed request bytes or failed to replace dummy authorization")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for start := 0; start < len(stream); start += 7 {
			_, _ = io.WriteString(w, stream[start:min(start+7, len(stream))])
			w.(http.Flusher).Flush()
		}
	}))
	defer upstream.Close()
	proxy, recorder, err := newLiveModelProxy(upstream.URL+"/base?credential="+secret, secret)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	request, _ := http.NewRequest(http.MethodPost, proxy.URL+"/v1/messages", strings.NewReader(requestBody))
	request.Header.Set("Authorization", "Bearer dummy")
	request.Header.Set("X-Api-Key", "dummy")
	request.Header.Set("X-Opencode-Session", "fixture-run")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(got) != stream {
		t.Fatal("proxy altered the streamed response", err)
	}
	items := liveFinishedObservations(t, recorder)
	o := items[0]
	if len(items) != 1 || o.RequestID != 1 || o.RunID != "fixture-run" || o.Model != "fixture-model" || o.MaxTokens != 123 || o.ToolCount != 1 || o.HTTPStatus != 200 || len(o.Errors) != 0 || o.StopReason != "end_turn" {
		t.Fatalf("unexpected HTTP metadata: %+v", o)
	}
	if o.Usage.InputTokens != 10 || o.Usage.OutputTokens != 7 || o.Usage.CacheCreationInputTokens != 20 || o.Usage.CacheReadInputTokens != 30 || o.Usage.CacheCreation.Ephemeral5mInputTokens != 12 || o.Usage.CacheCreation.Ephemeral1hInputTokens != 8 || o.ThinkingChars != int64(utf8.RuneCountInString(thinking)) || o.OutputChars != 2 {
		t.Fatalf("cumulative usage or character counts changed: %+v", o)
	}
	if o.HeadersMS == nil || o.FirstEventMS == nil || o.FirstThinkingMS == nil || o.LastThinkingMS == nil || o.FirstOutputMS == nil || o.LastOutputMS == nil || *o.HeadersMS > *o.FirstEventMS || *o.FirstEventMS > *o.FirstThinkingMS || *o.LastThinkingMS > *o.FirstOutputMS || o.FinishedAt.Before(o.StartedAt) {
		t.Fatalf("event timing order was lost: %+v", o)
	}
	raw, _ := json.Marshal(items)
	for _, forbidden := range []string{secret, thinking, "private-request-text", "答复", "credential="} {
		if bytes.Contains(raw, []byte(forbidden)) {
			t.Fatal("proxy retained private request, response or credential data")
		}
	}
	*items[0].HeadersMS = -1
	if *recorder.Snapshot()[0].HeadersMS < 0 {
		t.Fatal("Snapshot timing pointer aliases recorder state")
	}
}

func TestLiveModelProxyJSONAndRequestCancellation(t *testing.T) {
	const payload = `{"type":"message","content":[{"type":"text","text":"json-output"}],"usage":{"input_tokens":2,"output_tokens":3,"cache_read_input_tokens":5},"stop_reason":"end_turn"}`
	started, cancelled := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/json" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, payload)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_start\"}\n\n")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(cancelled)
	}))
	defer upstream.Close()
	proxy, recorder, err := newLiveModelProxy(upstream.URL, "test-secret")
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	response, err := http.Get(proxy.URL + "/json")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(raw) != payload {
		t.Fatal("proxy altered JSON response", err)
	}
	first := liveFinishedObservations(t, recorder)[0]
	if first.Usage.InputTokens != 2 || first.Usage.OutputTokens != 3 || first.Usage.CacheReadInputTokens != 5 || first.OutputChars != 11 {
		t.Fatalf("JSON observation lost usage: %+v", first)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, proxy.URL+"/stream", nil)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	cancel()
	response.Body.Close()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("proxy request cancellation did not reach upstream")
	}
	items := liveFinishedObservations(t, recorder)
	if len(items) != 2 || items[1].RequestID != 2 || len(items[1].Errors) == 0 {
		t.Fatalf("cancelled HTTP attempt was not archived: %+v", items)
	}
}

func TestLiveModelProxyObservationLimitDoesNotTruncateStream(t *testing.T) {
	stream := "data: " + strings.Repeat("x", (1<<20)+1) + "\n\n" +
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":9}}\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stream)
	}))
	defer upstream.Close()
	proxy, recorder, err := newLiveModelProxy(upstream.URL, "test-secret")
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	response, err := http.Get(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(raw) != stream {
		t.Fatal("observation buffer limit truncated or altered upstream bytes", err)
	}
	o := liveFinishedObservations(t, recorder)[0]
	if o.Usage.OutputTokens != 9 || len(o.Errors) != 1 || o.Errors[0] != "sse_line_observation_limit" {
		t.Fatalf("observer failed to resume after oversized SSE line: %+v", o)
	}
}

func TestLiveModelProxyClientCloseRequiresTerminalEvent(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		name := "before_message_stop"
		if terminal {
			name = "after_message_stop"
		}
		t.Run(name, func(t *testing.T) {
			// A stop_reason in message_delta is not the stream's terminal event.
			// Keep upstream open to reproduce the real consumer's early Close.
			stream := "data: {\"type\":\"message_start\"}\n\n" +
				"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":4}}\n\n"
			if terminal {
				stream += "data: {\"type\":\"message_stop\"}\n\n"
			}
			cancelled := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, stream)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(cancelled)
			}))
			defer upstream.Close()
			proxy, recorder, err := newLiveModelProxy(upstream.URL, "test-secret")
			if err != nil {
				t.Fatal(err)
			}
			defer proxy.Close()
			response, err := http.Get(proxy.URL)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]byte, len(stream))
			_, err = io.ReadFull(response.Body, got)
			response.Body.Close()
			if err != nil || string(got) != stream {
				t.Fatal("proxy changed terminal stream bytes", err)
			}
			select {
			case <-cancelled:
			case <-time.After(time.Second):
				t.Fatal("client close did not cancel the remaining upstream stream")
			}
			o := liveFinishedObservations(t, recorder)[0]
			if o.HTTPStatus != http.StatusOK || o.StopReason != "tool_use" || o.Usage.OutputTokens != 4 {
				t.Fatalf("client close discarded the completed response metadata: %+v", o)
			}
			if terminal {
				if o.TerminalEvent != "message_stop" || o.TerminalEventMS == nil || !o.ClosedAfterTerminal || len(o.Errors) != 0 {
					t.Fatalf("normal close after message_stop was marked as a failure: %+v", o)
				}
				*o.TerminalEventMS = -1
				if *recorder.Snapshot()[0].TerminalEventMS < 0 {
					t.Fatal("Snapshot terminal timing aliases recorder state")
				}
			} else if o.TerminalEvent != "" || o.TerminalEventMS != nil || o.ClosedAfterTerminal || len(o.Errors) == 0 {
				t.Fatalf("stop_reason hid cancellation before message_stop: %+v", o)
			}
		})
	}
}
