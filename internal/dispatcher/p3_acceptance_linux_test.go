//go:build linux

package dispatcher

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"xloom/internal/board"
)

const p3AcceptanceObservedAt = "2026-09-22T10:00:00Z"

func p3AcceptanceFact(id, description, scope, excerpt string) board.FactRecord {
	return board.FactRecord{ID: id, Description: description, Scope: scope, ObservedAt: p3AcceptanceObservedAt, Status: "valid", Legacy: false,
		Evidence: []board.EvidenceRef{{RunID: "fixture-observer", Path: "fixtures/" + id + ".json", Excerpt: excerpt}}}
}

// These pairs change observed evidence while preserving the user's request.
// Case names and oracle fields belong to the harness, never the task graph.
// The harness retains the supplied synthetic JSON before seeding these Facts.
func p3AcceptanceCases() []p3Case {
	witness := func(accepted bool, candidateID, responseID string) []board.FactRecord {
		response := `{"endpoint":"verifier.local","http_status":200,"submission_id":"S17","body":{"received":true}}`
		if accepted {
			response = `{"endpoint":"verifier.local","http_status":200,"submission_id":"S17","body":{"accepted":true,"challenge":"local-1","witness":"WITNESS-ALPHA"}}`
		}
		return []board.FactRecord{
			p3AcceptanceFact(candidateID, "The challenge artifact contained a witness value.", "local-1 challenge artifact", `{"challenge":"local-1","witness":"WITNESS-ALPHA","source":"challenge-artifact"}`),
			p3AcceptanceFact(responseID, "The verifier returned a response for submission S17.", "verifier.local submission S17", response),
		}
	}
	coverage := func(allResponses bool) []board.FactRecord {
		facts := []board.FactRecord{}
		for i, status := range []int{200, 401, 403} {
			path := []string{"/a", "/b", "/c"}[i]
			response := fmt.Sprintf(`{"host":"api.local","deployment":"prod","build":"build-42","tenant":"tenant-blue","identity":"anonymous","path":%q,"http_status":%d}`, path, status)
			if i == 2 && !allResponses {
				response = `{"host":"api.local","deployment":"prod","build":"build-42","tenant":"tenant-blue","identity":"anonymous","path":"/c","http_status":null,"response_received":false,"client_exit_code":0}`
			}
			facts = append(facts, p3AcceptanceFact(fmt.Sprintf("f%03d", i+1), "The fixture client retained its result for "+path+".", "api.local prod/build-42; tenant-blue; anonymous", response))
		}
		return facts
	}
	const witnessOrigin = "A local verifier accepts submissions for challenge local-1. A challenge artifact and the submission response have been retained."
	const witnessGoal = "Confirm that verifier.local explicitly accepted the exact witness WITNESS-ALPHA for challenge local-1. Retain the confirming response as the completion evidence."
	const coverageOrigin = "Inspect api.local in prod/build-42 as anonymous in tenant-blue. The requested paths are /a, /b and /c."
	const coverageGoal = "Obtain the actual HTTP response status for each of /a, /b and /c under the specified conditions, and account for all three statuses in the completion explanation."
	const mixedOrigin = "Inspect api.local in prod/build-42 as anonymous in tenant-blue at /a, /b and /c, and check the local-1 submission at verifier.local."
	const mixedGoal = "Obtain the actual HTTP response status for all three paths /a, /b and /c, and confirm that verifier.local explicitly accepted WITNESS-ALPHA for local-1. Completion must account for both the three response statuses and the confirming verifier response."
	cases := []p3Case{
		{Name: "acceptance/witness/receipt", Origin: witnessOrigin, Goal: witnessGoal, Presentation: "full", Facts: witness(false, "f001", "f002")},
		{Name: "acceptance/witness/confirmation", Origin: witnessOrigin, Goal: witnessGoal, Presentation: "full", Facts: witness(true, "f001", "f002"), WantComplete: true, RequiredIDs: []string{"f002"}},
		{Name: "acceptance/coverage/two_responses", Origin: coverageOrigin, Goal: coverageGoal, Presentation: "full", Facts: coverage(false)},
		{Name: "acceptance/coverage/three_responses", Origin: coverageOrigin, Goal: coverageGoal, Presentation: "full", Facts: coverage(true), WantComplete: true, RequiredIDs: []string{"f001", "f002", "f003"}},
		{Name: "acceptance/mixed/responses_and_receipt", Origin: mixedOrigin, Goal: mixedGoal, Presentation: "full", Facts: append(coverage(true), witness(false, "f004", "f005")...)},
		{Name: "acceptance/mixed/two_responses_and_confirmation", Origin: mixedOrigin, Goal: mixedGoal, Presentation: "full", Facts: append(coverage(false), witness(true, "f004", "f005")...)},
		{Name: "acceptance/mixed/three_responses_and_confirmation", Origin: mixedOrigin, Goal: mixedGoal, Presentation: "full", Facts: append(coverage(true), witness(true, "f004", "f005")...), WantComplete: true, RequiredIDs: []string{"f001", "f002", "f003", "f005"}},
	}
	const identityOrigin = "The target is GET /records/r7 on api.local in prod/build-42, tenant-blue, with no authenticated session."
	const identityGoal = "Verify that an anonymous request for /records/r7 is denied in tenant-blue on api.local prod/build-42. Completion must be supported by a response observed under those exact request conditions."
	for _, variant := range []struct {
		name, identity, deployment, build string
		status                            int
		complete                          bool
	}{
		{"authenticated_session", "admin", "prod", "build-42", 403, false},
		{"other_deployment", "anonymous", "staging", "build-41", 403, false},
		{"target_request", "anonymous", "prod", "build-42", 403, true},
	} {
		payload := fmt.Sprintf(`{"host":"api.local","deployment":%q,"build":%q,"tenant":"tenant-blue","identity":%q,"method":"GET","path":"/records/r7","http_status":%d}`, variant.deployment, variant.build, variant.identity, variant.status)
		entry := p3Case{Name: "acceptance/identity_environment/" + variant.name, Origin: identityOrigin, Goal: identityGoal, Presentation: "full", WantComplete: variant.complete,
			Facts: []board.FactRecord{p3AcceptanceFact("f001", "A GET request for /records/r7 returned the retained response.", "api.local "+variant.deployment+"/"+variant.build+"; tenant-blue; "+variant.identity, payload)}}
		if variant.complete {
			entry.RequiredIDs = []string{"f001"}
		}
		cases = append(cases, entry)
	}
	const businessOrigin = "A local ledger fixture has accounts A and B. Transfer T17 moves 10 units from A to B in ledger.local prod/build-42."
	const businessGoal = "Verify that transfer T17 is committed for 10 units from A to B on ledger.local prod/build-42. Confirm the corresponding account changes from A=100 and B=40 to A=90 and B=50 using the transaction and balance observations."
	for _, committed := range []bool{false, true} {
		name := "request_received"
		response := `{"host":"ledger.local","deployment":"prod","build":"build-42","http_status":202,"transaction":"T17","from":"A","to":"B","amount":10,"state":"pending"}`
		balances := `{"host":"ledger.local","deployment":"prod","build":"build-42","transaction":"T17","phase":"after_submission","balances":{"A":100,"B":40},"posted":false}`
		if committed {
			name = "ledger_posted"
			response = `{"host":"ledger.local","deployment":"prod","build":"build-42","http_status":200,"transaction":"T17","from":"A","to":"B","amount":10,"state":"committed"}`
			balances = `{"host":"ledger.local","deployment":"prod","build":"build-42","transaction":"T17","phase":"after_submission","balances":{"A":90,"B":50},"posted":true}`
		}
		entry := p3Case{Name: "acceptance/business/" + name, Origin: businessOrigin, Goal: businessGoal, Presentation: "full", WantComplete: committed,
			Facts: []board.FactRecord{
				p3AcceptanceFact("f001", "The ledger returned account balances before T17 was submitted.", "ledger.local prod/build-42; accounts A and B", `{"host":"ledger.local","deployment":"prod","build":"build-42","transaction":"T17","phase":"before_submission","balances":{"A":100,"B":40}}`),
				p3AcceptanceFact("f002", "The transfer endpoint returned a response for T17.", "ledger.local prod/build-42; transaction T17", response),
				p3AcceptanceFact("f003", "The ledger returned account balances after T17 was submitted.", "ledger.local prod/build-42; accounts A and B; transaction T17", balances),
			}}
		if committed {
			entry.RequiredIDs = []string{"f001", "f002", "f003"}
		}
		cases = append(cases, entry)
	}
	return cases
}

func TestP3AcceptanceFixturesKeepRequirementsAndEvidenceBoundaries(t *testing.T) {
	cases := p3AcceptanceCases()
	if len(cases) != 12 {
		t.Fatalf("got %d acceptance cases, want 12", len(cases))
	}
	counts := map[string]int{}
	requirements := map[string][2]string{}
	names := map[string]bool{}
	for _, entry := range cases {
		parts := strings.Split(entry.Name, "/")
		if len(parts) != 3 || names[entry.Name] {
			t.Fatalf("invalid or duplicate case name %q", entry.Name)
		}
		names[entry.Name] = true
		family := parts[1]
		counts[family]++
		request := [2]string{entry.Origin, entry.Goal}
		if prior, ok := requirements[family]; ok && prior != request {
			t.Errorf("%s changes the user requirement across evidence contrasts", family)
		}
		requirements[family] = request
		if entry.Presentation != "full" || entry.Origin == "" || entry.Goal == "" || entry.ExpectedAnswer != "" {
			t.Errorf("%s is not a complete-evidence semantic fixture", entry.Name)
		}
		ids := map[string]bool{}
		for _, fact := range entry.Facts {
			if ids[fact.ID] || len(fact.ID) != 4 || !strings.HasPrefix(fact.ID, "f00") || fact.Legacy || fact.Status != "valid" || fact.Scope == "" || fact.ObservedAt != p3AcceptanceObservedAt || len(fact.Evidence) != 1 {
				t.Errorf("%s has malformed observation metadata: %+v", entry.Name, fact)
			}
			ids[fact.ID] = true
			ref := fact.Evidence[0]
			if ref.RunID == "" || ref.Path != "fixtures/"+fact.ID+".json" || !json.Valid([]byte(ref.Excerpt)) {
				t.Errorf("%s has invalid retained JSON evidence", entry.Name)
			}
		}
		if entry.WantComplete != (len(entry.RequiredIDs) > 0) {
			t.Errorf("%s has no consistent completion oracle", entry.Name)
		}
		for _, id := range entry.RequiredIDs {
			if !ids[id] {
				t.Errorf("%s requires absent support %s", entry.Name, id)
			}
		}
		visible, _ := json.Marshal(struct {
			Origin, Goal string
			Facts        []board.FactRecord
		}{entry.Origin, entry.Goal, entry.Facts})
		for _, label := range []string{entry.Name, "WantComplete", "RequiredIDs", "ExpectedAnswer", "positive case", "negative case", "expected outcome"} {
			if strings.Contains(string(visible), label) {
				t.Errorf("%s leaks oracle label %q into task data", entry.Name, label)
			}
		}
	}
	for family, want := range map[string]int{"witness": 2, "coverage": 2, "mixed": 3, "identity_environment": 3, "business": 2} {
		if counts[family] != want {
			t.Errorf("%s has %d cases, want %d", family, counts[family], want)
		}
	}
	observed, err := time.Parse(time.RFC3339, p3AcceptanceObservedAt)
	if err != nil || !observed.Before(time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("observation timestamp is not a fixed past RFC3339 date")
	}
}

func TestP3AcceptanceFixtureEvidenceAgreesWithCompletionOracle(t *testing.T) {
	for _, entry := range p3AcceptanceCases() {
		t.Run(entry.Name, func(t *testing.T) {
			data := map[string]map[string]any{}
			for _, fact := range entry.Facts {
				var value map[string]any
				if err := json.Unmarshal([]byte(fact.Evidence[0].Excerpt), &value); err != nil {
					t.Fatal(err)
				}
				data[fact.ID] = value
			}
			witness := func(id string) bool {
				body, _ := data[id]["body"].(map[string]any)
				return body["accepted"] == true && body["challenge"] == "local-1" && body["witness"] == "WITNESS-ALPHA"
			}
			coverage := func() bool {
				for i, path := range []string{"/a", "/b", "/c"} {
					fact := data[fmt.Sprintf("f%03d", i+1)]
					status, present := fact["http_status"].(float64)
					if !present || status < 100 || status > 599 || fact["path"] != path || fact["build"] != "build-42" || fact["identity"] != "anonymous" || fact["tenant"] != "tenant-blue" {
						return false
					}
				}
				return true
			}
			complete := false
			switch strings.Split(entry.Name, "/")[1] {
			case "witness":
				complete = witness("f002")
			case "coverage":
				complete = coverage()
			case "mixed":
				complete = coverage() && witness("f005")
			case "identity_environment":
				fact := data["f001"]
				if fact["http_status"] != float64(403) {
					t.Fatal("identity and environment contrasts must preserve the observed denial")
				}
				complete = fact["identity"] == "anonymous" && fact["tenant"] == "tenant-blue" && fact["deployment"] == "prod" && fact["build"] == "build-42" && fact["path"] == "/records/r7" && fact["http_status"] == float64(403)
			case "business":
				before, _ := data["f001"]["balances"].(map[string]any)
				after, _ := data["f003"]["balances"].(map[string]any)
				transaction := data["f002"]
				complete = before["A"] == float64(100) && before["B"] == float64(40) && after["A"] == float64(90) && after["B"] == float64(50) && data["f003"]["posted"] == true && transaction["transaction"] == "T17" && transaction["state"] == "committed" && transaction["amount"] == float64(10) && transaction["from"] == "A" && transaction["to"] == "B"
			default:
				t.Fatal("unknown acceptance family")
			}
			if complete != entry.WantComplete {
				t.Fatalf("retained observations imply completion=%t, oracle=%t", complete, entry.WantComplete)
			}
		})
	}
}
