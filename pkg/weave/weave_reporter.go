package weave

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func ensureSessionSprint(ctx context.Context, client SessionClient, ptr *SessionPointer, repoRoot string) (string, error) {
	if ptr == nil {
		return "", fmt.Errorf("missing session pointer")
	}
	if ptr.SprintID != "" {
		return ptr.SprintID, nil
	}
	taskID, err := joinedTaskID(ptr)
	if err != nil {
		return "", err
	}
	sprint, err := client.CreateSprint(ctx, CreateSprintReq{
		TargetRepo: repoRoot,
		Gate:       weaveReporterSprintGate(repoRoot),
		TaskID:     taskID,
	})
	if err != nil {
		return "", err
	}
	if sprint.ID == "" {
		return "", fmt.Errorf("cloudbox returned empty sprint id")
	}
	ptr.SprintID = sprint.ID
	if err := WriteSessionPointer(repoRoot, ptr); err != nil {
		return "", err
	}
	return sprint.ID, nil
}

func weaveReportTerminal(ctx context.Context, repoRoot string, it *weaveItem, ev weaveTerminalEvidence) error {
	ptr, err := ReadSessionPointer(repoRoot)
	if err != nil {
		return err
	}
	if ptr == nil {
		return nil
	}
	sc, err := sessionClientForRepoRoot(repoRoot)
	if err != nil {
		return err
	}
	taskID, err := joinedTaskID(sc.pointer)
	if err != nil {
		return err
	}
	sprintID, err := ensureSessionSprint(ctx, sc.client, sc.pointer, sc.repoRoot)
	if err != nil {
		return err
	}
	req := weaveReporterRunReq(it, ev)
	if _, err := sc.client.UpsertRun(ctx, sprintID, req); err != nil {
		return err
	}
	detail, _ := json.Marshal(map[string]any{
		"issue":         req.Issue,
		"agent":         req.Agent,
		"status":        req.Status,
		"commits_ahead": req.CommitsAhead,
		"exit":          req.Exit,
		"sandbox":       req.Sandbox,
		"branch":        req.Branch,
		"sprint_id":     sprintID,
	})
	kind := "attempt"
	if ev.VerifyExit != nil || (it != nil && it.VerifyExit != nil) {
		kind = "verdict"
	}
	_, err = sc.client.AppendEvent(ctx, taskID, AppendEventReq{
		Kind:    kind,
		Summary: weaveReporterEventSummary(req),
		Detail:  detail,
	})
	return err
}

func weaveReporterSprintGate(repoRoot string) string {
	b, err := os.ReadFile(filepath.Join(repoRoot, ".agents", "weave", "suite-gate"))
	if err == nil {
		if gate := strings.TrimSpace(string(b)); gate != "" {
			return gate
		}
	}
	return "weave verify"
}

func weaveReporterRunReq(it *weaveItem, ev weaveTerminalEvidence) UpsertRunReq {
	if it == nil {
		return UpsertRunReq{}
	}
	exit := 0
	if it.ExitCode != nil {
		exit = *it.ExitCode
	}
	host, _ := os.Hostname()
	gateOutput := ev.VerifyOutput
	if gateOutput == "" {
		gateOutput = it.VerifyOutput
	}
	return UpsertRunReq{
		Issue:        weaveReporterIssue(it),
		Agent:        it.Tool,
		Host:         host,
		Branch:       it.Branch,
		Sandbox:      it.Workspace,
		Status:       weaveReporterStatus(it),
		CommitsAhead: it.CommitsAhead,
		Exit:         exit,
		Verdict:      weaveReporterVerdict(it, ev),
		GateOutput:   gateOutput,
		LogTail:      weaveReporterLogTail(it.LogPath),
		Tool:         it.Tool,
	}
}

func weaveReporterIssue(it *weaveItem) string {
	title := strings.TrimSpace(it.Title)
	if title == "" {
		return fmt.Sprintf("%d", it.ID)
	}
	return fmt.Sprintf("#%d %s", it.ID, title)
}

func weaveReporterStatus(it *weaveItem) string {
	switch it.State {
	case "todo", "queued", "allocated":
		return "queued"
	case "working":
		return "running"
	case "submitted":
		if it.VerifyExit != nil && *it.VerifyExit == 0 {
			return "verified"
		}
		return "submitted"
	case "done":
		return "merged"
	case "killed":
		return "killed"
	case "no-op":
		return "no-op"
	default:
		return "failed"
	}
}

func weaveReporterVerdict(it *weaveItem, ev weaveTerminalEvidence) string {
	verifyExit := it.VerifyExit
	output := it.VerifyOutput
	if ev.VerifyExit != nil {
		verifyExit = ev.VerifyExit
		output = ev.VerifyOutput
	}
	if verifyExit != nil {
		summary := weaveReporterOneLine(output)
		if summary != "" {
			return fmt.Sprintf("verify exit %d: %s", *verifyExit, summary)
		}
		return fmt.Sprintf("verify exit %d", *verifyExit)
	}
	if it.State != "" {
		return it.State
	}
	return "attempt"
}

func weaveReporterEventSummary(req UpsertRunReq) string {
	parts := []string{req.Issue, req.Status, fmt.Sprintf("%d commit(s) ahead", req.CommitsAhead)}
	if req.Exit != 0 {
		parts = append(parts, fmt.Sprintf("exit %d", req.Exit))
	}
	return strings.Join(parts, " - ")
}

func weaveReporterOneLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if len(line) > 240 {
			line = line[:240]
		}
		return line
	}
	return ""
}

func weaveReporterLogTail(path string) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if len(b) > 2000 {
		b = b[len(b)-2000:]
	}
	return string(b)
}
