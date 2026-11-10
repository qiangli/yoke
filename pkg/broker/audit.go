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

// Record is one served request. Ignored carries the request parameters the
// serving backend could not honor (X-Bashy-Ignored): a recorded run's numbers
// were not produced under them, however the request was written.
type Record struct {
	Time          time.Time      `json:"time"`
	Principal     string         `json:"principal"`
	Session       string         `json:"session,omitempty"`
	Class         string         `json:"class"`
	Method        string         `json:"method"`
	Path          string         `json:"path"`
	Backend       string         `json:"backend"`
	Model         string         `json:"model,omitempty"`
	UpstreamModel string         `json:"upstream_model,omitempty"`
	ModelDigest   string         `json:"model_digest,omitempty"`
	Options       map[string]any `json:"options,omitempty"`
	Sticky        string         `json:"sticky,omitempty"`
	Identity      string         `json:"identity,omitempty"`
	Use           int            `json:"use,omitempty"`
	Routed        string         `json:"routed,omitempty"`
	Ignored       string         `json:"ignored,omitempty"`
	Status        int            `json:"status"`
	WaitMS        int64          `json:"wait_ms"`
	WallMS        int64          `json:"wall_ms"`
	PromptTok     int            `json:"prompt_tokens,omitempty"`
	CachedTok     int            `json:"cached_tokens,omitempty"`
	OutputTok     int            `json:"completion_tokens,omitempty"`
	Error         string         `json:"error,omitempty"`
	// MemberListAgeMS is present for every member-bound peer decision. Offline
	// says the master was unreachable and the last known list was used.
	MemberListAgeMS   *int64 `json:"member_list_age_ms,omitempty"`
	MemberListOffline bool   `json:"member_list_offline,omitempty"`

	// CostUSD is the estimated USD cost of this request's tokens at the
	// llmprice table's rate for the model that served them. It is present
	// only when the table prices the model: an unknown or local model leaves
	// it out rather than writing a number that looks measured.
	CostUSD float64 `json:"cost_usd,omitempty"`
	// Unpriced is true when no price applies: the table has no row for the
	// model (an id nobody has priced) or the model is a local engine model
	// (ollama) whose cost is the host's electricity, not a list price. It is
	// the door saying "this request has no dollar figure", which is what a
	// usage report and an owner alarm need to tell apart from $0.
	Unpriced bool `json:"unpriced,omitempty"`
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
	status    int
	tail      []byte
	max       int
	wrote     bool
	full      []byte
	truncated bool
	capture   bool
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
	if c.capture {
		remaining := captureLimit - len(c.full)
		if len(p) > remaining {
			c.truncated = true
			c.full = append(c.full, p[:remaining]...)
		} else {
			c.full = append(c.full, p...)
		}
	}
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
	prompt, output, _ = parseUsage(tail)
	return
}

func parseUsage(tail []byte) (prompt, output, cached int) {
	lines := bytes.Split(tail, []byte("\n"))
	gotPrompt, gotOutput, gotCached := false, false, false
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		line = bytes.TrimPrefix(line, []byte("data:"))
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var v struct {
			Type       string `json:"type"`
			PromptEval int    `json:"prompt_eval_count"`
			Eval       int    `json:"eval_count"`
			Usage      *struct {
				Prompt     *int `json:"prompt_tokens"`
				Completion *int `json:"completion_tokens"`
				Input      *int `json:"input_tokens"`
				Output     *int `json:"output_tokens"`
				CacheRead  *int `json:"cache_read_input_tokens"`
				CacheWrite *int `json:"cache_creation_input_tokens"`
				Details    *struct {
					Cached int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
			Message *struct {
				Usage *struct {
					Input      *int `json:"input_tokens"`
					CacheRead  *int `json:"cache_read_input_tokens"`
					CacheWrite *int `json:"cache_creation_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &v) != nil {
			continue
		}
		if v.Usage != nil {
			u := v.Usage
			if !gotPrompt && v.Type != "message_delta" && (u.Prompt != nil || u.Input != nil || u.CacheRead != nil || u.CacheWrite != nil) {
				if u.Prompt != nil {
					prompt = *u.Prompt
				} else {
					if u.Input != nil {
						prompt += *u.Input
					}
					if u.CacheRead != nil {
						prompt += *u.CacheRead
					}
					if u.CacheWrite != nil {
						prompt += *u.CacheWrite
					}
				}
				gotPrompt = true
			}
			if !gotOutput && (u.Completion != nil || u.Output != nil) {
				if u.Completion != nil {
					output = *u.Completion
				} else {
					output = *u.Output
				}
				gotOutput = true
			}
			if !gotCached && (u.Details != nil || u.CacheRead != nil) {
				if u.Details != nil {
					cached = u.Details.Cached
				} else {
					cached = *u.CacheRead
				}
				gotCached = true
			}
		}
		if v.Message != nil && v.Message.Usage != nil {
			u := v.Message.Usage
			if !gotPrompt && (u.Input != nil || u.CacheRead != nil || u.CacheWrite != nil) {
				if u.Input != nil {
					prompt += *u.Input
				}
				if u.CacheRead != nil {
					prompt += *u.CacheRead
				}
				if u.CacheWrite != nil {
					prompt += *u.CacheWrite
				}
				gotPrompt = true
			}
			if !gotCached && u.CacheRead != nil {
				cached = *u.CacheRead
				gotCached = true
			}
		}
		if v.PromptEval+v.Eval > 0 {
			if !gotPrompt {
				prompt = v.PromptEval
				gotPrompt = true
			}
			if !gotOutput {
				output = v.Eval
				gotOutput = true
			}
		}
	}
	return
}

// refusalReason is the error message a refused response carried, so the
// audit record of a refusal says why — whichever surface wrote it (the
// door's own writeErr in any dialect, or a cligw/engine error body).
func refusalReason(status int, tail []byte) string {
	if status < 400 {
		return ""
	}
	var v struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(bytes.TrimSpace(tail), &v) == nil && len(v.Error) > 0 {
		var msg string
		if json.Unmarshal(v.Error, &msg) == nil && msg != "" {
			return msg
		}
		var obj struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(v.Error, &obj) == nil && obj.Message != "" {
			return obj.Message
		}
	}
	return http.StatusText(status)
}
