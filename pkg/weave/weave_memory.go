package weave

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/coreutils/pkg/weavecli"
	"github.com/qiangli/yoke/pkg/weave/memory"
)

func runWeaveRemember(cmd *cobra.Command, text string, issueID int64, tags []string, flags *weaveOutputFlags) error {
	mode := flags.mode()
	if strings.TrimSpace(text) == "" {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave remember",
			weavecli.ExitInvalidArg, fmt.Errorf("text required")))
	}
	dir, err := weaveQueueDirForCWD()
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave remember", weavecli.ExitPrecondFail, err))
	}
	st, _, err := memory.Open(dir, memory.Prefs{})
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave remember", weavecli.ExitGenericFail, err))
	}
	o := memory.Observation{
		IssueID:   issueID,
		Outcome:   "note",
		Summary:   text,
		Tags:      cleanMemoryTags(tags),
		CreatedAt: time.Now().UTC(),
	}
	if issueID > 0 {
		if q, err := loadWeaveQueue(dir); err == nil {
			if it := findWeaveItem(q, issueID); it != nil {
				o.Title = it.Title
				o.Tool = it.Tool
			}
		}
	}
	if err := st.Remember(context.Background(), o); err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave remember", weavecli.ExitGenericFail, err))
	}
	if mode == weavecli.OutputJSON {
		return ec(emitOK(cmd.OutOrStdout(), mode, "weave remember", o))
	}
	if issueID > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "weave remember: noted run #%d\n", issueID)
	} else {
		fmt.Fprintln(cmd.OutOrStdout(), "weave remember: noted")
	}
	return nil
}

func runWeaveRecall(cmd *cobra.Command, query string, issueID int64, filesCSV string, flags *weaveOutputFlags) error {
	mode := flags.mode()
	dir, err := weaveQueueDirForCWD()
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave recall", weavecli.ExitPrecondFail, err))
	}
	st, _, err := memory.Open(dir, memory.Prefs{})
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave recall", weavecli.ExitGenericFail, err))
	}
	obs, err := st.Recall(context.Background(), memory.Query{
		Title:   query,
		IssueID: issueID,
		Files:   splitMemoryCSV(filesCSV),
		Limit:   10,
	})
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave recall", weavecli.ExitGenericFail, err))
	}
	if mode == weavecli.OutputJSON {
		return ec(emitOK(cmd.OutOrStdout(), mode, "weave recall", obs))
	}
	for _, o := range obs {
		fmt.Fprintf(cmd.OutOrStdout(), "#%d %-9s %s\n", o.IssueID, o.Outcome, memoryOneLine(o))
	}
	return nil
}

func runWeaveMemory(cmd *cobra.Command, action string, issueID int64, flags *weaveOutputFlags) error {
	mode := flags.mode()
	dir, err := weaveQueueDirForCWD()
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave memory", weavecli.ExitPrecondFail, err))
	}
	all, err := weaveReadAllMemory(dir)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave memory", weavecli.ExitGenericFail, err))
	}
	switch action {
	case "list":
		if mode == weavecli.OutputJSON {
			return ec(emitOK(cmd.OutOrStdout(), mode, "weave memory list", all))
		}
		for _, o := range all {
			fmt.Fprintf(cmd.OutOrStdout(), "#%d %-9s %s\n", o.IssueID, o.Outcome, memoryOneLine(o))
		}
		return nil
	case "show":
		var hits []memory.Observation
		for _, o := range all {
			if o.IssueID == issueID {
				hits = append(hits, o)
			}
		}
		if mode == weavecli.OutputJSON {
			return ec(emitOK(cmd.OutOrStdout(), mode, "weave memory show", hits))
		}
		for _, o := range hits {
			fmt.Fprintf(cmd.OutOrStdout(), "#%d %-9s %s\n", o.IssueID, o.Outcome, memoryOneLine(o))
		}
		return nil
	case "export":
		events := make([]map[string]any, 0, len(all))
		for _, o := range all {
			events = append(events, map[string]any{
				"type":       "weave.memory.observation",
				"created_at": o.CreatedAt,
				"issue_id":   o.IssueID,
				"payload":    o,
			})
		}
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		_ = enc.Encode(events)
		return nil
	default:
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave memory",
			weavecli.ExitInvalidArg, fmt.Errorf("expected list, show <issue>, or export")))
	}
}

func weaveQueueDirForCWD() (string, error) {
	cwd, _ := os.Getwd()
	root, err := weaveRepoRoot(cwd)
	if err != nil {
		return "", err
	}
	return weaveQueueDir(root)
}

func weaveReadAllMemory(dir string) ([]memory.Observation, error) {
	st, _, err := memory.Open(dir, memory.Prefs{})
	if err != nil {
		return nil, err
	}
	reader, ok := st.(interface {
		ReadAll(context.Context) ([]memory.Observation, error)
	})
	if !ok {
		return st.Recall(context.Background(), memory.Query{Limit: 1 << 30})
	}
	return reader.ReadAll(context.Background())
}

func cleanMemoryTags(tags []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, tag := range tags {
		for _, part := range strings.Split(tag, ",") {
			part = strings.TrimSpace(part)
			if part != "" && !seen[part] {
				seen[part] = true
				out = append(out, part)
			}
		}
	}
	return out
}

func splitMemoryCSV(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func memoryOneLine(o memory.Observation) string {
	s := strings.TrimSpace(o.Summary)
	if s == "" {
		s = o.Title
	}
	if len(o.FilesTouched) > 0 {
		s += " [" + strings.Join(o.FilesTouched, ", ") + "]"
	}
	return weaveTruncate(s, 180)
}

func parseMemoryIssueArg(arg string) (int64, error) {
	id, err := strconv.ParseInt(arg, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("issue must be an integer: %q", arg)
	}
	return id, nil
}
