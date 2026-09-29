package cligw

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/llmgw/anthropic"
	"github.com/qiangli/yoke/pkg/llmgw/openai"
	"github.com/qiangli/yoke/pkg/llmgw/sched"
	"github.com/qiangli/yoke/pkg/toolcmd"
)

// Tool commands over the llm API (Sprint #324 S5, dhnt
// docs/tool-commands-design.md §4).
//
// The filter key slash=<canonical-name> does two things:
//
//  1. FILTER. Filter.Match keeps only agents whose tool declares a command of
//     that name, so band / auto / model / agent selection and quota policy
//     choose among the capable agents exactly as they choose among all agents.
//     A name no tool declares is 400 (with the declared names); a selector with
//     no capable agent is 404 (naming the unmet requirement).
//  2. EXECUTE. A routed request that carries slash= runs the command through
//     toolcmd.Run on the chosen agent — vendor features ON — instead of a
//     tools-off completion worker. The last user message is the command's
//     {args}. The reply is an ordinary chat completion (or Anthropic message);
//     the structured toolcmd envelope rides along as x_bashy_tool_command.
//
// No new model ids: the caller still asks for a band, a model or an agent.
//
// slash= is a PER-REQUEST key. Policy.Validate refuses it as a door-wide
// default (it would turn every completion into a command run), and the broker
// refuses it on a sticky binding (a slash request is not a completion from a
// frozen identity; see pkg/broker).

// ToolCommandHeader names the command a slash request ran: "<tool>:<name>".
const ToolCommandHeader = "X-Bashy-Tool-Command"

// Seams (tests replace them; production runs the real runner).
var (
	// runToolCommand runs one tool command. Unit tests inject a fake: they
	// must never spawn a real agent CLI.
	runToolCommand = toolcmd.Run
	// slashKeepalive is the SSE keepalive period of a streaming slash request.
	slashKeepalive = 15 * time.Second
)

// UnknownSlashError is a slash= name no tool in the fleet declares (400).
type UnknownSlashError struct {
	Name     string
	Declared []string
}

func (e *UnknownSlashError) Error() string {
	if len(e.Declared) == 0 {
		return fmt.Sprintf("cligw: no tool declares command %q (slash=%s); no tool in the fleet declares any command", e.Name, e.Name)
	}
	return fmt.Sprintf("cligw: no tool declares command %q (slash=%s); declared commands: %s",
		e.Name, e.Name, strings.Join(e.Declared, ", "))
}

// DeclaredCommands is the sorted union of command names every fleet tool
// declares — launchable or not, so a 400 lists the whole vocabulary in use.
func (c *FleetCatalog) DeclaredCommands() []string {
	tools, _ := c.fleet.Tools(false)
	var names []string
	for _, t := range tools {
		names = append(names, commandNames(t)...)
	}
	sort.Strings(names)
	return slices.Compact(names)
}

// toolsDeclaring names the tools that declare command name.
func (c *FleetCatalog) toolsDeclaring(name string) []string {
	tools, _ := c.fleet.Tools(false)
	var out []string
	for _, t := range tools {
		if _, ok := t.Command(name); ok {
			out = append(out, t.Name)
		}
	}
	sort.Strings(out)
	return out
}

// CheckSlash validates a slash= name against the fleet: nil when empty or
// declared by at least one tool, else *UnknownSlashError.
func (c *FleetCatalog) CheckSlash(name string) error {
	if name == "" {
		return nil
	}
	declared := c.DeclaredCommands()
	if slices.Contains(declared, name) {
		return nil
	}
	return &UnknownSlashError{Name: name, Declared: declared}
}

// filterQueryKeys are the filter keys /v1/models also accepts as query
// parameters (/v1/models?slash=plan&provider=anthropic).
var filterQueryKeys = []string{"kind", "provider", "tool", "band_source", "slash"}

// filterFromQuery parses the filter keys of a query string. Other
// parameters are ignored: a listing must not break on a client's own params.
func filterFromQuery(q url.Values) (Filter, error) {
	var parts []string
	for _, key := range filterQueryKeys {
		if v := strings.TrimSpace(q.Get(key)); v != "" {
			parts = append(parts, key+"="+v)
		}
	}
	return ParseFilter(strings.Join(parts, ","))
}

// slashCandidatesExist reports whether any agent admitted by sel (widened to
// the higher bands when the policy escalates) passes filter.
func (s *Server) slashCandidatesExist(ctx context.Context, sel Selector, filter Filter) bool {
	if s.policy.Escalate == EscalateUp && sel.Kind == SelectorBand {
		sel.MinBand = true
	}
	return len(s.catalog.Candidates(ctx, sel, filter)) > 0
}

// slashFailure is one structured slash error.
type slashFailure struct {
	Status  int
	Type    string // tool_command_* error type
	Message string
	Tool    string
	Command string
	Outcome string
	Refusal string
	Hint    string
}

func isAnthropicPath(p string) bool { return strings.HasSuffix(p, "/v1/messages") }

// writeSlashError answers in the dialect of the surface: an OpenAI error
// object, or an Anthropic error envelope on the Messages routes.
func writeSlashError(w http.ResponseWriter, anthropicSurface bool, f slashFailure) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(f.Status)
	_ = json.NewEncoder(w).Encode(slashErrorBody(anthropicSurface, f))
}

func slashErrorBody(anthropicSurface bool, f slashFailure) map[string]any {
	detail := map[string]any{"message": f.Message}
	add := func(k, v string) {
		if v != "" {
			detail[k] = v
		}
	}
	add("tool", f.Tool)
	add("command", f.Command)
	add("outcome", f.Outcome)
	add("refusal", f.Refusal)
	add("hint", f.Hint)
	if anthropicSurface {
		detail["type"] = anthropicErrType(f.Status)
		add("bashy_type", f.Type)
		return map[string]any{"type": "error", "error": detail}
	}
	detail["type"] = f.Type
	detail["code"] = f.Status
	return map[string]any{"error": detail}
}

func anthropicErrType(status int) string {
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity, http.StatusConflict:
		return "invalid_request_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusGatewayTimeout:
		return "timeout_error"
	}
	if status >= 500 {
		return "api_error"
	}
	return "invalid_request_error"
}

// classifySlash maps a toolcmd outcome to an HTTP status and error type.
// 200 means success.
//
//	launch-guard refusal     → 403 tool_command_refused (+hint)
//	agent-live refusal       → 409 tool_command_refused (+hint)
//	outcome unavailable      → 422 tool_command_unavailable
//	outcome timeout          → 504 tool_command_timeout
//	outcome cancelled        → 499 (client gone; nothing is written)
//	any other error          → 502 tool_command_failed
func classifySlash(res toolcmd.Result, err error) (int, string) {
	var refusal *toolcmd.RefusalError
	switch {
	case errors.As(err, &refusal) && refusal.Kind == toolcmd.RefusalLaunchGuard:
		return http.StatusForbidden, "tool_command_refused"
	case errors.As(err, &refusal) && refusal.Kind == toolcmd.RefusalAgentLive:
		return http.StatusConflict, "tool_command_refused"
	case res.Refusal == toolcmd.RefusalLaunchGuard:
		return http.StatusForbidden, "tool_command_refused"
	case res.Refusal == toolcmd.RefusalAgentLive:
		return http.StatusConflict, "tool_command_refused"
	case res.Outcome == toolcmd.OutcomeUnavailable || errors.Is(err, toolcmd.ErrUnavailable):
		return http.StatusUnprocessableEntity, "tool_command_unavailable"
	case res.Outcome == toolcmd.OutcomeTimeout || errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "tool_command_timeout"
	case res.Outcome == toolcmd.OutcomeCancelled || errors.Is(err, context.Canceled):
		return 499, "tool_command_cancelled"
	case err != nil || res.Outcome == toolcmd.OutcomeError:
		return http.StatusBadGateway, "tool_command_failed"
	}
	return http.StatusOK, ""
}

func slashFailureOf(status int, typ string, tool, command string, res toolcmd.Result, err error) slashFailure {
	msg := strings.TrimSpace(res.Error)
	if msg == "" && err != nil {
		msg = err.Error()
	}
	if status == http.StatusUnprocessableEntity {
		// The reason is the tool's own words ("/plan isn't available in this
		// environment."); keep them even when the error text is generic.
		if reason, _, _ := strings.Cut(strings.TrimSpace(res.Text), "\n"); reason != "" && !strings.Contains(msg, reason) {
			msg = strings.TrimPrefix(msg+": "+reason, ": ")
		}
	}
	if msg == "" {
		msg = fmt.Sprintf("tool command %s:%s ended with outcome %q", tool, command, res.Outcome)
	}
	return slashFailure{Status: status, Type: typ, Message: msg, Tool: tool, Command: command,
		Outcome: res.Outcome, Refusal: res.Refusal, Hint: res.Hint}
}

// slashCompletion is the OpenAI completion a slash request returns: the
// ordinary shape plus the structured toolcmd envelope.
type slashCompletion struct {
	openai.ChatCompletion
	ToolCommand *toolcmd.Result `json:"x_bashy_tool_command,omitempty"`
}

// slashMessage is the Anthropic Messages counterpart.
type slashMessage struct {
	*anthropic.MessagesResponse
	ToolCommand *toolcmd.Result `json:"x_bashy_tool_command,omitempty"`
}

// slashText is the reply text: the command's text, then any collected
// artifacts, one path per line.
func slashText(res toolcmd.Result) string {
	text := res.Text
	if len(res.Artifacts) > 0 {
		var b strings.Builder
		b.WriteString(strings.TrimRight(text, "\n"))
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("Artifacts:\n")
		for _, a := range res.Artifacts {
			b.WriteString("- " + a + "\n")
		}
		text = b.String()
	}
	return text
}

func slashCompletionOf(jobID, model string, res toolcmd.Result) *openai.ChatCompletion {
	stop := "stop"
	return &openai.ChatCompletion{
		ID: "chatcmpl-" + jobID, Object: "chat.completion", Created: time.Now().Unix(), Model: model,
		Choices: []openai.ChatChoice{{Index: 0, Message: &openai.ChatResponseMessage{Role: "assistant", Content: slashText(res)}, FinishReason: &stop}},
	}
}

// openAIStreamBytes renders comp as OpenAI SSE chunks ending in [DONE].
func openAIStreamBytes(comp *openai.ChatCompletion) []byte {
	var buf bytes.Buffer
	chunk := func(delta *openai.ChatResponseMessage, fin *string) {
		c := openai.ChatCompletion{ID: comp.ID, Object: "chat.completion.chunk", Created: comp.Created, Model: comp.Model,
			Choices: []openai.ChatChoice{{Index: 0, Delta: delta, FinishReason: fin}}}
		data, _ := json.Marshal(c)
		buf.WriteString("data: ")
		buf.Write(data)
		buf.WriteString("\n\n")
	}
	chunk(&openai.ChatResponseMessage{Role: "assistant", Content: comp.Choices[0].Message.Content}, nil)
	chunk(&openai.ChatResponseMessage{}, comp.Choices[0].FinishReason)
	buf.WriteString("data: [DONE]\n\n")
	return buf.Bytes()
}

// decodeSlashRequest reads the request in either dialect as an OpenAI chat
// request (the Anthropic surface is translated exactly as the gateway does).
func decodeSlashRequest(body []byte, anthropicSurface bool) (*openai.ChatRequest, error) {
	if anthropicSurface {
		var req anthropic.MessagesRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		return anthropic.ToOpenAI(&req)
	}
	var req openai.ChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	return &req, nil
}

// lastUserText is the command's {args}: the text of the last user message.
func lastUserText(req *openai.ChatRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			return strings.TrimSpace(openai.FlattenContent(req.Messages[i].Content))
		}
	}
	return ""
}

type slashRun struct {
	res toolcmd.Result
	err error
}

// serveSlash runs the routed agent's tool command and answers the request.
// It is bounded by the COMMAND's timeout, not the server's request timeout:
// a deep-research run legitimately outlives a completion.
func (s *Server) serveSlash(w http.ResponseWriter, r *http.Request, body []byte, model string, decision Decision, name string) {
	anth := isAnthropicPath(r.URL.Path)
	fail := func(f slashFailure) { writeSlashError(w, anth, f) }

	req, err := decodeSlashRequest(body, anth)
	if err != nil {
		fail(slashFailure{Status: http.StatusBadRequest, Type: "invalid_request", Message: "decode request: " + err.Error(), Command: name})
		return
	}
	agent, ok := s.catalog.Agent(decision.Agent)
	if !ok {
		fail(slashFailure{Status: http.StatusServiceUnavailable, Type: "tool_command_failed", Message: fmt.Sprintf("routed to unknown agent %q", decision.Agent), Command: name})
		return
	}
	tool, ok := s.catalog.fleet.Tool(agent.Tool)
	cmd, found := tool.Command(name)
	if !ok || !found {
		fail(slashFailure{Status: http.StatusNotFound, Type: "tool_command_not_found",
			Message: fmt.Sprintf("agent %s (tool %s) declares no command %q", agent.Name, agent.Tool, name), Tool: agent.Tool, Command: name})
		return
	}
	args := lastUserText(req)
	opts := toolcmd.Options{Agent: agent.Name}
	ctx, cancel := context.WithTimeout(r.Context(), toolcmd.CommandTimeout(cmd, opts))
	defer cancel()

	jobID, _ := sched.ResolveJobID(r)
	h := w.Header()
	h.Set(sched.JobIDHeader, jobID)
	h.Set(ToolCommandHeader, tool.Name+":"+cmd.Name)

	started := time.Now()
	done := make(chan slashRun, 1)
	go func() {
		res, err := runToolCommand(ctx, tool, cmd, args, opts)
		done <- slashRun{res, err}
	}()

	var run slashRun
	committed := false
	flusher, _ := w.(http.Flusher)
	if !req.Stream {
		run = <-done
	} else {
		// Hold the status line until the first keepalive tick: a fast refusal
		// (guard, live agent, unavailable) still gets its real status code,
		// and only a genuinely long command commits to a 200 SSE stream.
		ticker := time.NewTicker(slashKeepalive)
	wait:
		for {
			select {
			case run = <-done:
				break wait
			case <-ticker.C:
				if !committed {
					committed = true
					h.Set("Content-Type", "text/event-stream")
					h.Set("Cache-Control", "no-cache")
					h.Set("Connection", "keep-alive")
					w.WriteHeader(http.StatusOK)
				}
				if anth {
					_, _ = io.WriteString(w, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
				} else {
					_, _ = io.WriteString(w, ": keepalive\n\n")
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
		ticker.Stop()
	}
	s.recordSlash(model, agent.Name, tool.Name+":"+cmd.Name, time.Since(started))

	status, typ := classifySlash(run.res, run.err)
	if status == 499 {
		return // the client hung up; nothing to answer to
	}
	if status != http.StatusOK {
		f := slashFailureOf(status, typ, tool.Name, cmd.Name, run.res, run.err)
		if !committed {
			fail(f)
			return
		}
		// The stream is already a 200: report the failure in-band.
		data, _ := json.Marshal(slashErrorBody(anth, f))
		if anth {
			_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", data)
		} else {
			_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
		}
		if flusher != nil {
			flusher.Flush()
		}
		return
	}

	res := run.res
	comp := slashCompletionOf(jobID, model, res)
	if !req.Stream {
		if anth {
			writeJSON(w, http.StatusOK, slashMessage{MessagesResponse: anthropic.FromOpenAI(comp), ToolCommand: &res})
			return
		}
		writeJSON(w, http.StatusOK, slashCompletion{ChatCompletion: *comp, ToolCommand: &res})
		return
	}
	if !committed {
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
	}
	sse := openAIStreamBytes(comp)
	if anth {
		_ = anthropic.ConvertStream(bytes.NewReader(sse), w)
	} else {
		_, _ = w.Write(sse)
	}
	if flusher != nil {
		flusher.Flush()
	}
}

// recordSlash appends the slash run to usage.jsonl next to the routing
// decision (no tokens: a vendor command reports none through this path).
func (s *Server) recordSlash(model, agent, command string, latency time.Duration) {
	if s.usage == nil {
		return
	}
	_ = s.usage.Append(context.Background(), UsageRecord{
		SchemaVersion: UsageSchemaVersion, At: time.Now().UTC(), Principal: Principal,
		Model: model, Agent: agent, Command: command, LatencyMS: latency.Milliseconds(),
	})
}
