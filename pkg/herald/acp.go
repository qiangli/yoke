package herald

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/acp"
	"github.com/qiangli/yoke/pkg/chat"
	"github.com/spf13/cobra"
)

// newACPCmd serves this host as an ACP agent over the protocol's standard
// newline-delimited JSON-RPC stdin/stdout transport.
func newACPCmd() *cobra.Command {
	var agent, role, gateCmd, gateDir string
	var timeout time.Duration
	var readOnly, allowPremium bool

	c := &cobra.Command{
		Use:   "acp",
		Short: "Serve this machine as an ACP agent over stdin/stdout",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			binding, err := chat.ResolveAgent(agent, role)
			if err != nil {
				return err
			}

			runner := newChatRunner(cmd.Context(), binding, timeout, readOnly, allowPremium)
			defer runner.Close()

			// This process is already the ACP endpoint. Letting chat select its
			// own ACP rung could recursively drive another ACP agent to answer
			// the request we were asked. Keep the local pty turn machinery here;
			// restore the inherited setting when an in-process host returns.
			inheritedACP, hadACP := os.LookupEnv("BASHY_ACP")
			if err := os.Unsetenv("BASHY_ACP"); err != nil {
				return fmt.Errorf("herald acp: disable nested ACP: %w", err)
			}
			defer func() {
				if hadACP {
					_ = os.Setenv("BASHY_ACP", inheritedACP)
				} else {
					_ = os.Unsetenv("BASHY_ACP")
				}
			}()

			server := acp.NewAgent(runner, acp.AgentOptions{
				Gate:    gateCmd,
				GateDir: gateDir,
			}, &eofGraceReader{Reader: os.Stdin}, os.Stdout)
			<-server.Done()
			return nil
		},
	}
	c.Flags().StringVar(&agent, "agent", "", "local agent binding to serve")
	c.Flags().StringVar(&role, "role", "conductor", "local role to serve when --agent is omitted")
	c.Flags().StringVar(&gateCmd, "gate", "", "local command that verifies each end_turn claim")
	c.Flags().StringVar(&gateDir, "gate-dir", "", "directory for the gate (default: the ACP session cwd)")
	c.Flags().DurationVar(&timeout, "timeout", 0, "maximum time for each local turn")
	c.Flags().BoolVar(&readOnly, "read-only", false, "run the local agent without write authority")
	c.Flags().BoolVar(&allowPremium, "allow-premium", false, "allow premium-model turns past the normal budget gate")
	return c
}

// eofGraceReader gives the SDK's request handler time to flush the response to
// a final piped request before its read loop observes EOF and closes the
// connection. Interactive ACP hosts keep stdin open, so they never take this
// path; it exists for finite transcript probes such as `printf ... | herald
// acp`, where the producer closes immediately after the newline.
type eofGraceReader struct {
	io.Reader
	once sync.Once
}

func (r *eofGraceReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err == io.EOF {
		r.once.Do(func() { time.Sleep(100 * time.Millisecond) })
	}
	return n, err
}

type chatRunner struct {
	ctx          context.Context
	agent        string
	timeout      time.Duration
	readOnly     bool
	allowPremium bool

	mu       sync.Mutex
	sessions map[string]*chatSession
}

type chatSession struct {
	session *chat.Session
	mu      sync.Mutex
}

func newChatRunner(ctx context.Context, agent string, timeout time.Duration, readOnly, allowPremium bool) *chatRunner {
	return &chatRunner{
		ctx:          ctx,
		agent:        agent,
		timeout:      timeout,
		readOnly:     readOnly,
		allowPremium: allowPremium,
		sessions:     make(map[string]*chatSession),
	}
}

func (r *chatRunner) Run(ctx context.Context, req acp.TurnRequest) (acp.TurnResponse, error) {
	prompt := promptText(req.Prompt)
	if strings.TrimSpace(prompt) == "" {
		return acp.TurnResponse{StopReason: acp.StopReasonRefusal}, nil
	}

	turnCtx := ctx
	if r.timeout > 0 {
		var cancel context.CancelFunc
		turnCtx, cancel = context.WithTimeout(ctx, r.timeout)
		defer cancel()
	}

	cs, fresh, err := r.session(req, prompt)
	if err != nil {
		return cancelledResponse(turnCtx, err)
	}
	defer cs.mu.Unlock()

	if !fresh {
		if err := cs.session.Say(prompt); err != nil {
			return cancelledResponse(turnCtx, err)
		}
	}
	if err := cs.session.WaitIdle(turnCtx, 0); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			_ = cs.session.Interrupt()
		}
		return cancelledResponse(turnCtx, err)
	}

	reason := acp.StopReasonEndTurn
	if reported := cs.session.ACPStopReason(); reported != "" {
		reason = acp.StopReason(reported)
	}
	return acp.TurnResponse{
		Text:       cs.session.Turn(),
		StopReason: reason,
	}, nil
}

func (r *chatRunner) session(req acp.TurnRequest, prompt string) (*chatSession, bool, error) {
	r.mu.Lock()
	if s := r.sessions[req.SessionID]; s != nil {
		// Take the per-session turn lock before releasing the map lock. A
		// concurrent prompt can therefore never overtake the prompt that
		// created and published this session.
		s.mu.Lock()
		r.mu.Unlock()
		return s, false, nil
	}

	s, err := chat.Start(r.ctx, r.agent, chat.SessionOptions{
		Prompt:       prompt,
		Cwd:          req.Cwd,
		ReadOnly:     r.readOnly,
		AllowPremium: r.allowPremium,
		Mode:         "herald-acp",
	})
	if err != nil {
		r.mu.Unlock()
		return nil, false, err
	}
	cs := &chatSession{session: s}
	cs.mu.Lock()
	r.sessions[req.SessionID] = cs
	r.mu.Unlock()
	return cs, true, nil
}

func (r *chatRunner) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, s := range r.sessions {
		s.session.Close()
		delete(r.sessions, id)
	}
}

func promptText(blocks []acp.ContentBlock) string {
	var parts []string
	for _, block := range blocks {
		if block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func cancelledResponse(ctx context.Context, err error) (acp.TurnResponse, error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return acp.TurnResponse{StopReason: acp.StopReasonCancelled}, context.Canceled
	}
	return acp.TurnResponse{}, err
}
