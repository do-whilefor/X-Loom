package server

import (
	"net/http"
	"reflect"
	"testing"
)

func TestRegisteredStepOperationsBindRequestIdentity(t *testing.T) {
	for _, kind := range []string{"reason", "explore"} {
		for _, op := range []string{"heartbeat", "release", "conclude"} {
			t.Run(kind+"/"+op, func(t *testing.T) {
				f := newExecutionProtocolFixture(t)
				live := true
				if kind == "reason" {
					f = newDecisionBatchFixture(t)
					f.intent = f.newIntent().ID
				} else {
					f.register(kind, &live, 2)
					f.request("POST", f.base()+"/intents/"+f.intent+"/release", map[string]string{"worker": f.lease}, true, http.StatusOK, nil)
				}
				before := f.state()
				f.request("POST", f.base()+"/intents/"+f.intent+"/"+op, map[string]string{"worker": "unregistered-worker", "description": "Unbound result"}, true, http.StatusForbidden, nil)
				if !reflect.DeepEqual(before, f.state()) {
					t.Fatal("mismatched identity changed the Step or published a Fact")
				}
			})
		}
	}
}

func TestRegisteredStepOperationCannotUseAnotherStepPath(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	live := true
	f.register("explore", &live, 2)
	other := f.newIntent()
	before := f.state()
	f.request("POST", f.base()+"/intents/"+other.ID+"/conclude", map[string]string{"worker": f.lease, "description": "Wrong Step"}, true, http.StatusForbidden, nil)
	if !reflect.DeepEqual(before, f.state()) {
		t.Fatal("mismatched Step path changed shared state")
	}
}
