package board

import (
	"fmt"
	"sort"
	"strings"
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
	facts := map[string]string{}
	for _, f := range g.Facts {
		facts[f.ID] = f.Description
	}
	if format == "timeline" {
		type event struct{ at, text string }
		events := []event{{g.Project.CreatedAt, fmt.Sprintf("[%s] PROJECT CREATED\n  origin: %s\n  goal: %s", displayTime(g.Project.CreatedAt), facts["origin"], facts["goal"])}}
		for _, h := range g.Hints {
			events = append(events, event{h.CreatedAt, fmt.Sprintf("[%s] HINT by %s\n  %s", displayTime(h.CreatedAt), h.Creator, h.Content)})
		}
		for _, i := range g.Intents {
			from := strings.Join(i.From, ", ")
			meta := "  from: " + from
			if i.Worker != nil && i.ConcludedAt == nil {
				meta += "\n  worker: " + *i.Worker + " (in progress)"
			}
			events = append(events, event{i.CreatedAt, fmt.Sprintf("[%s] INTENT DECLARED %s by %s\n%s\n  %s", displayTime(i.CreatedAt), i.ID, i.Creator, meta, i.Description)})
			if i.ConcludedAt == nil || i.To == nil {
				continue
			}
			actor := Value(i.Worker)
			if actor == "" {
				actor = i.Creator
			}
			var text string
			if *i.To == "goal" {
				text = fmt.Sprintf("[%s] PROJECT COMPLETED by %s\n  via: %s from %s", displayTime(*i.ConcludedAt), actor, i.ID, from)
			} else {
				text = fmt.Sprintf("[%s] INTENT CONCLUDED %s by %s\n  from: %s\n  produced: %s\n  %s", displayTime(*i.ConcludedAt), i.ID, actor, from, *i.To, facts[*i.To])
			}
			events = append(events, event{*i.ConcludedAt, text})
		}
		sort.SliceStable(events, func(i, j int) bool { return events[i].at < events[j].at })
		parts := []string{}
		for _, e := range events {
			parts = append(parts, e.text)
		}
		return strings.Join(parts, "\n\n") + "\n", nil
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
