package foreman

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/agentlaunch"
)

// ControlSupported reports whether this build can host Foreman's local control
// transport. It is intentionally explicit: native Windows compiles the Unix
// socket calls but cannot run them, so callers must refuse before mutating work.
func ControlSupported() bool { return runtime.GOOS != "windows" }

// ObserveSession is a host-composed, non-inference observation lifetime. Its
// worker must not require Session.mu, which is held while the manager is busy.
// The returned stop function cancels and joins observation without ending work.
var ObserveSession func(context.Context, string, string) func()

// ServeControl serves commands until cancellation, stop, or the runtime bound.
// It joins accepted turns before returning, including their state persistence.
// An injected Runner must honor context cancellation and finish its cleanup.
func (s *Session) ServeControl(ctx context.Context, ready chan<- string) error {
	if !ControlSupported() {
		return fmt.Errorf("foreman: managed control sessions are not supported on native Windows")
	}
	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Snapshot before publishing the listener. Once the socket exists a first
	// tell can immediately enter Apply and hold s.mu for an entire turn. The
	// lifetime watcher must not need that mutex before it can observe stop.
	initial := s.State()
	if initial.Stopped {
		return nil
	}
	path := s.store.CtlSockPath()
	if err := s.store.Ensure(); err != nil {
		return err
	}
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	// The listener, watcher, connections, and accepted turns share one lifetime.
	// Merely closing the listener leaves handlers in Scan and turns persisting
	// state after ServeControl's caller has started tearing down the store.
	var workers sync.WaitGroup
	var connsMu sync.Mutex
	conns := make(map[net.Conn]struct{})
	watchDone := make(chan struct{})
	watchExited := make(chan struct{})
	go func() {
		defer close(watchExited)
		s.watchControlLifetime(serveCtx, cancel, ln, watchDone, initial.Deadline, initial.MaxRuntime)
	}()
	defer func() {
		cancel()
		close(watchDone)
		_ = ln.Close()
		connsMu.Lock()
		for conn := range conns {
			_ = conn.Close()
		}
		connsMu.Unlock()
		workers.Wait()
		<-watchExited
		_ = os.Remove(path)
	}()
	if ObserveSession != nil {
		if stop := ObserveSession(serveCtx, initial.ID, initial.Agent); stop != nil {
			defer stop()
		}
	}
	if ready != nil {
		select {
		case ready <- path:
		case <-serveCtx.Done():
			return nil
		}
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			if serveCtx.Err() != nil {
				return nil
			}
			return err
		}
		// CONCURRENTLY. A turn can run for many minutes, and Apply holds the session
		// for all of it. Handling connections inline meant the listener stopped
		// accepting the moment an agent started working — so the one time you most
		// need to say "stop, wrong file", the socket would not even take the call.
		connsMu.Lock()
		conns[conn] = struct{}{}
		connsMu.Unlock()
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() {
				connsMu.Lock()
				delete(conns, conn)
				connsMu.Unlock()
			}()
			s.handleControlConn(serveCtx, conn, &workers)
		}()
	}
}

func (s *Session) watchControlLifetime(ctx context.Context, cancel context.CancelFunc, ln net.Listener, done <-chan struct{}, deadline time.Time, maxRuntime string) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			_ = ln.Close()
			return
		case reason := <-s.stopCh:
			cancel() // terminate the active turn before waiting for its state lock
			s.closeLive()
			s.markStopped(StatusDone, reason)
			_ = ln.Close()
			return
		case <-ticker.C:
			// Compare wall time, not a monotonic timer. Laptop suspend pauses Go
			// timers; a hard runtime must expire immediately after wake rather than
			// granting the worker the whole pre-suspend duration again.
			if !deadline.IsZero() && !time.Now().Before(deadline) {
				cancel() // CommandContext -> agentpty's process-tree kill path.
				s.closeLive()
				s.markStopped(StatusBlocked, "max runtime "+maxRuntime+" exceeded")
				_ = ln.Close()
				return
			}
		}
	}
}

func (s *Session) markStopped(status, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Stopped {
		return
	}
	s.state.Stopped = true
	s.state.Status = status
	s.state.StopReason = reason
	s.state.Steering = false
	s.persistLocked()
}

func (s *Session) handleControlConn(ctx context.Context, conn net.Conn, workers *sync.WaitGroup) {
	defer conn.Close()
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		if ctx.Err() != nil {
			return
		}
		var cmd Command
		if err := json.Unmarshal(sc.Bytes(), &cmd); err != nil {
			fmt.Fprintf(conn, `{"ok":false,"error":%q}`+"\n", err.Error())
			continue
		}

		// A STEER to an agent that is already working goes STRAIGHT to it — it does
		// not queue behind the turn it is trying to interrupt.
		//
		// This is the fast path, and it is the only one that makes `tell` mean what
		// it says. Routing it through Apply would block on the session mutex until
		// the turn finished, at which point the "interruption" arrives as a fresh
		// instruction to an agent that has already done the wrong thing.
		if strings.EqualFold(strings.TrimSpace(cmd.Verb), CommandTell) {
			if steered, err := s.TrySteer(cmd.Message); err != nil {
				fmt.Fprintf(conn, `{"ok":false,"error":%q}`+"\n", err.Error())
				continue
			} else if steered {
				if err := s.noteSteer(cmd.Message); err != nil {
					fmt.Fprintf(conn, `{"ok":false,"error":%q}`+"\n", "steer delivered but history record failed: "+err.Error())
					continue
				}
				fmt.Fprintln(conn, `{"ok":true,"steered":true}`)
				continue
			}
		}

		// A KEY only ever goes to a live agent. There is no "queue a keystroke for
		// the next agent" — a keypress is meaningless without something to press it
		// at, and pretending otherwise would be the same lie in a new costume.
		if strings.EqualFold(strings.TrimSpace(cmd.Verb), CommandKey) {
			sent, err := s.TrySendKey(cmd.Message)
			if err != nil {
				fmt.Fprintf(conn, `{"ok":false,"error":%q}`+"\n", err.Error())
				continue
			}
			if !sent {
				fmt.Fprintf(conn, `{"ok":false,"error":%q}`+"\n", "no live agent to press a key at")
				continue
			}
			fmt.Fprintln(conn, `{"ok":true,"steered":true}`)
			continue
		}

		// Stop is a cancellation signal, not a queued turn. Apply holds s.mu for
		// the entire active turn; sending stop through Apply would therefore wait
		// until the agent had already finished. Wake the lifetime watcher through
		// its independent channel so it cancels the process tree immediately.
		if strings.EqualFold(strings.TrimSpace(cmd.Verb), CommandStop) {
			// Publish the ACK before cancellation closes accepted connections, but
			// a peer that stops reading must not withhold stop from the watcher.
			_ = conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
			fmt.Fprintln(conn, `{"ok":true,"accepted":true}`)
			s.requestStop("stopped by operator")
			return
		}

		// No live agent: this command STARTS a turn, which can take many minutes.
		// Ack that it was accepted and run it in the background — the caller asked us
		// to do a thing, not to hold its connection open while an LLM thinks.
		//
		// The outcome lands in state.json (status / steering / steer_why_not), which
		// is where `foreman status` reads it from, and is the honest place for it: a
		// 3-second ack could never have carried the result of a ten-minute turn.
		// This handler remains counted until after Add, so shutdown cannot see
		// zero workers while a handler is still able to launch another turn.
		workers.Add(1)
		go func(cmd Command) {
			defer workers.Done()
			if err := s.Apply(ctx, cmd); err != nil {
				_ = s.saveState()
				return
			}
			_ = s.saveState()
		}(cmd)
		fmt.Fprintln(conn, `{"ok":true,"accepted":true}`)
		continue
	}
	_ = sc.Err()
}

// Ack is what the daemon says it did with a command.
//
// Steered and Accepted are NOT the same thing and the caller must be able to tell
// them apart:
//
//	Steered  -- the message went to a LIVE agent, mid-turn. You interrupted it.
//	Accepted -- there was no live agent; the message STARTS a turn instead.
//
// Collapsing both into "ok" is exactly the lie this whole line of work exists to
// remove. An operator who typed a correction needs to know whether it landed on a
// working agent or merely got queued for a fresh one.
type Ack struct {
	OK       bool   `json:"ok"`
	Error    string `json:"error"`
	Steered  bool   `json:"steered"`
	Accepted bool   `json:"accepted"`
}

func SendCommand(root, id string, cmd Command) (Ack, error) {
	if !ControlSupported() {
		return Ack{}, fmt.Errorf("foreman: managed control sessions are not supported on native Windows")
	}
	store := NewStore(root, id)
	var ack Ack
	if err := agentlaunch.SendJSONControl(store.CtlSockPath(), cmd, &ack, 3*time.Second); err != nil {
		return ack, err
	}
	if !ack.OK {
		return ack, fmt.Errorf("foreman: control command failed: %s", ack.Error)
	}
	return ack, nil
}

func Tell(root, id, msg string) (Ack, error) {
	return SendCommand(root, id, Command{Verb: CommandTell, Message: msg})
}
