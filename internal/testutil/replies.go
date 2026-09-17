// Package testutil contains fixtures shared by runtime integration tests.
package testutil

import (
	"encoding/json"
	"strings"

	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/protocol"
)

// BindReplies lets scripted models declare response intent without predicting
// random request IDs. Explicit reply_to fields (including invalid ones) are
// never changed; protocol-validation tests exercise the tool directly.
func BindReplies(req model.Request, response model.Response) model.Response {
	response.Blocks = append([]model.Block(nil), response.Blocks...)
	note := req.System
	for _, msg := range req.Messages {
		for _, block := range msg.Blocks {
			if strings.HasPrefix(block.Text, "[harness state") {
				note = block.Text
			}
		}
	}
	if i := strings.LastIndex(note, "[harness state"); i >= 0 {
		note = note[i:]
	}
	for i, block := range response.Blocks {
		if block.Type != model.BlockToolUse || block.Name != "message" {
			continue
		}
		var input map[string]json.RawMessage
		if json.Unmarshal(block.Input, &input) != nil || input["reply_to"] != nil {
			continue
		}
		var kind string
		_ = json.Unmarshal(input["kind"], &kind)
		if kind != "response" {
			continue
		}
		var to protocol.Recipients
		_ = json.Unmarshal(input["to"], &to)
		want := map[string]bool{}
		for _, name := range to.Normalized() {
			want[name] = true
		}
		var ids []string
		for _, line := range strings.Split(note, "\n") {
			parts := strings.Fields(line)
			if len(parts) < 4 || parts[0] != "-" || parts[2] != "from" {
				continue
			}
			matches := want[strings.TrimSuffix(strings.TrimPrefix(parts[3], "@"), ":")]
			if len(parts) > 4 && strings.HasPrefix(parts[4], "(") {
				id := strings.Trim(strings.TrimSuffix(parts[4], ":"), "()")
				matches = matches || want[id]
				for ref := range want {
					if len(ref) >= 4 && strings.HasPrefix(id, ref) {
						matches = true
					}
				}
			}
			if matches {
				ids = append(ids, parts[1])
			}
		}
		input["reply_to"], _ = json.Marshal(ids)
		response.Blocks[i].Input, _ = json.Marshal(input)
	}
	return response
}
