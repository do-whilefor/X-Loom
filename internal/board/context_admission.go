package board

import "fmt"

// Leave room for a generated lease, counters and other runtime metadata that
// may grow between accepting human input and starting a run. Admission uses
// the same projection as execution, including JSON escaping and discovery.
const contextAdmissionReserve = 1024

func ValidateContextCapacity(state State) error {
	check := func(step string) error {
		if _, err := ContextView(state, step, DefaultContextViewBytes-contextAdmissionReserve); err != nil {
			return Err(422, fmt.Sprintf("input_context_limit: mandatory input for %s exceeds the %d-byte context budget (including runtime reserve): %v; shorten the proposed input or step; existing requirements are not truncated", contextTarget(step), DefaultContextViewBytes, err))
		}
		return nil
	}
	if err := check(""); err != nil {
		return err
	}
	for _, step := range state.Steps {
		// A failed Step may be explicitly retried. Its immutable description and
		// goal ancestry must still fit after accepting another human requirement.
		if step.Status != "completed" && step.Status != "abandoned" {
			if err := check(step.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

func contextTarget(step string) string {
	if step == "" {
		return "Decide"
	}
	return "Step " + step
}

func (t *Tx) CheckContextCapacity(project string) error {
	state, err := t.State(project)
	if err != nil {
		return err
	}
	return ValidateContextCapacity(state)
}
