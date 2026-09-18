package sdlc

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

func parseIssueNumbers(value string) ([]int, error) {
	var numbers []int
	seen := make(map[int]bool)
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("sdlc: invalid issue number %q", part)
		}
		if !seen[n] {
			seen[n] = true
			numbers = append(numbers, n)
		}
	}
	if len(numbers) == 0 {
		return nil, fmt.Errorf("sdlc: --issue requires at least one issue number")
	}
	return numbers, nil
}

// newChangesCmd polls already-tracked issues for human steering comments. It is
// intentionally separate from watch, which follows a local conductor run.
func newChangesCmd() *cobra.Command {
	var repo, issues, runsDir, stateFile string
	var ignoreAuthors []string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "changes --repo OWNER/NAME --issue N[,M...]",
		Short: "poll tracked GitHub issues for new steering comments",
		RunE: func(cmd *cobra.Command, _ []string) error {
			numbers, err := parseIssueNumbers(issues)
			if err != nil {
				return err
			}
			events, err := PollGitHubIssueChanges(cmd.Context(), IssueChangesOptions{Repo: repo, IssueNumbers: numbers, RunsDir: runsDir, StateFile: stateFile, IgnoreAuthors: ignoreAuthors})
			if err != nil {
				return err
			}
			if asJSON {
				b, _ := json.Marshal(map[string]any{"schema_version": schemaVersion, "events": events})
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
				return nil
			}
			for _, event := range events {
				fmt.Fprintf(cmd.OutOrStdout(), "#%d comment=%d author=%s created_at=%s\n%s\n", event.IssueNumber, event.CommentID, event.Author, event.CreatedAt.Format("2006-01-02T15:04:05Z07:00"), event.Body)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&repo, "repo", "", "GitHub repository (owner/name)")
	cmd.Flags().StringVar(&issues, "issue", "", "tracked issue number(s), comma-separated")
	cmd.Flags().StringVar(&runsDir, "runs-dir", ".bashy/generated/sdlc/runs", "local SDLC runs directory")
	cmd.Flags().StringVar(&stateFile, "state-file", "", "watermark state file (defaults under runs-dir)")
	cmd.Flags().StringSliceVar(&ignoreAuthors, "ignore-author", nil, "GitHub login to consume without emitting (repeatable or comma-separated)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	_ = cmd.MarkFlagRequired("repo")
	_ = cmd.MarkFlagRequired("issue")
	return cmd
}
