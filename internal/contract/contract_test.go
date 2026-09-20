package contract

import (
	"encoding/json"
	"testing"
)

func TestMixedOutputAndTaskContracts(t *testing.T) {
	cases := []struct {
		name, text, kind string
		conclude         bool
		open             int
		want             string
		bad              bool
	}{
		{"mixed", "Analysis first\n```json\n{\"accepted\":true,\"data\":{\"description\":\"confirmed\"}}\n```\nDone", "explore", false, 1, "fact", false},
		{"singular", `{"intent":{"from":["origin"],"description":"look"}}`, "reason", false, 0, "intents", false},
		{"first object", `example {"x":1} then {"accepted":true,"data":{"description":"confirmed"}}`, "explore", false, 1, "", true},
		{"empty reason", `{"accepted":true,"data":{}}`, "reason", false, 0, "", true},
		{"noop reason", `{"accepted":true,"data":{}}`, "reason", false, 1, "noop", false},
		{"bootstrap", `{"accepted":true,"data":{"fact":{"description":"evidence"},"complete":{"description":"done"}}}`, "bootstrap", false, 1, "complete", false},
		{"bootstrap conclude", `{"accepted":true,"data":{"fact":{"description":"partial"},"complete":{"description":"ignored"}}}`, "bootstrap", true, 1, "fact", false},
		{"rejected", `{"accepted":false,"reason":"no conclusion"}`, "explore", false, 1, "rejected", false},
		{"invalid JSON", `{"accepted": true,}`, "explore", false, 1, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := Parse(c.text, c.kind, c.conclude, c.open, 2)
			if (err != nil) != c.bad || err == nil && r.Kind != c.want {
				t.Fatalf("%+v %v", r, err)
			}
		})
	}
}

func TestExtractSelectsFirstParseableObject(t *testing.T) {
	for _, tt := range []struct{ input, want string }{
		{`preface {bad} later {"chosen":"second"} {"chosen":"third"}`, "second"},
		{"```JSON\n{\"chosen\":\"fenced\"}\n```", "fenced"},
		{`[1,{"chosen":"nested"}]`, "nested"},
		{`{"chosen":"outer","example":{"chosen":"inner"}}`, "outer"},
		{`text {"chosen":"escaped","text":"} and \\\""} suffix`, "escaped"},
		{`{"broken": {"chosen":"inner"}`, "inner"},
	} {
		t.Run(tt.want, func(t *testing.T) {
			m, err := Extract(tt.input)
			var got string
			_ = json.Unmarshal(m["chosen"], &got)
			if err != nil || got != tt.want {
				t.Fatalf("got %q, %v", got, err)
			}
		})
	}
	for _, input := range []string{"", "plain words", `{"bad": }`, "null", "[1,2]"} {
		if _, err := Extract(input); err == nil {
			t.Fatalf("accepted %q", input)
		}
	}
}

func TestContractEdgesMatchCairn(t *testing.T) {
	for _, input := range []string{
		`{"intents":null}`, `{"complete":null}`, `{"intent":null}`,
		`{"accepted":null,"description":"text"}`,
		`{"accepted":true,"data":{"complete":{"from":[],"description":"x"},"intents":[]}}`,
		`{"accepted":true,"data":{"intents":[{"from":["origin"]}]}}`,
	} {
		if _, err := Parse(input, "reason", false, 1, 2); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
	// Shape validation runs for every entry before truncating to max_intents.
	if _, err := Parse(`{"intents":[{"from":["origin"],"description":"ok"},{"from":["origin"]}]}`, "reason", false, 0, 1); err == nil {
		t.Fatal("ignored malformed excess entry")
	}
	// Field validation belongs to Server so one bad direction does not discard
	// all valid siblings. Values must not be normalized before Server sees them.
	r, err := Parse(`{"intents":[{"from":null,"description":42},{"from":["origin"],"description":" good "}]}`, "reason", false, 0, 2)
	if err != nil || len(r.Intents) != 2 {
		t.Fatalf("%+v %v", r, err)
	}
	raw, _ := json.Marshal(r.Intents[0].Input())
	if string(raw) != `{"description":42,"from":null}` {
		t.Fatalf("altered payload: %s", raw)
	}
	if r.Intents[1].Description != " good " {
		t.Fatal("reason description was trimmed")
	}
	if _, err := Parse(`{"intents":[]}`, "reason", false, 1, 0); err == nil {
		t.Fatal("invalid max_intents accepted")
	}
}

func TestConcludeCannotCompleteOrInventFact(t *testing.T) {
	r, err := Parse(`{"fact":{"description":" confirmed "},"complete":"ignored"}`, "bootstrap", true, 1, 2)
	if err != nil || r.Kind != "fact" || r.Fact != "confirmed" {
		t.Fatalf("%+v %v", r, err)
	}
	for _, input := range []string{`{"accepted":true,"data":{"complete":{"description":"done"}}}`, `{"fact":{"description":"   "}}`, `{"accepted":true,"data":{"fact":{"description":"ok"},"extra":1}}`} {
		if _, err := Parse(input, "bootstrap", true, 1, 2); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
}
func TestIntentLimit(t *testing.T) {
	r, err := Parse(`{"accepted":true,"data":{"intents":[{"from":["origin"],"description":"a"},{"from":["origin"],"description":"b"}]}}`, "reason", false, 0, 1)
	if err != nil || len(r.Intents) != 1 {
		t.Fatalf("%+v %v", r, err)
	}
}
