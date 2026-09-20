package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"xloom/internal/board"
)

func fixture(t *testing.T) (*board.Store, http.Handler) {
	t.Helper()
	s, err := board.Open(filepath.Join(t.TempDir(), "cairn.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, New(s)
}
func call(t *testing.T, h http.Handler, method, path, body string, want int, headers ...string) map[string]json.RawMessage {
	t.Helper()
	r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	for i := 0; i < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != want {
		t.Fatalf("%s %s: got %d want %d: %s", method, path, w.Code, want, w.Body.String())
	}
	var data map[string]json.RawMessage
	_ = json.Unmarshal(w.Body.Bytes(), &data)
	return data
}
func create(t *testing.T, h http.Handler) {
	call(t, h, "POST", "/projects", `{"title":"Test","origin":"Known","goal":"Done"}`, 201)
}
func TestLifecycleAndReopen(t *testing.T) {
	_, h := fixture(t)
	create(t, h)
	call(t, h, "POST", "/projects/proj_001/intents", `{"from":["origin"],"description":"Investigate","creator":"a"}`, 201)
	call(t, h, "POST", "/projects/proj_001/intents/i001/heartbeat", `{"worker":"b"}`, 200)
	call(t, h, "POST", "/projects/proj_001/intents/i001/conclude", `{"worker":"b","description":"Confirmed"}`, 200)
	call(t, h, "POST", "/projects/proj_001/complete", `{"from":["f001"],"description":"Evidence sufficient","worker":"r"}`, 200)
	call(t, h, "PUT", "/projects/proj_001/status", `{"status":"active"}`, 409)
	call(t, h, "POST", "/projects/proj_001/reopen", `{"creator":"human","description":"Completion was incorrect"}`, 200)
	d := call(t, h, "GET", "/projects/proj_001", "", 200)
	var g board.Graph
	raw, _ := json.Marshal(d)
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	if g.Project.Status != "active" || len(g.Facts) != 4 || len(g.Intents) != 2 {
		t.Fatalf("unexpected graph: %+v", g)
	}
	for _, i := range g.Intents {
		if board.Value(i.To) == "goal" {
			t.Fatal("completion edge survived reopen")
		}
	}
}
func TestStopAndExpiredRunCannotConclude(t *testing.T) {
	s, h := fixture(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	create(t, h)
	call(t, h, "POST", "/projects/proj_001/intents", `{"from":["origin"],"description":"Investigate","creator":"run-a","worker":"run-a"}`, 201)
	now = now.Add(time.Minute)
	call(t, h, "POST", "/projects/proj_001/intents/i001/conclude", `{"worker":"run-a","description":"Late"}`, 409, "X-Xloom-Run", "run-a", "X-Xloom-Intent", "i001")
	call(t, h, "POST", "/projects/proj_001/reason/claim", `{"worker":"r","trigger":"initial"}`, 200)
	call(t, h, "PUT", "/projects/proj_001/status", `{"status":"stopped"}`, 200)
	call(t, h, "POST", "/projects/proj_001/hints", `{"creator":"human","content":"Still writable"}`, 201)
	call(t, h, "POST", "/projects/proj_001/intents/i001/heartbeat", `{"worker":"b"}`, 403)
	d := call(t, h, "GET", "/projects/proj_001", "", 200)
	var p board.Project
	json.Unmarshal(d["project"], &p)
	if p.Reason != nil {
		t.Fatal("reason retained after stop")
	}
}
func TestConcurrentClaimHasOneOwner(t *testing.T) {
	_, h := fixture(t)
	create(t, h)
	call(t, h, "POST", "/projects/proj_001/intents", `{"from":["origin"],"description":"Investigate","creator":"a"}`, 201)
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for _, worker := range []string{"a", "b"} {
		wg.Add(1)
		go func(worker string) {
			defer wg.Done()
			w := httptest.NewRecorder()
			r := httptest.NewRequest("POST", "/projects/proj_001/intents/i001/heartbeat", bytes.NewBufferString(`{"worker":"`+worker+`"}`))
			h.ServeHTTP(w, r)
			codes <- w.Code
		}(worker)
	}
	wg.Wait()
	close(codes)
	counts := map[int]int{}
	for c := range codes {
		counts[c]++
	}
	if counts[200] != 1 || counts[409] != 1 {
		t.Fatal(counts)
	}
}
func TestValidationAndExport(t *testing.T) {
	_, h := fixture(t)
	call(t, h, "POST", "/projects", `{"title":" ","origin":"x","goal":"y"}`, 422)
	create(t, h)
	call(t, h, "POST", "/projects/proj_001/intents", `{"from":["goal"],"description":"x","creator":"a"}`, 400)
	call(t, h, "POST", "/projects/proj_001/intents", `{"from":["origin"],"description":"x","creator":"a","worker":"b"}`, 400)
	call(t, h, "PUT", "/settings", `{"intent_timeout":4,"reason_timeout":15}`, 422)
	call(t, h, "GET", "/projects/proj_001/export?format=yaml", "", 200)
	call(t, h, "GET", "/projects/proj_001/export?format=timeline", "", 200)
}
