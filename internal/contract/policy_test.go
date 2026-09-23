package contract

import (
	"fmt"
	"reflect"
	"testing"
)

func TestExecutionPolicyOutcomes(t *testing.T) {
	tests := []struct {
		name, kind, output, wantKind, wantOutcome, wantFact string
		conclude                                            bool
		wantErr                                             bool
	}{
		{
			name: "explore completed", kind: "explore",
			output:   `{"accepted":true,"outcome":"completed","data":{"description":"Verified local result"}}`,
			wantKind: "fact", wantOutcome: "completed", wantFact: "Verified local result",
		},
		{
			name: "explore completed during conclusion", kind: "explore", conclude: true,
			output:   `{"accepted":true,"outcome":"completed","data":{"description":"Previously verified result"}}`,
			wantKind: "fact", wantOutcome: "completed", wantFact: "Previously verified result",
		},
		{
			name: "bootstrap completed", kind: "bootstrap",
			output:   `{"accepted":true,"outcome":"completed","data":{"fact":{"description":"Verified evidence"},"complete":{"description":"Goal met"}}}`,
			wantKind: "complete", wantOutcome: "completed", wantFact: "Verified evidence",
		},
		{
			name: "bootstrap conclusion records completed evidence without completing project", kind: "bootstrap", conclude: true,
			output:   `{"accepted":true,"outcome":"completed","data":{"fact":{"description":"Verified evidence"},"complete":{"description":"Goal met"}}}`,
			wantKind: "fact", wantOutcome: "completed", wantFact: "Verified evidence",
		},
		{
			name: "bootstrap conclusion partial evidence is not completion", kind: "bootstrap", conclude: true,
			output:  `{"accepted":true,"outcome":"completed","data":{"fact":{"description":"Verified partial evidence"}}}`,
			wantErr: true,
		},
		{
			name: "bootstrap conclusion requires nonempty completion proof", kind: "bootstrap", conclude: true,
			output:  `{"accepted":true,"outcome":"completed","data":{"fact":{"description":"Verified partial evidence"},"complete":{"description":" "}}}`,
			wantErr: true,
		},
		{
			name: "bootstrap execute still requires completion data", kind: "bootstrap",
			output:  `{"accepted":true,"outcome":"completed","data":{"fact":{"description":"Evidence alone"}}}`,
			wantErr: true,
		},
		{
			name: "bootstrap conclusion rejects extra data", kind: "bootstrap", conclude: true,
			output:  `{"accepted":true,"outcome":"completed","data":{"fact":{"description":"Evidence"},"intents":[]}}`,
			wantErr: true,
		},
		{
			name: "completed still requires nonempty description", kind: "explore",
			output: `{"accepted":true,"outcome":"completed","data":{"description":"   "}}`, wantErr: true,
		},
		{
			name: "completed still requires data object", kind: "explore",
			output: `{"accepted":true,"outcome":"completed","data":null}`, wantErr: true,
		},
	}
	for _, kind := range []string{"explore", "bootstrap"} {
		for _, outcome := range []string{"continue", "incomplete"} {
			for _, conclude := range []bool{false, true} {
				phase := "execute"
				if conclude {
					phase = "conclude"
				}
				tests = append(tests, struct {
					name, kind, output, wantKind, wantOutcome, wantFact string
					conclude                                            bool
					wantErr                                             bool
				}{
					name: kind + " " + outcome + " " + phase, kind: kind, conclude: conclude,
					output:      `{"accepted":true,"outcome":"` + outcome + `","reason":"Assigned work remains"}`,
					wantOutcome: outcome, wantErr: conclude && outcome == "continue",
				})
			}
		}
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseWithPolicy(tt.output, tt.kind, tt.conclude, 1, 3, Policy{Version: 1})
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseWithPolicy() error = %v, wantErr %v; result=%+v", err, tt.wantErr, got)
			}
			if tt.wantErr {
				return
			}
			if got.Outcome != tt.wantOutcome || (tt.wantKind != "" && got.Kind != tt.wantKind) || got.Fact != tt.wantFact {
				t.Fatalf("result = %+v; want kind=%q outcome=%q fact=%q", got, tt.wantKind, tt.wantOutcome, tt.wantFact)
			}
			if tt.kind == "bootstrap" && !tt.conclude && tt.wantKind == "complete" && got.Complete.Description != "Goal met" {
				t.Fatalf("bootstrap completion data changed: %+v", got.Complete)
			}
			if tt.kind == "bootstrap" && tt.conclude && got.Complete.Description != "" {
				t.Fatalf("conclusion acquired project completion effects: %+v", got)
			}
			if tt.wantOutcome != "completed" && (len(got.Intents) != 0 || got.Complete.Description != "") {
				t.Fatalf("nonterminal or incomplete outcome acquired business effects: %+v", got)
			}
		})
	}
}

func TestExecutionPolicyRejectsAmbiguousEnvelopes(t *testing.T) {
	tests := []struct{ name, output string }{
		{"legacy success lacks outcome", `{"accepted":true,"data":{"description":"Continuing..."}}`},
		{"missing accepted", `{"outcome":"completed","data":{"description":"Done"}}`},
		{"null accepted", `{"accepted":null,"outcome":"completed","data":{"description":"Done"}}`},
		{"string accepted", `{"accepted":"true","outcome":"completed","data":{"description":"Done"}}`},
		{"null outcome", `{"accepted":true,"outcome":null,"data":{"description":"Done"}}`},
		{"empty outcome", `{"accepted":true,"outcome":"","data":{"description":"Done"}}`},
		{"unknown outcome", `{"accepted":true,"outcome":"finished","data":{"description":"Done"}}`},
		{"numeric outcome", `{"accepted":true,"outcome":1,"data":{"description":"Done"}}`},
		{"unknown top field", `{"accepted":true,"outcome":"completed","data":{"description":"Done"},"extra":true}`},
		{"continue missing reason", `{"accepted":true,"outcome":"continue"}`},
		{"continue null reason", `{"accepted":true,"outcome":"continue","reason":null}`},
		{"continue blank reason", `{"accepted":true,"outcome":"continue","reason":" \n "}`},
		{"continue numeric reason", `{"accepted":true,"outcome":"continue","reason":1}`},
		{"continue with data", `{"accepted":true,"outcome":"continue","reason":"More work","data":{"description":"Done"}}`},
		{"continue with empty data", `{"accepted":true,"outcome":"continue","reason":"More work","data":{}}`},
		{"continue with null data", `{"accepted":true,"outcome":"continue","reason":"More work","data":null}`},
		{"incomplete missing reason", `{"accepted":true,"outcome":"incomplete"}`},
		{"incomplete blank reason", `{"accepted":true,"outcome":"incomplete","reason":"  "}`},
		{"incomplete null reason", `{"accepted":true,"outcome":"incomplete","reason":null}`},
		{"incomplete object reason", `{"accepted":true,"outcome":"incomplete","reason":{}}`},
		{"incomplete with data", `{"accepted":true,"outcome":"incomplete","reason":"Blocked","data":{"description":"Done"}}`},
		{"incomplete with empty data", `{"accepted":true,"outcome":"incomplete","reason":"Blocked","data":{}}`},
		{"incomplete with null data", `{"accepted":true,"outcome":"incomplete","reason":"Blocked","data":null}`},
		{"rejected missing reason", `{"accepted":false}`},
		{"rejected null reason", `{"accepted":false,"reason":null}`},
		{"rejected blank reason", `{"accepted":false,"reason":" \t "}`},
		{"rejected array reason", `{"accepted":false,"reason":[]}`},
		{"rejected unknown top field", `{"accepted":false,"reason":"Unsupported","extra":true}`},
	}
	for _, kind := range []string{"explore", "bootstrap"} {
		for _, tt := range tests {
			t.Run(kind+"/"+tt.name, func(t *testing.T) {
				if got, err := ParseWithPolicy(tt.output, kind, false, 1, 3, Policy{Version: 1}); err == nil {
					t.Fatalf("accepted ambiguous envelope: %+v", got)
				}
			})
		}
	}
	for _, kind := range []string{"explore", "bootstrap", "reason"} {
		for _, conclude := range []bool{false, true} {
			got, err := ParseWithPolicy(`{"accepted":false,"reason":"No supported result"}`, kind, conclude, 1, 3, Policy{Version: 1})
			if err != nil || got.Kind != "rejected" {
				t.Errorf("%s conclude=%v rejection = %+v, %v", kind, conclude, got, err)
			}
		}
	}
}

func TestReasonPolicyGraphRPCGate(t *testing.T) {
	tests := []struct {
		name, output, wantKind string
		graphRPC               bool
		open                   int
		wantErr                bool
	}{
		{"RPC decided", `{"accepted":true,"data":{"decided":true}}`, "decided", true, 1, false},
		{"RPC complete", `{"accepted":true,"data":{"complete":{"from":["f001"],"description":"Goal met"}}}`, "complete", true, 0, false},
		{"RPC empty data noop", `{"accepted":true,"data":{}}`, "noop", true, 1, false},
		{"RPC empty intents noop", `{"accepted":true,"data":{"intents":[]}}`, "noop", true, 1, false},
		{"RPC empty intents without open work", `{"accepted":true,"data":{"intents":[]}}`, "", true, 0, true},
		{"RPC intents forbidden", `{"accepted":true,"data":{"intents":[{"from":["origin"],"description":"New step"}]}}`, "", true, 1, true},
		{"RPC singular intent forbidden", `{"accepted":true,"data":{"intent":{"from":["origin"],"description":"New step"}}}`, "", true, 1, true},
		{"RPC decided cannot hide intents", `{"accepted":true,"data":{"decided":true,"intents":[{"from":["origin"],"description":"Hidden step"}]}}`, "", true, 1, true},
		{"RPC decided cannot hide singular intent", `{"accepted":true,"data":{"decided":true,"intent":{"from":["origin"],"description":"Hidden step"}}}`, "", true, 1, true},
		{"RPC null intents cannot hide singular intent", `{"accepted":true,"data":{"intents":null,"intent":{"from":["origin"],"description":"Hidden step"}}}`, "", true, 1, true},
		{"plain intents supported", `{"accepted":true,"data":{"intents":[{"from":["origin"],"description":"New step"}]}}`, "intents", false, 0, false},
		{"plain singular intent supported", `{"accepted":true,"data":{"intent":{"from":["origin"],"description":"New step"}}}`, "intents", false, 0, false},
		{"plain empty intents noop", `{"accepted":true,"data":{"intents":[]}}`, "noop", false, 1, false},
		{"plain empty intents without open work", `{"accepted":true,"data":{"intents":[]}}`, "", false, 0, true},
		{"RPC rejected task", `{"accepted":false,"reason":"No supported decision"}`, "rejected", true, 1, false},
	}
	for _, version := range []int{0, 1} {
		for _, tt := range tests {
			t.Run(fmt.Sprintf("v%d/%s", version, tt.name), func(t *testing.T) {
				got, err := ParseWithPolicy(tt.output, "reason", false, tt.open, 3, Policy{Version: version, GraphRPC: tt.graphRPC})
				if (err != nil) != tt.wantErr {
					t.Fatalf("error = %v, wantErr %v; result=%+v", err, tt.wantErr, got)
				}
				if !tt.wantErr && (got.Kind != tt.wantKind || got.Outcome != "") {
					t.Fatalf("result = %+v, want kind %q without execution outcome", got, tt.wantKind)
				}
			})
		}
	}
}

func TestZeroPolicyPreservesLegacyParsing(t *testing.T) {
	tests := []struct {
		kind, output string
		conclude     bool
		open         int
	}{
		{"explore", `{"accepted":true,"data":{"description":"Continuing..."}}`, false, 1},
		{"explore", `{"description":"Legacy unwrapped fact"}`, true, 1},
		{"bootstrap", `{"accepted":true,"data":{"fact":{"description":"Evidence"},"complete":{"description":"Goal met"}}}`, false, 1},
		{"bootstrap", `{"fact":{"description":"Partial evidence"}}`, true, 1},
		{"reason", `{"accepted":true,"data":{"intents":[{"from":["origin"],"description":"New step"}]}}`, false, 0},
		{"reason", `{"accepted":true,"data":{"intents":[]}}`, false, 1},
		{"reason", `{"accepted":true,"data":{"intents":[]}}`, false, 0},
		{"explore", `{"accepted":false}`, false, 1},
		{"explore", `{"accepted":true,"data":{"description":"Legacy fact"},"legacy_metadata":1}`, false, 1},
	}
	for _, tt := range tests {
		t.Run(tt.kind+"/"+tt.output, func(t *testing.T) {
			want, oldErr := Parse(tt.output, tt.kind, tt.conclude, tt.open, 3)
			got, err := ParseWithPolicy(tt.output, tt.kind, tt.conclude, tt.open, 3, Policy{})
			if (err == nil) != (oldErr == nil) || !reflect.DeepEqual(got, want) {
				t.Fatalf("policy result=%+v error=%v; legacy result=%+v error=%v", got, err, want, oldErr)
			}
		})
	}
}

func TestPolicyRejectsUnsupportedVersions(t *testing.T) {
	for _, version := range []int{-1, 3, 999} {
		for _, kind := range []string{"explore", "bootstrap", "reason"} {
			// Even a declined task must not silently bypass an unknown protocol.
			if got, err := ParseWithPolicy(`{"accepted":false,"reason":"Unsupported"}`, kind, false, 1, 3, Policy{Version: version}); err == nil {
				t.Errorf("version=%d kind=%s accepted: %+v", version, kind, got)
			}
		}
	}
}

func TestLegacyPolicyRejectsMixedOutcomeProtocol(t *testing.T) {
	for _, kind := range []string{"bootstrap", "explore", "reason"} {
		for _, outcome := range []string{`"completed"`, `"continue"`, `"incomplete"`, `null`, `""`} {
			for _, accepted := range []bool{false, true} {
				output := fmt.Sprintf(`{"accepted":%t,"outcome":%s,"data":{"description":"Only part of the assigned work is finished"}}`, accepted, outcome)
				if got, err := ParseWithPolicy(output, kind, false, 1, 3, Policy{}); err == nil {
					t.Errorf("legacy %s accepted mixed protocol %s as %+v", kind, output, got)
				}
			}
		}
	}
}
