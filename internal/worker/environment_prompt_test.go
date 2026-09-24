package worker

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"xloom/internal/board"
)

func TestInitialPromptDescribesActualWorkspaceAndDeclaredEnvironment(t *testing.T) {
	for _, environment := range []string{"", "unknown", "kali-headless"} {
		for _, kind := range []string{"bootstrap", "explore", "reason"} {
			for _, workspace := range []string{"/workspace", "/workspace/team \"blue\"\nscan\\results"} {
				t.Run(environment+"/"+kind+"/"+strconv.Quote(workspace), func(t *testing.T) {
					t.Setenv("XLOOM_WORKER_ENVIRONMENT", environment)
					job := Job{
						Kind: kind, Workspace: workspace,
						Graph: board.Graph{
							Project: board.Project{ID: "environment-test"},
							Facts:   []board.Fact{{ID: "origin", Description: "Prior bash, nuclei and ffuf observations"}},
						},
					}
					prompt, err := Prompt(job, false, t.TempDir())
					if err != nil {
						t.Fatal(err)
					}
					intro, remaining, separated := strings.Cut(prompt, "\n\n")
					if !separated || !strings.HasPrefix(intro, "Environment:\n") || !strings.Contains(remaining, "<task_graph>") {
						t.Fatalf("initial prompt does not separate its environment from task data: %s", prompt)
					}
					if strings.Count(prompt, "Environment:") != 1 || !strings.Contains(intro, strconv.Quote(workspace)) {
						t.Fatalf("environment lost or changed the quoted workspace: %s", intro)
					}
					if strings.Contains(workspace, "\n") && strings.Contains(intro, workspace) {
						t.Fatalf("workspace newline escaped its quoted value: %s", intro)
					}
					for _, claim := range []string{"Kali Linux container", "kali-linux-headless"} {
						if strings.Contains(intro, claim) != (environment == "kali-headless") {
							t.Errorf("environment %q incorrectly describes %q: %s", environment, claim, intro)
						}
					}
					for _, tool := range []string{"bash", "nuclei", "ffuf"} {
						if strings.Contains(intro, tool) != (kind != "reason") {
							t.Errorf("%s environment gives incorrect execution guidance for %s: %s", kind, tool, intro)
						}
					}
				})
			}
		}
	}
}

func TestPhaseInstructionsDoNotRepeatEnvironment(t *testing.T) {
	t.Setenv("XLOOM_WORKER_ENVIRONMENT", "kali-headless")
	for _, kind := range []string{"bootstrap", "explore"} {
		t.Run(kind, func(t *testing.T) {
			job := Job{Kind: kind, Workspace: "/workspace/shared", ResultContractVersion: 2}
			initial, err := Prompt(job, false, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			conclusion, err := Prompt(job, true, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			for _, concluding := range []bool{false, true} {
				repair, err := repairInstruction(job, concluding, 1, &outputFailure{Reason: "invalid_contract", Detail: "fixture"})
				if err != nil {
					t.Fatal(err)
				}
				combined := initial + conclusion + repair
				for _, shared := range []string{"Environment:", "Kali Linux container", strconv.Quote(job.Workspace)} {
					if strings.Count(combined, shared) != 1 {
						t.Errorf("concluding=%t lost or repeated initial environment field %q", concluding, shared)
					}
				}
			}
		})
	}
}

func TestKaliWorkerImageDeclaresInstalledEnvironment(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "container", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "XLOOM_WORKER_ENVIRONMENT=kali-headless") {
		t.Fatal("Kali worker image does not declare its prompt environment")
	}
	if !regexp.MustCompile(`(?m)^\s+kali-linux-headless\s+\\\r?$`).Match(raw) {
		t.Fatal("Kali worker environment claim is missing its installed headless toolset")
	}
}
