package cligw

// Sprint: #290 (G0.3 cost per solve compares vendors)

import "testing"

func TestUsageCountsTheWholePrompt(t *testing.T) {
	claude := usageFromEvent([]byte(`{"type":"result","usage":{"input_tokens":2,"cache_creation_input_tokens":1500,"cache_read_input_tokens":12000,"output_tokens":80}}`))
	if claude.InputTokens != 13502 || claude.CachedInputTokens != 12000 || claude.TotalTokens != 13582 {
		t.Errorf("claude usage = %+v, want input 13502 (2+1500+12000), cached 12000, total 13582", claude)
	}
	codex := usageFromEvent([]byte(`{"type":"turn.completed","usage":{"input_tokens":8000,"cached_input_tokens":6000,"output_tokens":17}}`))
	if codex.InputTokens != 8000 || codex.CachedInputTokens != 6000 || codex.TotalTokens != 8017 {
		t.Errorf("codex usage = %+v, want input 8000 (includes cached), total 8017", codex)
	}
}
