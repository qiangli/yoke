package chat

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/agentpty"
	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/room"
)

const commandSchemaVersion = "bashy-chat-command-v1"

// CommandEnvelope is the structured `bashy chat command --json` response.
type CommandEnvelope struct {
	SchemaVersion string   `json:"schema_version"`
	Session       string   `json:"session,omitempty"`
	Tool          string   `json:"tool,omitempty"`
	Command       string   `json:"command"`
	Slash         string   `json:"slash,omitempty"`
	Actor         string   `json:"actor,omitempty"`
	Declared      []string `json:"declared,omitempty"`
	Outcome       string   `json:"outcome"`
	ExitCode      int      `json:"exit_code"`
	Error         string   `json:"error,omitempty"`
}

type exitCodeError struct {
	code int
	err  error
}

func (e *exitCodeError) Error() string { return e.err.Error() }
func (e *exitCodeError) Unwrap() error { return e.err }
func (e *exitCodeError) ExitCode() int { return e.code }

// ExitCode maps a chat CLI error to its process exit status: 0 on nil, 2 on an
// invalid or undeclared command refusal, and 1 otherwise.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var ec interface{ ExitCode() int }
	if errors.As(err, &ec) {
		return ec.ExitCode()
	}
	return 1
}

type commandDeliveryTiming struct {
	poll             time.Duration
	socketTimeout    time.Duration
	settle           time.Duration
	readyTimeout     time.Duration
	responseGrace    time.Duration
	postReadyTimeout time.Duration
}

var defaultCommandDeliveryTiming = commandDeliveryTiming{
	poll:             100 * time.Millisecond,
	socketTimeout:    20 * time.Second,
	settle:           1200 * time.Millisecond,
	readyTimeout:     25 * time.Second,
	responseGrace:    1500 * time.Millisecond,
	postReadyTimeout: 15 * time.Second,
}

// commandSession is the live-session slice RunSessionCommand drives. Production
// callers drive cardCommandSession over the room card's control socket and
// capture log; unit tests can script a fake TUI session directly.
type commandSession interface {
	WaitReadyBefore(ctx context.Context) error
	SendVerbatim(line string) error
	WaitReadyAfter(ctx context.Context) error
}

var openCommandSession = func(c room.Card, timing commandDeliveryTiming, warnings io.Writer) commandSession {
	return &cardCommandSession{card: c, timing: timing, warnings: warnings}
}

type cardCommandSession struct {
	card        room.Card
	timing      commandDeliveryTiming
	warnings    io.Writer
	preSendSize int64
	sentAt      time.Time
}

func readFileTail(path string, maxBytes int64) (size int64, modTime time.Time, tail string, ok bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, time.Time{}, "", false
	}
	size, modTime = fi.Size(), fi.ModTime()
	if size == 0 || maxBytes <= 0 {
		return size, modTime, "", true
	}
	f, err := os.Open(path)
	if err != nil {
		return size, modTime, "", true
	}
	defer f.Close()
	offset := int64(0)
	if size > maxBytes {
		offset = size - maxBytes
	}
	if offset > 0 {
		_, _ = f.Seek(offset, io.SeekStart)
	}
	b, _ := io.ReadAll(f)
	return size, modTime, string(b), true
}

func (s *cardCommandSession) WaitReadyBefore(ctx context.Context) error {
	sock := strings.TrimSpace(s.card.CtlSock)
	if sock == "" {
		return fmt.Errorf("chat: %s has no control socket", s.card.ID)
	}
	sockDeadline := time.Now().Add(s.timing.socketTimeout)
	for {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		if s.card.PID > 0 && !room.PidAlive(s.card.PID) {
			return fmt.Errorf("chat: %s exited before its command could be sent", s.card.ID)
		}
		if time.Now().After(sockDeadline) {
			return fmt.Errorf("chat: %s never opened control socket %s", s.card.ID, sock)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.timing.poll):
		}
	}

	logPath := strings.TrimSpace(s.card.LogPath)
	if logPath == "" {
		return nil
	}

	status := room.NewTimelineReader(func(e room.Event) bool {
		return e.Type == room.EventStatus && e.Target == s.card.ID &&
			strings.EqualFold(strings.TrimSpace(e.Body), "ready") && e.TS >= s.card.Joined
	})

	readyDeadline := time.Now().Add(s.timing.readyTimeout)
	var lastSize int64 = -1
	var lastChange time.Time
	warned := false
	for {
		if s.card.PID > 0 && !room.PidAlive(s.card.PID) {
			return fmt.Errorf("chat: %s exited before its command could be sent", s.card.ID)
		}
		size, modTime, tail, exists := readFileTail(logPath, readyTailBytes)
		gateActive := exists && size > 0 && agentpty.ClassifyGate(tail).Kind != agentpty.GateNone
		if exists {
			if lastSize < 0 {
				lastSize = size
				lastChange = modTime
			} else if size != lastSize {
				lastSize = size
				lastChange = time.Now()
			}
			if size > 0 && !gateActive && time.Since(lastChange) >= s.timing.settle {
				return nil
			}
		}
		if !gateActive {
			if pos, err := status.Latest(ctx); err == nil && pos.Seq > 0 {
				return nil
			}
		}
		if time.Now().After(readyDeadline) && !gateActive {
			if !warned && s.warnings != nil {
				fmt.Fprintf(s.warnings, "chat: warning: %s did not settle within %s; sending command anyway\n", s.card.ID, s.timing.readyTimeout)
				warned = true
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.timing.poll):
		}
	}
}

func (s *cardCommandSession) SendVerbatim(line string) error {
	if logPath := strings.TrimSpace(s.card.LogPath); logPath != "" {
		if fi, err := os.Stat(logPath); err == nil {
			s.preSendSize = fi.Size()
		}
	}
	s.sentAt = time.Now()
	if err := steerSend(s.card.CtlSock, agentpty.TextFrame(line)); err != nil {
		return fmt.Errorf("chat: could not send command to %s: %w", s.card.ID, err)
	}
	return nil
}

func (s *cardCommandSession) WaitReadyAfter(ctx context.Context) error {
	logPath := strings.TrimSpace(s.card.LogPath)
	if logPath == "" {
		return nil
	}
	deadline := time.Now().Add(s.timing.postReadyTimeout)
	lastSize := s.preSendSize
	lastChange := s.sentAt
	if lastChange.IsZero() {
		lastChange = time.Now()
	}
	sawGrowth := false
	for {
		if s.card.PID > 0 && !room.PidAlive(s.card.PID) {
			return nil
		}
		size, _, tail, exists := readFileTail(logPath, readyTailBytes)
		if exists && size != lastSize {
			if size > s.preSendSize {
				sawGrowth = true
			}
			lastSize = size
			lastChange = time.Now()
		}
		gateClear := !exists || size == 0 || agentpty.ClassifyGate(tail).Kind == agentpty.GateNone
		if sawGrowth {
			if gateClear && time.Since(lastChange) >= s.timing.settle {
				return nil
			}
		} else if time.Since(s.sentAt) >= s.timing.responseGrace && gateClear {
			return nil
		}
		if time.Now().After(deadline) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.timing.poll):
		}
	}
}

func cardToolName(c room.Card) string {
	if t := strings.TrimSpace(c.Tool); t != "" {
		return t
	}
	if b := strings.TrimSpace(c.Binding); b != "" {
		head, _, _ := strings.Cut(b, ":")
		return strings.TrimSpace(head)
	}
	return ""
}

func resolveCardTool(cat *fleet.Catalog, c room.Card) (fleet.Tool, bool) {
	if cat == nil {
		cat = newCatalog()
	}
	if name := cardToolName(c); name != "" {
		if tl, ok := cat.Tool(name); ok {
			return tl, true
		}
	}
	for _, candidate := range []string{strings.TrimSpace(c.Nick), strings.TrimSpace(c.ID)} {
		if candidate == "" {
			continue
		}
		if _, tl, _, err := cat.Binding(candidate); err == nil {
			return tl, true
		}
		if tl, ok := cat.Tool(candidate); ok {
			return tl, true
		}
	}
	return fleet.Tool{}, false
}

func isSelfDestructiveSessionCommand(name string) bool {
	switch name {
	case "clear", "exit":
		return true
	default:
		return false
	}
}

func callerEnvAgentNames() []string {
	var out []string
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" {
			return
		}
		for _, existing := range out {
			if strings.EqualFold(existing, v) {
				return
			}
		}
		out = append(out, v)
	}
	for _, k := range []string{"BASHY_AGENT_ID", "BASHY_AGENT", "WEAVE_AGENT", "WEAVE_CONDUCTOR"} {
		add(os.Getenv(k))
	}
	if p := strings.TrimSpace(os.Getenv("BASHY_PRINCIPAL")); p != "" {
		if _, nick, ok := strings.Cut(p, "agent/"); ok {
			add(nick)
		}
	}
	return out
}

// callerOwnsSession reports whether the invoking process is running inside the
// target room card's own live session, detected via stamped/declared caller
// environment, tool-session claim digest, or strict process ancestry.
func callerOwnsSession(c room.Card) (string, bool) {
	for _, ident := range callerEnvAgentNames() {
		claimID := room.AgentClaimID(ident)
		if strings.EqualFold(ident, c.ID) ||
			(c.Nick != "" && strings.EqualFold(ident, c.Nick)) ||
			(claimID != "" && strings.EqualFold(claimID, c.ID)) {
			return fmt.Sprintf("caller identity %q matches session %s", ident, c.ID), true
		}
	}
	if c.SessionClaim != "" && bus.CurrentSessionClaim != nil {
		for _, cand := range []string{strings.TrimSpace(c.Nick), strings.TrimSpace(c.ID)} {
			if cand == "" {
				continue
			}
			if hash := bus.HashSessionClaim(bus.CurrentSessionClaim(cand)); hash != "" &&
				subtle.ConstantTimeCompare([]byte(c.SessionClaim), []byte(hash)) == 1 {
				return fmt.Sprintf("caller session claim matches %s", c.ID), true
			}
		}
	}
	anchor := c.OwnerPID
	if anchor <= 0 {
		anchor = c.PID
	}
	if anchor > 1 && anchor != os.Getpid() && bus.ProcessHasAncestor(anchor) {
		return fmt.Sprintf("caller is a descendant of session pid %d", anchor), true
	}
	return "", false
}

// RunSessionCommand resolves a declared TUI session command for the live
// session, waits for readiness, sends the rendered slash line verbatim (never
// prepending inbox mail or merging queued steers), waits until the session is
// ready again (bounded), and records a room timeline command event.
func RunSessionCommand(ctx context.Context, sessionID, cmdName string, cmdArgs []string, timeout time.Duration, force bool, warnings io.Writer) (CommandEnvelope, error) {
	return runSessionCommandWith(ctx, newCatalog(), sessionID, cmdName, cmdArgs, defaultCommandDeliveryTiming, timeout, force, warnings)
}

func runSessionCommandWith(ctx context.Context, cat *fleet.Catalog, sessionID, cmdName string, cmdArgs []string, timing commandDeliveryTiming, timeout time.Duration, force bool, warnings io.Writer) (CommandEnvelope, error) {
	name := strings.TrimSpace(cmdName)
	env := CommandEnvelope{
		SchemaVersion: commandSchemaVersion,
		Session:       strings.TrimSpace(sessionID),
		Command:       name,
		Outcome:       "error",
		ExitCode:      1,
	}
	if strings.TrimSpace(sessionID) == "" || name == "" {
		err := &exitCodeError{code: 2, err: errors.New("chat command: usage: bashy chat command <session> <name> [args...]")}
		env.ExitCode, env.Error = 2, err.Error()
		return env, err
	}

	c, err := findMember(sessionID)
	if err != nil {
		env.Error = err.Error()
		return env, err
	}
	env.Session = c.ID
	env.Tool = cardToolName(c)

	tool, ok := resolveCardTool(cat, c)
	if ok {
		env.Tool = tool.Name
	}
	declared := tool.DeclaredCommandNames(fleet.ToolCommandTUI)
	env.Declared = declared
	if !ok || len(declared) == 0 {
		refusal := &exitCodeError{
			code: 2,
			err:  fmt.Errorf("chat: session %s (tool %q) declares no tui session commands (declared: none)", c.ID, env.Tool),
		}
		env.Outcome, env.ExitCode, env.Error = "refused", 2, refusal.Error()
		return env, refusal
	}

	tc, found := tool.Command(name)
	if !found || tc.Mode != fleet.ToolCommandTUI {
		refusal := &exitCodeError{
			code: 2,
			err:  fmt.Errorf("chat: command %q is not declared for %s (tool %s); declared: %s", name, c.ID, tool.Name, strings.Join(declared, ", ")),
		}
		env.Outcome, env.ExitCode, env.Error = "refused", 2, refusal.Error()
		return env, refusal
	}

	if !force && isSelfDestructiveSessionCommand(tc.Name) {
		if why, self := callerOwnsSession(c); self {
			refusal := &exitCodeError{
				code: 2,
				err:  fmt.Errorf("chat: refusing to run %q on caller's own session %s (%s); pass --force to override", tc.Name, c.ID, why),
			}
			env.Outcome, env.ExitCode, env.Error = "refused", 2, refusal.Error()
			return env, refusal
		}
	}

	rendered := fleet.RenderSlash(tc.Slash, strings.Join(cmdArgs, " "))
	env.Slash = rendered

	if timeout > 0 {
		timing.postReadyTimeout = timeout
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	sess := openCommandSession(c, timing, warnings)
	if err := sess.WaitReadyBefore(ctx); err != nil {
		env.Error = err.Error()
		return env, err
	}
	if err := sess.SendVerbatim(rendered); err != nil {
		env.Error = err.Error()
		return env, err
	}
	if err := sess.WaitReadyAfter(ctx); err != nil {
		env.Error = err.Error()
		return env, err
	}

	actor := principalName()
	env.Actor = actor
	_ = room.Emit(room.Event{
		Type:   room.EventCommand,
		Actor:  actor,
		Target: c.ID,
		Topic:  tc.Name,
		Body:   rendered,
	})
	env.Outcome = "ok"
	env.ExitCode = 0
	return env, nil
}

func newChatCommandCmd() *cobra.Command {
	var (
		asJSON  bool
		force   bool
		timeout time.Duration
	)
	cmd := &cobra.Command{
		Use:   "command <session> <name> [args...]",
		Short: "run a declared vendor session command (clear, exit, resume, rename, compact, status) on a live session",
		Long: `Run one of a live session's declared vendor TUI commands (mode: tui in the
tool's commands: block) without exiting and restarting the session.

Unlike 'bashy chat steer', 'bashy chat command' sends the rendered slash line
verbatim to the session's control channel: pending inbox items and queued steers
are never prepended or merged into the command line.

Destructive commands (clear, exit) aimed at the caller's own session are refused
unless --force is given.`,
		SilenceUsage: true,
		Args:         cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			var sessionID, name string
			var rest []string
			if len(args) > 0 {
				sessionID = args[0]
			}
			if len(args) > 1 {
				name = args[1]
				rest = args[2:]
			}
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			res, err := RunSessionCommand(ctx, sessionID, name, rest, timeout, force, cmd.ErrOrStderr())
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				_ = enc.Encode(res)
				return err
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "chat: sent command %s (%s) to %s\n", res.Command, res.Slash, res.Session)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit a bashy-chat-command-v1 JSON envelope")
	cmd.Flags().BoolVar(&force, "force", false, "allow clear or exit on the caller's own live session")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "bound the readiness wait (default 15s)")
	return cmd
}
