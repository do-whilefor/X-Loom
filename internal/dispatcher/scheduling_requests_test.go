package dispatcher

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"xloom/internal/board"
)

func TestDispatchCandidateRequestCounts(t *testing.T) {
	s, _, _, _ := automaticRetryFixture(t, 0, "")
	var reason, explore atomic.Int32
	s.Client.HTTP = &http.Client{Transport: executionQueryTransport(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/executions/check") {
			switch r.URL.Query().Get("kind") {
			case "reason":
				reason.Add(1)
			case "explore":
				explore.Add(1)
			}
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
	retryTicks(t, s, 1)
	if got := reason.Load(); got != 1 {
		t.Fatalf("initial Decide checks = %d", got)
	}
	reason.Store(0)
	retryTicks(t, s, 1)
	if got := explore.Load(); got != 1 {
		t.Fatalf("chosen Execute checks = %d", got)
	}
	t.Logf("candidate checks: initial Decide=1, chosen Execute=%d", explore.Load())
}

func TestRetryKeyPreservesLegacyBytes(t *testing.T) {
	s, _, _, _ := automaticRetryFixture(t, 0, "")
	g := board.Graph{Project: board.Project{ID: "fixture"}}
	if got := s.retryKey(g, "reason", nil); got != "reason:6131a7e2e21f30d058650b89faec6a12380760e4c8ef0d39b481400bb08c2ac0" {
		t.Fatalf("nil collection identity changed: %s", got)
	}
	g.Facts, g.Hints = []board.Fact{}, []board.Hint{}
	g.Intents = []board.Intent{{ID: "open"}, {ID: "done", To: board.Ptr("fact")}, {ID: "abandoned", ConcludedAt: board.Ptr("now")}}
	s.stateRevisions[g.Project.ID] = 7
	if got := s.retryKey(g, "reason", nil); got != "reason:a86d0bc22e11e5da882fdc78a134a95cafd08467213a711605afcc547492a35d" {
		t.Fatalf("ordered completion identity changed: %s", got)
	}
}
