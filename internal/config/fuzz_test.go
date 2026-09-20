package config

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// FuzzMaskJSONC: comments and trailing commas are blanked without moving a
// byte, which is what lets an edit be applied to the original at the offsets
// found in the masked copy; what comes out is JSON.
func FuzzMaskJSONC(f *testing.F) {
	for _, s := range []string{
		`{"a":1}`, "{\n // c\n \"a\": 1,\n}", `{"a":"// not a comment","b":[1,2,],}`, `{"a":"\"/*"} /* c */`, `{/*`, `{"a": "x\\"}`, ``, `[1,]`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		out, err := maskJSONC(b)
		if err != nil {
			return
		}
		if len(out) != len(b) {
			t.Fatalf("masking moved bytes: %d became %d for %q", len(b), len(out), b)
		}
		if !json.Valid(out) {
			t.Fatalf("masked output is not JSON: %q from %q", out, b)
		}
	})
}

// FuzzEditJSONField: setting a field either fails or yields a document that
// still parses (as JSONC) and reads back the value that was set.
func FuzzEditJSONField(f *testing.F) {
	for _, s := range []string{`{}`, `{"model":"a/b"}`, "{\n  // keep me\n  \"model\": \"a/b\", \"web\": {\"port\": 1},\n}", `{"a":{"b":{"c":1}}}`, `[]`, `{"model": "x$1"}`} {
		f.Add([]byte(s), "model", `"p/m"`)
	}
	f.Fuzz(func(t *testing.T, content []byte, key, value string) {
		if !json.Valid([]byte(value)) || key == "" || !utf8.ValidString(key) || strings.ContainsFunc(key, unicode.IsControl) {
			return // keys are the names of settings, not arbitrary bytes
		}
		out, err := EditJSONField(content, []string{key}, json.RawMessage(value))
		if err != nil {
			return
		}
		clean, err := maskJSONC(out)
		if err != nil {
			t.Fatalf("the edit broke the document: %v\n%q\nfrom %q", err, out, content)
		}
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(clean, &doc); err != nil {
			t.Fatalf("the edited document is not an object: %v: %q", err, out)
		}
		var want, got any
		_ = json.Unmarshal([]byte(value), &want)
		_ = json.Unmarshal(doc[key], &got)
		if w, _ := json.Marshal(want); string(w) != func() string { g, _ := json.Marshal(got); return string(g) }() {
			t.Fatalf("set %s = %s, read back %s\n%q", key, value, doc[key], out)
		}
	})
}
