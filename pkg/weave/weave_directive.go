package weave

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

const directiveEventLimit = 100

type directiveDetail struct {
	Run  int64
	Verb string
	Arg  string
}

type directiveAckDetail struct {
	DirectiveID string `json:"directive_id"`
	Run         int64  `json:"run"`
	Verb        string `json:"verb"`
	Status      string `json:"status"`
}

func consumeDirectives(ctx context.Context, client SessionClient, taskID, queueDir, cursor string) (newCursor string, applied int, err error) {
	resp, err := client.GetEvents(ctx, taskID, cursor, directiveEventLimit)
	if err != nil {
		return cursor, 0, err
	}
	newCursor = resp.Cursor
	if newCursor == "" {
		newCursor = cursor
	}
	for _, ev := range resp.Events {
		if ev.Kind != "directive" {
			continue
		}
		d, parseErr := parseDirectiveDetail(ev.Detail)
		status := "applied"
		if parseErr != nil {
			status = "error"
		} else {
			status = applyDirective(queueDir, d)
			if status == "applied" {
				applied++
			}
		}
		ackDetail, _ := json.Marshal(directiveAckDetail{
			DirectiveID: ev.ID,
			Run:         d.Run,
			Verb:        d.Verb,
			Status:      status,
		})
		if _, err := client.AppendEvent(ctx, taskID, AppendEventReq{
			Kind:    "ack",
			Summary: status,
			Detail:  ackDetail,
		}); err != nil {
			return cursor, applied, err
		}
	}
	return newCursor, applied, nil
}

func parseDirectiveDetail(raw json.RawMessage) (directiveDetail, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return directiveDetail{}, err
	}
	d := directiveDetail{}
	if b, ok := fields["run"]; ok {
		if err := unmarshalDirectiveRun(b, &d.Run); err != nil {
			return directiveDetail{}, err
		}
	}
	if b, ok := fields["verb"]; ok {
		_ = json.Unmarshal(b, &d.Verb)
	}
	if b, ok := fields["arg"]; ok {
		_ = json.Unmarshal(b, &d.Arg)
	}
	return d, nil
}

func unmarshalDirectiveRun(raw json.RawMessage, out *int64) error {
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		*out = n
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return err
	}
	*out = n
	return nil
}

func applyDirective(queueDir string, d directiveDetail) string {
	var err error
	switch d.Verb {
	case "say":
		err = applyDirectiveSay(queueDir, d.Run, d.Arg)
	case "add":
		err = applyDirectiveAdd(queueDir, d.Arg)
	case "prio", "prioritize":
		err = applyDirectivePrio(queueDir, d.Run, d.Arg)
	case "kill":
		err = applyDirectiveKill(queueDir, d.Run, d.Arg)
	default:
		return "unknown_verb"
	}
	if err != nil {
		return "error"
	}
	return "applied"
}

func applyDirectiveAdd(queueDir, title string) error {
	if strings.TrimSpace(title) == "" {
		return fmt.Errorf("title required")
	}
	return withWeaveQueueLock(queueDir, func(q *weaveQueue) error {
		prio := "p2"
		it := &weaveItem{
			ID:       q.NextID,
			Title:    title,
			Priority: prio,
			State:    "todo",
			Created:  time.Now().UTC(),
		}
		q.NextID++
		q.Items = append(q.Items, it)
		return nil
	})
}

func applyDirectivePrio(queueDir string, id int64, tier string) error {
	if !isValidPriority(tier) {
		return fmt.Errorf("priority must be one of p0|p1|p2|p3 (got %q)", tier)
	}
	return withWeaveQueueLock(queueDir, func(q *weaveQueue) error {
		it := findWeaveItem(q, id)
		if it == nil {
			return fmt.Errorf("run #%d not found", id)
		}
		it.Priority = tier
		return nil
	})
}

func applyDirectiveSay(queueDir string, id int64, text string) error {
	q, err := loadWeaveQueue(queueDir)
	if err != nil {
		return err
	}
	it := findWeaveItem(q, id)
	if it == nil {
		return fmt.Errorf("run #%d not found", id)
	}
	if it.State != "working" || it.WrapperPid == 0 || !pidAlive(it.WrapperPid) {
		return fmt.Errorf("run #%d has no live subagent (state=%q)", it.ID, it.State)
	}
	if it.CtlSock == "" {
		return fmt.Errorf("run #%d has no control socket", it.ID)
	}
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r", " "), "\n", " ")
	return weaveWriteControlFrame(it.CtlSock, text+"\r\n")
}

func applyDirectiveKill(queueDir string, id int64, reason string) error {
	var wrapperPid int
	var workspace string
	var verifyCommand string
	var verifyItem *weaveItem
	if err := withWeaveQueueLock(queueDir, func(q *weaveQueue) error {
		it := findWeaveItem(q, id)
		if it == nil {
			return fmt.Errorf("run #%d not found", id)
		}
		if it.State != "working" {
			return fmt.Errorf("run #%d state is %q (kill requires working)", id, it.State)
		}
		wrapperPid = it.WrapperPid
		workspace = it.Workspace
		verifyCommand = it.VerifyCommand
		copy := *it
		verifyItem = &copy
		return nil
	}); err != nil {
		return err
	}
	if wrapperPid > 0 {
		weaveStopWrapper(wrapperPid)
	}

	base := "HEAD"
	ahead, head := weaveMeasureBranch(workspace, base)
	dirty, dirtyFiles, untrackedFiles := weaveMeasureDirtiness(workspace)
	var verifyExit *int
	var verifyOutput string
	var verifyTree string
	if verifyCommand != "" && (ahead > 0 || dirty) {
		verifyExit, verifyOutput, verifyTree = weaveCollectVerifyEvidence(workspace, queueDir, verifyCommand, verifyItem, dirty, dirtyFiles)
	}

	err := withWeaveQueueLock(queueDir, func(q *weaveQueue) error {
		it := findWeaveItem(q, id)
		if it == nil {
			return fmt.Errorf("run #%d not found", id)
		}
		if it.State != "working" && it.State != "killed" {
			return fmt.Errorf("run #%d state is %q (kill requires working)", id, it.State)
		}
		it.CommitsAhead = ahead
		it.Head = head
		it.Dirty = dirty
		it.DirtyFiles = dirtyFiles
		it.UntrackedFiles = untrackedFiles
		if verifyExit != nil {
			it.VerifyExit = verifyExit
			it.VerifyOutput = verifyOutput
			it.VerifyTree = verifyTree
		}
		it.State = "killed"
		killCode := -1
		if it.ExitCode == nil {
			it.ExitCode = &killCode
		}
		it.FinishedAt = time.Now().UTC()
		it.WrapperPid = 0
		it.CtlSock = ""
		note := "[killed by directive"
		if reason != "" {
			note += ": " + reason
		}
		if ahead > 0 {
			note += fmt.Sprintf(" - %d wrapper-verified commit(s) ahead at %.12s", ahead, head)
		}
		if !strings.HasPrefix(it.Body, "[killed") {
			it.Body = note + "]\n\n" + it.Body
		}
		return nil
	})
	if err == nil && verifyExit != nil {
		if cleanErr := weaveCleanupManagedGOCache(queueDir, verifyItem); cleanErr != nil {
			return cleanErr
		}
	}
	return err
}

func readDirectiveCursor(queueDir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(queueDir, "directive-cursor"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func writeDirectiveCursor(queueDir, cursor string) error {
	if err := os.MkdirAll(queueDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(queueDir, "directive-cursor")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(cursor), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func runWeaveConduct(cmd *cobra.Command, interval time.Duration, flags *weaveOutputFlags) error {
	const verb = "weave conduct"
	mode := flags.mode()
	if interval <= 0 {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb,
			weavecli.ExitInvalidArg, fmt.Errorf("--interval must be positive")))
	}
	sc, err := sessionClientForRepo()
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb, weavecli.ExitPrecondFail, err))
	}
	taskID, err := joinedTaskID(sc.pointer)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb, weavecli.ExitPrecondFail, err))
	}
	queueDir, err := weaveQueueDir(sc.repoRoot)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb, weavecli.ExitGenericFail, err))
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	for {
		cursor, err := readDirectiveCursor(queueDir)
		if err != nil {
			return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb, weavecli.ExitGenericFail, err))
		}
		next, applied, err := consumeDirectives(ctx, sc.client, taskID, queueDir, cursor)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb, weavecli.ExitGenericFail, err))
		}
		if next != cursor {
			if err := writeDirectiveCursor(queueDir, next); err != nil {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb, weavecli.ExitGenericFail, err))
			}
		}
		if mode == weavecli.OutputJSON && applied > 0 {
			_ = emitOK(cmd.OutOrStdout(), mode, verb, map[string]any{
				"cursor":  next,
				"applied": applied,
			})
		}

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func newWeaveConductCmd() *cobra.Command {
	var flags weaveOutputFlags
	var interval time.Duration
	cmd := &cobra.Command{
		Use:   "conduct",
		Short: "Poll a joined shared session for host-side directives",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 0 {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), flags.mode(), "weave conduct",
					weavecli.ExitInvalidArg, fmt.Errorf("expected no arguments")))
			}
			return runWeaveConduct(cmd, interval, &flags)
		},
	}
	flags.attach(cmd)
	cmd.Flags().DurationVar(&interval, "interval", 3*time.Second, "Directive poll interval")
	return cmd
}
