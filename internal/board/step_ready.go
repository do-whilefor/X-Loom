package board

// StepReady is checked at claim, registration and process start, each inside
// the write transaction. Already running work can retain independent evidence;
// invalidating its premise does not silently cancel it or rewrite its inputs.
func (t *Tx) StepReady(project, id string) error {
	s, err := t.State(project)
	if err != nil {
		return err
	}
	return stepReady(s, id)
}

func stepReady(s State, id string) error {
	for _, step := range s.Steps {
		if step.ID == id {
			if step.Status == "abandoned" {
				return Err(409, "Step was abandoned")
			}
			if step.FinalReport {
				if err := reportReady(s, step); err != nil {
					return err
				}
			}
			return s.ValidateFactSources(step.From, false)
		}
	}
	return Err(404, "Step not found")
}

func (t *Tx) executionStepReady(e Execution) error {
	state, err := t.State(e.ProjectID)
	if err != nil {
		return err
	}
	if err = stepReady(state, e.Intent); err != nil {
		return err
	}
	return t.checkReportInput(state, e.Intent, e.Job)
}
