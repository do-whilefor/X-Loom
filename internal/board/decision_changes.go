package board

type StateChange struct {
	Revision int64  `json:"revision"`
	Op       string `json:"op"`
	ID       string `json:"id"`
}

// StateChanges is the bounded discovery index for a new clean Decide. Event
// bodies and past execution inputs are unnecessary: current FGS records supply
// the knowledge, including corrections and the status of abandoned plans.
func (t *Tx) StateChanges(project string, after, through int64) ([]StateChange, error) {
	if _, err := t.Load(project); err != nil {
		return nil, err
	}
	if after < 0 || through < after {
		return nil, Err(422, "invalid change revision interval")
	}
	rows, err := t.Query(`SELECT revision,json_extract(event,'$.op'),json_extract(event,'$.id') FROM xloom_state_events WHERE project_id=? AND revision>? AND revision<=? ORDER BY revision LIMIT 1000`, project, after, through)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StateChange{}
	for rows.Next() {
		var change StateChange
		if err := rows.Scan(&change.Revision, &change.Op, &change.ID); err != nil {
			return nil, err
		}
		out = append(out, change)
	}
	return out, rows.Err()
}
