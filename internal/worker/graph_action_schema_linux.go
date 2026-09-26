package worker

// Describe every input field available in this mode, including the transition
// discriminator. Conditional requirements and operation semantics remain with
// the draft/server validators; this is not a second action validator.
func graphActionPayloadSchema(kind string) map[string]any {
	text := map[string]any{"type": "string"}
	ids := map[string]any{"type": "array", "items": text}
	properties := map[string]any{
		"description": text,
		"reason":      text,
		"sources":     ids,
	}
	if kind == "reason" {
		properties["action"] = map[string]any{
			"type": "string", "enum": []string{"add", "achieve", "withdraw", "abandon", "priority"},
			"description": "Required for goal (add/achieve/withdraw) and step (add/abandon/priority).",
		}
		for _, name := range []string{"id", "condition", "parent_id", "goal_id", "source", "target"} {
			properties[name] = text
		}
		properties["from"] = ids
		properties["priority"] = map[string]any{"type": "integer", "minimum": 0, "maximum": 1000000}
		properties["final_report"] = map[string]any{"type": "boolean", "description": "Required true for final report Steps. All non-report Steps in this goal subtree and explicit source producers must be completed or abandoned; valid scoped facts are added to from automatically."}
		properties["kind"] = map[string]any{"type": "string", "enum": []string{"supersedes", "refutes", "narrows"}}
	} else {
		properties["scope"] = text
		properties["observed_at"] = map[string]any{"type": "string", "format": "date-time"}
		properties["claim"] = text
		properties["status"] = map[string]any{"type": "string", "enum": []string{"candidate", "verified", "refuted"}}
		properties["replace_support"] = map[string]any{"type": "boolean"}
		properties["evidence"] = map[string]any{"type": "array", "items": map[string]any{
			"type": "object", "properties": map[string]any{
				"path": text,
				// Historical references remain valid for Findings backed by sources.
				"run_id":     text,
				"excerpt":    text,
				"start_line": map[string]any{"type": "integer"},
				"end_line":   map[string]any{"type": "integer"},
			},
		}}
	}
	return map[string]any{"type": "object", "properties": properties}
}
