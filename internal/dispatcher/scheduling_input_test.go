package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"xloom/internal/board"
	"xloom/internal/config"
)

func TestScheduleInputRequestsLargeCompactPages(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%v", legacy), func(t *testing.T) {
			var requests atomic.Int32
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				request := requests.Add(1)
				if r.URL.Path != "/projects/p/scheduling" || r.URL.Query().Get("namespace") != "xloom" || r.URL.Query().Get("limit") != "1000" {
					t.Errorf("unexpected schedule request: %s", r.URL)
				}
				start, end, next := 0, 205, 0
				if request == 1 {
					if r.URL.Query().Get("offset") != "0" {
						t.Errorf("first request skipped the first steps: %s", r.URL)
					}
					if legacy {
						end, next = 100, 100
					}
				} else {
					start = int(request-1) * 100
					end = min(start+100, 205)
					if end < 205 {
						next = end
					}
					if !legacy || request > 3 || r.URL.Query().Get("offset") != fmt.Sprint(start) || r.URL.Query().Get("expected_version") != "version" {
						t.Errorf("unexpected compatibility page: %s", r.URL)
					}
				}
				page := board.SchedulePage{Project: board.Project{ID: "p", Generation: 3}, StateVersion: "version", RetryKey: "reason:key", Revision: 9, DecisionRevision: 8, NextOffset: next, ExecutionChecks: map[string]board.ExecutionCheck{}}
				for n := start; n < end; n++ {
					id := fmt.Sprintf("i%03d", n)
					page.Intents = append(page.Intents, board.Intent{ID: id})
					page.Steps = append(page.Steps, board.Step{ID: id, Status: "open"})
					page.ExecutionChecks["explore:"+id] = board.ExecutionCheck{}
				}
				_ = json.NewEncoder(w).Encode(page)
			}))
			defer api.Close()
			s := New(config.Config{Server: api.URL}, &batchProtocolRunner{})
			input, err := s.scheduleInput(context.Background(), "p")
			wantRequests := int32(1)
			if legacy {
				wantRequests = 3
			}
			if err != nil || requests.Load() != wantRequests || len(input.Intents) != 205 || len(input.Steps) != 205 || len(input.ExecutionChecks) != 205 || input.StateVersion != "version" || input.Project.Generation != 3 {
				t.Fatalf("schedule lost metadata or rebuilt pages unnecessarily: requests=%d intents=%d steps=%d checks=%d error=%v", requests.Load(), len(input.Intents), len(input.Steps), len(input.ExecutionChecks), err)
			}
		})
	}
}
