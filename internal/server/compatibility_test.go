package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"xloom/internal/board"
)

func graph(t *testing.T, h http.Handler, id string) board.Graph {
	t.Helper()
	data := call(t, h, "GET", "/projects/"+id, "", 200)
	raw, _ := json.Marshal(data)
	var g board.Graph
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

func detail(t *testing.T, h http.Handler, method, path, body string, status int, want string) {
	t.Helper()
	d := call(t, h, method, path, body, status)
	var got string
	if err := json.Unmarshal(d["detail"], &got); err != nil {
		t.Fatalf("invalid detail: %s", d["detail"])
	}
	if got != want {
		t.Fatalf("detail %q, want %q", got, want)
	}
}

func TestCairnStatusAndErrorSemantics(t *testing.T) {
	_, h := fixture(t)
	create(t, h)
	cases := []struct {
		method, path, body string
		status             int
		message            string
	}{
		{"GET", "/projects/missing", "", 404, "Project not found"},
		{"DELETE", "/projects/missing", "", 404, "Project not found"},
		{"POST", "/projects/proj_001/intents", `{"from":["missing"],"description":"x","creator":"r"}`, 404, "Fact missing not found"},
		{"POST", "/projects/proj_001/intents", `{"from":["goal"],"description":"x","creator":"r"}`, 400, "goal cannot be used in from"},
		{"POST", "/projects/proj_001/intents", `{"from":["origin"],"description":"x","creator":"r","worker":"w"}`, 400, "worker must be null or equal to creator"},
		{"POST", "/projects/proj_001/intents/missing/heartbeat", `{"worker":"w"}`, 404, "Intent not found"},
		{"POST", "/projects/proj_001/reason/heartbeat", `{"worker":"w"}`, 409, "Project reason is not currently claimed"},
		{"POST", "/projects/proj_001/reopen", `{"description":"x","creator":"h"}`, 403, "Project is active"},
		{"GET", "/projects/proj_001/export?format=invalid", "", 400, "Supported formats: yaml, timeline"},
	}
	for _, c := range cases {
		t.Run(c.method+c.path+c.message, func(t *testing.T) { detail(t, h, c.method, c.path, c.body, c.status, c.message) })
	}
	call(t, h, "POST", "/projects/proj_001/intents", `{"from":["origin"],"description":"x","creator":"a","worker":"a"}`, 201)
	for _, op := range []string{"heartbeat", "release", "conclude"} {
		detail(t, h, "POST", "/projects/proj_001/intents/i001/"+op, `{"worker":"b","description":"x"}`, 409, "Intent is currently claimed by a")
	}
	call(t, h, "POST", "/projects/proj_001/intents/i001/conclude", `{"worker":"a","description":"confirmed"}`, 200)
	for _, op := range []string{"heartbeat", "release", "conclude"} {
		detail(t, h, "POST", "/projects/proj_001/intents/i001/"+op, `{"worker":"a","description":"x"}`, 409, "Intent already concluded")
	}
	call(t, h, "POST", "/projects/proj_001/complete", `{"from":["f001"],"description":"done","worker":"r"}`, 200)
	detail(t, h, "PUT", "/projects/proj_001/status", `{"status":"stopped"}`, 409, "Completed projects cannot change status")
	detail(t, h, "POST", "/projects/proj_001/intents", `{"from":["origin"],"description":"x","creator":"r"}`, 403, "Project is completed")
	call(t, h, "POST", "/projects/proj_001/hints", `{"creator":"human","content":"correction"}`, 201)
	call(t, h, "PUT", "/projects/proj_001/title", `{"title":" renamed "}`, 200)
	if graph(t, h, "proj_001").Project.Title != "renamed" {
		t.Fatal("title must remain editable after completion")
	}
}

func TestInputValidationRollsBackCountersAndWrites(t *testing.T) {
	_, h := fixture(t)
	for _, body := range []string{
		`null`, `[]`, `{} {}`, `{"title":"x","origin":"y","goal":"z","hints":[{"content":"x","creator":"h"},{}]}`,
		`{"title":"x","origin":"y","goal":"z","bootstrap_enabled":"sometimes"}`,
		`{"title":"x","origin":"y","goal":"z","hints":{}}`,
	} {
		call(t, h, "POST", "/projects", body, 422)
	}
	call(t, h, "POST", "/projects", `{"title":" title ","origin":" initial ","goal":" end ","bootstrap_enabled":false,"unknown":"ignored","hints":[{"content":" clue ","creator":" human "}]}`, 201)
	g := graph(t, h, "proj_001")
	if g.Project.Title != "title" || g.Project.Bootstrap || g.Hints[0].ID != "h001" || g.Hints[0].Content != "clue" {
		t.Fatalf("invalid normalization or rollback: %+v", g)
	}
	for _, body := range []string{
		`{"from":[],"description":"x","creator":"a"}`, `{"from":["  "],"description":"x","creator":"a"}`,
		`{"from":["origin","origin"],"description":"x","creator":"a"}`, `{"from":["origin"],"description":"x","creator":"a","worker":" "}`,
	} {
		call(t, h, "POST", "/projects/proj_001/intents", body, 422)
	}
	call(t, h, "POST", "/projects/proj_001/intents", `{"from_":[" origin "],"description":" x ","creator":" a "}`, 201)
	g = graph(t, h, "proj_001")
	if len(g.Intents) != 1 || g.Intents[0].ID != "i001" || g.Intents[0].From[0] != "origin" {
		t.Fatalf("invalid requests mutated graph: %+v", g)
	}
	call(t, h, "PUT", "/projects/proj_001/status", `{"status":" stopped "}`, 422)
	call(t, h, "PUT", "/settings", `{"intent_timeout":30}`, 422)
}

func TestReasonLeaseAndIntentReleaseSemantics(t *testing.T) {
	s, h := fixture(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	create(t, h)
	call(t, h, "POST", "/projects/proj_001/reason/release", `{"worker":"any"}`, 200)
	call(t, h, "POST", "/projects/proj_001/reason/claim", `{"worker":"a","trigger":"initial"}`, 200)
	now = now.Add(5 * time.Second)
	call(t, h, "POST", "/projects/proj_001/reason/claim", `{"worker":"a","trigger":"ignored"}`, 200)
	r := graph(t, h, "proj_001").Project.Reason
	if r.Trigger != "initial" || r.Heartbeat != r.StartedAt {
		t.Fatalf("claim must be idempotent: %+v", r)
	}
	for _, op := range []string{"claim", "heartbeat", "release"} {
		detail(t, h, "POST", "/projects/proj_001/reason/"+op, `{"worker":"b","trigger":"competing"}`, 409, "Project reason is currently claimed by a")
	}
	call(t, h, "POST", "/projects/proj_001/reason/heartbeat", `{"worker":"a"}`, 200)
	r = graph(t, h, "proj_001").Project.Reason
	if r.Heartbeat == r.StartedAt {
		t.Fatal("heartbeat was not renewed")
	}
	now = now.Add(16 * time.Second)
	detail(t, h, "POST", "/projects/proj_001/reason/heartbeat", `{"worker":"a"}`, 409, "Project reason is not currently claimed")
	call(t, h, "POST", "/projects/proj_001/reason/claim", `{"worker":"b","trigger":"retry"}`, 200)
	call(t, h, "POST", "/projects/proj_001/intents", `{"from":["origin"],"description":"work","creator":"a","worker":"a"}`, 201)
	old := graph(t, h, "proj_001").Intents[0].Heartbeat
	call(t, h, "POST", "/projects/proj_001/intents/i001/release", `{"worker":"a"}`, 200)
	call(t, h, "POST", "/projects/proj_001/intents/i001/release", `{"worker":"b"}`, 200)
	i := graph(t, h, "proj_001").Intents[0]
	if i.Worker != nil || board.Value(i.Heartbeat) != board.Value(old) {
		t.Fatalf("release changed historical heartbeat: %+v", i)
	}
	// Original Cairn allows unclaimed intents to be concluded without first
	// sending a heartbeat. The stricter fence is opt-in via dispatcher headers.
	call(t, h, "POST", "/projects/proj_001/intents/i001/conclude", `{"worker":"b","description":"confirmed"}`, 200)
}

func TestIndependentStoresSerializeClaims(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.db")
	a, err := board.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := board.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	handlers := []http.Handler{New(a), New(b)}
	create(t, handlers[0])
	call(t, handlers[0], "POST", "/projects/proj_001/intents", `{"from":["origin"],"description":"work","creator":"r"}`, 201)
	for _, path := range []string{"/projects/proj_001/intents/i001/heartbeat", "/projects/proj_001/reason/claim"} {
		var wg sync.WaitGroup
		codes := make(chan int, 12)
		start := make(chan struct{})
		for n := range 12 {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				<-start
				w := httptest.NewRecorder()
				r := httptest.NewRequest("POST", path, strings.NewReader(fmt.Sprintf(`{"worker":"w%d","trigger":"competing"}`, n)))
				handlers[n%2].ServeHTTP(w, r)
				codes <- w.Code
			}(n)
		}
		close(start)
		wg.Wait()
		close(codes)
		counts := map[int]int{}
		for code := range codes {
			counts[code]++
		}
		if counts[200] != 1 || counts[409] != 11 {
			t.Fatalf("%s claim race: %v", path, counts)
		}
	}
}

func TestBootstrapFenceAndRevocationAcrossStopResume(t *testing.T) {
	for _, interruption := range []string{"none", "stop_resume", "reopen"} {
		t.Run(interruption, func(t *testing.T) {
			_, h := fixture(t)
			create(t, h)
			call(t, h, "POST", "/projects/proj_001/intents", `{"from":["origin"],"description":"bootstrap","creator":"boot@run","worker":"boot@run"}`, 201)
			headers := []string{"X-Xloom-Run", "boot@run", "X-Xloom-Intent", "i001", "X-Xloom-Lease", "bootstrap"}
			call(t, h, "POST", "/projects/proj_001/intents/i001/conclude", `{"worker":"boot@run","description":"confirmed"}`, 200, headers...)
			call(t, h, "POST", "/projects/proj_001/intents", `{"from":["f001"],"description":"late exploration","creator":"boot@run"}`, 409, headers...)
			want := 200
			if interruption == "stop_resume" {
				call(t, h, "PUT", "/projects/proj_001/status", `{"status":"stopped"}`, 200)
				call(t, h, "PUT", "/projects/proj_001/status", `{"status":"active"}`, 200)
				want = 409
			}
			if interruption == "reopen" {
				call(t, h, "POST", "/projects/proj_001/complete", `{"from":["f001"],"description":"done","worker":"boot@run"}`, 200, headers...)
				call(t, h, "POST", "/projects/proj_001/reopen", `{"creator":"human","description":"correction"}`, 200)
				want = 409
			}
			call(t, h, "POST", "/projects/proj_001/complete", `{"from":["f001"],"description":"done","worker":"boot@run"}`, want, headers...)
			g := graph(t, h, "proj_001")
			if want == 409 && g.Project.Status != "active" {
				t.Fatalf("revoked result changed state: %+v", g.Project)
			}
		})
	}
}

func TestExpiredAndReplacedFencesRejectLateResults(t *testing.T) {
	s, h := fixture(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	create(t, h)
	call(t, h, "POST", "/projects/proj_001/intents", `{"from":["origin"],"description":"work","creator":"old","worker":"old"}`, 201)
	call(t, h, "POST", "/projects/proj_001/reason/claim", `{"worker":"reason-old","trigger":"initial"}`, 200)
	now = now.Add(time.Minute)
	call(t, h, "POST", "/projects/proj_001/intents/i001/heartbeat", `{"worker":"new"}`, 200)
	call(t, h, "POST", "/projects/proj_001/reason/claim", `{"worker":"reason-new","trigger":"retry"}`, 200)
	call(t, h, "POST", "/projects/proj_001/intents/i001/conclude", `{"worker":"old","description":"late"}`, 409, "X-Xloom-Run", "old", "X-Xloom-Intent", "i001")
	call(t, h, "POST", "/projects/proj_001/intents", `{"from":["origin"],"description":"late","creator":"reason-old"}`, 409, "X-Xloom-Run", "reason-old", "X-Xloom-Lease", "reason")
	call(t, h, "POST", "/projects/proj_001/complete", `{"from":["origin"],"description":"late","worker":"reason-old"}`, 409, "X-Xloom-Run", "reason-old", "X-Xloom-Lease", "reason")
	g := graph(t, h, "proj_001")
	if len(g.Facts) != 2 || len(g.Intents) != 1 {
		t.Fatalf("late result mutated graph: %+v", g)
	}
	call(t, h, "DELETE", "/projects/proj_001", "", 204)
	call(t, h, "POST", "/projects/proj_001/intents/i001/conclude", `{"worker":"new","description":"deleted"}`, 404, "X-Xloom-Run", "new", "X-Xloom-Intent", "i001")
}

func TestRestartAndDeletePreserveIDsAndCascade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart.db")
	s, err := board.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	h := New(s)
	create(t, h)
	call(t, h, "PUT", "/settings", `{"intent_timeout":30,"reason_timeout":45}`, 200)
	call(t, h, "POST", "/projects/proj_001/hints", `{"content":"note","creator":"h"}`, 201)
	call(t, h, "POST", "/projects/proj_001/intents", `{"from":["origin"],"description":"work","creator":"r","worker":"r"}`, 201)
	call(t, h, "PUT", "/projects/proj_001/status", `{"status":"stopped"}`, 200)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = board.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h = New(s)
	g := graph(t, h, "proj_001")
	if g.Project.Status != "stopped" || len(g.Hints) != 1 || g.Intents[0].Worker != nil {
		t.Fatalf("restart lost state: %+v", g)
	}
	call(t, h, "DELETE", "/projects/proj_001", "", 204)
	err = s.Do(context.Background(), func(tx *board.Tx) error {
		for _, table := range []string{"facts", "intents", "intent_sources", "hints", "scoped_counters", "xloom_revoked_runs"} {
			var count int
			if err := tx.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				t.Fatalf("%s rows survived deletion", table)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	call(t, h, "POST", "/projects", `{"title":"second","origin":"start","goal":"finish"}`, 201)
	if graph(t, h, "proj_002").Project.ID != "proj_002" {
		t.Fatal("project IDs reused")
	}
	d := call(t, h, "GET", "/settings", "", 200)
	if !bytes.Equal(d["intent_timeout"], []byte("30")) {
		t.Fatal(d)
	}
}

func TestRoutingWebAndExportContentTypes(t *testing.T) {
	_, h := fixture(t)
	create(t, h)
	for _, path := range []string{"/missing", "/projects/proj_001/reason/unknown", "/projects/proj_001/intents/i001/unknown"} {
		detail(t, h, "POST", path, `{}`, 404, "Not Found")
	}
	detail(t, h, "PATCH", "/projects/proj_001", `{}`, 405, "Method Not Allowed")
	for _, item := range []struct{ path, typ, contains string }{{"/", "text/html", "Cairn"}, {"/static/index.html", "text/html", "Cairn"}, {"/static/favicon.svg", "image/svg+xml", "<svg"}, {"/static/vendor/alpine.min.js", "javascript", "Alpine"}, {"/projects/proj_001/export", "text/plain", "origin: Known"}, {"/projects/proj_001/export?format=timeline", "text/plain", "PROJECT CREATED"}} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", item.path, nil))
		if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), item.typ) || !strings.Contains(w.Body.String(), item.contains) {
			t.Fatalf("%s: %d %s", item.path, w.Code, w.Header().Get("Content-Type"))
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/projects/?q=x", nil))
	if w.Code != 307 || w.Header().Get("Location") != "/projects?q=x" {
		t.Fatalf("trailing slash redirect: %d %s", w.Code, w.Header().Get("Location"))
	}
	call(t, h, "PUT", "/settings", `{"intent_timeout":30.0,"reason_timeout":"45.0"}`, 200)
}

func TestReopenDetectsInconsistentCompletionWithoutMutation(t *testing.T) {
	for _, corruption := range []string{"missing", "multiple", "no_sources"} {
		t.Run(corruption, func(t *testing.T) {
			s, h := fixture(t)
			create(t, h)
			call(t, h, "POST", "/projects/proj_001/complete", `{"from":["origin"],"description":"done","worker":"r"}`, 200)
			err := s.Do(context.Background(), func(tx *board.Tx) error {
				var q string
				switch corruption {
				case "missing":
					q = "DELETE FROM intents"
				case "no_sources":
					q = "DELETE FROM intent_sources"
				case "multiple":
					q = "INSERT INTO intents SELECT 'i002',project_id,to_fact_id,description,creator,worker,last_heartbeat_at,created_at,concluded_at FROM intents WHERE id='i001'"
				}
				_, err := tx.Exec(q)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			call(t, h, "POST", "/projects/proj_001/reopen", `{"creator":"human","description":"feedback"}`, 409)
			g := graph(t, h, "proj_001")
			if g.Project.Status != "completed" || len(g.Facts) != 2 {
				t.Fatalf("failed reopen mutated state: %+v", g)
			}
		})
	}
}
