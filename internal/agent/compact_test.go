package agent

import (
	"strings"
	"testing"

	"github.com/nicodes/stavlos/internal/model"
)

// TestSummaryRequestBound: the summary is bounded in tokens where the model
// honours max tokens, and in words in the prompt where it does not.
func TestSummaryRequestBound(t *testing.T) {
	req := summaryRequest("m", "T", "", model.Info{})
	if req.MaxTokens != summaryMaxTokens || strings.Contains(req.System, "words") || req.Model != "m" || !strings.HasSuffix(req.Messages[0].Blocks[0].Text, "T") {
		t.Fatalf("honours max tokens: %+v", req)
	}
	req = summaryRequest("m", "T", "", model.Info{Capabilities: model.Capabilities{IgnoresMaxTokens: true}})
	if req.MaxTokens != 0 || !strings.Contains(req.System, "under "+summaryWords+" words") {
		t.Fatalf("ignores max tokens: %+v", req)
	}
}

// TestSummaryRequestCarriesThePreviousSummary: with an earlier summary the
// call carries both and says the transcript wins where they disagree; the
// template shapes every summary.
func TestSummaryRequestCarriesThePreviousSummary(t *testing.T) {
	req := summaryRequest("m", "TRANSCRIPT", "EARLIER", model.Info{})
	text := req.Messages[0].Blocks[0].Text
	if !strings.Contains(text, "EARLIER") || !strings.Contains(text, "TRANSCRIPT") || strings.Index(text, "EARLIER") > strings.Index(text, "TRANSCRIPT") {
		t.Fatalf("both, earlier first: %q", text)
	}
	if !strings.Contains(req.System, "Merge both") || !strings.Contains(req.System, "the transcript is newer and wins") || !strings.Contains(req.System, "## Next") {
		t.Fatalf("system: %q", req.System)
	}
	if plain := summaryRequest("m", "TRANSCRIPT", "", model.Info{}); strings.Contains(plain.System, "Merge both") || !strings.Contains(plain.System, "## Task") {
		t.Fatalf("without an earlier summary: %q", plain.System)
	}
}
