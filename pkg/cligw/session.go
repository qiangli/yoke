package cligw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/agentlaunch"
	"github.com/qiangli/yoke/pkg/fleet"
)

// Sticky sessions keep ONE warm CLI process across turns for sticky
// bind=worker/reset=none bindings. Only stdin-stream-json tools (claude, agy)
// accept further turns on stdin: the transport reads one NDJSON user message
// per line and runs one turn for each, so the process stays up while stdin
// stays open. Every other warm mode is one-shot by construction (cold exits,
// stdin closes the session) and stays that way: DialSticky refuses those
// tools loudly instead of faking continuity.

// ErrStickyUnsupported marks a tool that cannot hold a multi-turn session.
// The broker answers it with 501 and the reason.
var ErrStickyUnsupported = errors.New("cligw: tool does not keep a warm CLI across turns")

// ErrStickyCapped marks a room with no headroom for another sticky worker.
// The broker answers it with 429.
var ErrStickyCapped = errors.New("cligw: sticky worker concurrency cap reached")

// StickySession owns one CLI process for one sticky binding and accepts one
// Turn at a time. It is not pooled: the broker reserves it for the binding
// and retires it when the binding goes away.
type StickySession struct {
	mu sync.Mutex

	agent  string
	launch agentlaunch.Launch
	tool   fleet.Tool
	cwd    string

	cmd    *exec.Cmd
	stdin  *os.File
	lines  <-chan workerLine
	wait   <-chan error
	stderr *lockedBuffer

	turns   int
	system  string // explicit system/developer instructions of the first turn
	closed  bool
	dead    bool
	started time.Time
	cleanup sync.Once
	// onClose drops the server's reservation count. Set once by DialSticky
	// before the session is handed out; nil for standalone sessions.
	onClose func()
}

// NewStickySession resolves agent and pre-starts its CLI with the neutral
// prompt, like a warm pool worker. Only WarmStdinStreamJSON tools are
// accepted: any other mode would exit after the first turn, and a session
// that silently restarts per turn is a reset, not a conversation.
func NewStickySession(ctx context.Context, agent string) (*StickySession, error) {
	cwd, launch, tool, mode, err := resolveWorkerSeat(ctx, agent)
	if err != nil {
		return nil, err
	}
	if mode != WarmStdinStreamJSON {
		_ = os.RemoveAll(cwd)
		return nil, fmt.Errorf("%w: tool %q runs warm mode %q (want stdin-stream-json); sticky bind=worker/reset=none is not supported on it",
			ErrStickyUnsupported, tool.Name, mode)
	}
	s := &StickySession{agent: agent, launch: launch, tool: tool, cwd: cwd}
	if err := s.startLocked(s.argv("", "")); err != nil {
		s.removeDir()
		return nil, err
	}
	return s, nil
}

// Agent reports the fleet agent this session was resolved for.
func (s *StickySession) Agent() string { return s.agent }

// StartedAt reports when the CLI process was (re)started.
func (s *StickySession) StartedAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}

// Turn sends one completion prompt to the held CLI and returns when that
// turn's terminal event arrives. The process stays up for the next turn:
// stdin is never closed between turns. The first turn carries the request's
// system instructions (native channel or inline, like a one-shot worker);
// later turns send only the new turn's text because the CLI already holds
// the instructions. Callers freeze the system and tool set per session: a
// mid-session change must be refused (409), never silently dropped.
func (s *StickySession) Turn(ctx context.Context, input CompletionPrompt, onEvent func(Event)) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Result{Outcome: OutcomeError}, errors.New("cligw: sticky session is closed")
	}
	if s.dead {
		return Result{Outcome: OutcomeError}, errors.New("cligw: sticky worker is gone")
	}
	prompt := input.Prompt
	first := s.turns == 0
	if s.tool.Name == "muse" {
		prompt = museCompletionPrompt(input)
	} else if !s.nativeSystemPrompt() {
		if first {
			prompt = inlineSystemPrompt(systemPrompt(input.System), prompt)
		}
	} else if first && input.System != "" {
		// Native system overrides are launch-time settings. A session
		// prewarmed neutral is relaunched before its first turn when the
		// request adds instructions; later turns cannot change them.
		s.killLocked()
		if err := s.startLocked(s.argv("", input.System)); err != nil {
			s.dead = true
			return Result{Outcome: OutcomeError}, err
		}
	}
	if first {
		s.system = input.System
	}
	body, err := s.promptBody(prompt)
	if err != nil {
		return Result{Outcome: OutcomeError}, err
	}
	if _, err := s.stdin.Write(body); err != nil {
		s.killLocked()
		s.dead = true
		return Result{Outcome: OutcomeError}, fmt.Errorf("cligw: deliver turn to %s: %w", s.agent, err)
	}

	result := Result{Outcome: OutcomeOK}
	var text strings.Builder
	var sawTerminal bool
	var waitErr, scanErr error
	var explicitFailure bool
	var sawTextDelta bool
	var terminalVerdict fleet.Verdict
	processDone := s.wait == nil
	streamDone := s.lines == nil
	for !sawTerminal && !(processDone && streamDone) {
		select {
		case <-ctx.Done():
			s.killLocked()
			s.dead = true
			result.Text = text.String()
			result.Usage = estimatedUsage(prompt, result.Text)
			result.Outcome = OutcomeError
			return result, ctx.Err()
		case err, ok := <-s.wait:
			if ok {
				waitErr = err
			}
			processDone = true
			s.wait = nil
		case line, ok := <-s.lines:
			if !ok {
				streamDone = true
				s.lines = nil
				continue
			}
			if line.err != nil {
				scanErr = line.err
				continue
			}
			ev, parsed := parseEvent(line.data, s.tool.CLI.Launch.EventsDone)
			if !parsed {
				if str := strings.TrimSpace(string(line.data)); str != "" {
					ev = Event{Type: "output", Raw: append(json.RawMessage(nil), line.data...)}
					if !s.tool.EventsOnStdout() {
						ev.Text = str
					}
				}
			}
			isTextDelta := claudeTextDelta(ev.Raw)
			if isTextDelta {
				sawTextDelta = true
			} else if sawTextDelta && s.tool.Name == "claude" && ev.Type == "assistant" {
				// Claude follows partial stream events with a full assistant
				// snapshot. Keep it for terminal metadata, but do not
				// duplicate it into the turn text (one-shot workers do the
				// same; see Worker.DoCompletion).
				ev.Text = ""
			}
			if ev.Text != "" {
				text.WriteString(ev.Text)
			}
			if ev.Done {
				sawTerminal = true
				if text.Len() == 0 {
					if final := terminalText(ev.Raw); final != "" {
						text.WriteString(final)
						ev.Text = final
					}
				}
				result.Raw = append(result.Raw[:0], ev.Raw...)
				result.Usage = usageFromEvent(ev.Raw)
				terminalVerdict = s.tool.CLI.Launch.EventsOutcome.Read(ev.Raw)
			}
			if eventFailed(ev) {
				explicitFailure = true
			}
			if onEvent != nil && (parsed || !s.tool.EventsOnStdout()) {
				onEvent(ev)
			}
		}
	}

	result.Text = text.String()
	if result.Usage.InputTokens == 0 && result.Usage.OutputTokens == 0 && result.Usage.TotalTokens == 0 {
		result.Usage = estimatedUsage(prompt, result.Text)
	}
	if !sawTerminal {
		// The CLI exited (or its stream ended) before this turn's terminal
		// event: the session cannot serve this turn or any later one.
		s.killLocked()
		s.dead = true
		result.Outcome = OutcomeError
		return result, s.runError(waitErr, scanErr)
	}
	s.turns++
	if failed := waitErr != nil || scanErr != nil || explicitFailure; failed {
		result.Outcome = OutcomeError
		return result, s.runError(waitErr, scanErr)
	}
	if s.tool.CLI.Launch.EventsOutcome.Declared() && terminalVerdict != fleet.VerdictSucceeded {
		result.Outcome = OutcomeError
		return result, s.runError(nil, nil)
	}
	return result, nil
}

// Close retires the session: stdin closes so a waiting CLI can exit, then
// the whole process group is killed. It is idempotent.
func (s *StickySession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeLocked()
}

func (s *StickySession) closeLocked() error {
	if s.closed {
		return nil
	}
	s.closed = true
	if s.stdin != nil {
		_ = s.stdin.Close()
		s.stdin = nil
	}
	s.killLocked()
	s.removeDir()
	if s.onClose != nil {
		onClose := s.onClose
		s.onClose = nil
		onClose()
	}
	return nil
}

func (s *StickySession) killLocked() {
	if s.cmd == nil {
		return
	}
	killProcessGroup(s.cmd)
	if s.wait != nil {
		<-s.wait
		s.wait = nil
	}
	s.cmd = nil
	s.lines = nil
}

func (s *StickySession) startLocked(argv []string) error {
	if len(argv) == 0 {
		return errors.New("cligw: empty sticky session argv")
	}
	if _, err := agentlaunch.EnsureManaged(context.Background(), s.launch); err != nil {
		return err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = s.cwd
	cmd.Env = workerEnv(os.Environ(), s.launch)
	prepareProcessGroup(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("cligw: %s stdout: %w", s.agent, err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("cligw: %s stdin: %w", s.agent, err)
	}
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("cligw: start %s: %w", s.agent, err)
	}
	f, ok := stdin.(*os.File)
	if !ok {
		_ = stdin.Close()
		killProcessGroup(cmd)
		_ = cmd.Wait()
		return fmt.Errorf("cligw: %s stdin is not a pipe", s.agent)
	}
	linec := make(chan workerLine, 64)
	waitc := make(chan error, 1)
	go scanWorkerLines(stdout, linec)
	go func() {
		waitc <- cmd.Wait()
		close(waitc)
	}()
	s.cmd, s.stdin, s.lines, s.wait, s.stderr = cmd, f, linec, waitc, stderr
	s.started = time.Now()
	return nil
}

func (s *StickySession) runError(waitErr, scanErr error) error {
	parts := []error{waitErr, scanErr}
	err := errors.Join(parts...)
	if err == nil {
		err = errors.New("CLI reported an error outcome")
	}
	return fmt.Errorf("cligw: %s: %w", s.agent, err)
}

func (s *StickySession) removeDir() {
	s.cleanup.Do(func() { _ = os.RemoveAll(s.cwd) })
}

func (s *StickySession) nativeSystemPrompt() bool {
	return s.tool.Name == "claude" || s.tool.Name == "codex"
}

func (s *StickySession) promptBody(prompt string) ([]byte, error) {
	switch s.tool.Name {
	case "claude":
		v := map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": prompt}}
		body, err := json.Marshal(v)
		return append(body, '\n'), err
	case "agy":
		v := map[string]any{"event": "user", "message": map[string]any{"content": prompt}}
		body, err := json.Marshal(v)
		return append(body, '\n'), err
	default:
		return nil, fmt.Errorf("cligw: tool %q declares stdin-stream-json without a measured message shape", s.tool.Name)
	}
}

func (s *StickySession) argv(prompt, requestSystem string) []string {
	w := &Worker{launch: s.launch, tool: s.tool, mode: WarmStdinStreamJSON, cwd: s.cwd}
	return w.argv(prompt, requestSystem)
}
