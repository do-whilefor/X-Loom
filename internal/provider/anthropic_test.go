package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"

	"xloom/internal/agent"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func testProvider(roundTrip roundTripFunc) *Anthropic {
	return &Anthropic{
		BaseURL: "https://example.invalid/step_plan",
		Token:   "test-token",
		Timeout: 5 * time.Second,
		Client:  &http.Client{Transport: roundTrip},
	}
}

func testResponse(req *http.Request, status int, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

func TestGenerateRetriesTransientTransportBeforeResponse(t *testing.T) {
	const body = `{"role":"assistant","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn"}`
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"EOF", io.EOF},
		{"connection reset", syscall.ECONNRESET},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			var payloads []string
			p := testProvider(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.URL.String() != "https://example.invalid/step_plan/v1/messages" {
					t.Errorf("unexpected endpoint: %s", req.URL)
				}
				raw, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatal(err)
				}
				payloads = append(payloads, string(raw))
				if calls == 1 {
					return nil, tc.err
				}
				return testResponse(req, http.StatusOK, "application/json", body), nil
			})
			got, err := p.Generate(context.Background(), []agent.Message{agent.Text("user", "Reply OK")}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if calls != 2 || got.Text() != "OK" || len(payloads) != 2 || payloads[0] != payloads[1] {
				t.Fatalf("calls=%d, response=%q, request payloads equal=%v", calls, got.Text(), len(payloads) == 2 && payloads[0] == payloads[1])
			}
		})
	}
}

func TestGenerateStopsAfterThreeTransportAttempts(t *testing.T) {
	var calls int
	p := testProvider(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, io.EOF
	})
	_, err := p.Generate(context.Background(), []agent.Message{agent.Text("user", "Reply OK")}, nil, nil)
	var modelErr *agent.ModelError
	if calls != 3 || !errors.As(err, &modelErr) || modelErr.Kind != agent.ErrorTransport || !errors.Is(err, io.EOF) {
		t.Fatalf("calls=%d, error=%v", calls, err)
	}
}

func TestGenerateStillRetriesUnavailableHTTPStatus(t *testing.T) {
	var calls int
	p := testProvider(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return testResponse(req, http.StatusServiceUnavailable, "application/json", `{}`), nil
		}
		return testResponse(req, http.StatusOK, "application/json", `{"role":"assistant","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn"}`), nil
	})
	got, err := p.Generate(context.Background(), []agent.Message{agent.Text("user", "Reply OK")}, nil, nil)
	if err != nil || calls != 2 || got.Text() != "OK" {
		t.Fatalf("calls=%d, response=%q, error=%v", calls, got.Text(), err)
	}
}

func TestGenerateDoesNotRetryExpiredContext(t *testing.T) {
	var calls int
	p := testProvider(func(req *http.Request) (*http.Response, error) {
		calls++
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := p.Generate(ctx, []agent.Message{agent.Text("user", "Reply OK")}, nil, nil)
	if calls != 1 || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("calls=%d, error=%v", calls, err)
	}
}

func TestGenerateDoesNotRetryPermanentTransportError(t *testing.T) {
	var calls int
	p := testProvider(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("permanent TLS error")
	})
	_, err := p.Generate(context.Background(), []agent.Message{agent.Text("user", "Reply OK")}, nil, nil)
	var modelErr *agent.ModelError
	if calls != 1 || !errors.As(err, &modelErr) || modelErr.Kind != agent.ErrorTransport {
		t.Fatalf("calls=%d, error=%v", calls, err)
	}
}

func TestGenerateDoesNotReplayPartialStream(t *testing.T) {
	const stream = "data: {\"type\":\"message_start\"}\n\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n"
	var calls, deltas int
	p := testProvider(func(req *http.Request) (*http.Response, error) {
		calls++
		return testResponse(req, http.StatusOK, "text/event-stream", stream), nil
	})
	_, err := p.Generate(context.Background(), []agent.Message{agent.Text("user", "Reply OK")}, nil, func(event agent.Event) {
		if event.Type == "text_delta" {
			deltas++
		}
	})
	if calls != 1 || deltas != 1 || err == nil {
		t.Fatalf("calls=%d, emitted deltas=%d, error=%v", calls, deltas, err)
	}
}

func TestRetryableTransport(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"EOF", io.EOF, true},
		{"unexpected EOF", io.ErrUnexpectedEOF, true},
		{"connection reset", syscall.ECONNRESET, true},
		{"broken pipe", syscall.EPIPE, true},
		{"connection refused", syscall.ECONNREFUSED, true},
		{"request timeout", context.DeadlineExceeded, true},
		{"cancellation", context.Canceled, false},
		{"permanent error", errors.New("invalid certificate"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryableTransport(tc.err); got != tc.want {
				t.Fatalf("retryableTransport(%v)=%v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
