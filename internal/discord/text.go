package discord

import (
	"strings"
	"unicode/utf16"
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
