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
	ycode := usageFromEvent([]byte(`{"type":"turn.completed","usage":{"prompt_tokens":25,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":7}}}`))
	if ycode.InputTokens != 25 || ycode.CachedInputTokens != 7 || ycode.TotalTokens != 29 {
		t.Errorf("ycode usage = %+v, want input 25 cached 7 total 29", ycode)
	}
	ycodePayload := usageFromEvent([]byte(`{"type":"result","result":"ok","response":{"usage":{"input_tokens":17,"output_tokens":3,"cache_read_input_tokens":5}}}`))
	if ycodePayload.InputTokens != 17 || ycodePayload.CachedInputTokens != 5 || ycodePayload.TotalTokens != 20 {
		t.Errorf("ycode provider payload = %+v, want input 17 cached 5 total 20", ycodePayload)
	}
}
