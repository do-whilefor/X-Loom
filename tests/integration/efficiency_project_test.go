//go:build linux

package integration

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"xloom/internal/board"
)

type auditAPIObservation struct {
	Method     string    `json:"method"`
	Path       string    `json:"path"`
	Started    time.Time `json:"started"`
	DurationMS float64   `json:"duration_ms"`
	Status     int       `json:"status"`
	Bytes      int       `json:"response_bytes"`
}

type auditAPIRecorder struct {
	mu   sync.Mutex
	rows []auditAPIObservation
}

type auditResponse struct {
	http.ResponseWriter
	status, bytes int
}

func (w *auditResponse) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
func (w *auditResponse) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += n
	return n, err
}
func (r *auditAPIRecorder) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		started := time.Now()
		out := &auditResponse{ResponseWriter: w}
		next.ServeHTTP(out, q)
		if out.status == 0 {
			out.status = http.StatusOK
		}
		row := auditAPIObservation{Method: q.Method, Path: q.URL.Path, Started: started.UTC(), DurationMS: float64(time.Since(started)) / float64(time.Millisecond), Status: out.status, Bytes: out.bytes}
		r.mu.Lock()
		r.rows = append(r.rows, row)
		r.mu.Unlock()
	})
}
func (r *auditAPIRecorder) snapshot() []auditAPIObservation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]auditAPIObservation(nil), r.rows...)
}

func TestAuditAPIRecorder(t *testing.T) {
	r := &auditAPIRecorder{}
	h := r.wrap(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		w.WriteHeader(201)
		w.WriteHeader(500)
		_, _ = w.Write([]byte("test"))
	}))
	q := httptest.NewRequest("POST", "/test?token=never-retain", strings.NewReader("secret-body"))
	q.Header.Set("Authorization", "secret-header")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, q)
	rows := r.snapshot()
	if len(rows) != 1 || rows[0].Path != "/test" || rows[0].Status != 201 || rows[0].Bytes != 4 || rows[0].DurationMS < 0 {
		t.Fatalf("invalid observation: %+v", rows)
	}
	raw, _ := json.Marshal(rows)
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "never-retain") {
		t.Fatal("recorder leaked request content")
	}
	rows[0].Path = "mutated"
	if r.snapshot()[0].Path != "/test" {
		t.Fatal("snapshot aliases recorder")
	}
}

func TestLiveEfficiencyProject(t *testing.T) {
	if os.Getenv("XLOOM_LIVE_EFFICIENCY_TEST") != "1" {
		t.Skip("opt in to the local audit fixture and real model")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get("http://127.0.0.1:18080/health")
	if err != nil {
		t.Fatal("local fixture is not ready")
	}
	defer response.Body.Close()
	var health struct {
		Service string `json:"service"`
		Status  string `json:"status"`
	}
	if json.NewDecoder(response.Body).Decode(&health) != nil || response.StatusCode != 200 || health.Service != "ParcelHub" || health.Status != "ok" {
		t.Fatal("unexpected local fixture identity")
	}
	origin := `Audit the local ParcelHub multi-tenant order/report application at http://127.0.0.1:18080.
This service and every account are synthetic and explicitly authorized for this test. Scope is ONLY that local service and /workspace/auditlab. Do not scan any other host, read environment credentials, install packages, alter service source, or repair the target. Source and product/role documentation are under /workspace/auditlab. The service is already running; do not start another instance. Test users and access instructions are in the local documentation.
The workspace is a source/document snapshot. The running backend is in a separate container sharing loopback; its database and process namespace are not available to you. Validate live state through HTTP, not local SQLite files or process inspection.
Investigate tenant isolation, object authorization, role restrictions and refund business rules, report/export handling and file boundaries, and authentication/admin/search controls. Use a few independent directions when useful. Read source to form hypotheses, then verify dynamically. Distinguish exploitable boundary failures from public identifiers, version banners, missing cosmetic headers and correctly rejected requests. Use both tenants and both allowed/denied roles for controls. Preserve exact request/response evidence in files (including method/path/actor/status/body) before publishing Facts/Findings. Keep destructive test changes limited to synthetic refundable orders; use distinct orders if parallel work would interfere.
After substantive exploration, independently review and consolidate the evidence into /workspace/auditlab/report.json and report.md. JSON must be an object with nonempty findings and validated_controls arrays, and a limitations array. Each finding needs title, endpoint, actor, impact, reproduction and evidence_paths. Describe actual tested controls and any unfinished coverage. Do not infer success from a scanner or from source alone. Complete when all requested areas are either tested or explicitly limited, and the report and evidence exist. Do not keep generating follow-up work for cosmetic issues.`
	goal := "Deliver an evidence-backed security audit of the local multi-tenant order/report app covering tenant isolation, role/refund authorization, report/export file boundaries and authentication/admin/search controls. Retain reproducible request/response evidence, verified findings, negative controls and consolidated JSON/Markdown reports."
	runObservedProject(t, "ParcelHub multi-tenant application audit", origin, goal, "structural_report_delivery_only", validateEfficiencyReport)
}

// This checks report delivery only. Exploit coverage and evidence validity must
// also be independently reviewed against the fixture and retained HTTP traces.
func validateEfficiencyReport(state board.State, files map[string][]byte) []string {
	failures := []string{}
	if state.Graph.Project.Status != "completed" {
		failures = append(failures, "project did not complete")
	}
	var report struct {
		Findings []struct {
			Title         string          `json:"title"`
			Endpoint      string          `json:"endpoint"`
			Actor         string          `json:"actor"`
			Impact        string          `json:"impact"`
			Reproduction  json.RawMessage `json:"reproduction"`
			EvidencePaths []string        `json:"evidence_paths"`
		} `json:"findings"`
		Controls    []json.RawMessage `json:"validated_controls"`
		Limitations []json.RawMessage `json:"limitations"`
	}
	if err := json.Unmarshal(files["/workspace/auditlab/report.json"], &report); err != nil {
		failures = append(failures, "missing or invalid report.json")
	} else if len(report.Findings) == 0 || len(report.Controls) == 0 || report.Limitations == nil {
		failures = append(failures, "report is missing findings, controls or limitations")
	} else {
		for _, finding := range report.Findings {
			if strings.TrimSpace(finding.Title) == "" || strings.TrimSpace(finding.Endpoint) == "" || strings.TrimSpace(finding.Actor) == "" || strings.TrimSpace(finding.Impact) == "" || !auditNonemptyJSON(finding.Reproduction) || len(finding.EvidencePaths) == 0 {
				failures = append(failures, "finding is missing required fields")
			}
			for _, evidence := range finding.EvidencePaths {
				if !strings.HasPrefix(evidence, "/workspace/") || path.Clean(evidence) != evidence || len(files[evidence]) == 0 {
					failures = append(failures, "finding evidence file is absent or outside the retained workspace")
				}
			}
		}
		for _, control := range report.Controls {
			if !auditNonemptyJSON(control) {
				failures = append(failures, "empty validated control")
			}
		}
	}
	if len(files["/workspace/auditlab/report.md"]) == 0 {
		failures = append(failures, "missing report.md")
	}
	return failures
}

func auditNonemptyJSON(raw json.RawMessage) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v) != ""
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	default:
		return false
	}
}

func TestEfficiencyReportRequiresDeliverables(t *testing.T) {
	state := board.State{Graph: board.Graph{Project: board.Project{Status: "completed"}}}
	if len(validateEfficiencyReport(state, nil)) == 0 {
		t.Fatal("completion alone passed")
	}
	valid := `{"findings":[{"title":"test","endpoint":"/api/test","actor":"test.user","impact":"boundary failure","reproduction":["replay request"],"evidence_paths":["/workspace/auditlab/evidence.json"]}],"validated_controls":["anonymous request denied"],"limitations":[]}`
	files := map[string][]byte{"/workspace/auditlab/report.json": []byte(valid), "/workspace/auditlab/report.md": []byte("report"), "/workspace/auditlab/evidence.json": []byte("evidence")}
	if failures := validateEfficiencyReport(state, files); len(failures) != 0 {
		t.Fatal(failures)
	}
	state.Graph.Project.Status = "active"
	if len(validateEfficiencyReport(state, files)) == 0 {
		t.Fatal("active project passed")
	}
	state.Graph.Project.Status = "completed"
	for _, invalid := range []string{`{"findings":[{}],"validated_controls":[{}],"limitations":[]}`, strings.Replace(valid, `"limitations":[]`, `"limitations":null`, 1), strings.Replace(valid, "/workspace/auditlab/evidence.json", "/etc/passwd", 1), strings.Replace(valid, "/workspace/auditlab/evidence.json", "/workspace/missing.json", 1), strings.Replace(valid, `["replay request"]`, `[]`, 1)} {
		files["/workspace/auditlab/report.json"] = []byte(invalid)
		if len(validateEfficiencyReport(state, files)) == 0 {
			t.Fatal("incomplete or unbound evidence passed")
		}
	}
}

func retainedWorkspaceArchive(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for _, member := range []struct{ name, body string }{
		{"workspace/auditlab/Response.txt", "uppercase evidence"},
		{"workspace/auditlab/response.txt", "lowercase evidence"},
	} {
		if err := writer.WriteHeader(&tar.Header{Name: member.name, Mode: 0600, Size: int64(len(member.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(writer, member.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	// Include bytes beyond tar's logical EOF to exercise the stream drain.
	buffer.Write(make([]byte, 2048))
	buffer.WriteString("retained transport trailer")
	return buffer.Bytes()
}

func assertRetainedArchive(t *testing.T, output string, original []byte) {
	t.Helper()
	retained, err := os.ReadFile(filepath.Join(output, "workspace.tar"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(retained, original) {
		t.Fatalf("retained archive differs from original stream: got %d bytes, want %d", len(retained), len(original))
	}
}

func TestRetainLiveWorkspacePreservesCaseAndOriginalArchive(t *testing.T) {
	output, original := t.TempDir(), retainedWorkspaceArchive(t)
	files, err := retainLiveWorkspace(bytes.NewReader(original), output)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || string(files["/workspace/auditlab/Response.txt"]) != "uppercase evidence" || string(files["/workspace/auditlab/response.txt"]) != "lowercase evidence" {
		t.Fatalf("case-distinct members were lost from evidence map: %v", files)
	}
	assertRetainedArchive(t, output, original)
}

func TestRetainLiveWorkspaceKeepsArchiveAfterExtractionFailure(t *testing.T) {
	output, original := t.TempDir(), retainedWorkspaceArchive(t)
	// A regular file blocks the extraction directory without relying on modes
	// that root or a bind-mounted filesystem might ignore.
	if err := os.WriteFile(filepath.Join(output, "workspace"), []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := retainLiveWorkspace(bytes.NewReader(original), output); err == nil {
		t.Fatal("extraction failure was discarded")
	}
	assertRetainedArchive(t, output, original)
}

type retainedArchiveReadError struct{ err error }

func (reader retainedArchiveReadError) Read([]byte) (int, error) { return 0, reader.err }

func TestRetainLiveWorkspaceReportsReadErrorAfterTarEnd(t *testing.T) {
	output, original := t.TempDir(), retainedWorkspaceArchive(t)
	want := errors.New("synthetic archive transport failure")
	source := io.MultiReader(bytes.NewReader(original), retainedArchiveReadError{want})
	if _, err := retainLiveWorkspace(source, output); !errors.Is(err, want) {
		t.Fatalf("archive drain error = %v, want %v", err, want)
	}
	assertRetainedArchive(t, output, original)
}
