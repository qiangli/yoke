package cligw

// Sprint: #290 (G0.3 cost per solve compares vendors)

import (
	"bytes"
	"testing"

	"github.com/qiangli/yoke/pkg/llmgw/openai"
)

func TestUsageCountsTheWholePrompt(t *testing.T) {
	claude := usageFromEvent([]byte(`{"type":"result","usage":{"input_tokens":2,"cache_creation_input_tokens":1500,"cache_read_input_tokens":12000,"output_tokens":80}}`))
	if claude.InputTokens != 13502 || claude.CachedInputTokens != 12000 || claude.CacheWriteTokens != 1500 || claude.TotalTokens != 13582 {
		t.Errorf("claude usage = %+v, want input 13502 (2+1500+12000), cached 12000, total 13582", claude)
	}
	codex := usageFromEvent([]byte(`{"type":"turn.completed","usage":{"input_tokens":8000,"cached_input_tokens":6000,"output_tokens":17}}`))
	if codex.InputTokens != 8000 || codex.CachedInputTokens != 6000 || codex.CacheWriteTokens != 0 || codex.TotalTokens != 8017 {
		t.Errorf("codex usage = %+v, want input 8000 (includes cached), total 8017", codex)
	}
	ycode := usageFromEvent([]byte(`{"type":"turn.completed","usage":{"prompt_tokens":25,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":7}}}`))
	if ycode.InputTokens != 25 || ycode.CachedInputTokens != 7 || ycode.CacheWriteTokens != 0 || ycode.TotalTokens != 29 {
		t.Errorf("ycode usage = %+v, want input 25 cached 7 total 29", ycode)
	}
	ycodePayload := usageFromEvent([]byte(`{"type":"result","result":"ok","response":{"usage":{"input_tokens":17,"output_tokens":3,"cache_read_input_tokens":5}}}`))
	if ycodePayload.InputTokens != 17 || ycodePayload.CachedInputTokens != 5 || ycodePayload.TotalTokens != 20 {
		t.Errorf("ycode provider payload = %+v, want input 17 cached 5 total 20", ycodePayload)
	}
}

func TestEncodeChatIncludesCacheWriteInUsage(t *testing.T) {
	for _, stream := range []bool{false, true} {
		_, body := EncodeChat(openai.ChatRequest{Stream: stream}, "ok", "stop", "id", nil, Usage{
			InputTokens: 15, CachedInputTokens: 7, CacheWriteTokens: 5, OutputTokens: 2, TotalTokens: 17,
		})
		if !bytes.Contains(body, []byte(`"cache_write_tokens":5`)) || !bytes.Contains(body, []byte(`"cached_tokens":7`)) {
			t.Errorf("stream=%v body missing cache counts: %s", stream, body)
		}
	}
}
