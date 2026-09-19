package tui

import (
	"github.com/nicodes/stavlos/internal/oauth"
	"strings"
	"testing"

	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/tui/dialog"
)

func ids(items []overlayItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.id
	}
	return out
}

func TestFilterItems(t *testing.T) {
	items := []overlayItem{
		{id: "anthropic", label: "Anthropic"},
		{id: "openai", label: "OpenAI"},
		{id: "openrouter", label: "OpenRouter"},
		{id: "ollama", label: "Ollama"},
		{id: "other", label: "Other"},
	}
	cases := []struct {
		q    string
		want string
	}{
		{"", "anthropic openai openrouter ollama other"},
		{"  ", "anthropic openai openrouter ollama other"},
		{"open", "openai openrouter"},
		{"OPEN", "openai openrouter"},
		{"ROUTER", "openrouter"},
		{"o", "anthropic openai openrouter ollama other"},
		{"zzz", ""},
	}
	for _, c := range cases {
		got := strings.Join(ids(filterItems(items, c.q)), " ")
		if got != c.want {
			t.Errorf("filterItems(%q) = %q, want %q", c.q, got, c.want)
		}
	}
}

func TestFilterItemsMatchesIDNotLabel(t *testing.T) {
	items := []overlayItem{{id: "anthropic/claude-sonnet-4", label: "Claude Sonnet 4"}}
	if got := filterItems(items, "sonnet-4"); len(got) != 1 {
		t.Fatalf("id match: got %d items", len(got))
	}
	if got := filterItems(items, "claude sonnet"); len(got) != 1 {
		t.Fatalf("label match: got %d items", len(got))
	}
}

func TestOverlayCursorFollowsFilter(t *testing.T) {
	o := newOverlay(ovProviders, overlayList, "t")
	o.setItems([]overlayItem{{id: "a", label: "A"}, {id: "b", label: "B"}, {id: "c", label: "C"}})
	o.move(2)
	if o.selected().id != "c" {
		t.Fatalf("cursor: got %q", o.selected().id)
	}
	o.move(5)
	if o.selected().id != "c" {
		t.Fatalf("cursor clamps at end: got %q", o.selected().id)
	}
	o.input.SetValue("b")
	o.update(nil) // re-filter on value change
	if sel := o.selected(); sel == nil || sel.id != "b" {
		t.Fatalf("after filter: got %v", sel)
	}
	o.input.SetValue("zzz")
	o.update(nil)
	if o.selected() != nil {
		t.Fatal("no match should select nothing")
	}
}

func TestModelHint(t *testing.T) {
	cases := []struct {
		in   protocol.ModelInfo
		want string
	}{
		{protocol.ModelInfo{Context: 200000, InputPrice: 3, OutputPrice: 15}, "ctx 200k · $3/$15 per 1M"},
		{protocol.ModelInfo{Context: 128000}, "ctx 128k"},
		{protocol.ModelInfo{Context: 1048576, InputPrice: 0.15, OutputPrice: 0.6}, "ctx 1m · $0.15/$0.6 per 1M"},
		{protocol.ModelInfo{Context: 32768, InputPrice: 0, OutputPrice: 2}, "ctx 33k · $0/$2 per 1M"},
		{protocol.ModelInfo{InputPrice: 1, OutputPrice: 2}, "$1/$2 per 1M"},
		{protocol.ModelInfo{}, ""},
	}
	for _, c := range cases {
		if got := modelHint(c.in); got != c.want {
			t.Errorf("modelHint(%+v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestModelItems(t *testing.T) {
	items := modelItems([]protocol.ModelInfo{
		{ID: "anthropic/claude-sonnet-4", Provider: "anthropic", Name: "Claude Sonnet 4", Context: 200000},
		{ID: "ollama/llama3", Provider: "ollama"},
	})
	if len(items) != 2 {
		t.Fatalf("got %d items", len(items))
	}
	if items[0].id != "anthropic/claude-sonnet-4" || items[0].label != "Claude Sonnet 4" || items[0].sub != "anthropic/claude-sonnet-4" || items[0].hint != "ctx 200k" {
		t.Errorf("item 0: %+v", items[0])
	}
	if items[1].label != "ollama/llama3" {
		t.Errorf("nameless model should use id as label: %+v", items[1])
	}
}

func TestProviderItems(t *testing.T) {
	items := providerItems([]protocol.ProviderInfo{
		{ID: "openai", Name: "ChatGPT", Kind: "subscription", Label: "ChatGPT Plus/Pro subscription", Connected: true, Account: "nico@example.com"},
		{ID: "xai", Name: "Grok", Kind: "subscription", Label: "SuperGrok subscription"},
	})
	if got, want := strings.Join(ids(items), " "), "openai xai"; got != want {
		t.Fatalf("ids = %q, want %q (no Other entry)", got, want)
	}
	if items[0].label != "ChatGPT" || items[0].hint != "connected · nico@example.com" || !items[0].good {
		t.Errorf("connected row: %+v", items[0])
	}
	if items[1].label != "Grok" || items[1].hint != "SuperGrok subscription · not signed in" || items[1].good {
		t.Errorf("not-connected row: %+v", items[1])
	}
	// Connected without an account still reads "connected".
	only := providerItems([]protocol.ProviderInfo{{ID: "xai", Name: "Grok", Connected: true}})
	if only[0].hint != "connected" || !only[0].good {
		t.Errorf("connected without account: %+v", only[0])
	}
	if got := providerItems(nil); len(got) != 0 {
		t.Fatalf("empty list should yield no rows: %+v", got)
	}
}

func TestSpacedCode(t *testing.T) {
	cases := map[string]string{"ABCD-EFGH": "A B C D - E F G H", " ab ": "a b", "": "", "X": "X"}
	for in, want := range cases {
		if got := spacedCode(in); got != want {
			t.Errorf("spacedCode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoginOverlayView(t *testing.T) {
	o := newOverlay(ovProviders, overlayLogin, "")
	o.switchLogin("ChatGPT")

	// Starting state: no URL yet, just the spinner.
	v := stripANSI(o.view(100, "⠋"))
	if !strings.Contains(v, "Sign in to ChatGPT") || !strings.Contains(v, "⠋ "+loginStartText) {
		t.Fatalf("starting view:\n%s", v)
	}
	if strings.Contains(v, "›") {
		t.Fatalf("login mode must not render a text field:\n%s", v)
	}

	o.setLogin("https://auth.openai.com/device", "ABCD-EFGH", "The code expires in 15 minutes.", "")
	v = stripANSI(o.view(100, "⠋"))
	for _, want := range []string{
		"Open this URL on any device:",
		"  https://auth.openai.com/device",
		"and enter the code:",
		"  A B C D - E F G H",
		"The code expires in 15 minutes.",
		"⠋ " + loginWaitingText,
		loginKeysWaiting,
	} {
		if !strings.Contains(v, want) {
			t.Errorf("missing %q in:\n%s", want, v)
		}
	}
	for _, l := range strings.Split(v, "\n") {
		if w := len([]rune(l)); w > dialog.MaxWidth {
			t.Errorf("line wider than %d: %q", dialog.MaxWidth, l)
		}
	}

	// Error state keeps the URL and code but swaps the waiting line.
	o.setLoginError("login expired")
	v = stripANSI(o.view(100, "⠋"))
	if !strings.Contains(v, "login expired") || !strings.Contains(v, loginKeysError) {
		t.Fatalf("error view:\n%s", v)
	}
	if strings.Contains(v, loginWaitingText) || strings.Contains(v, "⠋") {
		t.Fatalf("error view must not show the spinner:\n%s", v)
	}
	if !strings.Contains(v, "A B C D - E F G H") {
		t.Fatalf("error view should keep the code:\n%s", v)
	}

	// A fresh setLogin (retry) clears the error.
	o.setLogin("https://auth.openai.com/device", "WXYZ-1234", "", "")
	v = stripANSI(o.view(100, "⠙"))
	if strings.Contains(v, "login expired") || !strings.Contains(v, "W X Y Z - 1 2 3 4") || !strings.Contains(v, "⠙ "+loginWaitingText) {
		t.Fatalf("retry view:\n%s", v)
	}

	// Long URLs are hard-wrapped, never truncated.
	long := "https://example.com/" + strings.Repeat("abcdefghij", 12)
	o.setLogin(long, "AB", "", "")
	v = stripANSI(o.view(100, ""))
	joined := strings.NewReplacer("\n", "", " ", "", "│", "").Replace(v)
	if !strings.Contains(joined, long) {
		t.Fatalf("long URL should survive wrapping:\n%s", v)
	}
	for _, l := range strings.Split(v, "\n") {
		if w := len([]rune(l)); w > dialog.MaxWidth {
			t.Errorf("line wider than %d: %q", dialog.MaxWidth, l)
		}
	}
}

func TestOverlayViewListsAndPages(t *testing.T) {
	o := newOverlay(ovModels, overlayList, "Select a model")
	var items []overlayItem
	for i := 0; i < 25; i++ {
		items = append(items, overlayItem{id: "m" + string(rune('a'+i)), label: "Model " + string(rune('A'+i)), hint: "ctx 1k"})
	}
	o.setItems(items)
	v := stripANSI(o.view(100, ""))
	if !strings.Contains(v, "Select a model") || !strings.Contains(v, "▸ Model A") {
		t.Fatalf("view:\n%s", v)
	}
	if !strings.Contains(v, "↓ 15 more") || strings.Contains(v, "↑") {
		t.Fatalf("more markers:\n%s", v)
	}
	o.move(overlayMaxRows) // page down
	v = stripANSI(o.view(100, ""))
	if !strings.Contains(v, "▸ Model K") || !strings.Contains(v, "↑ 1 more") {
		t.Fatalf("after page:\n%s", v)
	}
	for _, l := range strings.Split(v, "\n") {
		if w := len([]rune(l)); w > dialog.MaxWidth {
			t.Errorf("line wider than %d: %q", dialog.MaxWidth, l)
		}
	}
}

func TestCompositeKeepsWidth(t *testing.T) {
	base := strings.TrimSuffix(strings.Repeat(strings.Repeat("x", 80)+"\n", 12), "\n")
	box := "+--+\n|ab|\n+--+"
	out := dialog.Composite(base, 80, 12, box)
	lines := strings.Split(out, "\n")
	if len(lines) != 12 {
		t.Fatalf("got %d lines", len(lines))
	}
	for i, l := range lines {
		if len([]rune(l)) != 80 {
			t.Errorf("line %d width %d: %q", i, len([]rune(l)), l)
		}
	}
	if !strings.Contains(lines[5], "|ab|") {
		t.Errorf("box not centered: %q", lines[5])
	}
}

func TestLoginOverlayBrowserMode(t *testing.T) {
	o := newOverlay(ovProviders, overlayLogin, "")
	o.switchLogin("ChatGPT")
	o.setLogin("https://auth.example/oauth/authorize?x=1", "", "Complete the sign-in in your browser.", "")
	if !o.login.browser {
		t.Fatal("browser mode not detected")
	}
	out := strings.Join(o.loginLines(60, "⠋"), "\n")
	if !strings.Contains(out, "browser should open") || !strings.Contains(out, "auth.example") || strings.Contains(out, "enter the code") {
		t.Fatalf("%s", out)
	}
	o.setLogin("https://x/dev", "AB-CD", "", "")
	out = strings.Join(o.loginLines(60, "⠋"), "\n")
	if !strings.Contains(out, "enter the code") || !strings.Contains(out, "A B - C D") {
		t.Fatalf("%s", out)
	}
}

// TestLoginOverlayTakesAKey: a sign-in whose method is a pasted key shows
// a field instead of a code and a spinner, echoes nothing back, and its
// hints say what enter does.
func TestLoginOverlayTakesAKey(t *testing.T) {
	o := newOverlay(ovProviders, overlayLogin, "Sign in to Z.ai Coding Plan")
	o.setLogin("https://z.ai/manage-apikey/apikey-list", "", "Create a key for your GLM Coding Plan and paste it here.", oauth.MethodAPIKey)
	if !o.login.key || o.login.browser {
		t.Fatalf("an apikey login is neither a code nor a browser callback: %+v", o.login)
	}
	o.input.SetValue("zk-secret")
	v := stripANSI(o.view(80, "-"))
	switch {
	case !strings.Contains(v, "Create a key for your plan at:"):
		t.Fatalf("no console link:\n%s", v)
	case strings.Contains(v, loginWaitingText):
		t.Fatalf("nothing is polled for a key:\n%s", v)
	case strings.Contains(v, "zk-secret"):
		t.Fatalf("the key should not be echoed:\n%s", v)
	case !strings.Contains(v, "enter: sign in"):
		t.Fatalf("no hint:\n%s", v)
	}
}
