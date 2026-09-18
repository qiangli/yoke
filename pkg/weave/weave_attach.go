package weave

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/coreutils/pkg/weavecli"
	"github.com/qiangli/yoke/pkg/agentpty"
)

func attachRouteLine(line string) (frame string, detach bool) {
	switch strings.TrimSpace(line) {
	case "/detach", "/quit":
		return "", true
	default:
		return line, false
	}
}

func runWeaveAttach(cmd *cobra.Command, id int64, flags *weaveOutputFlags) error {
	mode := flags.mode()
	const verb = "weave attach"

	cwd, _ := os.Getwd()
	root, err := weaveRepoRoot(cwd)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb,
			weavecli.ExitPrecondFail, err))
	}
	dir, _ := weaveQueueDir(root)
	q, err := loadWeaveQueue(dir)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb,
			weavecli.ExitGenericFail, err))
	}
	it := findWeaveItem(q, id)
	if it == nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb,
			weavecli.ExitInvalidArg, fmt.Errorf("run #%d not found%s", id, weaveOtherActiveQueuesHintSuffix(dir))))
	}
	if it.State != "working" || it.WrapperPid == 0 || !pidAlive(it.WrapperPid) {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb,
			weavecli.ExitStateConflict, fmt.Errorf("run #%d has no live subagent (state=%q)", it.ID, it.State)))
	}
	if it.CtlSock == "" {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb,
			weavecli.ExitStateConflict, fmt.Errorf("run #%d has no control socket — its wrapper predates `weave say` or ran with --pty=never", it.ID)))
	}
	if it.LogPath == "" {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb,
			weavecli.ExitStateConflict, fmt.Errorf("run #%d has no PTY capture (state=%q) — it either hasn't started or ran interactively (PTY passthrough)", it.ID, it.State)))
	}
	f, err := os.Open(it.LogPath)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb,
			weavecli.ExitStateConflict, fmt.Errorf("log missing on disk: %s", it.LogPath)))
	}
	defer f.Close()

	errOut := cmd.ErrOrStderr()
	fmt.Fprintf(errOut, "attached to run #%d (%s) — type to instruct, /detach to leave (subagent keeps running)\n", it.ID, attachToolName(it))

	signalCtx, stopSignals := signal.NotifyContext(cmd.Context(), os.Interrupt)
	defer stopSignals()
	ctx, cancel := context.WithCancel(signalCtx)
	defer cancel()
	followCtx, stopFollow := context.WithCancel(ctx)
	followDone := make(chan error, 1)
	go func() {
		out := cmd.OutOrStdout()
		if _, err := io.Copy(out, f); err != nil {
			followDone <- err
			return
		}
		followDone <- weaveFollowLog(followCtx, out, f, dir, id)
	}()

	lines := make(chan string)
	scanErr := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(cmd.InOrStdin())
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			case <-ctx.Done():
				scanErr <- nil
				return
			}
		}
		scanErr <- scanner.Err()
	}()

	var attachErr error
	followStopped := false
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case err := <-followDone:
			followStopped = true
			if err != nil {
				attachErr = err
			}
			break loop
		case err := <-scanErr:
			if err != nil {
				attachErr = err
			}
			break loop
		case line := <-lines:
			frame, detach := attachRouteLine(line)
			if detach {
				break loop
			}
			// agentpty.TextFrame, not `frame + "\r\n"`. Attach used to hand-append
			// the terminator and send whatever the operator typed verbatim — so an
			// embedded newline or NUL (a paste, most obviously) went down the socket
			// raw, where `weave say` and `meet say` would both have escaped it.
			if err := weaveWriteControlFrame(it.CtlSock, agentpty.TextFrame(frame)); err != nil {
				attachErr = err
				break loop
			}
		}
	}

	cancel()
	stopFollow()
	if !followStopped {
		<-followDone
	}
	fmt.Fprintln(errOut, "detached (subagent still running)")
	if attachErr != nil {
		return ec(weavecli.EmitError(errOut, mode, verb, weavecli.ExitDepUnhealthy, attachErr))
	}
	if mode == weavecli.OutputJSON {
		return ec(emitOK(cmd.OutOrStdout(), mode, verb, map[string]any{
			"issue": it.ID,
			"state": "detached",
		}))
	}
	return nil
}

func attachToolName(it *weaveItem) string {
	if it.Tool != "" {
		return it.Tool
	}
	return "-"
}
