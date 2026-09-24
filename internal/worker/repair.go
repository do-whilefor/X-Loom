package worker

import (
	"encoding/json"
	"errors"
	"fmt"

	"xloom/internal/agent"
	"xloom/internal/contract"
)

const maxOutputRepairs = 2
const maxContinuations = 3

type outputFailure struct {
	Reason string
	Detail string
}

func (e *outputFailure) Error() string { return e.Reason + ": " + e.Detail }

func truncated(m agent.Message) bool { return m.StopReason == "max_tokens" || m.StopReason == "length" }
func hasToolCalls(m agent.Message) bool {
	for _, b := range m.Content {
		if b.Type == "tool_use" {
			return true
		}
	}
	return false
}
func lastAssistant(history []agent.Message) (agent.Message, bool) {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "assistant" {
			return history[i], true
		}
	}
	return agent.Message{}, false
}

func awaitingInstructionResponse(history []agent.Message) bool {
	if len(history) == 0 {
		return false
	}
	last := history[len(history)-1]
	if last.Role != "user" {
		return false
	}
	for _, b := range last.Content {
		if b.Type != "tool_result" {
			return true
		}
	}
	return len(last.Content) == 0
}

// A parseable prefix is never a final result when its model response was cut
// off. The original stop reason remains in the persisted assistant message.
func outputProblem(j Job, concluding bool, m agent.Message) *outputFailure {
	if truncated(m) {
		return &outputFailure{Reason: "output_truncated", Detail: "the model hit its output token limit; even a parseable JSON prefix is not a completed result"}
	}
	if hasToolCalls(m) {
		return &outputFailure{Reason: "unexpected_tool_call", Detail: "a final result is required; tool calls cannot supply a result during pure-output repair"}
	}
	if _, err := parseOutput(j, concluding, m.Text()); err != nil {
		return &outputFailure{Reason: "invalid_contract", Detail: err.Error()}
	}
	return nil
}

func parseOutput(j Job, concluding bool, text string) (contract.Result, error) {
	return contract.ParseWithPolicy(text, j.Kind, concluding, j.openCount(), j.Budget.MaxIntents, contract.Policy{Version: j.ResultContractVersion, GraphRPC: j.GraphRPC})
}

// A terminal result can be persisted or replayed only after applying the same
// immutable job contract used at the live model boundary.
func checkedResult(j Job, r Result) Result {
	if r.Status != "success" {
		return r
	}
	parsed, err := parseOutput(j, r.Conclude, r.Text)
	if err != nil {
		r.Status, r.FailureKind, r.Error = "failed", "result_contract", err.Error()
	} else if parsed.Outcome == "incomplete" {
		r.Status, r.FailureKind, r.Error = "failed", "incomplete", parsed.Reason
	} else if parsed.Outcome == "continue" {
		r.Status, r.FailureKind, r.Error = "failed", "result_contract", "continue is not a terminal result"
	}
	r.Retryable = false
	return r
}

func repairInstruction(j Job, concluding bool, attempt int, problem *outputFailure) (string, error) {
	if problem == nil {
		return "", errors.New("result repair requires a failure reason")
	}
	reason, _ := json.Marshal(problem.Error())
	fallback := "If no supported factual result is available, return {\"accepted\":false,\"reason\":\"...\"}."
	if j.ResultContractVersion >= 1 && j.Kind != "reason" {
		fallback = "An unfinished accepted task must report incomplete with remaining work and its blocker, not completed or rejected merely to satisfy JSON formatting."
		if !concluding {
			fallback += " If further execution can finish the task, report continue; the runtime will restore tools in this same run under the original deadline."
		}
	}
	return fmt.Sprintf("Result-format repair %d/%d in the same session. All tools are disabled. The previous response cannot be submitted: %s. Produce one complete JSON object using the original task contract and the current phase restrictions. Replace the invalid response; do not append a suffix or include the earlier JSON. Use only existing evidence. Do not invent facts, force accepted:true, or declare completion without its required proof. %s A truncated response must be rewritten more briefly, not trusted as a complete answer.\n", attempt, maxOutputRepairs, reason, fallback), nil
}
