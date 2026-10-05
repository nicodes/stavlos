package tools

import (
	"encoding/json"
	"testing"
)

func TestMessageResponseDefaults(t *testing.T) {
	for _, tc := range []struct {
		input, kind string
		bad         bool
	}{
		{`{"to":["worker"]}`, KindRequest, false},
		{`{"to":["worker"],"expect_response":false}`, KindSteer, false},
		{`{"channel":"work"}`, KindInfo, false},
		{`{"to":["worker"],"reply_to":["r1"]}`, KindResponse, false},
		{`{"to":["worker"],"reply_to":["r1"],"expect_response":true}`, KindResponseRequest, false},
		{`{"channel":"work","expect_response":true}`, "", true},
		{`{"to":["worker"],"kind":"info","expect_response":false}`, "", true},
	} {
		var in messageInput
		if err := json.Unmarshal([]byte(tc.input), &in); err != nil {
			t.Fatal(err)
		}
		got, err := messageKind(in, in.To.Normalized())
		if (err != nil) != tc.bad || got != tc.kind {
			t.Errorf("%s: %s %v", tc.input, got, err)
		}
	}
}
