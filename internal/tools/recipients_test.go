package tools

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/nicodes/stavlos/internal/policy"
)

func TestMessagePolicyChecksEveryRecipient(t *testing.T) {
	sub := (messageTool{}).Subject(json.RawMessage(`{"to":["Scout","@HuMaN","scout"],"text":"hello"}`))
	if sub.Kind != policy.KindID || !reflect.DeepEqual(sub.Values, []string{"scout", "user"}) {
		t.Fatalf("policy subjects: %+v", sub)
	}
	rules := policy.New(policy.Rule{Tool: "message", Pattern: "*", Verb: policy.Allow}, policy.Rule{Tool: "message", Pattern: "user", Verb: policy.Deny})
	if rules.Decide("message", sub) != policy.Deny {
		t.Fatal("mixed send bypassed a denied recipient")
	}
}
