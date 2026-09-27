package broker

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Every served request leaves one run record (principal, session, class,
// backend, model + digest, options, sticky key + identity + use, tokens,
// wait, wall, status): the audit trail Q8 asks for and the evidence a
// benchmark row cites. The same records, filtered by session lineage, are
// the session's turns (Q5).

// Record is one served request.
type Record struct {
	Time        time.Time      `json:"time"`
	Principal   string         `json:"principal"`
	Session     string         `json:"session,omitempty"`
	Class       string         `json:"class"`
	Method      string         `json:"method"`
	Path        string         `json:"path"`
	Backend     string         `json:"backend"`
	Model       string         `json:"model,omitempty"`
	ModelDigest string         `json:"model_digest,omitempty"`
	Options     map[string]any `json:"options,omitempty"`
	Sticky      string         `json:"sticky,omitempty"`
	Identity    string         `json:"identity,omitempty"`
	Use         int            `json:"use,omitempty"`
	Routed      string         `json:"routed,omitempty"`
	Status      int            `json:"status"`
	WaitMS      int64          `json:"wait_ms"`
	WallMS      int64          `json:"wall_ms"`
	PromptTok   int            `json:"prompt_tokens,omitempty"`
	OutputTok   int            `json:"completion_tokens,omitempty"`
	Error       string         `json:"error,omitempty"`
}

// auditLog appends records to a JSONL file and keeps a bounded in-memory
// ring for the session turn view.
type auditLog struct {
	mu   sync.Mutex
	path string
	ring []Record
	cap  int
}

func newAuditLog(path string) *auditLog { return &auditLog{path: path, cap: 10000} }

func (a *auditLog) append(r Record) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ring = append(a.ring, r)
	if len(a.ring) > a.cap {
		a.ring = a.ring[len(a.ring)-a.cap:]
	}
	if a.path == "" {
		return
	}
	data, err := json.Marshal(r)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(a.path), 0o700)
	f, err := os.OpenFile(a.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write(append(data, '\n'))
	_ = f.Close()
}

// snapshot copies the ring.
func (a *auditLog) snapshot() []Record {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Record(nil), a.ring...)
}

// turns returns the records visible to (principal, session): its own, and
// its ancestors' per the lineage rules. A turn's visibility is that of a
// non-exported item in the session that made it — a subshell's turns never
// flow back, and a child sees none of its parent's turns.
func (a *auditLog) turns(principal, session string) []Record {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []Record
	for _, r := range a.ring {
		if r.Session == "" {
			continue
		}
		if visible(scoped{Principal: r.Principal, Session: r.Session, Created: r.Time}, principal, session) {
			out = append(out, r)
		}
	}
	return out
}

// captureWriter records the status and keeps the response tail so usage can
// be parsed after the body has streamed through. Flush is forwarded so a
// streaming response is never buffered behind it.
type captureWriter struct {
	http.ResponseWriter
	status int
	tail   []byte
	max    int
	wrote  bool
}

func newCaptureWriter(w http.ResponseWriter) *captureWriter {
	return &captureWriter{ResponseWriter: w, max: 64 << 10}
}

func (c *captureWriter) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *captureWriter) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	c.wrote = true
	c.tail = append(c.tail, p...)
	if len(c.tail) > c.max {
		c.tail = c.tail[len(c.tail)-c.max:]
	}
	return c.ResponseWriter.Write(p)
}

func (c *captureWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (c *captureWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// parseTokens reads token counts from a response tail: Ollama native
// (prompt_eval_count/eval_count on the final object), OpenAI (usage.
// prompt_tokens/completion_tokens) or Anthropic (usage.input_tokens/
// output_tokens), JSON or SSE/NDJSON.
func parseTokens(tail []byte) (prompt, output int) {
	lines := bytes.Split(tail, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		line = bytes.TrimPrefix(line, []byte("data:"))
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var v struct {
			PromptEval int `json:"prompt_eval_count"`
			Eval       int `json:"eval_count"`
			Usage      *struct {
				Prompt     int `json:"prompt_tokens"`
				Completion int `json:"completion_tokens"`
				Input      int `json:"input_tokens"`
				Output     int `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(line, &v) != nil {
			continue
		}
		if v.Usage != nil && (v.Usage.Prompt+v.Usage.Completion+v.Usage.Input+v.Usage.Output) > 0 {
			return v.Usage.Prompt + v.Usage.Input, v.Usage.Completion + v.Usage.Output
		}
		if v.PromptEval+v.Eval > 0 {
			return v.PromptEval, v.Eval
		}
	}
	return 0, 0
}
