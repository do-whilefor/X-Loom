package board

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestExportCairnYAMLAndTimeline(t *testing.T) {
	at := "2026-01-01T08:00:00Z"
	g := Graph{Project: Project{ID: "proj_001", Title: "项目", Status: "completed", CreatedAt: at, Bootstrap: false}, Facts: []Fact{{ID: "origin", Description: "start"}, {ID: "goal", Description: "finish"}, {ID: "f001", Description: "confirmed\nsecond line"}}, Hints: []Hint{{ID: "h001", Content: "human hint", Creator: "human", CreatedAt: at}}, Intents: []Intent{
		{ID: "i001", From: []string{"origin"}, To: Ptr("f001"), Description: "investigate", Creator: "reasoner", Worker: Ptr("explorer"), CreatedAt: at, ConcludedAt: &at},
		{ID: "i002", From: []string{"origin", "f001"}, To: Ptr("goal"), Description: "solved", Creator: "reasoner", Worker: Ptr("reasoner"), CreatedAt: at, ConcludedAt: &at},
	}}
	yml, err := Export(g, "yaml")
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := yaml.Unmarshal([]byte(yml), &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out["project"], map[string]any{"title": "项目", "origin": "start", "goal": "finish", "bootstrap_enabled": false}) {
		t.Fatalf("project YAML: %+v", out)
	}
	intents := out["intents"].([]any)
	entry := intents[1].(map[string]any)
	if _, ok := entry["id"]; ok {
		t.Fatal("Cairn export omits intent IDs")
	}
	if _, ok := entry["last_heartbeat_at"]; ok {
		t.Fatal("Cairn export omits lease heartbeat")
	}
	if !reflect.DeepEqual(entry["from"], []any{"origin", "f001"}) {
		t.Fatal(entry)
	}
	timeline, err := Export(g, "timeline")
	if err != nil {
		t.Fatal(err)
	}
	ts := displayTime(at)
	want := strings.Join([]string{
		"[" + ts + "] PROJECT CREATED\n  origin: start\n  goal: finish",
		"[" + ts + "] HINT by human\n  human hint",
		"[" + ts + "] INTENT DECLARED i001 by reasoner\n  from: origin\n  investigate",
		"[" + ts + "] INTENT CONCLUDED i001 by explorer\n  from: origin\n  produced: f001\n  confirmed\nsecond line",
		"[" + ts + "] INTENT DECLARED i002 by reasoner\n  from: origin, f001\n  solved",
		"[" + ts + "] PROJECT COMPLETED by reasoner\n  via: i002 from origin, f001",
	}, "\n\n") + "\n"
	if timeline != want {
		t.Fatalf("timeline ordering/format mismatch:\n%s\nwant:\n%s", timeline, want)
	}
}

func TestExportOptionalFieldsAndTimestampFallback(t *testing.T) {
	g := Graph{Project: Project{Title: "empty", CreatedAt: "invalid"}, Facts: []Fact{}}
	yml, err := Export(g, "yaml")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(yml, "hints:") || strings.Contains(yml, "intents:") {
		t.Fatal("empty optional arrays should be omitted")
	}
	if displayTime("invalid") != "invalid" || displayOptional(nil) != nil {
		t.Fatal("invalid timestamp fallback changed")
	}
	parsed, _ := time.Parse(time.RFC3339, "2026-01-01T08:00:00+08:00")
	if got := displayTime("2026-01-01T08:00:00+08:00"); got != parsed.In(time.Local).Format("2006-01-02 15:04:05") {
		t.Fatal(got)
	}
}
