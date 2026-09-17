package discord

import (
	"strings"

	"github.com/nicodes/stavlos/internal/protocol"
)

// Non-breaking spaces survive Discord Markdown without starting a code block.
const answerIndent = "\u00a0\u00a0\u00a0\u00a0"

func answerChoice(mark, text string) string {
	return answerIndent + mark + " " + strings.ReplaceAll(text, "\n", "\n"+answerIndent+"\u00a0\u00a0\u00a0")
}

// answeredQuestions preserves the submitted choices on the resolved card.
// The draft, rather than splitting Answers by commas, distinguishes selected
// options from custom text that happens to match an option label.
func answeredQuestions(p protocol.PromptInfo, d *draft) string {
	if len(p.Questions) == 0 {
		return "❓"
	}
	var lines []string
	headings := map[int]string{}
	for i, q := range p.Questions {
		if i > 0 {
			lines = append(lines, "")
		}
		headings[len(lines)] = q.Question
		lines = append(lines, questionHeading(q.Question, 2000))
		if d == nil {
			continue
		}
		for n, o := range q.Options {
			mark := "⬜"
			if d.selected[i][n] {
				mark = "✅"
			}
			line := o.Label
			if o.Description != "" {
				line += " — " + o.Description
			}
			lines = append(lines, answerChoice(mark, line))
		}
		if custom := strings.TrimSpace(d.text[i]); custom != "" {
			lines = append(lines, answerChoice("✅", custom))
		}
	}
	budget := 2000
	if units(strings.Join(lines, "\n")) > budget {
		// Keep every question, option marker and custom-answer row visible.
		// A very long early question must not consume the later answers.
		n := 0
		for _, line := range lines {
			if line != "" {
				n++
			}
		}
		limit := max(1, (budget-len(lines)+1)/max(1, n))
		for i, line := range lines {
			if question, ok := headings[i]; ok {
				lines[i] = questionHeading(question, limit)
			} else if units(line) > limit {
				lines[i] = clip(line, limit-1) + "…"
			}
		}
	}
	return strings.Join(lines, "\n")
}
