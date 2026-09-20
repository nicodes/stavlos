package protocol

import (
	"encoding/json"

	"github.com/nicodes/stavlos/internal/event"
)

// Prompts and trust: what waits on the human, and the answers.

// PromptInfo is a pending permission/question/trust prompt.
type PromptInfo struct {
	ID          string          `json:"id"`
	Channel     string          `json:"channel"`
	Agent       string          `json:"agent,omitempty"`
	From        string          `json:"from,omitempty"`         // the asking agent\'s name, for a client that does not hold its channel\'s tree
	Role        string          `json:"role,omitempty"`         // the asking agent's role, including prompts from other channels
	ChannelName string          `json:"channel_name,omitempty"` // the channel\'s name, likewise
	Kind        PromptKind      `json:"kind"`                   // permission | question | trust
	Tool        string          `json:"tool,omitempty"`
	Input       json.RawMessage `json:"input,omitempty"`
	Question    string          `json:"question,omitempty"`
	Options     []string        `json:"options,omitempty"`
	ClaimedBy   string          `json:"claimed_by,omitempty"`
	Escalated   bool            `json:"escalated"` // visible to fallback tier; questions are visible immediately
	Created     string          `json:"created"`
	Dir         string          `json:"dir,omitempty"` // a boundary prompt: the call reaches outside the channel's directories; "allow_always" adds this one
	// Why the call asks, beyond policy, so that whatever answers waiting
	// prompts in bulk (a mode switch) decides as the agent runtime would:
	// Sticky is an ask no mode answers (an edit to a file that steers the
	// harness); Egress sends data off the machine, which auto leaves asking.
	Sticky         bool       `json:"sticky,omitempty"`
	Egress         bool       `json:"egress,omitempty"`
	Prefix         string     `json:"prefix,omitempty"`          // what "allow_prefix" would remember for this call (a command prefix, a host); "" when the call has none
	Questions      []Question `json:"questions,omitempty"`       // one question per prompt; legacy servers may send batches
	QuestionNumber int        `json:"question_number,omitempty"` // one-based position in the tool call's sequence
	QuestionTotal  int        `json:"question_total,omitempty"`
}

// QuestionPosition is the display position of a question. Older multi-question
// prompts keep their local numbering; new prompts each contain one question.
func (p PromptInfo) QuestionPosition(index int) (int, int) {
	if len(p.Questions) == 1 && p.QuestionNumber > 0 && p.QuestionTotal >= p.QuestionNumber {
		return p.QuestionNumber, p.QuestionTotal
	}
	return index + 1, len(p.Questions)
}

// Question is an ask_user question: a checklist. Options are
// always present; the human may pick any number of them and add a typed
// answer of their own, all joined with ", " in the answer.
type Question = event.Question
type QuestionOption = event.QuestionOption
type QuestionAnswer = event.QuestionAnswer
type PromptListParams struct {
	Channel string `json:"channel,omitempty"`
}
type PromptListResult struct {
	Prompts []PromptInfo `json:"prompts"`
}
type PromptClaimParams struct {
	ID string `json:"id"`
}
type PromptReplyParams struct {
	ID     string `json:"id"`
	Answer string `json:"answer"`           // allow | deny | allow_always | text
	Dir    string `json:"dir,omitempty"`    // boundary prompt + allow_always: add this directory instead of the offered one
	Reason string `json:"reason,omitempty"` // deny: an optional note the agent sees in its tool result
	// Answers contains the current question's answer (legacy batches have one entry per question)
	// (a picked label, several joined with ", ", or typed text).
	Answers []string         `json:"answers,omitempty"`
	Details []QuestionAnswer `json:"details,omitempty"`
}

type TrustStatusParams struct {
	Dir string `json:"dir"`
}
type TrustStatusResult struct {
	Dir     string   `json:"dir"`
	Pending bool     `json:"pending"`
	Hash    string   `json:"hash,omitempty"`
	Files   []string `json:"files,omitempty"` // what would be trusted
}
type TrustReplyParams struct {
	Dir   string `json:"dir"`
	Hash  string `json:"hash"`
	Trust bool   `json:"trust"`
}
