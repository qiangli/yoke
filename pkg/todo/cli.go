// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package todo

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/role"

	"github.com/qiangli/coreutils/pkg/weavecli"
	"github.com/qiangli/yoke/pkg/issue"
	"github.com/qiangli/yoke/pkg/kb"
	"github.com/qiangli/yoke/pkg/ref"
)

// storeFunc resolves the store for the current scope, returning it and a short scope
// label so every command can print WHICH list it is acting on.
type storeFunc func() (*issue.Store, string, error)

// itemJSON wraps an issue for JSON output, computing the overdue flag.
type itemJSON struct {
	*issue.Issue
	Ref     string `json:"ref"` // todo:<full id> — the canonical address (pkg/ref)
	Overdue bool   `json:"overdue"`
}

// refOf is the item's canonical ref, full id.
func refOf(it *issue.Issue) string { return ref.Format(ref.Todo, it.ID) }

// shortRef is the ref at the listing's id width: `todo:<8-hex>`, which resolves
// git-style by prefix. The full id is in --json and in `todo show`.
func shortRef(id string) string {
	if id == "" || len(id) < 8 {
		return shortID(id) // "(no-id)" or the short id itself, unprefixed
	}
	return ref.Format(ref.Todo, shortID(id))
}

// listItem is one row of the `todo list` result envelope — a projection of an
// issue into the fields a list consumer needs, rather than the full issue
// record (which drags in register-only fields like kind/stage/refs/weave).
//
//	state    — the lifecycle vocabulary (todo|assigned|doing|blocked|done);
//	           this is the issue's Status under the name a list consumer reads.
//	scope    — the resolved scope label, repeated per row so an item stays
//	           self-describing once rows from several lists are merged.
//	age      — the human-readable duration the text view shows (3d, 5h, 12m);
//	created  — the machine timestamp behind it.
type listItem struct {
	ID        string     `json:"id"`
	Seq       int        `json:"seq,omitempty"`
	Title     string     `json:"title"`
	State     string     `json:"state"`
	Priority  string     `json:"priority,omitempty"`
	Scope     string     `json:"scope"`
	Created   time.Time  `json:"created"`
	Age       string     `json:"age"`
	Due       *time.Time `json:"due,omitempty"`
	Overdue   bool       `json:"overdue,omitempty"`
	Recurring string     `json:"recurring,omitempty"`
	Assignee  string     `json:"assignee,omitempty"`
	Closed    *time.Time `json:"closed,omitempty"`

	// Sprint is the sprint card this item is a story of, or 0 when it is a
	// free-standing item. Emitted so a reader (the steward board, the web
	// console) can group stories under their card without re-reading every
	// item file — and never required, because todo does not depend on sprint.
	Sprint int64 `json:"sprint,omitempty"`
}

// listResult is the result object of the `todo list --json` envelope: the
// resolved scope/folder (which list this is — the same context the text header
// prints) and the projected rows. Wrapping items under result keeps the shape
// aligned with the other bashy verbs (weave list --json -> result.items).
type listResult struct {
	Scope  string     `json:"scope"`
	Folder string     `json:"folder"`
	Count  int        `json:"count"`
	Items  []listItem `json:"items"`
}

// toListItems projects the store's issues into list-envelope rows, stamping each
// with the resolved scope so a row is self-describing out of context. age is
// computed here (not stored) so it stays consistent with the text view.
func toListItems(items []*issue.Issue, scope string) []listItem {
	out := make([]listItem, 0, len(items))
	for _, it := range items {
		out = append(out, listItem{
			ID:        it.ID,
			Seq:       it.Seq,
			Title:     it.Title,
			State:     it.Status,
			Priority:  it.Priority,
			Scope:     scope,
			Created:   it.Created,
			Age:       age(it.Created),
			Due:       it.Due,
			Overdue:   IsOverdue(it),
			Recurring: it.Recurring,
			Assignee:  it.Assignee,
			Closed:    it.Closed,
			Sprint:    it.Sprint,
		})
	}
	return out
}

// NewTodoCmd builds `bashy todo` — the task list over one item model. The scope is
// AUTO-DETECTED so an agent can just `bashy todo add …` and it lands correctly:
//
//	default   inside a git repo → THAT repo's docs/todo/ (committed); otherwise your
//	          personal host list (~/.bashy/todo/<owner>/)
//	--user    force the personal list even inside a repo
//	--repo    force the repo list (error if not in a git repo)
//	--dir P   point the list at any base directory P
//
// Every command prints a one-line header with the resolved folder, so there is never
// any doubt about which list you are on.
func NewTodoCmd() *cobra.Command {
	var baseDir string
	var forceRepo, forceUser bool
	root := &cobra.Command{
		Use:   "todo",
		Short: "the task list — auto: repo docs/todo/ if in a git repo, else your host list",
		Long: "todo tracks work as simple items (todo -> doing -> done, or blocked). The SCOPE is\n" +
			"auto-detected from where you are, so no flag is needed for the common case:\n\n" +
			"  in a git repo    → THAT repo's docs/todo/ (committed, travels with the clone) —\n" +
			"                     the structured replacement for an ad-hoc TODO.md.\n" +
			"  not in a repo    → your personal host list (~/.bashy/todo/<owner>/, not committed).\n\n" +
			"Overrides: --base-dir <root> shows ANOTHER project's list (<root>/docs/todo/) —\n" +
			"so one agent can travel between repos in a single session; --user forces the\n" +
			"personal list even inside a repo; --repo forces the repo list. Every command prints\n" +
			"a header showing the resolved folder, so which list you are on is never in doubt.\n\n" +
			"Relation to the other trackers: `bashy sprint` is a TIME-BOX (a window grouping\n" +
			"items); a sprint story is a repo todo filed with `todo add --sprint N`;\n" +
			"`bashy weave` is the execution queue (`weave add --from-todo` seeds a run).",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	// Mounted under `bashy`, so cobra's generated `completion` verb documents a
	// `todo` binary that does not exist.
	root.CompletionOptions.DisableDefaultCmd = true
	root.PersistentFlags().BoolVar(&forceRepo, "repo", false, "force THIS repo's committed list (docs/todo/); error if not in a git repo")
	root.PersistentFlags().BoolVar(&forceUser, "user", false, "force your personal host list (~/.bashy/todo/<owner>/), even inside a repo")
	root.PersistentFlags().StringVar(&baseDir, "base-dir", "", "show the list of ANOTHER project root (<root>/docs/todo/) — travel repos without cd")

	sf := func() (*issue.Store, string, error) {
		st, label, err := ResolveStore(DefaultOwner, forceRepo, forceUser, baseDir)
		if err != nil {
			return nil, "", err
		}
		// Assign stable running numbers to any legacy items, once, so every command
		// (list, show 3, done 3, …) sees consistent handles. Best-effort.
		_ = EnsureSeq(st)
		return st, label, nil
	}

	root.AddCommand(
		newAddCmd(sf),
		newListCmd(sf),
		newShowCmd(sf),
		newStatusCmd(sf),
		newDoneCmd(sf),
		newStartCmd(sf),
		newEditCmd(sf),
		newRmCmd(sf),
	)
	return root
}

// folder is the resolved on-disk directory of a store (where the item files live).
func folder(st *issue.Store) string { return filepath.Join(st.Root, st.Sub) }

// header is the "which list am I on" line printed before command output. scope is the
// label ResolveStore returns ("repo <root>" | "user <owner>" | "dir <path>"); the
// folder makes the exact location unambiguous.
func header(scope string, st *issue.Store) string {
	word := scope
	if i := strings.IndexByte(scope, ' '); i > 0 {
		word = scope[:i]
	}
	return fmt.Sprintf("todo [%s] %s", word, folder(st))
}

func emitJSON(cmd *cobra.Command, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), string(b))
	return nil
}

// noteArg resolves --note the way `skill add -` and `agent add -` resolve a
// source: "-" is stdin, anything else is the literal body. Sprint 178 filed a
// story whose body was the single character "-".
func noteArg(cmd *cobra.Command, note string) (string, error) {
	if note != "-" {
		return note, nil
	}
	data, err := io.ReadAll(cmd.InOrStdin())
	if err != nil {
		return "", fmt.Errorf("--note -: %w", err)
	}
	return string(data), nil
}

func newAddCmd(sf storeFunc) *cobra.Command {
	var priority, note string
	var dueStr, recurring, assignee string
	var sprint int64
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "add <title>",
		Short: "add a task to the list",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, _, err := sf()
			if err != nil {
				return err
			}
			if cmd.Flags().Changed("sprint") && sprint < 1 {
				return fmt.Errorf("--sprint must be a positive sprint number")
			}
			due, err := parseDue(dueStr)
			if err != nil {
				return err
			}
			if note, err = noteArg(cmd, note); err != nil {
				return err
			}
			it, err := Add(st, strings.Join(args, " "), note, priority, due, recurring, assignee)
			if err != nil {
				return err
			}
			if cmd.Flags().Changed("sprint") {
				it.Sprint = sprint
				if _, err := st.Save(it); err != nil {
					return err
				}
			}
			var notice AssignmentNotice
			if it.Assignee != "" {
				notice = notifyAssignee("todo", it)
			}
			if jsonOut {
				out := map[string]any{"id": it.ID, "status": it.Status, "title": it.Title, "sprint": it.Sprint}
				if notice.Assignee != "" {
					out["assignee_notified"] = notice.Notified
					if !notice.Notified {
						out["assignee_reason"] = notice.Reason
					}
				}
				return emitJSON(cmd, out)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "added %s [%s] — %s\n", it.ID[:8], it.Status, it.Title)
			printAssignmentNotice(cmd, notice)
			return nil
		},
	}
	cmd.Flags().StringVar(&priority, "priority", "", "priority tier (p0|p1|p2|p3)")
	cmd.Flags().StringVar(&note, "note", "", "task body/details (- reads stdin); this item's own facts — a reusable procedure belongs in a kb runbook, cited as [[kb:<slug>]]")
	cmd.Flags().StringVar(&dueStr, "due", "", "deadline (e.g. 2026-07-20, +3d)")
	cmd.Flags().StringVar(&recurring, "recurring", "", "cadence (default=driven by `sprint advance`; or daily, weekly, 24h, cron)")
	// ONE FLAG, DOMAIN TITLES: an item's --owner is its ASSIGNEE.
	role.AttachOwner(cmd.Flags(), &assignee, role.Assignee,
		"who is working the item (notified over bashy notify; see bashy inbox)")
	cmd.Flags().Int64Var(&sprint, "sprint", 0, "sprint number this story belongs to")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "machine-readable output")
	return cmd
}

func newListCmd(sf storeFunc) *cobra.Command {
	var status string
	var jsonOut, all, reverse bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "list tasks (priority first — p0 before p1 …; ties by number #1 first; --reverse flips; open by default, --all includes done)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, scope, err := sf()
			if err != nil {
				return err
			}
			items, err := List(st, status)
			if err != nil {
				return err
			}
			if status == "" && !all {
				var open []*issue.Issue
				for _, it := range items {
					if it.Status != StatusDone {
						open = append(open, it)
					}
				}
				items = open
			}
			if reverse {
				slices.Reverse(items)
			}
			if jsonOut {
				// Machine-readable envelope — same versioned shape the other bashy
				// verbs use (schema_version + command + status + result.items), via
				// the shared weavecli emitter so schema_version stays pinned to the
				// one constant every verb agrees on. Text output below is unchanged.
				res := listResult{
					Scope:  scope,
					Folder: folder(st),
					Count:  len(items),
					Items:  toListItems(items, scope),
				}
				weavecli.EmitOK(cmd.OutOrStdout(), weavecli.OutputJSON, "todo list", res)
				return nil
			}
			// The header names WHICH list — auto-detected scope + exact folder — so
			// there is never confusion about where these tasks live.
			fmt.Fprintln(cmd.OutOrStdout(), header(scope, st))
			// A committed list is shared through git; say how fresh this
			// host's copy is, and what to run when it is behind.
			if strings.HasPrefix(scope, "repo") {
				if fresh := RepoFreshness(st.Root); fresh != "" {
					fmt.Fprintln(cmd.OutOrStdout(), fresh)
				}
			}
			if len(items) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no tasks (bashy todo add \"...\")")
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			// #  is the stable running number — the short handle for `todo show 3`,
			// `todo done 3`, etc. ID stays for scripts / cross-tool references.
			hasAssignee := false
			for _, it := range items {
				if it.Assignee != "" {
					hasAssignee = true
					break
				}
			}
			headerStr := "#\tREF\tSTATUS\tPRIO\tAGE\tDUE"
			if hasAssignee {
				headerStr += "\tASSIGNEE"
			}
			headerStr += "\tTITLE"
			fmt.Fprintln(w, headerStr)
			for _, it := range items {
				dueStr := "-"
				if it.Due != nil {
					dueStr = it.Due.Format("2006-01-02")
					if IsOverdue(it) {
						dueStr += " (OVERDUE)"
					}
				}
				row := fmt.Sprintf("%d\t%s\t%s\t%s\t%s\t%s",
					it.Seq, shortRef(it.ID), it.Status, dash(it.Priority), age(it.Created), dueStr)
				if hasAssignee {
					row += fmt.Sprintf("\t%s", dash(it.Assignee))
				}
				row += fmt.Sprintf("\t%s\n", trunc(it.Title, 60))
				fmt.Fprint(w, row)
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&status, "status", "", "filter by status (todo|assigned|doing|blocked|done)")
	cmd.Flags().BoolVar(&all, "all", false, "include done tasks")
	cmd.Flags().BoolVar(&reverse, "reverse", false, "reverse the order (default is priority first, then #1 first)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "machine-readable output")
	return cmd
}

func newShowCmd(sf storeFunc) *cobra.Command {
	var jsonOut, links bool
	cmd := &cobra.Command{
		Use:   "show <id|prefix>",
		Short: "show one task",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, _, err := sf()
			if err != nil {
				return err
			}
			it, err := ResolveRef(st, args[0])
			if err != nil {
				return err
			}
			if jsonOut {
				if links {
					out, in := resolveLinks(st, it)
					return emitJSON(cmd, struct {
						*issue.Issue
						Ref      string    `json:"ref"`
						Overdue  bool      `json:"overdue"`
						Outbound []linkRef `json:"outbound"`
						Inbound  []linkRef `json:"inbound"`
					}{Issue: it, Ref: refOf(it), Overdue: IsOverdue(it), Outbound: out, Inbound: in})
				}
				return emitJSON(cmd, itemJSON{Issue: it, Ref: refOf(it), Overdue: IsOverdue(it)})
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "#%d  %s  %s\n\n", it.Seq, refOf(it), it.Title)
			fmt.Fprintf(w, "  status    %s\n", it.Status)
			if it.Priority != "" {
				fmt.Fprintf(w, "  priority  %s\n", it.Priority)
			}
			fmt.Fprintf(w, "  created   %s\n", it.Created.Format(time.RFC3339))
			if it.Closed != nil {
				fmt.Fprintf(w, "  done      %s\n", it.Closed.Format(time.RFC3339))
			}
			if it.Due != nil {
				dueLine := it.Due.Format(time.RFC3339)
				if IsOverdue(it) {
					dueLine += " (OVERDUE)"
				}
				fmt.Fprintf(w, "  due       %s\n", dueLine)
			}
			if it.Recurring != "" {
				fmt.Fprintf(w, "  recurring %s\n", it.Recurring)
			}
			if it.Assignee != "" {
				fmt.Fprintf(w, "  assignee  %s\n", it.Assignee)
			}
			if it.Sprint != 0 {
				fmt.Fprintf(w, "  sprint    #%d\n", it.Sprint)
			}
			if it.Weave != 0 {
				if it.Status == StatusDone {
					fmt.Fprintf(w, "  weave     #%d\n", it.Weave)
				} else {
					fmt.Fprintf(w, "  weave     #%d (in flight)\n", it.Weave)
				}
			}
			if body := strings.TrimSpace(it.Body); body != "" {
				fmt.Fprintf(w, "\n%s\n", body)
			}
			if links {
				out, in := resolveLinks(st, it)
				printLinks(w, out, in)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "machine-readable output")
	cmd.Flags().BoolVar(&links, "links", false, "resolve the body's links at read time: outbound citations + inbound backlinks (kb pages and todos)")
	return cmd
}

// linkRef is one resolved (or dangling) link on the todo show --links surface.
type linkRef struct {
	Ref    string `json:"ref"`              // <kind>:<id> — any ref vocabulary kind (pkg/ref)
	Seq    int    `json:"seq,omitempty"`    // a kb target's ring-local seq (kb:<seq>), when resolved and minted
	Title  string `json:"title,omitempty"`  // the target's title, when resolved
	Type   string `json:"type,omitempty"`   // a kb target's page type (runbook, lesson, …); empty for todos
	Status string `json:"status,omitempty"` // "resolved" | "dangling" | "external" | "unknown"
}

// resolveLinks computes an item's outbound links and inbound backlinks through
// the SAME resolver kb uses (pkg/kb is the one home of the parser; todo calls it
// read-only). The graph is this repo's kb pages plus its todo/issue records.
func resolveLinks(st *issue.Store, it *issue.Issue) (outbound, inbound []linkRef) {
	var nodes []kb.LinkNode
	if pages, err := kb.Open(filepath.Join(st.Root, kb.RepoSub)).List(); err == nil {
		nodes = append(nodes, kb.KBNodes(pages)...)
	}
	if items, err := st.List(); err == nil {
		for _, x := range items {
			nodes = append(nodes, kb.TodoNode(x.ID, x.Title, x.Body))
		}
	}
	self := kb.TodoNode(it.ID, it.Title, it.Body)

	for _, l := range kb.ParseLinks(self.Body) {
		if n, ok := kb.ResolveLink(l, nodes); ok && n.Ref() != self.Ref() {
			outbound = append(outbound, linkRef{Ref: n.Ref(), Seq: n.Seq, Title: n.Title, Type: n.Type, Status: "resolved"})
		} else {
			// external (a kind another store owns), unknown (a scheme outside
			// the vocabulary), or dangling (a kb/todo target that is not here).
			outbound = append(outbound, linkRef{Ref: l.Ref(), Status: l.Status()})
		}
	}
	for _, n := range kb.Backlinks(self, nodes) {
		inbound = append(inbound, linkRef{Ref: n.Ref(), Title: n.Title, Type: n.Type, Status: "resolved"})
	}
	return outbound, inbound
}

func printLinks(w io.Writer, outbound, inbound []linkRef) {
	fmt.Fprintf(w, "\n  outbound (%d)\n", len(outbound))
	for _, l := range outbound {
		fmt.Fprintf(w, "    -> %s  %s  %s\n", l.Ref, dash(l.Title), l.Status)
	}
	fmt.Fprintf(w, "  inbound (%d)\n", len(inbound))
	for _, l := range inbound {
		fmt.Fprintf(w, "    <- %s  %s\n", l.Ref, dash(l.Title))
	}
}

func newStatusCmd(sf storeFunc) *cobra.Command {
	return &cobra.Command{
		Use:   "status <id|prefix> <todo|assigned|doing|blocked|done>",
		Short: "update a task's status",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, _, err := sf()
			if err != nil {
				return err
			}
			it, err := SetStatus(st, args[0], args[1])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s -> %s — %s\n", it.ID[:8], it.Status, it.Title)
			return nil
		},
	}
}

func newDoneCmd(sf storeFunc) *cobra.Command {
	return &cobra.Command{
		Use:   "done <id|prefix>",
		Short: "mark a task done (alias for status done)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, _, err := sf()
			if err != nil {
				return err
			}
			it, err := SetStatus(st, args[0], StatusDone)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "done %s — %s\n", it.ID[:8], it.Title)
			return nil
		},
	}
}

func newStartCmd(sf storeFunc) *cobra.Command {
	return &cobra.Command{
		Use:   "start <id|prefix>",
		Short: "mark a task in progress (alias for status doing)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, _, err := sf()
			if err != nil {
				return err
			}
			it, err := SetStatus(st, args[0], StatusDoing)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "doing %s — %s\n", it.ID[:8], it.Title)
			return nil
		},
	}
}

func newEditCmd(sf storeFunc) *cobra.Command {
	var title, priority, note string
	var dueStr, recurring, assignee string
	var sprint int64
	cmd := &cobra.Command{
		Use:   "edit <id|prefix>",
		Short: "modify a task's title/priority/note",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, _, err := sf()
			if err != nil {
				return err
			}
			it, err := ResolveRef(st, args[0])
			if err != nil {
				return err
			}
			if title != "" {
				it.Title = title
			}
			if cmd.Flags().Changed("priority") {
				it.Priority = priority
			}
			if cmd.Flags().Changed("note") {
				if it.Body, err = noteArg(cmd, note); err != nil {
					return err
				}
			}
			if cmd.Flags().Changed("due") {
				due, err := parseDue(dueStr)
				if err != nil {
					return err
				}
				it.Due = due
			}
			if cmd.Flags().Changed("recurring") {
				if err := ValidateCadence(recurring); err != nil {
					return err
				}
				it.Recurring = recurring
			}
			ownerChanged := cmd.Flags().Changed("owner")
			reassigned := ownerChanged && assignee != ""
			if ownerChanged {
				if it.Sprint != 0 && (it.Status == StatusDone || it.Closed != nil) {
					return fmt.Errorf("done sprint story %s cannot retroactively assign or reopen; acceptance provenance must be recorded by `bashy sprint accept %d %s`", it.ID, it.Sprint, it.ID)
				}
				canonical, err := canonicalAssignee(assignee)
				if err != nil {
					return err
				}
				it.Assignee = canonical
				if canonical != "" {
					it.Status = StatusAssigned
				} else if it.Status == StatusAssigned {
					it.Status = StatusTodo
				}
			}
			if cmd.Flags().Changed("sprint") {
				if sprint < 0 {
					return fmt.Errorf("--sprint must be zero (unlink) or a positive sprint number")
				}
				it.Sprint = sprint
			}
			if _, err := st.Save(it); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "edited %s — %s\n", it.ID[:8], it.Title)
			if reassigned {
				printAssignmentNotice(cmd, notifyAssignee("todo", it))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&title, "title", "", "new title")
	cmd.Flags().StringVar(&priority, "priority", "", "new priority (p0|p1|p2|p3)")
	cmd.Flags().StringVar(&note, "note", "", "replace the task body/details (- reads stdin) — the whole body, so re-supply what should stay; a reusable procedure belongs in a kb runbook, cited as [[kb:<slug>]]")
	cmd.Flags().StringVar(&dueStr, "due", "", "deadline (e.g. 2026-07-20, +3d)")
	cmd.Flags().StringVar(&recurring, "recurring", "", "cadence (default=driven by `sprint advance`; or daily, weekly, 24h, cron)")
	// ONE FLAG, DOMAIN TITLES: an item's --owner is its ASSIGNEE.
	role.AttachOwner(cmd.Flags(), &assignee, role.Assignee,
		"who is working the item (notified over bashy notify; see bashy inbox)")
	cmd.Flags().Int64Var(&sprint, "sprint", 0, "sprint number this story belongs to (0 unlinks)")
	return cmd
}

func newRmCmd(sf storeFunc) *cobra.Command {
	return &cobra.Command{
		Use:   "rm <id|prefix>",
		Short: "remove a task",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, _, err := sf()
			if err != nil {
				return err
			}
			it, err := Remove(st, args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed %s — %s\n", it.ID[:8], it.Title)
			return nil
		},
	}
}

// printAssignmentNotice reports whether an assignment actually reached the
// assignee, so `todo add`/`todo edit --owner` are never silent about it.
// A blank Assignee means the caller has nothing to report (no assignment
// made) and prints nothing.
func printAssignmentNotice(cmd *cobra.Command, notice AssignmentNotice) {
	if notice.Assignee == "" {
		return
	}
	if notice.Notified {
		fmt.Fprintf(cmd.OutOrStdout(), "  notified %s (bashy inbox --as %s)\n", notice.Assignee, notice.Assignee)
		return
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "  %s not notified: %s\n", notice.Assignee, notice.Reason)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func age(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func parseDue(s string) (*time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	if strings.HasPrefix(s, "+") {
		s = s[1:]
		if strings.HasSuffix(s, "d") {
			days, err := strconv.Atoi(s[:len(s)-1])
			if err != nil {
				return nil, fmt.Errorf("invalid relative days: %s", s)
			}
			t := time.Now().UTC().AddDate(0, 0, days)
			return &t, nil
		}
		if strings.HasSuffix(s, "h") {
			hours, err := strconv.Atoi(s[:len(s)-1])
			if err != nil {
				return nil, fmt.Errorf("invalid relative hours: %s", s)
			}
			t := time.Now().UTC().Add(time.Duration(hours) * time.Hour)
			return &t, nil
		}
		if d, err := time.ParseDuration(s); err == nil {
			t := time.Now().UTC().Add(d)
			return &t, nil
		}
		return nil, fmt.Errorf("invalid relative due: +%s", s)
	}
	if t, err := time.Parse("2006-01-02T15:04", s); err == nil {
		return &t, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return &t, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return &t, nil
	}
	return nil, fmt.Errorf("invalid due date format: %s", s)
}

// shortID renders an item's id for a table, and survives one that has none.
//
// `it.ID[:8]` panicked the whole command on the first record with an empty id,
// which is reachable from ordinary data: the loader skips malformed files with a
// warning and keeps going, so a partially-parsed item reaches the renderer with
// nothing in its id. Crashing `todo list` because ONE record is malformed loses
// every other item on the list — the failure is total where the fault was local.
//
// A missing id is shown rather than hidden. It means a record nothing can
// address — `todo show`, `done` and `rm` all take an id — so silently printing a
// blank column would leave someone wondering why the row will not respond.
func shortID(id string) string {
	switch {
	case id == "":
		return "(no-id)"
	case len(id) < 8:
		return id
	default:
		return id[:8]
	}
}
