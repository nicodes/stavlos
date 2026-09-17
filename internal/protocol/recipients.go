package protocol

import (
	"encoding/json"
	"strings"
)

// Recipients advertises an array to tools while still reading older single-
// recipient calls from models and event logs.
type Recipients []string

func (r *Recipients) UnmarshalJSON(raw []byte) error {
	var one string
	if json.Unmarshal(raw, &one) == nil && strings.TrimSpace(string(raw)) != "null" {
		*r = Recipients{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return err
	}
	*r = many
	return nil
}

func Recipient(ref string) string {
	ref = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(ref), "@"))
	if ref == "human" {
		return "user"
	}
	return ref
}

func (r Recipients) Normalized() []string {
	out := make([]string, 0, len(r))
	seen := map[string]bool{}
	for _, ref := range r {
		ref = Recipient(ref)
		if !seen[ref] {
			out = append(out, ref)
			seen[ref] = true
		}
	}
	return out
}
