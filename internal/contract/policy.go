package contract

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Policy comes from the immutable registered job, never the model response.
// Version zero preserves the execution result format of existing jobs.
type Policy struct {
	Version  int
	GraphRPC bool
}

func ParseWithPolicy(output, kind string, conclude bool, openIntents, maxIntents int, policy Policy) (Result, error) {
	if policy.Version < 0 || policy.Version > 2 {
		return Result{}, fmt.Errorf("unsupported result contract version %d", policy.Version)
	}
	m, err := Extract(output)
	if err != nil {
		return Result{}, err
	}
	if policy.Version == 0 {
		if _, exists := m["outcome"]; exists {
			return Result{}, errors.New("outcome requires result contract version 1; legacy jobs cannot mix result protocols")
		}
	}
	if policy.Version >= 1 && (kind == "explore" || kind == "bootstrap") {
		var accepted bool
		if raw := m["accepted"]; len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &accepted) != nil {
			return Result{}, errors.New("accepted must be true or false")
		}
		if !accepted {
			reason, err := text(m["reason"])
			if err != nil || len(m) != 2 {
				return Result{}, errors.New("rejection requires only accepted:false and a nonempty reason")
			}
			return Result{Kind: "rejected", Reason: reason}, nil
		}
		outcome, err := text(m["outcome"])
		if err != nil {
			return Result{}, errors.New("outcome must be completed, continue or incomplete")
		}
		switch outcome {
		case "continue", "incomplete":
			reason, err := text(m["reason"])
			if err != nil || len(m) != 3 {
				return Result{}, errors.New("continue/incomplete requires only accepted, outcome and a nonempty reason")
			}
			if outcome == "continue" && conclude {
				return Result{}, errors.New("continue is unavailable during conclusion; report completed or incomplete")
			}
			return Result{Kind: outcome, Outcome: outcome, Reason: reason}, nil
		case "completed":
			if len(m) != 3 || m["data"] == nil {
				return Result{}, errors.New("completed requires only accepted, outcome and data")
			}
			if policy.Version == 2 {
				return parseEvidenceResult(m["data"], kind, conclude)
			}
			if kind == "bootstrap" && conclude {
				// A budget boundary changes which effects are allowed, not the
				// evidence required to claim that the assigned work is complete.
				if _, err := parseObject(m, kind, false, openIntents, maxIntents); err != nil {
					return Result{}, err
				}
			}
			r, err := parseObject(m, kind, conclude, openIntents, maxIntents)
			r.Outcome = outcome
			return r, err
		default:
			return Result{}, fmt.Errorf("unknown execution outcome %q", outcome)
		}
	}
	r, err := parseObject(m, kind, conclude, openIntents, maxIntents)
	if err == nil && kind == "reason" && policy.GraphRPC && len(r.Intents) > 0 {
		return Result{}, errors.New("live graph jobs must create steps through graph_action; final intent/intents would submit a second plan; return decided after committed decisions")
	}
	return r, err
}
