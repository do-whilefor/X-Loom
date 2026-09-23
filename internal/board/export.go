package board

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

func displayTime(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.In(time.Local).Format("2006-01-02 15:04:05")
}
func displayOptional(s *string) *string {
	if s == nil {
		return nil
	}
	return Ptr(displayTime(*s))
}
func Export(g Graph, format string) (string, error) {
	if format != "yaml" {
		return "", fmt.Errorf("graph-only export supports yaml; use ExportTimeline for FGS history")
	}
	facts := map[string]string{}
	for _, f := range g.Facts {
		facts[f.ID] = f.Description
	}
	type project struct {
		Title     string `yaml:"title"`
		Origin    string `yaml:"origin"`
		Goal      string `yaml:"goal"`
		Bootstrap bool   `yaml:"bootstrap_enabled"`
	}
	type hint struct {
		Content string `yaml:"content"`
		Creator string `yaml:"creator"`
		Created string `yaml:"created_at"`
	}
	type intent struct {
		From        []string `yaml:"from"`
		To          *string  `yaml:"to"`
		Description string   `yaml:"description"`
		Creator     string   `yaml:"creator"`
		Worker      *string  `yaml:"worker"`
		Created     string   `yaml:"created_at"`
		Concluded   *string  `yaml:"concluded_at"`
	}
	out := struct {
		Project project  `yaml:"project"`
		Hints   []hint   `yaml:"hints,omitempty"`
		Facts   []Fact   `yaml:"facts"`
		Intents []intent `yaml:"intents,omitempty"`
	}{Project: project{g.Project.Title, facts["origin"], facts["goal"], g.Project.Bootstrap}, Facts: g.Facts}
	for _, h := range g.Hints {
		out.Hints = append(out.Hints, hint{h.Content, h.Creator, displayTime(h.CreatedAt)})
	}
	for _, i := range g.Intents {
		out.Intents = append(out.Intents, intent{i.From, i.To, i.Description, i.Creator, i.Worker, displayTime(i.CreatedAt), displayOptional(i.ConcludedAt)})
	}
	data, err := yaml.Marshal(out)
	return string(data), err
}
