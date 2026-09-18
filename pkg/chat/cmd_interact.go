package chat

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/agentpty"
	"github.com/qiangli/yoke/pkg/room"
)

// findMember resolves an id/nick to a live room member, with a helpful error when
// nothing (or more than one thing) matches — a control verb must never guess.
//
// An AGENT is named by its identity (`elif`), so it resolves exactly. A TASK is
// named by its work (`weave-412-9931`), and several may be live at once, which
// is why the ambiguous case still has to be reported rather than guessed at.
func findMember(id string) (room.Card, error) {
	c, ok, err := room.Find(id)
	if err != nil {
		return room.Card{}, err
	}
	if ok {
		return c, nil
	}
	members, _ := room.Members()
	if strings.TrimSpace(id) == "" {
		return room.Card{}, fmt.Errorf("chat: name an agent or task id (%d live) — `bashy chat sessions`", len(members))
	}
	return room.Card{}, fmt.Errorf("chat: nothing live matches %q (or it is ambiguous) — `bashy chat sessions`", id)
}

// newChatSessionsCmd lists the host room's members — the live-agent board.
func newChatSessionsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "sessions",
		Aliases: []string{"ls"},
		Short:   "list live agents and tasks in the host room (all launch paths)",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			members, err := room.Members()
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if len(members) == 0 {
				fmt.Fprintln(w, "nothing live — start an agent with `bashy chat --agent NICK` or `--band N`")
				return nil
			}
			fmt.Fprintf(w, "%-24s %-22s %-4s %-11s %-8s %-9s %s\n",
				"ID", "BINDING", "BAND", "MODE", "PID", "REACH", "JOINED")
			for _, c := range members {
				band := "-"
				if c.Band > 0 {
					band = "L" + fmt.Sprint(c.Band)
				}
				mode := c.Mode
				if mode == "" {
					mode = "-"
				}
				fmt.Fprintf(w, "%-24s %-22s %-4s %-11s %-8d %-9s %s\n",
					c.ID, c.Binding, band, mode, c.PID, reachLabel(c), c.Joined)
			}
			// WHAT TO DO WITH A ROW. The list already answered "what is alive";
			// it did not answer the question anyone reading it actually has,
			// which is how to take one back. A detached session survives the
			// terminal that started it — the pty path makes it a session leader
			// — so after closing a TUI these rows are exactly the sessions a
			// returning operator wants, and the reattach command was something
			// they had to already know.
			fmt.Fprintln(w)
			fmt.Fprintln(w, "reattach:  bashy chat attach <ID>          take the terminal back")
			fmt.Fprintln(w, "           bashy coach attach --agent <ID>  steer it without taking it")
			if unreachable(members) {
				// A session with no control socket can still be watched through
				// its log, and saying so is better than letting someone
				// conclude the session is broken.
				fmt.Fprintln(w, "  a row marked `log-only` has no control socket: `bashy chat log <ID>` follows it,")
				fmt.Fprintln(w, "  but nothing can steer it — it was launched on a path that never opened one.")
			}
			return nil
		},
	}
	return cmd
}

// formatEvent renders one timeline entry.
//
// The addressing fields are not decoration. A `notify` event carries WHO sent it
// (Principal — required, the REPORT/AUTHOR invariant) and WHERE it was addressed
// (Topic / To / Room), and the original renderer printed none of them: it showed
// Type, Target and Body only, so every notification appeared as an anonymous line
// of text with no sender and no topic. Publishing into a log that cannot show who
// published or what they addressed is publishing into a void — the notification
// exists, and carries no more information than silence.
func formatEvent(e room.Event) string {
	line := fmt.Sprintf("%s  %-9s", e.TS, e.Type)

	// Who: Actor for session events, Principal for notifications.
	switch {
	case e.Principal != "":
		line += "  <" + e.Principal + ">"
	case e.Actor != "":
		line += "  <" + e.Actor + ">"
	}

	// Where: the addressing that decides who this was meant for.
	var addr []string
	if e.Topic != "" {
		addr = append(addr, "#"+e.Topic)
	}
	if e.To != "" {
		addr = append(addr, "→"+e.To)
	}
	if e.Room != "" {
		addr = append(addr, "@"+e.Room)
	}
	if e.Target != "" {
		addr = append(addr, e.Target)
	}
	if len(addr) > 0 {
		line += "  " + strings.Join(addr, " ")
	}

	if e.Body != "" {
		line += "  " + e.Body
	}
	return line
}

// newChatTimelineCmd prints the host room's event log — join/leave/steer/status.
func newChatTimelineCmd() *cobra.Command {
	var n int
	cmd := &cobra.Command{
		Use:   "timeline",
		Short: "print the host room's event timeline (join/leave/steer/status/note)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			events, err := room.Timeline(n)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if len(events) == 0 {
				fmt.Fprintln(w, "timeline empty")
				return nil
			}
			for _, e := range events {
				fmt.Fprintln(w, formatEvent(e))
			}
			return nil
		},
	}
	cmd.Flags().IntVarP(&n, "tail", "n", 50, "show only the last N events (0 = all)")
	return cmd
}

// newChatSteerCmd injects a line into a live session mid-turn.
func newChatSteerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "steer <id> <text>",
		Short: "inject a line into a live session (the one control surface: mid-turn steering)",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := findMember(args[0])
			if err != nil {
				return err
			}
			text := strings.Join(args[1:], " ")
			if err := agentpty.SendFrame(c.CtlSock, agentpty.TextFrame(text)); err != nil {
				return fmt.Errorf("chat: could not steer %s: %w", c.ID, err)
			}
			_ = room.Emit(room.Event{Type: room.EventSteer, Actor: principalName(), Target: c.ID, Body: text})
			fmt.Fprintf(cmd.ErrOrStderr(), "chat: steered %s\n", c.ID)
			return nil
		},
	}
	return cmd
}

// grantKeys returns the keystroke(s) that answer a tool's approval prompt —
// always-allow-for-session, or just this one action. Per-tool because each TUI's
// confirm dialog reads different keys; the y/n/a family is the common default.
func grantKeys(tool string, always bool) string {
	switch tool {
	case "ycode":
		if always {
			return "a" // "always allow for this session"
		}
		return "y"
	}
	if always {
		return "a"
	}
	return "y"
}

// newChatGrantCmd answers a live session's PENDING approval prompt remotely,
// so a supervisor can elevate an attended session to unattended LIVE — no exit,
// no relaunch (the usability killer). The steer channel already reaches the
// agent's confirm dialog; this makes it a first-class, discoverable action
// instead of "type `a` into the prompt yourself".
func newChatGrantCmd() *cobra.Command {
	var once bool
	cmd := &cobra.Command{
		Use:   "grant <id>",
		Short: "approve a live session's pending prompt remotely (always-allow; --once for one action) — no relaunch",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := ""
			if len(args) > 0 {
				id = args[0]
			}
			c, err := findMember(id)
			if err != nil {
				return err
			}
			keys := grantKeys(c.Tool, !once)
			if keys == "" {
				return fmt.Errorf("chat: don't know how to grant approval for tool %q — approve at its terminal", c.Tool)
			}
			if err := agentpty.SendFrame(c.CtlSock, agentpty.VerbatimFrame([]byte(keys))); err != nil {
				return fmt.Errorf("chat: could not grant %s: %w", c.ID, err)
			}
			mode := "always-allow"
			if once {
				mode = "allow-once"
			}
			_ = room.Emit(room.Event{Type: room.EventGrant, Actor: principalName(), Target: c.ID, Body: mode})
			fmt.Fprintf(cmd.ErrOrStderr(), "chat: granted %s (%s)\n", c.ID, mode)
			return nil
		},
	}
	cmd.Flags().BoolVar(&once, "once", false, "allow just this one action (default: always-allow for the session)")
	return cmd
}

// newChatInterruptCmd sends ESC — the only thing that breaks a tool loop mid-turn.
func newChatInterruptCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "interrupt <id>",
		Short: "send ESC to a live session — breaks a tool loop a queued line cannot reach",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := ""
			if len(args) > 0 {
				id = args[0]
			}
			c, err := findMember(id)
			if err != nil {
				return err
			}
			if err := agentpty.SendFrame(c.CtlSock, agentpty.VerbatimFrame([]byte{0x1b})); err != nil {
				return fmt.Errorf("chat: could not interrupt %s: %w", c.ID, err)
			}
			_ = room.Emit(room.Event{Type: room.EventInterrupt, Actor: principalName(), Target: c.ID})
			fmt.Fprintf(cmd.ErrOrStderr(), "chat: sent ESC to %s\n", c.ID)
			return nil
		},
	}
	return cmd
}

// newChatAttachCmd follows an instance's capture and forwards typed lines as steers
// — the `weave attach` pattern over any room member, so a SECOND party can watch
// and instruct a session someone else (or a coach) is driving.
func newChatAttachCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "attach <id>",
		Short: "watch and steer a live session (type to instruct, /detach to leave)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := ""
			if len(args) > 0 {
				id = args[0]
			}
			c, err := findMember(id)
			if err != nil {
				return err
			}
			return attachSession(cmd, c)
		},
	}
	return cmd
}

func attachSession(cmd *cobra.Command, c room.Card) error {
	return AttachTo(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr(), c)
}

// AttachTo follows a live member's capture and forwards typed lines as steers.
//
// Split out of the cobra verb so the launcher can reach it too: `chat --agent X
// --attach` on an already-live X lands here rather than reimplementing the
// follow loop, which is how the two would drift.
func AttachTo(parent context.Context, in io.Reader, out, errOut io.Writer, c room.Card) error {
	if strings.TrimSpace(c.LogPath) == "" {
		return fmt.Errorf("chat: %s has no capture to follow (it may have launched without a log)", c.ID)
	}
	f, err := os.Open(c.LogPath)
	if err != nil {
		return fmt.Errorf("chat: capture missing on disk: %s", c.LogPath)
	}
	defer f.Close()

	fmt.Fprintf(errOut, "attached to %s (%s) — type to instruct, /detach to leave (the agent keeps running)\n",
		c.ID, c.Binding)

	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	// Follower: dump what's there, then poll for growth. Raw ANSI, so a native-TUI
	// capture looks like a redraw stream — good enough to see activity and steer,
	// not a clean transcript (that is what invoke's headless capture is for).
	go func() {
		_, _ = io.Copy(out, f)
		buf := make([]byte, 4096)
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
			if !room.PidAlive(c.PID) {
				fmt.Fprintln(errOut, "\nchat: session ended")
				cancel()
				return
			}
			for {
				nr, rerr := f.Read(buf)
				if nr > 0 {
					_, _ = out.Write(buf[:nr])
				}
				if rerr != nil || nr == 0 {
					break
				}
			}
		}
	}()

	scanner := bufio.NewScanner(in)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		line := scanner.Text()
		switch strings.TrimSpace(line) {
		case "/detach", "/quit":
			fmt.Fprintln(errOut, "detached (the agent keeps running)")
			return nil
		default:
			if err := agentpty.SendFrame(c.CtlSock, agentpty.TextFrame(line)); err != nil {
				return fmt.Errorf("chat: steer failed: %w", err)
			}
			_ = room.Emit(room.Event{Type: room.EventSteer, Actor: principalName(), Target: c.ID, Body: line})
		}
	}
	return scanner.Err()
}

// reachLabel says how much of a live session a returning operator can actually
// take back.
//
// The distinction is not cosmetic. A session with a control socket can be
// STEERED — input reaches it — while one without can only be READ. Both are
// "alive", and a list that showed them identically would send somebody to
// attach to a session that will never answer, which reads as a hang rather than
// as a missing capability.
func reachLabel(c room.Card) string {
	if strings.TrimSpace(c.CtlSock) == "" {
		return "log-only"
	}
	if _, err := os.Stat(c.CtlSock); err != nil {
		// The card advertises a socket that is not there. The process is alive
		// (Members pruned it otherwise), so this is a session that lost its
		// control channel — worth distinguishing from one that never had one.
		return "no-ctl"
	}
	return "steerable"
}

// unreachable reports whether any listed session is read-only, so the footer
// only explains that when it applies.
func unreachable(members []room.Card) bool {
	for _, c := range members {
		if reachLabel(c) != "steerable" {
			return true
		}
	}
	return false
}
