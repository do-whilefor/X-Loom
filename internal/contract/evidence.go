package contract

import (
	"encoding/json"
	"errors"
)

// Version two binds a completed exploration to one evidence-backed result.
// The runtime freezes file selections; the board verifies their provenance.
func parseEvidenceResult(raw json.RawMessage, kind string, conclude bool) (Result, error) {
	data, err := object(raw)
	if err != nil {
		return Result{}, errors.New("completed data must be an object")
	}
	r := Result{Kind: "fact", Outcome: "completed"}
	_, byID := data["fact_id"]
	_, byFact := data["fact"]
	if byID == byFact {
		return Result{}, errors.New("completed requires exactly one of fact_id or fact")
	}
	want := 1
	if kind == "bootstrap" {
		want++
		complete, err := object(data["complete"])
		if err != nil || len(complete) != 1 {
			return Result{}, errors.New("bootstrap completed requires complete.description")
		}
		why, err := text(complete["description"])
		if err != nil {
			return Result{}, errors.New("bootstrap completion reason must be nonempty")
		}
		if !conclude {
			r.Kind, r.Complete.Description = "complete", why
		}
	}
	if len(data) != want {
		return Result{}, errors.New("unexpected completed result field")
	}
	if byID {
		r.FactID, err = text(data["fact_id"])
		if err != nil || len(r.FactID) > 256 {
			return Result{}, errors.New("fact_id must be a nonempty fact identifier")
		}
		return r, nil
	}
	fact, err := object(data["fact"])
	if err != nil || len(fact) != 4 {
		return Result{}, errors.New("fact requires only description, scope, observed_at and evidence")
	}
	for _, key := range []string{"description", "scope", "observed_at"} {
		if _, err := text(fact[key]); err != nil {
			return Result{}, errors.New("fact requires nonempty " + key)
		}
	}
	var refs []map[string]json.RawMessage
	if json.Unmarshal(fact["evidence"], &refs) != nil || len(refs) == 0 || len(refs) > 32 {
		return Result{}, errors.New("fact evidence requires 1-32 file selections")
	}
	for _, ref := range refs {
		if ref == nil {
			return Result{}, errors.New("evidence selection must be an object")
		}
		if _, err := text(ref["path"]); err != nil {
			return Result{}, errors.New("evidence selection requires a path")
		}
		for key, value := range ref {
			if string(value) == "null" {
				return Result{}, errors.New("evidence selection fields cannot be null")
			}
			switch key {
			case "path", "run_id", "excerpt":
				var valueText string
				if json.Unmarshal(value, &valueText) != nil {
					return Result{}, errors.New("evidence text fields must be strings")
				}
			case "start_line", "end_line":
				var line int
				if json.Unmarshal(value, &line) != nil || line < 0 {
					return Result{}, errors.New("evidence line bounds must be nonnegative integers")
				}
			default:
				return Result{}, errors.New("unexpected evidence selection field")
			}
		}
	}
	r.FactPayload = append(json.RawMessage(nil), data["fact"]...)
	return r, nil
}
