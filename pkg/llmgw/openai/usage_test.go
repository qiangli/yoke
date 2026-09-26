package openai

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestParseUsage_NonStreamingJSON(t *testing.T) {
	tail := []byte(`{"id":"x","usage":{"prompt_tokens":151,"completion_tokens":17,"total_tokens":168}}`)
	got := ParseUsage(tail)
	if got.PromptTokens != 151 || got.CompletionTokens != 17 || got.TotalTokens != 168 {
		t.Errorf("got %+v, want 151/17/168", got)
	}
}

func TestParseUsage_SSELastChunkWins(t *testing.T) {
	tail := []byte(strings.Join([]string{
		`data: {"id":"x","choices":[{"delta":{"content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		``,
		`data: {"id":"x","choices":[{"delta":{}}],"usage":{"prompt_tokens":5,"completion_tokens":6,"total_tokens":11}}`,
		``,
		`data: [DONE]`,
		``,
		``,
	}, "\n"))
	got := ParseUsage(tail)
	if got.PromptTokens != 5 || got.CompletionTokens != 6 || got.TotalTokens != 11 {
		t.Errorf("got %+v, want the last usage-bearing chunk 5/6/11", got)
	}
}

// A truncated leading chunk (the tail filled up mid-stream) must not
// stop the back-to-front walk from finding the real usage block.
func TestParseUsage_TolerateTruncatedLeadingChunk(t *testing.T) {
	tail := []byte("ent\":\"tail of a cut chunk\"}}]}\n\n" +
		`data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}` + "\n\n")
	got := ParseUsage(tail)
	if got.TotalTokens != 7 {
		t.Errorf("got %+v, want total=7", got)
	}
}

func TestParseUsage_NoneFound(t *testing.T) {
	if got := ParseUsage([]byte("  ")); got != (Usage{}) {
		t.Errorf("got %+v, want zero value for empty tail", got)
	}
	if got := ParseUsage([]byte(`{"id":"x","choices":[]}`)); got != (Usage{}) {
		t.Errorf("got %+v, want zero value when no usage block", got)
	}
}

func TestAuditTailReader_KeepsTrailingBytesAndFiresOnClose(t *testing.T) {
	const body = "0123456789abcdef"
	done := make(chan []byte, 1)
	r := NewAuditTailReader(io.NopCloser(strings.NewReader(body)), 4, func(tail []byte, err error) {
		if err != nil {
			t.Errorf("unexpected read err: %v", err)
		}
		done <- tail
	})
	// Read in small bites so the tail buffer has to shift.
	buf := make([]byte, 3)
	for {
		if _, err := r.Read(buf); err != nil {
			break
		}
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	tail := <-done
	if string(tail) != "cdef" {
		t.Errorf("tail=%q, want %q", tail, "cdef")
	}
}

// A single read larger than the buffer keeps only the trailing slice.
func TestAuditTailReader_ReadBiggerThanBuffer(t *testing.T) {
	done := make(chan []byte, 1)
	r := NewAuditTailReader(io.NopCloser(strings.NewReader("0123456789")), 3, func(tail []byte, err error) {
		done <- tail
	})
	if _, err := io.ReadAll(r); err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	if tail := <-done; string(tail) != "789" {
		t.Errorf("tail=%q, want %q", tail, "789")
	}
}

func TestAuditTailReader_ReportsReadError(t *testing.T) {
	want := errors.New("boom")
	done := make(chan error, 1)
	r := NewAuditTailReader(&errReader{err: want}, 8, func(_ []byte, err error) {
		done <- err
	})
	if _, err := io.ReadAll(r); err == nil {
		t.Fatal("expected the read error to surface")
	}
	_ = r.Close()
	if got := <-done; !errors.Is(got, want) {
		t.Errorf("onClose err=%v, want %v", got, want)
	}
}

type errReader struct{ err error }

func (e *errReader) Read([]byte) (int, error) { return 0, e.err }
func (e *errReader) Close() error             { return nil }
