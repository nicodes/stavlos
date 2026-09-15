package agent

import (
	"strings"
	"testing"

	"github.com/nicodes/stavlos/internal/model"
)

// TestSummaryRequestBound: the summary is bounded in tokens where the model
// honours max tokens, and in words in the prompt where it does not.
func TestSummaryRequestBound(t *testing.T) {
	req := summaryRequest("m", "T", model.Info{})
	if req.MaxTokens != summaryMaxTokens || strings.Contains(req.System, "words") || req.Model != "m" || !strings.HasSuffix(req.Messages[0].Blocks[0].Text, "T") {
		t.Fatalf("honours max tokens: %+v", req)
	}
	req = summaryRequest("m", "T", model.Info{Capabilities: model.Capabilities{IgnoresMaxTokens: true}})
	if req.MaxTokens != 0 || !strings.Contains(req.System, "under "+summaryWords+" words") {
		t.Fatalf("ignores max tokens: %+v", req)
	}
}
