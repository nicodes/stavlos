package protocol

import (
	"strings"

	"github.com/nicodes/stavlos/internal/event"
)

// PermissionResult describes the recorded decision without inferring an allow
// from a disappearing prompt. Shared by terminal and Discord history.
func PermissionResult(p PromptInfo, r event.AskResolvedPayload) string {
	if r.Outcome == event.AskWithdrawn {
		return "Permission withdrawn"
	}
	var text string
	switch r.Answer {
	case AnswerAllow:
		text = "Allowed once"
	case AnswerAllowAlways:
		text = "Allowed for this channel"
		if p.Dir != "" {
			dir := r.Dir
			if dir == "" {
				dir = p.Dir
			}
			text = "Allowed and added directory: " + dir
		}
	case AnswerAllowPrefix:
		text = "Allowed prefix: " + p.Prefix
	case AnswerDeny:
		text = "Denied"
	default:
		text = "Permission resolved: " + r.Answer
	}
	if r.Outcome == event.AskDefaulted {
		text = "Defaulted · " + text
	}
	if strings.TrimSpace(r.Reason) != "" {
		text += " · " + r.Reason
	}
	return text
}
