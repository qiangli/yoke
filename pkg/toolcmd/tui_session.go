package toolcmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/qiangli/yoke/pkg/agentpty"
	"github.com/qiangli/yoke/pkg/chat"
)

// chatTUISession adapts a live chat.Session to tuiSession.
type chatTUISession struct {
	s *chat.Session
}

// Ready waits for the control socket to be bound and the TUI to draw and go
// quiet: a line typed into a tool still painting its splash is swallowed.
func (c *chatTUISession) Ready(ctx context.Context) error {
	if c.s.CtlSock != "" {
		deadline := time.Now().Add(20 * time.Second)
		for {
			if _, err := os.Stat(c.s.CtlSock); err == nil {
				break
			}
			if !c.s.Live() {
				return fmt.Errorf("%s exited before its session was ready", c.s.Nick)
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("%s never opened a control channel", c.s.Nick)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
	// Drawn: something on the screen (bounded; a silent TUI still proceeds).
	deadline := time.Now().Add(25 * time.Second)
	for c.s.Output() == "" && time.Now().Before(deadline) && c.s.Live() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	// Quiet.
	if err := c.s.WaitIdle(ctx, 1500*time.Millisecond); err != nil {
		return err
	}
	if !c.s.Live() {
		return fmt.Errorf("%s exited before its session was ready", c.s.Nick)
	}
	return nil
}

func (c *chatTUISession) Say(text string) error { return c.s.Say(text) }

func (c *chatTUISession) Key(b []byte) error {
	return agentpty.SendFrame(c.s.CtlSock, agentpty.VerbatimFrame(b))
}

func (c *chatTUISession) WaitIdle(ctx context.Context, quiet time.Duration) error {
	return c.s.WaitIdle(ctx, quiet)
}

func (c *chatTUISession) Turn() string   { return c.s.Turn() }
func (c *chatTUISession) Output() string { return c.s.Output() }
func (c *chatTUISession) Live() bool     { return c.s.Live() }

// QuitLine is a control command, not a turn: typed straight onto the control
// channel so an exhausted budget can still end the session.
func (c *chatTUISession) QuitLine(line string) error {
	if line == "" || c.s.CtlSock == "" {
		return c.s.Quit()
	}
	return agentpty.SendFrame(c.s.CtlSock, agentpty.TextFrame(line))
}

func (c *chatTUISession) Close() { c.s.Close() }
