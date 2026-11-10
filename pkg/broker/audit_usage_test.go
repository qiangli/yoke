package broker

import "testing"

func TestParseUsageCacheAndAnthropicStream(t *testing.T) {
	for _, tc := range []struct {
		name, body                         string
		prompt, output, cached, cacheWrite int
	}{
		{"openai", `{"usage":{"prompt_tokens":30,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":12}}}`, 30, 4, 12, 0},
		{"anthropic", `{"usage":{"input_tokens":3,"cache_creation_input_tokens":5,"cache_read_input_tokens":7,"output_tokens":2}}`, 15, 2, 7, 5},
		{"anthropic sse", "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3,\"cache_creation_input_tokens\":5,\"cache_read_input_tokens\":7}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":0,\"output_tokens\":2}}\n\n", 15, 2, 7, 5},
		{"cligw", `{"usage":{"prompt_tokens":15,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":7,"cache_write_tokens":5}}}`, 15, 2, 7, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prompt, output, cached, cacheWrite := parseUsage([]byte(tc.body))
			if prompt != tc.prompt || output != tc.output || cached != tc.cached || cacheWrite != tc.cacheWrite {
				t.Fatalf("usage = %d/%d/%d/%d, want %d/%d/%d/%d", prompt, output, cached, cacheWrite, tc.prompt, tc.output, tc.cached, tc.cacheWrite)
			}
		})
	}
}
