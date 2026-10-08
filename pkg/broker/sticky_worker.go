package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/qiangli/yoke/pkg/cligw"
	"github.com/qiangli/yoke/pkg/llmgw/openai"
)

// Sticky CLI sessions (bind=worker/reset=none): the broker holds one warm CLI
// per binding and forwards only the new turn. The transcript prefix was
// already verified by sticky.use before this runs, so the delta is the suffix
// past the recorded transcript. A turn that changes the frozen system
// instructions or tool set is 409, never silently dropped or reset. A turn
// the worker fails retires the worker and rolls the transcript back, so the
// retry restores the full conversation on a fresh worker (502, never a silent
// reset and never a stuck empty delta).

// stickyWorker is one warm CLI held for a binding. *cligw.StickySession is
// the production implementation; tests substitute a fake.
type stickyWorker interface {
	Turn(ctx context.Context, prompt cligw.CompletionPrompt, onEvent func(cligw.Event)) (cligw.Result, error)
	Close() error
}

// StickyDialer is an optional CLIBackend capability: reserve one warm CLI for
// a sticky binding. A backend without it serves bind=worker/reset=none with
// 501. cligw's server implements it for stdin-stream-json tools.
type StickyDialer interface {
	DialSticky(ctx context.Context, agent string) (stickyWorker, error)
}

// stickyEntry is one binding's held worker plus the conversation shape its
// first turn froze: the explicit system/developer instructions and the tool
// set the CLI was started with.
type stickyEntry struct {
	sess     stickyWorker
	agent    string
	sysSig   string
	toolsSig string
}

// dropStickyWorker retires the binding's held worker, if any. It is the
// eviction path for binding delete and TTL expiry as well as failed turns.
func (b *Broker) dropStickyWorker(key string) {
	b.stickyWMu.Lock()
	entry := b.stickyW[key]
	delete(b.stickyW, key)
	b.stickyWMu.Unlock()
	if entry != nil {
		_ = entry.sess.Close()
	}
}

// evictStickyWorker retires a binding's worker when the binding goes away.
func (b *Broker) evictStickyWorker(principal, key string) {
	b.dropStickyWorker(storeKey(principal, key))
}

// serveStickyWorker runs one turn of a bind=worker/reset=none CLI binding on
// its held session. binding is the pre-use copy, so binding.Transcript is the
// recorded prefix the new messages must extend.
func (b *Broker) serveStickyWorker(w http.ResponseWriter, r *http.Request, ri *reqInfo, binding *Binding, payload map[string]json.RawMessage) {
	agent := binding.Identity.Agent
	dialer, ok := b.opts.CLI.(StickyDialer)
	if !ok {
		writeErr(w, r.URL.Path, http.StatusNotImplemented,
			fmt.Sprintf("sticky bind=worker / reset=none on agent %q is not available yet (this door's CLI backend does not dial sticky workers); use bind=identity reset=each", agent))
		return
	}
	var messages []json.RawMessage
	_ = json.Unmarshal(payload["messages"], &messages)
	old := append([]string(nil), binding.Transcript...)
	offset := len(old)
	if len(messages) < offset {
		// sticky.use already refused this as a divergence; stay defensive.
		offset = len(messages)
	}
	var req openai.ChatRequest
	if tmp, err := json.Marshal(payload); err != nil || json.Unmarshal(tmp, &req) != nil {
		writeErr(w, r.URL.Path, http.StatusBadRequest, "sticky: the request body is not a chat request")
		return
	}
	req.Model = agent
	if len(req.Messages) != len(messages) {
		writeErr(w, r.URL.Path, http.StatusBadRequest, "sticky: messages are not chat messages")
		return
	}
	if len(messages)-offset == 0 {
		writeErr(w, r.URL.Path, http.StatusBadRequest, "sticky: no new messages after the recorded transcript")
		return
	}

	key := storeKey(ri.principal, binding.Spec.Key)
	toolsNow := toolsSig(req)
	b.stickyWMu.Lock()
	entry := b.stickyW[key]
	if entry != nil && entry.agent != agent {
		// The binding's identity cannot change while the key lives (put
		// refuses a second identity), so this is unreachable in practice.
		// Retire defensively rather than serve one agent on another's worker.
		stale := entry
		entry = nil
		delete(b.stickyW, key)
		b.stickyWMu.Unlock()
		_ = stale.sess.Close()
		b.stickyWMu.Lock()
	}
	fresh := entry == nil
	if !fresh {
		if sysNow := explicitSystem(req.Messages[offset:]); sysNow != "" && entry.sysSig != sysNow {
			b.stickyWMu.Unlock()
			writeErr(w, r.URL.Path, http.StatusConflict,
				fmt.Sprintf("sticky: key %q holds a conversation with different instructions; a held worker cannot change them mid-session", binding.Spec.Key))
			return
		}
		if toolsNow != entry.toolsSig {
			b.stickyWMu.Unlock()
			writeErr(w, r.URL.Path, http.StatusConflict,
				fmt.Sprintf("sticky: key %q holds a conversation with a different tool set; a held worker cannot change it mid-session", binding.Spec.Key))
			return
		}
	}
	b.stickyWMu.Unlock()

	// Render before dialing: a malformed turn is 400 with no side effects,
	// never an orphaned worker. A new worker restores the whole recorded
	// conversation; a held one gets only the new turn.
	deltaReq := req
	if fresh {
		deltaReq.Messages = req.Messages
	} else {
		deltaReq.Messages = req.Messages[offset:]
	}
	turn, err := cligw.RenderCompletionPrompt(&deltaReq)
	if err != nil {
		writeErr(w, r.URL.Path, http.StatusBadRequest, fmt.Sprintf("sticky: %v", err))
		return
	}

	if fresh {
		sess, err := dialer.DialSticky(r.Context(), agent)
		if err != nil {
			switch {
			case errors.Is(err, cligw.ErrStickyUnsupported):
				writeErr(w, r.URL.Path, http.StatusNotImplemented, fmt.Sprintf("sticky bind=worker / reset=none: %v", err))
			case errors.Is(err, cligw.ErrStickyCapped):
				w.Header().Set("Retry-After", "5")
				writeErr(w, r.URL.Path, http.StatusTooManyRequests, fmt.Sprintf("sticky bind=worker / reset=none: %v", err))
			default:
				writeErr(w, r.URL.Path, http.StatusBadGateway, fmt.Sprintf("sticky %q: dial worker: %v", binding.Spec.Key, err))
			}
			return
		}
		b.stickyWMu.Lock()
		if live := b.stickyW[key]; live != nil && live.agent == agent {
			entry = live
			fresh = false
		} else {
			if live != nil {
				_ = live.sess.Close()
			}
			entry = &stickyEntry{sess: sess, agent: agent, sysSig: explicitSystem(req.Messages), toolsSig: toolsNow}
			b.stickyW[key] = entry
		}
		b.stickyWMu.Unlock()
		if !fresh {
			_ = sess.Close()
		}
	}

	var deltas []string
	result, err := entry.sess.Turn(r.Context(), turn, func(ev cligw.Event) {
		if ev.Text != "" {
			deltas = append(deltas, ev.Text)
		}
	})
	if err != nil || result.Outcome == cligw.OutcomeError {
		message := "worker reported an error outcome"
		if err != nil {
			message = err.Error()
		}
		b.dropStickyWorker(key)
		b.sticky.restoreTranscript(ri.principal, ri.session, binding.Spec.Key, old)
		writeErr(w, r.URL.Path, http.StatusBadGateway, fmt.Sprintf("sticky %q: %s", binding.Spec.Key, message))
		return
	}

	text, finish := cligw.FinishChat(deltaReq, result.Text)
	if result.Usage.Estimated {
		w.Header().Set("X-Bashy-Usage-Estimated", "true")
	}
	contentType, body := cligw.EncodeChat(deltaReq, text, finish, cligw.NewCompletionID(), deltas, result.Usage)
	ri.rec.Model = agent
	ri.rec.PromptTok, ri.rec.OutputTok = int(result.Usage.InputTokens), int(result.Usage.OutputTokens)
	b.finish(ri, http.StatusOK, 0, "")
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// explicitSystem joins the canonical system/developer message contents: the
// instruction channel a held worker was started with. Empty means the turn
// (or conversation) carries none and inherits the frozen value.
func explicitSystem(messages []openai.ChatMessage) string {
	var parts []string
	for _, m := range messages {
		switch strings.ToLower(strings.TrimSpace(m.Role)) {
		case "system", "developer":
			parts = append(parts, string(canonicalJSON(m.Content)))
		}
	}
	return strings.Join(parts, "\n\n")
}

// toolsSig freezes the request-level tool set a held worker was started
// with. Messages travel per turn; tools do not, so any change is a 409.
func toolsSig(req openai.ChatRequest) string {
	raw, _ := json.Marshal(struct {
		Tools  []openai.Tool `json:"tools"`
		Choice any           `json:"choice"`
	}{req.Tools, req.ToolChoice})
	return string(raw)
}

func canonicalJSON(raw json.RawMessage) json.RawMessage {
	trimmed := json.RawMessage(strings.TrimSpace(string(raw)))
	if len(trimmed) == 0 {
		return json.RawMessage("null")
	}
	var v any
	if json.Unmarshal(trimmed, &v) != nil {
		return trimmed
	}
	out, err := json.Marshal(v)
	if err != nil {
		return trimmed
	}
	return out
}
