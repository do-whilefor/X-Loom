package board

import (
	"slices"
	"strings"
)

// MatchingStep finds an already planned direction in the current board round.
// Sources form a set; priority and worker ownership are not new task inputs.
// Terminal steps also match: adding the same plan must not bypass the explicit
// execution retry authorization. New evidence or a changed task remains new.
func (s State) MatchingStep(goal string, from []string, description string) (Step, bool) {
	if goal == "" {
		goal = "goal"
	}
	sources := append([]string(nil), from...)
	slices.Sort(sources)
	sources = slices.Compact(sources)
	for _, step := range s.Steps {
		if step.GoalID != goal || strings.TrimSpace(step.Description) != strings.TrimSpace(description) {
			continue
		}
		existing := append([]string(nil), step.From...)
		slices.Sort(existing)
		if slices.Equal(sources, slices.Compact(existing)) {
			return step, true
		}
	}
	return Step{}, false
}
