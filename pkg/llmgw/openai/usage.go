package openai

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

// UsageTailSize is how many trailing bytes of the backend response a
// caller should keep for usage extraction. Usage blocks live at the
// tail in both shapes (non-streaming: end of the JSON; streaming:
// last data chunk before [DONE]). 8 KB is generous — a usage block is
// ~80 bytes.
const UsageTailSize = 8 << 10

// Usage is the {prompt_tokens, completion_tokens, total_tokens}
// triple every OpenAI-compat endpoint emits. Ollama's /v1/* surface
// mirrors this shape verbatim. Pointer-bool isn't needed: missing
// fields decode as zero, and zero is a perfectly fine "unknown."
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// AuditTailReader wraps a backend response body so the reverse proxy
// can stream chunks through to the client untouched while the last N
// bytes accumulate in memory. On Close (which httputil.ReverseProxy
// calls after the full body is forwarded) the onClose callback fires
// asynchronously so the bookkeeping doesn't slow the client response.
type AuditTailReader struct {
	src     io.ReadCloser
	tail    []byte
	max     int
	onClose func(tail []byte, readErr error)
	readErr error
	closed  bool
}

// NewAuditTailReader wraps src, keeping at most max trailing bytes.
// onClose (optional) fires in its own goroutine once the body is
// closed, with a snapshot of the tail and any read error observed.
func NewAuditTailReader(src io.ReadCloser, max int, onClose func([]byte, error)) *AuditTailReader {
	return &AuditTailReader{
		src:     src,
		tail:    make([]byte, 0, max),
		max:     max,
		onClose: onClose,
	}
}

func (t *AuditTailReader) Read(p []byte) (int, error) {
	n, err := t.src.Read(p)
	if n > 0 {
		t.appendTail(p[:n])
	}
	if err != nil && err != io.EOF {
		t.readErr = err
	}
	return n, err
}

func (t *AuditTailReader) appendTail(b []byte) {
	if len(b) >= t.max {
		// Single read bigger than the buffer — keep the trailing slice.
		t.tail = append(t.tail[:0], b[len(b)-t.max:]...)
		return
	}
	if len(t.tail)+len(b) <= t.max {
		t.tail = append(t.tail, b...)
		return
	}
	// Shift: drop enough from the front to make room.
	keep := t.max - len(b)
	t.tail = append(t.tail[:0], t.tail[len(t.tail)-keep:]...)
	t.tail = append(t.tail, b...)
}

func (t *AuditTailReader) Close() error {
	err := t.src.Close()
	if t.closed {
		return err
	}
	t.closed = true
	if t.onClose != nil {
		// Copy the tail so the callback can run in another goroutine
		// without racing future writes (there won't be any after Close,
		// but the buffer is reused by sync.Pool elsewhere — defensive).
		snap := append([]byte(nil), t.tail...)
		re := t.readErr
		go t.onClose(snap, re)
	}
	return err
}

// ParseUsage walks the tail bytes looking for an OpenAI-style usage
// block. Tries two shapes in order:
//  1. The tail is a complete JSON document with a top-level "usage"
//     key (non-streaming response, when the whole body fit in 8 KB).
//  2. The tail contains SSE chunks (`data: {...}\n\n`). Walks each
//     chunk back-to-front and returns the first one carrying usage.
//
// Returns the zero value when no usage was found — that's fine for
// the caller, the absence signals "client didn't ask for usage" or
// "the backend doesn't emit one for this shape" (e.g. Ollama's native
// /api/chat).
func ParseUsage(tail []byte) Usage {
	tail = bytes.TrimSpace(tail)
	if len(tail) == 0 {
		return Usage{}
	}
	// Shape 1: bare JSON object with usage at the top level.
	if tail[0] == '{' {
		var doc struct {
			Usage Usage `json:"usage"`
		}
		if err := json.Unmarshal(tail, &doc); err == nil && (doc.Usage.PromptTokens|doc.Usage.CompletionTokens|doc.Usage.TotalTokens) != 0 {
			return doc.Usage
		}
	}
	// Shape 2: SSE chunks. Walk back-to-front so we find the last
	// usage-bearing chunk (some clients send several; the final one
	// is canonical). Also tolerates a leading partial chunk that
	// got truncated when the tail filled up — we just skip
	// unparseable chunks.
	lines := strings.Split(string(tail), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var doc struct {
			Usage *Usage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &doc); err != nil {
			continue
		}
		if doc.Usage != nil &&
			(doc.Usage.PromptTokens|doc.Usage.CompletionTokens|doc.Usage.TotalTokens) != 0 {
			return *doc.Usage
		}
	}
	return Usage{}
}
