package event

import (
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/vocabulary.txt")

// TestEveryInputKindHasARule: a kind is declared with its rule or not at all.
// The constants are read from the source, so a kind added without a rule
// fails here and not in nine switches later.
func TestEveryInputKindHasARule(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "types.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var declared []InputKind
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || vs.Type == nil {
			return true
		}
		if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "InputKind" {
			return true
		}
		for _, v := range vs.Values {
			if lit, ok := v.(*ast.BasicLit); ok {
				s, _ := strconv.Unquote(lit.Value)
				declared = append(declared, InputKind(s))
			}
		}
		return true
	})
	slices.Sort(declared)
	if len(declared) == 0 || !slices.Equal(declared, InputKinds()) {
		t.Fatalf("declared kinds %v, kinds with a rule %v", declared, InputKinds())
	}
	if r := InputKind("from-a-newer-build").Rule(); !r.Wakes || r.MidTurn || r.Harness {
		t.Fatalf("an unknown kind: %+v", r)
	}
}

// TestVocabularyIsPinned: the event log outlives the build that wrote it, so
// its vocabulary (event types and input kinds) is pinned in
// testdata/vocabulary.txt. Adding to it is safe and needs only -update.
// Renaming or removing a word, or changing what a payload means, strands the
// events already in somebody's log: that needs a migration in
// internal/eventlog (migrations), and then -update.
//
//	go test ./internal/event -run TestVocabularyIsPinned -update
func TestVocabularyIsPinned(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "types.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var words []string
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || vs.Type == nil {
			return true
		}
		id, ok := vs.Type.(*ast.Ident)
		if !ok || (id.Name != "Type" && id.Name != "InputKind" && id.Name != "TurnReason" && id.Name != "AskOutcome" && id.Name != "TodoStatus") {
			return true
		}
		for _, v := range vs.Values {
			if lit, ok := v.(*ast.BasicLit); ok {
				s, _ := strconv.Unquote(lit.Value)
				words = append(words, id.Name+" "+s)
			}
		}
		return true
	})
	slices.Sort(words)
	got := strings.Join(words, "\n") + "\n"
	const path = "testdata/vocabulary.txt"
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if string(want) == got {
		return
	}
	var gone []string
	for _, w := range strings.Split(strings.TrimSpace(string(want)), "\n") {
		if !slices.Contains(words, w) {
			gone = append(gone, w)
		}
	}
	if len(gone) > 0 {
		t.Fatalf("words removed or renamed: %v. Logs already hold them: add a migration to internal/eventlog, then run with -update.", gone)
	}
	t.Fatalf("the vocabulary grew: run with -update to pin it.")
}
