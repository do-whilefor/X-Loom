package contract

import (
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
func TestIntentLimit(t *testing.T) {
	r, err := Parse(`{"accepted":true,"data":{"intents":[{"from":["origin"],"description":"a"},{"from":["origin"],"description":"b"}]}}`, "reason", false, 0, 1)
	if err != nil || len(r.Intents) != 1 {
		t.Fatalf("%+v %v", r, err)
	}
}
