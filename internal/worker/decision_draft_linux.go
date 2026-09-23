package worker

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

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
	request      func(context.Context, GraphRequest) (string, error)
}

func (d *decisionDraft) invalidate() {
	d.keys, d.actions, d.version = nil, nil, ""
	d.reread, d.overviewRead = true, false
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
	switch a.Op {
	case "reset":
		d.keys, d.actions, d.version = nil, nil, ""
		return `{"draft":true,"reset":true}`, nil
	case "preview", "commit":
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
			if strings.Contains(err.Error(), "state_changed") {
				d.uncertain = false
				d.invalidate()
			}
			return "", err
		}
		var receipt board.DecisionReceipt
		if err = json.Unmarshal([]byte(raw), &receipt); err != nil {
			return "", err
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
	if len(d.actions) >= 64 {
		return "", errors.New("at most 64 draft actions are allowed")
	}
	if d.version == "" {
		d.version = currentVersion
	}
	d.keys, d.actions = append(d.keys, a.IdempotencyKey), append(d.actions, item)
	return draftReply(item), nil
}

func draftReply(item board.DecisionAction) string {
	reply := map[string]any{"draft": true, "op": item.Op}
	if item.Ref != "" {
		reply["id"] = "$" + item.Ref
	}
	raw, _ := json.Marshal(reply)
	return string(raw)
}
