// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package issue

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/atlas"
)

// NewIssueCmd builds `bashy issue` — the project's issue register.
func NewIssueCmd(repoRoot func() (string, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "issue",
		Short: "the project's issue register: bugs, features, requirements — before anyone starts work",
		Long: `issue is the durable, COMMITTED record of what is wrong, what is wanted, and what
is required -- filed before anyone starts working on it.

It fills the one hole in the lifecycle nothing else covered. weave tracks work an
agent is ACTIVELY DOING (a per-machine queue, with a workspace and a branch).
sprint tracks what a conductor is planning RIGHT NOW. Neither can hold a bug
nobody has triaged, a requirement nobody has scheduled, or a feature somebody
merely asked for -- so those lived as bullet points in docs/TODO.md, invisible to
every verb, unqueryable, and impossible to close.

THE REGISTER IS COMMITTED. It lives in .bashy/issues/ inside the repo -- source,
not scratch. A requirement must travel with the clone, show up in a diff, be
reviewable in a pull request, survive the machine it was typed on, and need no
forge to exist. Each issue is a markdown file with a YAML frontmatter block: you
can read the whole register with 'cat' and edit it with any editor.

IDS ARE SHORT HASHES, NOT #1/#2. The register merges across branches, and a
monotonic counter is a merge-conflict generator -- two branches both file "#7" and
one must be renumbered, breaking every reference to it. Refer to an issue by any
unique prefix, exactly like a git commit.

AN ISSUE LIVES IN ONE REPO BUT MAY BE ABOUT SEVERAL. --refs names the other
modules or repos it touches: a bug whose fix spans a library and its consumer is
one issue, not two.`,
		Example: `  bashy issue add "trap -p prints the wrong signal name" --kind bug --refs ../sh
  bashy issue list --kind bug --status open
  bashy issue show a3f2
  bashy issue triage a3f2 --stage test --priority p1
  bashy issue close a3f2 --resolution fixed`,
	}
	cmd.AddCommand(
		newAddCmd(repoRoot),
		newListCmd(repoRoot),
		newShowCmd(repoRoot),
		newTriageCmd(repoRoot),
		newCommentCmd(repoRoot),
		newCloseCmd(repoRoot),
		newReopenCmd(repoRoot),
	)
	return cmd
}

// newCommentCmd appends to an issue's discussion thread.
//
// An issue is not a row in a table; it is where a DECISION gets argued and recorded.
// Strip the thread and you keep the title and lose the reasoning — which is the half
// that mattered a year later, when someone asks why the feature was declined. The
// comment lands in the markdown body, so it is readable with `cat` and reviewable in
// the diff like everything else here.
func newCommentCmd(repoRoot func() (string, error)) *cobra.Command {
	var body, bodyFile string
	cmd := &cobra.Command{
		Use:   `comment <id> "<text>"`,
		Short: "append to an issue's thread",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := store(repoRoot)
			if err != nil {
				return err
			}
			it, err := s.Resolve(args[0])
			if err != nil {
				return err
			}
			if len(args) == 2 {
				body = args[1]
			}
			if bodyFile != "" {
				b, err := os.ReadFile(bodyFile)
				if err != nil {
					return err
				}
				body = string(b)
			}
			if strings.TrimSpace(body) == "" {
				return fmt.Errorf("nothing to say (give the text, or --body-file)")
			}
			it.Body = strings.TrimRight(it.Body, "\n") +
				fmt.Sprintf("\n\n---\n**%s** · %s\n\n%s\n",
					whoami(), time.Now().UTC().Format(time.RFC3339), strings.TrimSpace(body))
			if _, err := s.Save(it); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "issue %s: comment added\n", it.ID[:8])
			return nil
		},
	}
	cmd.Flags().StringVar(&body, "body", "", "the comment")
	cmd.Flags().StringVar(&bodyFile, "body-file", "", "read the comment from a file")
	return cmd
}

func store(repoRoot func() (string, error)) (*Store, error) {
	root, err := repoRoot()
	if err != nil {
		return nil, err
	}
	return New(root), nil
}

func newAddCmd(repoRoot func() (string, error)) *cobra.Command {
	var kind, body, bodyFile, priority, reporter string
	var refs, labels []string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   `add "<title>"`,
		Short: "file a new issue into the register",
		Long: `add files an issue. It is born "open" -- filed, not yet triaged.

That distinction is the whole point: a register that cannot hold an UNTRIAGED
thought is a register nobody files into, and the thought goes back to being a
bullet in a document nobody greps.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := store(repoRoot)
			if err != nil {
				return err
			}
			title := ""
			if len(args) == 1 {
				title = args[0]
			}
			if bodyFile != "" {
				b, err := os.ReadFile(bodyFile)
				if err != nil {
					return err
				}
				body = string(b)
			}
			if reporter == "" {
				reporter = whoami()
			}
			it := &Issue{
				Kind: kind, Title: title, Body: body, Priority: priority,
				Refs: refs, Labels: labels, Reporter: reporter,
			}
			path, err := s.Add(it)
			if err != nil {
				return err
			}
			if asJSON {
				return emit(cmd, map[string]any{"id": it.ID, "kind": it.Kind, "status": it.Status, "path": path})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "issue %s filed (%s, open) — %s\n", it.ID[:8], it.Kind, it.Title)
			fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", path)
			fmt.Fprintf(cmd.OutOrStdout(), "  commit it: the register is part of the repo\n")
			return nil
		},
	}
	cmd.Flags().StringVar(&kind, "kind", KindTask, "bug|feature|requirement|task")
	cmd.Flags().StringVar(&body, "body", "", "the details")
	cmd.Flags().StringVar(&bodyFile, "body-file", "", "read the details from a file ('-' is not supported; give a path)")
	cmd.Flags().StringVar(&priority, "priority", "", "p0|p1|p2|p3")
	cmd.Flags().StringArrayVar(&refs, "refs", nil, "another module/repo this issue touches (repeatable)")
	cmd.Flags().StringArrayVar(&labels, "label", nil, "a label (repeatable)")
	cmd.Flags().StringVar(&reporter, "reporter", "", "who filed it (defaults to the agent or user)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return cmd
}

func newListCmd(repoRoot func() (string, error)) *cobra.Command {
	var kind, status, stage string
	var all, asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "what is on the register",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := store(repoRoot)
			if err != nil {
				return err
			}
			all_, err := s.List()
			if err != nil {
				return err
			}
			var out []*Issue
			for _, it := range all_ {
				if !all && !it.Open() {
					continue // closed issues are history; show them on request
				}
				if kind != "" && it.Kind != kind {
					continue
				}
				if status != "" && it.Status != status {
					continue
				}
				if stage != "" && it.Stage != stage {
					continue
				}
				out = append(out, it)
			}
			if asJSON {
				return emit(cmd, out)
			}
			if len(out) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "the register is empty (`bashy issue add \"...\"`)")
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%-8s %-12s %-8s %-7s %-6s %-40s %s\n",
				"ID", "KIND", "STATUS", "STAGE", "PRIO", "TITLE", "REFS")
			for _, it := range out {
				fmt.Fprintf(cmd.OutOrStdout(), "%-8s %-12s %-8s %-7s %-6s %-40s %s\n",
					it.ID[:8], it.Kind, it.Status, dash(it.Stage), dash(it.Priority),
					trunc(it.Title, 40), strings.Join(it.Refs, ","))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "", "bug|feature|requirement|task")
	cmd.Flags().StringVar(&status, "status", "", "open|triaged|closed")
	cmd.Flags().StringVar(&stage, "stage", "", "plan|code|test|deploy")
	cmd.Flags().BoolVar(&all, "all", false, "include closed issues")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return cmd
}

func newShowCmd(repoRoot func() (string, error)) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "show <id>",
		Short: "show one issue (by id or unique prefix)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := store(repoRoot)
			if err != nil {
				return err
			}
			it, err := s.Resolve(args[0])
			if err != nil {
				return err
			}
			if asJSON {
				return emit(cmd, it)
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "%s  %s\n\n", it.ID, it.Title)
			fmt.Fprintf(w, "  kind      %s\n", it.Kind)
			fmt.Fprintf(w, "  status    %s\n", it.Status)
			if it.Stage != "" {
				fmt.Fprintf(w, "  stage     %s\n", it.Stage)
			}
			if it.Priority != "" {
				fmt.Fprintf(w, "  priority  %s\n", it.Priority)
			}
			if len(it.Refs) > 0 {
				fmt.Fprintf(w, "  refs      %s\n", strings.Join(it.Refs, ", "))
			}
			if it.Weave != 0 {
				if it.Status == StatusClosed {
					fmt.Fprintf(w, "  weave     #%d\n", it.Weave)
				} else {
					fmt.Fprintf(w, "  weave     #%d (in flight)\n", it.Weave)
				}
			}
			fmt.Fprintf(w, "  reporter  %s\n", dash(it.Reporter))
			fmt.Fprintf(w, "  created   %s\n", it.Created.Format(time.RFC3339))
			if it.Closed != nil {
				fmt.Fprintf(w, "  closed    %s (%s) by %s\n", it.Closed.Format(time.RFC3339), it.Resolution, dash(it.ClosedBy))
			}
			if it.Body != "" {
				fmt.Fprintf(w, "\n%s\n", it.Body)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return cmd
}

// newTriageCmd is the verb that turns a thought into work.
func newTriageCmd(repoRoot func() (string, error)) *cobra.Command {
	var stage, priority string
	cmd := &cobra.Command{
		Use:   "triage <id> --stage <stage> [--priority pN]",
		Short: "accept an issue and say which part of the lifecycle it belongs to",
		Long: `triage moves an issue from "open" (somebody said this) to "triaged" (we accept it,
and here is what it is).

--stage is REQUIRED, and that is the point. Deciding which part of the lifecycle an
issue belongs to -- is this a design question, a code change, a test gap, or a
deploy problem? -- IS triage. An accepted issue with no stage is an issue nobody
actually thought about, and it is exactly the kind of item that sits in a backlog
for a year.

The vocabulary is the atlas's, the same closed set every verb and every weave item
declares.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := store(repoRoot)
			if err != nil {
				return err
			}
			it, err := s.Resolve(args[0])
			if err != nil {
				return err
			}
			if stage == "" {
				return fmt.Errorf("--stage is required (one of: %s)\n\nDeciding which part of the lifecycle an issue belongs to IS triage; an accepted issue with no stage was never really thought about", strings.Join(workStages(), ", "))
			}
			if !validWorkStage(stage) {
				return fmt.Errorf("unknown stage %q (want one of: %s)", stage, strings.Join(workStages(), ", "))
			}
			it.Stage = stage
			if priority != "" {
				it.Priority = priority
			}
			it.Status = StatusTriaged
			if _, err := s.Save(it); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "issue %s triaged (%s/%s) — %s\n", it.ID[:8], it.Stage, dash(it.Priority), it.Title)
			fmt.Fprintf(cmd.OutOrStdout(), "  put it to work: bashy weave add --from-issue %s\n", it.ID[:8])
			return nil
		},
	}
	cmd.Flags().StringVar(&stage, "stage", "", "plan|code|test|deploy (required)")
	cmd.Flags().StringVar(&priority, "priority", "", "p0|p1|p2|p3")
	return cmd
}

func newCloseCmd(repoRoot func() (string, error)) *cobra.Command {
	var resolution, note string
	cmd := &cobra.Command{
		Use:   "close <id> --resolution <fixed|declined|duplicate|obsolete>",
		Short: "settle an issue",
		Long: `close settles an issue, and RECORDS WHY.

--resolution is required. "Closed" alone loses the single most useful fact about a
dead issue: whether it was fixed, or refused. A register of issues that merely
vanished teaches nobody anything, and the same feature gets proposed again next
quarter.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := store(repoRoot)
			if err != nil {
				return err
			}
			it, err := s.Resolve(args[0])
			if err != nil {
				return err
			}
			valid := []string{"fixed", "declined", "duplicate", "obsolete"}
			if resolution == "" {
				return fmt.Errorf("--resolution is required (one of: %s)\n\n\"closed\" alone loses the most useful fact about a dead issue: whether it was fixed, or refused", strings.Join(valid, ", "))
			}
			ok := false
			for _, v := range valid {
				if resolution == v {
					ok = true
				}
			}
			if !ok {
				return fmt.Errorf("unknown resolution %q (want one of: %s)", resolution, strings.Join(valid, ", "))
			}
			now := time.Now().UTC()
			it.Status = StatusClosed
			it.Closed = &now
			it.Resolution = resolution
			it.ClosedBy = whoami()
			if note != "" {
				it.Body = strings.TrimRight(it.Body, "\n") + "\n\n## Resolution\n\n" + note + "\n"
			}
			if _, err := s.Save(it); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "issue %s closed (%s) — %s\n", it.ID[:8], resolution, it.Title)
			return nil
		},
	}
	cmd.Flags().StringVar(&resolution, "resolution", "", "fixed|declined|duplicate|obsolete (required)")
	cmd.Flags().StringVar(&note, "note", "", "why — appended to the body")
	return cmd
}

func newReopenCmd(repoRoot func() (string, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "reopen <id>",
		Short: "put a settled issue back on the register",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := store(repoRoot)
			if err != nil {
				return err
			}
			it, err := s.Resolve(args[0])
			if err != nil {
				return err
			}
			it.Status = StatusOpen
			it.Closed, it.Resolution, it.ClosedBy = nil, "", ""
			if _, err := s.Save(it); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "issue %s reopened — %s\n", it.ID[:8], it.Title)
			return nil
		},
	}
}

// workStages is the atlas vocabulary minus "cross" — the same subtraction weave makes.
// A VERB may serve every stage; a unit of WORK cannot BE every stage.
func workStages() []string {
	out := make([]string, 0, 4)
	for _, s := range atlas.Stages() {
		if s != atlas.StageCross {
			out = append(out, s)
		}
	}
	return out
}

func validWorkStage(s string) bool {
	for _, v := range workStages() {
		if v == s {
			return true
		}
	}
	return false
}

// whoami attributes a record to the agent when one is driving, else the user.
func whoami() string {
	for _, k := range []string{"BASHY_PRINCIPAL", "WEAVE_AGENT", "USER", "LOGNAME"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return "unknown"
}

func emit(cmd *cobra.Command, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), string(b))
	return nil
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
