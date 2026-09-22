//go:build linux

package worker

import (
	"context"
	"encoding/json"

	"xloom/internal/agent"
	"xloom/internal/cvss"
)

// Scoring is pure computation; it neither verifies a finding nor writes to
// the graph. It is available only to pentest jobs, including read-only Decide.
func cvssTool() agent.Tool {
	definition := agent.Definition{
		Name:        "cvss31",
		Description: "Calculate a CVSS 3.1 Base vector with all eight metrics AV/AC/PR/UI/S/C/I/A. Returns the normalized vector, baseScore, severity and computation factors. Use evidence-backed metrics for verified vulnerabilities only; this calculation does not verify a vulnerability or its impact. No network, files or graph changes.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"vector":{"type":"string","minLength":1}},"required":["vector"],"additionalProperties":false}`),
	}
	return agent.Tool{Definition: definition, Execute: func(ctx context.Context, raw json.RawMessage) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if err := agent.ValidateArguments(definition.Schema, raw); err != nil {
			return "", err
		}
		var input struct {
			Vector string `json:"vector"`
		}
		if err := json.Unmarshal(raw, &input); err != nil {
			return "", err
		}
		result, err := cvss.Calculate(input.Vector)
		if err != nil {
			return "", err
		}
		encoded, err := json.Marshal(result)
		return string(encoded), err
	}}
}
