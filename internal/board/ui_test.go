package board

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectScenarioPersistsAndLegacySavePreservesMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	err = s.Do(ctx, func(tx *Tx) error {
		g := Graph{Project: Project{ID: "proj_001", Title: "audit", Status: "active", Scenario: "audit", CreatedAt: tx.Now}}
		if err := tx.Save(g); err != nil {
			return err
		}
		// A caller compiled against the old graph schema will omit Scenario.
		g.Project.Scenario = ""
		g.Project.Title = "renamed by a legacy client"
		return tx.Save(g)
	})
	if err != nil {
		s.Close()
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	err = s.Do(ctx, func(tx *Tx) error {
		g, err := tx.Load("proj_001")
		if err != nil {
			return err
		}
		if g.Project.Scenario != "audit" || g.Project.Title != "renamed by a legacy client" {
			t.Fatalf("lost persisted metadata: %+v", g.Project)
		}
		if _, err = tx.Exec("DELETE FROM projects WHERE id=?", g.Project.ID); err != nil {
			return err
		}
		var count int
		if err = tx.QueryRow("SELECT COUNT(*) FROM xloom_project_metadata").Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatal("deleted project's metadata survived")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestProjectScenarioOmittedFromLegacyJSONAndInvalidSaveRejected(t *testing.T) {
	s := openTestStore(t)
	err := s.Do(context.Background(), func(tx *Tx) error {
		g := Graph{Project: Project{ID: "proj_001", Title: "legacy", Status: "active", CreatedAt: tx.Now}}
		if err := tx.Save(g); err != nil {
			return err
		}
		loaded, err := tx.Load(g.Project.ID)
		if err != nil {
			return err
		}
		for _, v := range []any{loaded.Project, loaded.Summarize()} {
			raw, err := json.Marshal(v)
			if err != nil {
				return err
			}
			if strings.Contains(string(raw), "scenario") {
				t.Fatalf("legacy response changed: %s", raw)
			}
		}
		g.Project.Scenario = "reason"
		if err := tx.Save(g); err == nil {
			t.Fatal("an execution kind was accepted as a project scenario")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestExecutionResultProjectionDropsUnknownFieldsAndBoundsText(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"text": strings.Repeat("证", 64*1024+5), "error": strings.Repeat("误", 4*1024+2),
		"job": "private-input", "env": map[string]string{"secret": "private-env"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := executionResultView(raw)
	if got == nil || !got.Truncated || len([]rune(got.Text)) != 64*1024 || len([]rune(got.Error)) != 4*1024 {
		t.Fatal("result projection did not bound text without splitting Unicode")
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private-") {
		t.Fatal("unexpected result fields escaped the projection")
	}
	for _, raw := range []string{`null`, `{}`, `{"env":{"secret":"value"}}`, `{"text":12}`, `invalid`} {
		if executionResultView([]byte(raw)) != nil {
			t.Fatalf("unexpected result for %s", raw)
		}
	}
	got = executionResultView([]byte(`{"text":"hello","truncated":true}`))
	if got == nil || got.Truncated {
		t.Fatal("producer-controlled truncation flag was trusted")
	}
}
