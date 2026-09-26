package cligw

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/agentlaunch"
	"github.com/qiangli/yoke/pkg/fleet"
)

// WarmMode is the transport used to deliver a prompt to a pre-started CLI.
type WarmMode string

const (
	WarmCold            WarmMode = "cold"
	WarmStdinStreamJSON WarmMode = "stdin-stream-json"
	WarmACP             WarmMode = "acp"
	WarmStdin           WarmMode = "stdin"
)

// Outcome is the CLI's terminal verdict.
type Outcome string

const (
	OutcomeOK    Outcome = "ok"
	OutcomeError Outcome = "error"
)

// Usage is token accounting extracted from the tool's terminal event.
// Estimated is true when the CLI did not provide token counts.
type Usage struct {
	InputTokens       int64 `json:"input_tokens"`
	CachedInputTokens int64 `json:"cached_input_tokens,omitempty"`
	OutputTokens      int64 `json:"output_tokens"`
	TotalTokens       int64 `json:"total_tokens"`
	Estimated         bool  `json:"estimated"`
}

// Event is one parsed stdout event. Text contains an assistant text delta (or
// a complete assistant message for tools that do not emit deltas). Raw keeps
// the exact JSON object for consumers that need tool-specific fields.
type Event struct {
	Type string          `json:"type"`
	Text string          `json:"text,omitempty"`
	Done bool            `json:"done,omitempty"`
	Raw  json.RawMessage `json:"raw"`
}

// Result is the terminal result of a one-shot Worker.
type Result struct {
	Text    string          `json:"text"`
	Usage   Usage           `json:"usage"`
	Outcome Outcome         `json:"outcome"`
	Raw     json.RawMessage `json:"raw,omitempty"`
}

// Worker owns one CLI process for one agent and accepts exactly one Do call.
// Warm workers are started by NewWorker before the prompt is known. Cold and
// ACP workers defer their process start until Do.
type Worker struct {
	mu sync.Mutex

	agent  string
	launch agentlaunch.Launch
	tool   fleet.Tool
	mode   WarmMode
	cwd    string

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  <-chan workerLine
	wait   <-chan error
	stderr *lockedBuffer

	used    bool
	closed  bool
	started time.Time
	cleanup sync.Once
}

type workerLine struct {
	data []byte
	err  error
}

// NewWorker resolves agent through the fleet registry and creates a one-shot
// worker in a fresh temporary working directory.
func NewWorker(ctx context.Context, agent string) (*Worker, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cwd, err := os.MkdirTemp("", "cligw-worker-")
	if err != nil {
		return nil, fmt.Errorf("cligw: create worker directory: %w", err)
	}
	failed := true
	defer func() {
		if failed {
			_ = os.RemoveAll(cwd)
		}
	}()

	opt := agentlaunch.Options{ReadOnly: true, Sandbox: "read-only"}
	launch, err := agentlaunch.Resolve(agent, opt)
	if err != nil {
		return nil, err
	}
	// Resolve already finalizes the argv. ApplySandbox is intentionally also
	// applied here: cligw's pure-completion boundary remains fail-closed even if
	// a caller replaces the resolver with a host-local profile.
	launch.Args = agentlaunch.ApplySandbox(launch.ToolName, launch.Args, opt)
	tool, ok := agentlaunch.NewCatalog().Tool(launch.ToolName)
	if !ok {
		return nil, fmt.Errorf("cligw: resolved tool %q is not in the fleet catalog", launch.ToolName)
	}
	mode := WarmMode(strings.TrimSpace(tool.CLI.Launch.Warm))
	if mode == "" {
		mode = WarmCold
	}
	switch mode {
	case WarmCold, WarmStdinStreamJSON, WarmACP, WarmStdin:
	default:
		return nil, fmt.Errorf("cligw: tool %q has unsupported warm mode %q", tool.Name, mode)
	}

	w := &Worker{agent: agent, launch: launch, tool: tool, mode: mode, cwd: cwd}
	// ACP is deliberately a cold fallback until the ACP worker transport lands.
	if mode != WarmCold && mode != WarmACP {
		if err := w.prepareSystemPrompt(""); err != nil {
			return nil, err
		}
		if err := w.start(w.argv("", "")); err != nil {
			return nil, err
		}
	}
	failed = false
	return w, nil
}

// Agent reports the fleet agent this worker was resolved for.
func (w *Worker) Agent() string { return w.agent }

// WarmMode reports how this worker receives its prompt.
func (w *Worker) WarmMode() WarmMode { return w.mode }

// StartedAt is zero for a cold worker until Do starts its process.
func (w *Worker) StartedAt() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.started
}

// Do sends the worker its only prompt, streams parsed events to onEvent, and
// waits for the entire owned process group to terminate.
func (w *Worker) Do(ctx context.Context, prompt string, onEvent func(Event)) (Result, error) {
	return w.DoCompletion(ctx, CompletionPrompt{Prompt: prompt}, onEvent)
}

// DoCompletion sends one completion while preserving a request's system
// messages as native CLI instructions where the tool supports that channel.
func (w *Worker) DoCompletion(ctx context.Context, input CompletionPrompt, onEvent func(Event)) (Result, error) {
	w.mu.Lock()
	if w.used || w.closed {
		w.mu.Unlock()
		return Result{Outcome: OutcomeError}, errors.New("cligw: worker is one-shot and is already dead")
	}
	w.used = true
	mode := w.mode
	prompt := input.Prompt
	if !w.nativeSystemPrompt() {
		prompt = inlineSystemPrompt(systemPrompt(input.System), prompt)
	}
	// Native system overrides are launch-time settings. A worker prewarmed with
	// the neutral prompt must be relaunched when this request adds instructions.
	if mode != WarmCold && mode != WarmACP && input.System != "" && w.nativeSystemPrompt() {
		w.killAndWait(w.cmd, w.wait)
		w.cmd, w.stdin, w.lines, w.wait, w.stderr = nil, nil, nil, nil, nil
		if err := w.prepareSystemPrompt(input.System); err != nil {
			w.mu.Unlock()
			w.removeDir()
			return Result{Outcome: OutcomeError}, err
		}
		if err := w.startLocked(w.argv("", input.System)); err != nil {
			w.mu.Unlock()
			w.removeDir()
			return Result{Outcome: OutcomeError}, err
		}
	}
	if mode == WarmCold || mode == WarmACP {
		if err := w.prepareSystemPrompt(input.System); err != nil {
			w.mu.Unlock()
			w.removeDir()
			return Result{Outcome: OutcomeError}, err
		}
		if err := w.startLocked(w.argv(prompt, input.System)); err != nil {
			w.mu.Unlock()
			w.removeDir()
			return Result{Outcome: OutcomeError}, err
		}
	}
	stdin, lines, wait, cmd := w.stdin, w.lines, w.wait, w.cmd
	w.mu.Unlock()
	defer w.removeDir()

	if mode != WarmCold && mode != WarmACP {
		body, err := w.promptBody(prompt)
		if err != nil {
			w.killAndWait(cmd, wait)
			return Result{Outcome: OutcomeError}, err
		}
		if _, err := stdin.Write(body); err != nil {
			_ = stdin.Close()
			w.killAndWait(cmd, wait)
			return Result{Outcome: OutcomeError}, fmt.Errorf("cligw: deliver prompt to %s: %w", w.agent, err)
		}
	}
	if stdin != nil {
		if err := stdin.Close(); err != nil {
			w.killAndWait(cmd, wait)
			return Result{Outcome: OutcomeError}, fmt.Errorf("cligw: close %s stdin: %w", w.agent, err)
		}
	}

	result := Result{Outcome: OutcomeOK}
	var text strings.Builder
	var waitErr error
	var scanErr error
	var processDone, streamDone bool
	var terminalVerdict fleet.Verdict
	var explicitFailure bool
	var sawTextDelta bool

	for !processDone || !streamDone {
		select {
		case <-ctx.Done():
			killProcessGroup(cmd)
			if !processDone {
				waitErr = <-wait
				processDone = true
			}
			result.Text = text.String()
			result.Usage = estimatedUsage(prompt, result.Text)
			result.Outcome = OutcomeError
			return result, ctx.Err()
		case err, ok := <-wait:
			if ok {
				waitErr = err
			}
			processDone = true
			// A CLI can exit while a descendant keeps stdout open. The worker owns
			// the whole group, so the turn boundary retires those descendants too.
			killProcessGroup(cmd)
			wait = nil
		case line, ok := <-lines:
			if !ok {
				streamDone = true
				lines = nil
				continue
			}
			if line.err != nil {
				scanErr = line.err
				continue
			}
			ev, parsed := parseEvent(line.data, w.tool.CLI.Launch.EventsDone)
			if !parsed {
				if s := strings.TrimSpace(string(line.data)); s != "" {
					ev = Event{Type: "output", Raw: append(json.RawMessage(nil), line.data...)}
					if !w.tool.EventsOnStdout() {
						ev.Text = s
					}
				}
			}
			isTextDelta := claudeTextDelta(ev.Raw)
			if isTextDelta {
				sawTextDelta = true
			} else if sawTextDelta && w.tool.Name == "claude" && ev.Type == "assistant" {
				// Claude follows partial stream events with a full assistant
				// snapshot. Keep it for terminal metadata, but do not duplicate it.
				ev.Text = ""
			}
			if ev.Text != "" {
				text.WriteString(ev.Text)
			}
			if ev.Done {
				result.Raw = append(result.Raw[:0], ev.Raw...)
				result.Usage = usageFromEvent(ev.Raw)
				terminalVerdict = w.tool.CLI.Launch.EventsOutcome.Read(ev.Raw)
			}
			if eventFailed(ev) {
				explicitFailure = true
			}
			if onEvent != nil && (parsed || !w.tool.EventsOnStdout()) {
				onEvent(ev)
			}
		}
	}

	result.Text = text.String()
	if result.Usage.InputTokens == 0 && result.Usage.OutputTokens == 0 && result.Usage.TotalTokens == 0 {
		result.Usage = estimatedUsage(prompt, result.Text)
	}
	failed := waitErr != nil || scanErr != nil || explicitFailure
	if w.tool.CLI.Launch.EventsOutcome.Declared() && terminalVerdict != fleet.VerdictSucceeded {
		failed = true
	}
	if failed {
		result.Outcome = OutcomeError
		return result, w.runError(waitErr, scanErr)
	}
	return result, nil
}

// Close retires a worker that was acquired but not used. It is idempotent.
func (w *Worker) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	cmd, wait := w.cmd, w.wait
	w.mu.Unlock()
	if cmd != nil {
		w.killAndWait(cmd, wait)
	}
	w.removeDir()
	return nil
}

func (w *Worker) start(argv []string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.startLocked(argv)
}

func (w *Worker) startLocked(argv []string) error {
	if len(argv) == 0 {
		return errors.New("cligw: empty worker argv")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = w.cwd
	cmd.Env = agentlaunch.PrincipalEnv(os.Environ(), w.launch)
	prepareProcessGroup(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("cligw: %s stdout: %w", w.agent, err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("cligw: %s stdin: %w", w.agent, err)
	}
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("cligw: start %s: %w", w.agent, err)
	}
	linec := make(chan workerLine, 64)
	waitc := make(chan error, 1)
	go scanWorkerLines(stdout, linec)
	go func() {
		waitc <- cmd.Wait()
		close(waitc)
	}()
	w.cmd, w.stdin, w.lines, w.wait, w.stderr = cmd, stdin, linec, waitc, stderr
	w.started = time.Now()
	return nil
}

func scanWorkerLines(r io.Reader, out chan<- workerLine) {
	defer close(out)
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64<<10), 4<<20)
	for s.Scan() {
		out <- workerLine{data: append([]byte(nil), s.Bytes()...)}
	}
	if err := s.Err(); err != nil {
		out <- workerLine{err: err}
	}
}

func (w *Worker) argv(prompt, requestSystem string) []string {
	args := append([]string(nil), w.launch.Args...)
	events := w.tool.EventsStdoutArgv()
	switch w.tool.Name {
	case "claude":
		if w.mode == WarmStdinStreamJSON {
			args = moveArgFirst(args, "-p")
		}
		extra := append([]string{}, events...)
		extra = append(extra, "--include-partial-messages", "--system-prompt", systemPrompt(requestSystem))
		extra = append(extra, "--tools", "", "--no-session-persistence", "--strict-mcp-config", "--setting-sources", "")
		if w.mode == WarmStdinStreamJSON {
			extra = append([]string{"--input-format", "stream-json"}, extra...)
			args = append(args, extra...)
		} else {
			args = insertBeforePromptFlag(args, extra)
		}
	case "agy":
		if w.mode == WarmStdinStreamJSON {
			args = removeArg(args, "-p")
			args = removePair(args, "--print-timeout")
			args = append(args, "--input-format", "stream-json")
			args = appendMissingSequence(args, events)
			args = append(args, "-p=")
		} else {
			args = insertBeforePromptFlag(args, events)
		}
	case "codex":
		args = shortSandbox(args)
		args = insertAfter(args, "exec", events)
		args = insertAfter(args, "exec", []string{"-c", "model_instructions_file=" + strconv.Quote(w.codexInstructionsPath())})
	default:
		args = insertBeforePromptFlag(args, events)
	}
	argv := append([]string{w.launch.Tool}, args...)
	if w.mode == WarmCold || w.mode == WarmACP {
		argv = append(argv, prompt)
	}
	return argv
}

func systemPrompt(request string) string {
	if strings.TrimSpace(request) == "" {
		return neutralSystemPrompt
	}
	return neutralSystemPrompt + "\n\n" + request
}

func (w *Worker) nativeSystemPrompt() bool {
	return w.tool.Name == "claude" || w.tool.Name == "codex"
}

func (w *Worker) codexInstructionsPath() string {
	return filepath.Join(w.cwd, "instructions.md")
}

func (w *Worker) prepareSystemPrompt(request string) error {
	if w.tool.Name != "codex" {
		return nil
	}
	if err := os.WriteFile(w.codexInstructionsPath(), []byte(systemPrompt(request)+"\n"), 0o600); err != nil {
		return fmt.Errorf("cligw: write codex system prompt: %w", err)
	}
	return nil
}

func (w *Worker) promptBody(prompt string) ([]byte, error) {
	switch w.mode {
	case WarmStdin:
		return []byte(prompt), nil
	case WarmStdinStreamJSON:
		var v any
		switch w.tool.Name {
		case "claude":
			v = map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": prompt}}
		case "agy":
			v = map[string]any{"event": "user", "message": map[string]any{"content": prompt}}
		default:
			return nil, fmt.Errorf("cligw: tool %q declares stdin-stream-json without a measured message shape", w.tool.Name)
		}
		body, err := json.Marshal(v)
		return append(body, '\n'), err
	default:
		return nil, nil
	}
}

func parseEvent(line []byte, done fleet.EventsDone) (Event, bool) {
	var obj map[string]any
	if err := json.Unmarshal(line, &obj); err != nil {
		return Event{}, false
	}
	typ, _ := obj["type"].(string)
	if typ == "" {
		typ, _ = obj["event"].(string)
	}
	ev := Event{Type: typ, Raw: append(json.RawMessage(nil), line...), Done: done.Match(line)}
	ev.Text = eventText(obj)
	return ev, true
}

func eventText(obj map[string]any) string {
	if typ, _ := obj["type"].(string); typ == "item.completed" {
		if item, ok := obj["item"].(map[string]any); ok {
			if kind, _ := item["type"].(string); kind == "agent_message" {
				return stringValue(item["text"])
			}
		}
	}
	if typ, _ := obj["type"].(string); typ == "stream_event" {
		if event, ok := obj["event"].(map[string]any); ok {
			if stringValue(event["type"]) == "content_block_delta" {
				if delta, ok := event["delta"].(map[string]any); ok && stringValue(delta["type"]) == "text_delta" {
					return stringValue(delta["text"])
				}
			}
		}
	}
	if msg, ok := obj["message"].(map[string]any); ok {
		switch content := msg["content"].(type) {
		case string:
			return content
		case []any:
			var b strings.Builder
			for _, part := range content {
				if m, ok := part.(map[string]any); ok && stringValue(m["type"]) == "text" {
					b.WriteString(stringValue(m["text"]))
				}
			}
			return b.String()
		}
	}
	if delta, ok := obj["delta"].(map[string]any); ok {
		return stringValue(delta["text"])
	}
	if typ, _ := obj["type"].(string); typ == "text" {
		return stringValue(obj["text"])
	}
	return ""
}

func claudeTextDelta(raw []byte) bool {
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil || stringValue(obj["type"]) != "stream_event" {
		return false
	}
	event, _ := obj["event"].(map[string]any)
	delta, _ := event["delta"].(map[string]any)
	return stringValue(event["type"]) == "content_block_delta" && stringValue(delta["type"]) == "text_delta"
}

func eventFailed(ev Event) bool {
	switch ev.Type {
	case "turn.failed", "error", "failed":
		return true
	}
	return false
}

func usageFromEvent(raw []byte) Usage {
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return Usage{}
	}
	for _, candidate := range []any{obj["usage"], nested(obj, "result", "usage"), obj["token_usage"]} {
		m, ok := candidate.(map[string]any)
		if !ok {
			continue
		}
		u := Usage{
			InputTokens:       number(m, "input_tokens", "prompt_tokens", "input"),
			CachedInputTokens: number(m, "cached_input_tokens", "cache_read_input_tokens"),
			OutputTokens:      number(m, "output_tokens", "completion_tokens", "output"),
			TotalTokens:       number(m, "total_tokens", "total"),
		}
		if u.TotalTokens == 0 {
			u.TotalTokens = u.InputTokens + u.OutputTokens
		}
		if u.InputTokens != 0 || u.OutputTokens != 0 || u.TotalTokens != 0 {
			return u
		}
	}
	return Usage{}
}

func estimatedUsage(prompt, answer string) Usage {
	input := estimateTokens(prompt)
	output := estimateTokens(answer)
	return Usage{InputTokens: input, OutputTokens: output, TotalTokens: input + output, Estimated: true}
}

func estimateTokens(s string) int64 {
	if s == "" {
		return 0
	}
	n := int64(len([]rune(s))+3) / 4
	if n < 1 {
		return 1
	}
	return n
}

func nested(m map[string]any, keys ...string) any {
	var v any = m
	for _, key := range keys {
		next, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = next[key]
	}
	return v
}

func number(m map[string]any, keys ...string) int64 {
	for _, key := range keys {
		if n, ok := m[key].(float64); ok {
			return int64(n)
		}
	}
	return 0
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

func insertBeforePromptFlag(args, extra []string) []string {
	if len(extra) == 0 {
		return args
	}
	for _, seq := range [][]string{extra} {
		if containsSequence(args, seq) {
			return args
		}
	}
	for i := len(args) - 1; i >= 0; i-- {
		if args[i] == "-p" || args[i] == "--message" {
			out := append([]string(nil), args[:i]...)
			out = append(out, extra...)
			return append(out, args[i:]...)
		}
	}
	return append(args, extra...)
}

func appendMissingSequence(args, extra []string) []string {
	if len(extra) == 0 || containsSequence(args, extra) {
		return args
	}
	return append(args, extra...)
}

func containsSequence(args, seq []string) bool {
	for i := 0; i+len(seq) <= len(args); i++ {
		match := true
		for j := range seq {
			if args[i+j] != seq[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func removeArg(args []string, target string) []string {
	out := args[:0]
	for _, arg := range args {
		if arg != target {
			out = append(out, arg)
		}
	}
	return out
}

func removePair(args []string, flag string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == flag {
			i++
			continue
		}
		out = append(out, args[i])
	}
	return out
}

func moveArgFirst(args []string, target string) []string {
	for i, arg := range args {
		if arg == target {
			out := []string{target}
			out = append(out, args[:i]...)
			return append(out, args[i+1:]...)
		}
	}
	return args
}

func insertAfter(args []string, target string, extra []string) []string {
	if len(extra) == 0 || containsSequence(args, extra) {
		return args
	}
	for i, arg := range args {
		if arg == target {
			out := append([]string(nil), args[:i+1]...)
			out = append(out, extra...)
			return append(out, args[i+1:]...)
		}
	}
	return append(args, extra...)
}

func shortSandbox(args []string) []string {
	out := append([]string(nil), args...)
	for i := 0; i < len(out); i++ {
		if out[i] == "--sandbox" {
			out[i] = "-s"
		}
	}
	return out
}

func (w *Worker) killAndWait(cmd *exec.Cmd, wait <-chan error) {
	if cmd == nil {
		return
	}
	killProcessGroup(cmd)
	if wait != nil {
		<-wait
	}
}

func (w *Worker) runError(waitErr, scanErr error) error {
	parts := []error{waitErr, scanErr}
	if w.stderr != nil {
		if msg := strings.TrimSpace(w.stderr.String()); msg != "" {
			parts = append(parts, errors.New(msg))
		}
	}
	err := errors.Join(parts...)
	if err == nil {
		err = errors.New("CLI reported an error outcome")
	}
	return fmt.Errorf("cligw: %s: %w", w.agent, err)
}

func (w *Worker) removeDir() {
	w.cleanup.Do(func() { _ = os.RemoveAll(w.cwd) })
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}
