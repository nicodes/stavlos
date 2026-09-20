package discord

import (
	"strings"
	"unicode/utf16"

	"github.com/nicodes/stavlos/internal/present"
)

func units(s string) int { return len(utf16.Encode([]rune(s))) }
func clip(s string, n int) string {
	var b strings.Builder
	for _, r := range s {
		k := 1
		if r > 0xffff {
			k = 2
		}
		if n < k {
			break
		}
		n -= k
		b.WriteRune(r)
	}
	return b.String()
}

// clipMarkdown closes a code block when a shortened card cuts through it, so
// the decision or error appended afterwards remains outside the command.
func clipMarkdown(text string, limit int) string {
	if units(text) <= limit {
		return text
	}
	part := clip(text, max(0, limit-4))
	open := false
	for _, line := range strings.Split(part, "\n") {
		if strings.HasPrefix(line, "```") {
			open = !open
		}
	}
	if open {
		part += "\n```"
	}
	return part
}

// questionHeading is shared by open questions and their submitted results.
// Keep the closing Markdown delimiter intact when a long question is clipped.
func questionHeading(question string, limit int) string {
	const prefix, suffix = "❓ **", "**"
	question = strings.NewReplacer("\\", "\\\\", "*", "\\*", "_", "\\_", "`", "\\`").Replace(question)
	room := max(0, limit-units(prefix+suffix))
	if units(question) > room {
		question = strings.TrimRight(clip(question, max(0, room-1)), "\\") + "…"
	}
	return prefix + question + suffix
}

// splitText counts UTF-16 units (Discord's limit), splits preferentially at
// newlines, and balances fenced code blocks across continuation messages.
func splitText(text string) []string {
	var out []string
	fence := ""
	for text != "" {
		prefix := ""
		if fence != "" {
			prefix = fence + "\n"
		}
		part := clip(text, 1950-units(prefix))
		if len(part) < len(text) {
			if i := strings.LastIndex(part, "\n"); i > len(part)/2 {
				part = part[:i+1]
			}
		}
		text = text[len(part):]
		for _, line := range strings.Split(part, "\n") {
			if strings.HasPrefix(line, "```") {
				if fence != "" {
					fence = ""
				} else {
					fence = clip(line, 40)
				}
			}
		}
		msg := prefix + part
		if fence != "" {
			msg += "\n```"
		}
		out = append(out, msg)
	}
	return out
}

func stripBot(text, bot string) string {
	text = strings.TrimSpace(text)
	for _, mention := range []string{"<@" + bot + ">", "<@!" + bot + ">"} {
		if text == mention {
			return ""
		}
		if strings.HasPrefix(text, mention) && len(text) > len(mention) && strings.ContainsRune(" \n\t", rune(text[len(mention)])) {
			return strings.TrimSpace(text[len(mention):])
		}
	}
	return text
}

// The human is implicit in the Discord channel, but co-recipients remain
// visible before an agent-authored message's body.
func channelMessageText(to []string, text string) string {
	return present.Addressed(present.Without(to, "user"), text) // the user is reading it
}
