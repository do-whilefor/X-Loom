package worker

import (
	"bytes"
	"embed"
	"errors"
	"os"
	"path/filepath"
	"text/template"
	"xloom/internal/board"
)

//go:embed prompts/*.md
var prompts embed.FS

func Prompt(j Job, conclude bool, runDir string) (string, error) {
	if j.Kind != "bootstrap" && j.Kind != "explore" && j.Kind != "reason" {
		return "", errors.New("unknown task")
	}
	if conclude && j.Kind == "reason" {
		return "", errors.New("reason has no conclusion phase")
	}
	graph, err := board.Export(j.Graph, "yaml")
	if err != nil {
		return "", err
	}
	path := filepath.Join(runDir, "graph.yaml")
	if err = os.WriteFile(path, []byte(graph), 0600); err != nil {
		return "", err
	}
	context := "Read the complete task graph from " + path + ". Long evidence belongs in files; cite its path in the result. Distinguish confirmed findings from hypotheses.\n"
	name := j.Kind
	if conclude {
		name += "_conclude"
	}
	t, err := template.ParseFS(prompts, "prompts/"+name+".md")
	if err != nil {
		return "", err
	}
	var body bytes.Buffer
	if err = t.Execute(&body, j.Budget); err != nil {
		return "", err
	}
	if j.Kind == "explore" {
		return context + body.String() + intentContext(j), nil
	}
	return context + body.String(), nil
}
func intentContext(j Job) string {
	if j.Intent == nil {
		return ""
	}
	return "\nCurrent intent " + j.Intent.ID + ": " + j.Intent.Description
}
