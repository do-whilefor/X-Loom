package contract

import (
	"reflect"
	"testing"
)

func TestCompatibilityDirectionsKeepInvalidSiblingsForServerValidation(t *testing.T) {
	parsed, err := Parse(`{"accepted":true,"data":{"intents":[{"from":["origin",42],"description":42},{"from":["origin"],"description":"  Valid sibling  "}]}}`, "reason", false, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	// The compatibility parser checks required keys but leaves source and field
	// validation to the Server, where a bad direction is skipped independently.
	want := []Direction{{From: []string{"origin", ""}}, {From: []string{"origin"}, Description: "  Valid sibling  "}}
	if parsed.Kind != "intents" || !reflect.DeepEqual(parsed.Intents, want) {
		t.Fatalf("compatibility direction boundary changed: %+v", parsed)
	}
}
