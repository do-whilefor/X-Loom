package worker

import (
	"encoding/hex"
	"encoding/json"
	"errors"

	"xloom/internal/board"
)

func validateSnapshotInput(j Job) error {
	ref := j.InputSnapshot
	validDigest := func(value string) bool {
		decoded, err := hex.DecodeString(value)
		return err == nil && len(decoded) == 32 && hex.EncodeToString(decoded) == value
	}
	if ref == nil || ref.Version != 1 || !validDigest(ref.ID) || !validDigest(ref.StateVersion) ||
		ref.ProjectID != j.Graph.Project.ID || ref.Generation != j.Graph.Project.Generation || ref.Generation < 0 || ref.Revision < 0 || ref.DecisionRevision < 0 ||
		ref.FactCount < 0 || ref.HintCount < 0 || ref.OpenCount < 0 || ref.StepCount < 0 || j.State != nil ||
		len(j.Graph.Facts)+len(j.Graph.Intents)+len(j.Graph.Hints) != 0 {
		return errors.New("invalid immutable input snapshot binding")
	}
	view := j.InputView
	if j.Kind == "reason" {
		if j.Intent != nil || j.Decision == nil || (j.Decision.Version != 1 && j.Decision.Version != 2) || j.Decision.StateVersion != ref.StateVersion ||
			j.Decision.Generation != ref.Generation || j.Decision.ToRevision != ref.Revision || len(j.InputView) != 0 {
			return errors.New("invalid decision snapshot binding")
		}
		view = j.Decision.View
	} else if j.Decision != nil {
		return errors.New("Execute cannot carry a Decide input")
	}
	if len(view) > board.DefaultContextViewBytes || !json.Valid(view) {
		return errors.New("invalid bounded snapshot view")
	}
	return nil
}
