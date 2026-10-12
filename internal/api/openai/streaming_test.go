package openai

import (
	"testing"

	"git.gasi.dev/robin/kiro-proxy/internal/streaming"
)

func TestReasoningRemoveNeverEmitsVisibleContent(t *testing.T) {
	acc := newAccumulator("remove", nil)

	delta := acc.apply(streaming.KiroEvent{
		Type:            streaming.EventThinking,
		ThinkingContent: "private analysis",
	})

	if delta != nil {
		t.Fatalf("remove emitted visible delta %#v", delta)
	}
	if acc.thinking != "private analysis" {
		t.Fatalf("internal thinking = %q, want usage-preserving accumulation", acc.thinking)
	}
}
