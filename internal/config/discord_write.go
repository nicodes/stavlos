package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/nicodes/stavlos/internal/paths"
	"github.com/nicodes/stavlos/internal/statefile"
)

var globalWriteMu sync.Mutex

// SetDiscordEnabled changes just the global enabled value, preserving JSONC
// comments, credentials, formatting and other settings. Replacement is atomic.
func SetDiscordEnabled(enabled bool) error {
	globalWriteMu.Lock()
	defer globalWriteMu.Unlock()
	path := filepath.Join(paths.ConfigDir(), "stavlos.json")
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		if !enabled && errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	b, err := os.ReadFile(resolved)
	if err != nil {
		return err
	}
	out, err := editDiscordEnabled(b, enabled)
	if err != nil {
		return err
	}
	if bytes.Equal(b, out) {
		return nil
	}
	return statefile.WriteAtomic(resolved, out, 0o600, false) // private whatever it was: it may name a token
}

func editDiscordEnabled(b []byte, enabled bool) ([]byte, error) {
	clean, err := maskJSONC(b)
	if err != nil {
		return nil, err
	}
	start, end, err := propertySpan(clean, "discord")
	if err != nil {
		return nil, err
	}
	if start < 0 || string(clean[start:end]) == "null" {
		if !enabled {
			return b, nil
		}
		return nil, errors.New("add a discord block to the global stavlos.json first")
	}
	a, z, err := propertySpan(clean[start:end], "enabled")
	if err != nil {
		return nil, err
	}
	value := strconv.FormatBool(enabled)
	if a >= 0 {
		return []byte(string(b[:start+a]) + value + string(b[start+z:])), nil
	}
	comma := ""
	if len(bytes.TrimSpace(clean[start+1:end-1])) > 0 {
		comma = ","
	}
	line := bytes.LastIndexByte(b[:start], '\n') + 1
	indent := ""
	for i := line; i < start && (b[i] == ' ' || b[i] == '\t'); i++ {
		indent += string(b[i])
	}
	eol := "\n"
	if bytes.Contains(b, []byte("\r\n")) {
		eol = "\r\n"
	}
	insert := eol + indent + "  \"enabled\": " + value + comma
	return []byte(string(b[:start+1]) + insert + string(b[start+1:])), nil
}

// propertySpan locates the last instance of a property (matching JSON decoding)
// using decoder byte offsets. RawMessage excludes the colon and whitespace.
func propertySpan(b []byte, key string) (int, int, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	t, err := d.Token()
	if err != nil || t != json.Delim('{') {
		return -1, -1, errors.New("expected a JSON object in the global configuration")
	}
	start, end := -1, -1
	for d.More() {
		k, err := d.Token()
		if err != nil {
			return -1, -1, err
		}
		var raw json.RawMessage
		if err := d.Decode(&raw); err != nil {
			return -1, -1, err
		}
		if k == key {
			end = int(d.InputOffset())
			start = end - len(raw)
		}
	}
	return start, end, nil
}

// maskJSONC removes comments and trailing commas without changing byte offsets.
// Strings are left untouched, including escaped quotes and comment-like tokens.
func maskJSONC(b []byte) ([]byte, error) {
	out := append([]byte(nil), b...)
	for i := 0; i < len(out); i++ {
		if out[i] == '"' {
			i = quotedEnd(out, i)
			continue
		}
		if out[i] != '/' || i+1 >= len(out) {
			continue
		}
		start := i
		switch out[i+1] {
		case '/':
			for i < len(out) && out[i] != '\n' {
				out[i] = ' '
				i++
			}
		case '*':
			i += 2
			for i+1 < len(out) && !(out[i] == '*' && out[i+1] == '/') {
				i++
			}
			if i+1 >= len(out) {
				return nil, errors.New("unterminated configuration comment")
			}
			i++
			for j := start; j <= i; j++ {
				if out[j] != '\n' && out[j] != '\r' {
					out[j] = ' '
				}
			}
		}
	}
	for i := 0; i < len(out); i++ {
		if out[i] == '"' {
			i = quotedEnd(out, i)
			continue
		}
		if out[i] != ',' {
			continue
		}
		j := i + 1
		for j < len(out) && bytes.ContainsRune([]byte(" \n\r\t"), rune(out[j])) {
			j++
		}
		if j < len(out) && (out[j] == '}' || out[j] == ']') {
			out[i] = ' '
		}
	}
	if !json.Valid(out) {
		return nil, errors.New("invalid global JSON configuration")
	}
	return out, nil
}

func quotedEnd(b []byte, start int) int {
	for i := start + 1; i < len(b); i++ {
		if b[i] == '\\' {
			i++
			continue
		}
		if b[i] == '"' {
			return i
		}
	}
	return len(b)
}
