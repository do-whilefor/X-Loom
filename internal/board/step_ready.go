package board

// StepReady is checked at claim, registration and process start, each inside
// the write transaction. Already running work can retain independent evidence;
// invalidating its premise does not silently cancel it or rewrite its inputs.
func (t *Tx) StepReady(project, id string) error {
	if err := t.StepAvailable(project, id); err != nil {
		return err
	}
	s, err := t.State(project)
	if err != nil {
		return err
	}
	for _, step := range s.Steps {
		if step.ID == id {
			return s.ValidateFactSources(step.From, false)
		}
	}
	return Err(404, "Step not found")
}
