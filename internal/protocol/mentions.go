package protocol

import "strings"

// Mentions lists the @names in a chat message, lowercased, in order and
// each once. A mention is an @ at the start of the text or after a
// character that cannot be part of a name ("me@example.com" is not one),
// followed by letters, digits, "-" or "_"; a trailing "-" or "_" is not
// part of it.
func Mentions(text string) []string {
	var out []string
	seen := map[string]bool{}
	for i := 0; i < len(text); i++ {
		if text[i] != '@' || i > 0 && isNameByte(text[i-1]) {
			continue
		}
		j := i + 1
		for j < len(text) && isNameByte(text[j]) {
			j++
		}
		name := strings.TrimRight(strings.ToLower(text[i+1:j]), "-_")
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
		i = j - 1
	}
	return out
}

func isNameByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '-' || b == '_'
}
