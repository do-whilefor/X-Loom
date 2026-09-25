package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"xloom/internal/agent"
	"xloom/internal/board"
)

const committedDecisionText = `{"accepted":true,"data":{"decided":true}}`

// The draft is intentionally process-local. Its only durable authority is the
// server receipt; transcripts do not re-authorize a lost or stale draft.
type decisionDraft struct {
	version      string
	keys         []string
	actions      []board.DecisionAction
	committed    bool
	uncertain    bool
	reread       bool
	overviewRead bool
	reviewData   string
	reviewReady  bool
	request      func(context.Context, GraphRequest) (string, error)
}

func (d *decisionDraft) invalidate() {
	d.keys, d.actions, d.version = nil, nil, ""
	d.reviewData, d.reviewReady = "", false
	d.reread, d.overviewRead = true, false
}

// A tool result is not yet a model observation. Keep the authoritative review
// through compaction and allow completion only from a subsequent request.
// The private draft and review are deliberately discarded together on resume.
func (d *decisionDraft) beforeRequest(loop *agent.Loop) {
	loop.ContextData = nil
	if d.reviewData != "" {
		loop.ContextData = []string{d.reviewData}
		d.reviewReady = true
	}
}

func (d *decisionDraft) completes() bool {
	return len(d.actions) > 0 && d.actions[len(d.actions)-1].Op == "complete"
}

func (d *decisionDraft) observeRead(section string) {
	if !d.reread {
		return
	}
	if section == "" || section == "overview" {
		d.overviewRead = true
	} else if d.overviewRead {
		d.reread = false
	}
}

func batchDecision(j Job) bool {
	return j.Kind == "reason" && j.Decision != nil && j.Decision.Version == 2
}

func (d *decisionDraft) result() (string, bool) { return committedDecisionText, d.committed }

func (d *decisionDraft) recover(ctx context.Context) (string, error) {
	raw, err := d.request(ctx, GraphRequest{Op: "decision_receipt"})
	if err != nil {
		return "", err
	}
	var receipt board.DecisionReceipt
	if err = json.Unmarshal([]byte(raw), &receipt); err != nil {
		return "", err
	}
	if receipt.Committed {
		d.committed = true
	}
	d.uncertain = false
	return raw, nil
}

var draftRef = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)

func (d *decisionDraft) action(ctx context.Context, a board.StateAction, currentVersion string) (string, error) {
	if d.committed {
		return "", errors.New("decision already committed; no further actions are allowed")
	}
	if d.uncertain {
		if receipt, err := d.recover(ctx); err != nil {
			return "", err
		} else if d.committed {
			return receipt, nil
		}
	}
	if d.reread && a.Op != "reset" {
		return "", errors.New("read the current overview and affected graph section after state_changed or recovery before rebuilding the draft")
	}
	if a.Op == "reset" || a.Op == "preview" || a.Op == "commit" {
		var payload map[string]json.RawMessage
		if len(a.Payload) != 0 && (json.Unmarshal(a.Payload, &payload) != nil || payload == nil || len(payload) != 0) {
			return "", errors.New("preview/commit/reset payload must be omitted or {}; draft unchanged")
		}
	}
	switch a.Op {
	case "reset":
		d.keys, d.actions, d.version = nil, nil, ""
		d.reviewData, d.reviewReady = "", false
		return `{"draft":true,"reset":true}`, nil
	case "preview", "commit":
		if a.Op == "commit" && d.completes() && !d.reviewReady {
			return "", errors.New("completion requires preview and a subsequent model request to review its evidence before commit; draft unchanged")
		}
		if a.Op == "preview" {
			d.reviewData, d.reviewReady = "", false
		}
		if d.version == "" {
			d.version = currentVersion
		}
		batch := board.DecisionBatch{ExpectedVersion: d.version, Actions: append([]board.DecisionAction{}, d.actions...)}
		if a.Op == "commit" {
			d.uncertain = true
		}
		raw, err := d.request(ctx, GraphRequest{Op: "decision_" + a.Op, Batch: &batch})
		if err != nil {
			// A definitive version conflict did not apply any writes. Force the
			// model to read new information and rebuild, not just change a hash.
			if graphStateConflict(err) {
				d.uncertain = false
				d.invalidate()
			}
			return "", err
		}
		var receipt board.DecisionReceipt
		if err = json.Unmarshal([]byte(raw), &receipt); err != nil {
			return "", err
		}
		if a.Op == "preview" && d.completes() {
			if receipt.CompletionReview == nil || receipt.ValidationScope != "protocol_only" || receipt.CompletionReview.StateVersion != d.version || receipt.CompletionReview.Acceptance != "not_checked" {
				return "", errors.New("completion preview omitted its evidence review; completion remains unreviewed")
			}
			review, err := json.Marshal(receipt.CompletionReview)
			if err != nil {
				return "", err
			}
			d.reviewData = "<completion_review>\n" + string(review) + "\n</completion_review>"
		}
		if a.Op == "commit" {
			if !receipt.Committed {
				return "", errors.New("commit returned no committed receipt")
			}
			d.committed, d.uncertain = true, false
		}
		return raw, nil
	case "goal", "step", "fact_relation", "complete":
	default:
		return "", errors.New("unknown draft operation")
	}
	if !draftRef.MatchString(a.IdempotencyKey) {
		return "", errors.New("draft key must start with a letter and use at most 64 letters, digits, underscore or hyphen")
	}
	var payload map[string]any
	if json.Unmarshal(a.Payload, &payload) != nil || payload == nil {
		return "", errors.New("draft payload must be an object")
	}
	if a.Op == "goal" && payload["id"] == "goal" {
		return "", errors.New("root goal cannot be changed by goal actions; use complete with supporting facts and proof; draft unchanged")
	}
	if err := validateDraftFields(a.Op, payload); err != nil {
		return "", err
	}
	if a.Op == "complete" {
		var completion struct {
			From        []string `json:"from"`
			Description string   `json:"description"`
		}
		decoder := json.NewDecoder(bytes.NewReader(a.Payload))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&completion); err != nil || len(completion.From) == 0 || strings.TrimSpace(completion.Description) == "" {
			return "", errors.New("complete payload requires only from:[fact IDs] and description:proof; draft unchanged")
		}
	}
	if a.Op == "step" && payload["action"] == "add" {
		from, _ := payload["from"].([]any)
		for _, source := range from {
			if source == "goal" {
				return "", errors.New("step from cannot include root goal; use goal_id to bind the target; from accepts evidence (origin is allowed); draft unchanged")
			}
		}
	}
	canonical, _ := json.Marshal(payload)
	item := board.DecisionAction{Op: a.Op, Payload: canonical}
	if (a.Op == "goal" || a.Op == "step") && payload["action"] == "add" {
		item.Ref = a.IdempotencyKey
	}
	for n, key := range d.keys {
		if key == a.IdempotencyKey {
			old, _ := json.Marshal(d.actions[n])
			next, _ := json.Marshal(item)
			if string(old) != string(next) {
				return "", errors.New("draft key already identifies another action; reset before replacing a plan")
			}
			return draftReply(item), nil
		}
	}
	if d.completes() {
		return "", errors.New("complete must be the last action; reset before changing the proposed completion; draft unchanged")
	}
	if len(d.actions) >= 64 {
		return "", errors.New("at most 64 draft actions are allowed")
	}
	if d.version == "" {
		d.version = currentVersion
	}
	d.keys, d.actions = append(d.keys, a.IdempotencyKey), append(d.actions, item)
	d.reviewData, d.reviewReady = "", false
	return draftReply(item), nil
}

// Reject missing action fields before a bad draft reserves its key. The board
// remains authoritative for references, state transitions and evidence.
func validateDraftFields(op string, payload map[string]any) error {
	text := func(key string) bool {
		value, ok := payload[key].(string)
		return ok && strings.TrimSpace(value) != ""
	}
	ids := func(key string) bool {
		values, ok := payload[key].([]any)
		if !ok || len(values) == 0 {
			return false
		}
		for _, value := range values {
			id, ok := value.(string)
			if !ok || strings.TrimSpace(id) == "" {
				return false
			}
		}
		return true
	}
	valid := true
	switch op {
	case "goal":
		switch payload["action"] {
		case "add":
			valid = text("condition")
		case "achieve":
			valid = text("id") && text("reason") && ids("sources")
		case "withdraw":
			valid = text("id") && text("reason")
		default:
			valid = false
		}
	case "step":
		if value, exists := payload["priority"]; exists {
			priority, ok := value.(float64)
			if !ok || priority < 0 || priority > 1000000 || priority != float64(int(priority)) {
				return errors.New("step priority must be an integer between 0 and 1000000; draft unchanged")
			}
		}
		switch payload["action"] {
		case "add":
			valid = ids("from") && text("description")
		case "priority", "abandon":
			valid = text("id") && text("reason")
		default:
			valid = false
		}
	case "fact_relation":
		kind, _ := payload["kind"].(string)
		valid = (kind == "supersedes" || kind == "refutes" || kind == "narrows") && text("source") && text("target") && text("reason")
	}
	if !valid {
		return errors.New(op + " payload is missing required action fields; see the tool contract; draft unchanged")
	}
	return nil
}

func draftReply(item board.DecisionAction) string {
	reply := map[string]any{"draft": true, "op": item.Op}
	if item.Ref != "" {
		reply["id"] = "$" + item.Ref
	}
	raw, _ := json.Marshal(reply)
	return string(raw)
}
