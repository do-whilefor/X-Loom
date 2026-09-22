package contract

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ReplanJudgment reports whether the supplied changes warrant planning. Basis
// contains graph IDs; Missing describes evidence needed to make the judgment.
// The caller verifies the referenced graph state and its revision.
type ReplanJudgment struct {
	Decision string   `json:"decision"`
	Basis    []string `json:"basis"`
	Missing  []string `json:"missing"`
}

// ErrJudgmentRejected distinguishes an explicit refusal from malformed output
// and from a valid judgment of unknown.
var ErrJudgmentRejected = errors.New("replan judgment rejected")

// ParseReplanJudgment accepts exactly one JSON object. Unlike the legacy task
// contract, this narrow protocol does not extract JSON from surrounding prose.
func ParseReplanJudgment(output string) (ReplanJudgment, error) {
	m, err := replanObject(output)
	if err != nil {
		return ReplanJudgment{}, err
	}
	if raw, ok := m["accepted"]; ok {
		var accepted *bool
		var reason string
		if len(m) != 2 || json.Unmarshal(raw, &accepted) != nil || accepted == nil || *accepted || json.Unmarshal(m["reason"], &reason) != nil || strings.TrimSpace(reason) == "" {
			return ReplanJudgment{}, errors.New("rejection requires only accepted:false and a nonempty reason")
		}
		return ReplanJudgment{}, fmt.Errorf("%w: %s", ErrJudgmentRejected, strings.TrimSpace(reason))
	}
	if len(m) != 3 || m["decision"] == nil || m["basis"] == nil || m["missing"] == nil {
		return ReplanJudgment{}, errors.New("judgment requires only decision, basis and missing")
	}
	var judgment ReplanJudgment
	if err := json.Unmarshal(m["decision"], &judgment.Decision); err != nil {
		return ReplanJudgment{}, errors.New("decision must be replan, keep or unknown")
	}
	if judgment.Decision != "replan" && judgment.Decision != "keep" && judgment.Decision != "unknown" {
		return ReplanJudgment{}, errors.New("decision must be replan, keep or unknown")
	}
	if judgment.Basis, err = replanStrings(m["basis"], "basis"); err != nil {
		return ReplanJudgment{}, err
	}
	if judgment.Missing, err = replanStrings(m["missing"], "missing"); err != nil {
		return ReplanJudgment{}, err
	}
	if judgment.Decision == "unknown" {
		if len(judgment.Missing) == 0 {
			return ReplanJudgment{}, errors.New("unknown requires missing evidence")
		}
	} else if len(judgment.Basis) == 0 || len(judgment.Missing) != 0 {
		return ReplanJudgment{}, errors.New("replan and keep require basis and no missing evidence")
	}
	return judgment, nil
}

func replanStrings(raw json.RawMessage, field string) ([]string, error) {
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
		return nil, fmt.Errorf("%s must be an array of nonempty strings", field)
	}
	seen := make(map[string]bool, len(values))
	for i, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			return nil, fmt.Errorf("%s must contain nonempty, unique strings", field)
		}
		seen[value] = true
		values[i] = value
	}
	return values, nil
}

func replanObject(output string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(strings.NewReader(output))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return nil, errors.New("judgment must be a JSON object")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, errors.New("judgment field must be a string")
		}
		if _, exists := fields[key]; exists {
			return nil, fmt.Errorf("duplicate judgment field %q", key)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("judgment must contain only one JSON object")
	}
	return fields, nil
}
