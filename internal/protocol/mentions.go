package protocol

import "strings"

// Addressees splits a channel chat post into the agents it is addressed to
// and its message. The names are the @name words at the very front, each
// followed by a space (a trailing , : or ; is allowed); the message is
// everything after them, left exactly as written, so an @ inside it means
// whatever its author meant. Names are lowercased; a post without a leading
// @name has none.
func Addressees(text string) (names []string, message string) {
	rest := strings.TrimLeft(text, " \t")
	for strings.HasPrefix(rest, "@") {
		end := strings.IndexAny(rest, " \t\n")
		word := rest
		if end >= 0 {
			word = rest[:end]
		}
		name := strings.ToLower(strings.TrimRight(word[1:], ",:;"))
		if name == "" {
			break
		}
		names = append(names, name)
		if end < 0 {
			return names, ""
		}
		rest = strings.TrimLeft(rest[end:], " \t\n")
	}
	if len(names) == 0 {
		return nil, text
	}
	return names, rest
}
