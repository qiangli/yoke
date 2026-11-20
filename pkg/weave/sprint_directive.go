package weave

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/coreutils/pkg/weavecli"
	"github.com/qiangli/yoke/pkg/issue"
	todopkg "github.com/qiangli/yoke/pkg/todo"
)

const sprintDirectivesBlockHeaderPrefix = "SPRINT STANDING DIRECTIVES ("

var inlineSprintTrailerRe = regexp.MustCompile(`(?i)\bTrailers:[^\n]*\bSprint:\s*#?(\d+)\b`)

// SprintDirective is a standing operator rule or pre-approval stored on a
// sprint card. Active directives live in weaveStory.Directives and every add or
// removal is audited onto weaveStory.Thread; no separate store is used.
type SprintDirective struct {
	ID         string    `json:"id"`
	Text       string    `json:"text"`
	By         string    `json:"by"`
	Preapprove []string  `json:"preapprove,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// MatchesPreapprove reports whether d pre-approves action (for example "defer"
// or "drop") on target (for example "p2").
func (d SprintDirective) MatchesPreapprove(action, target string) bool {
	action = strings.ToLower(strings.TrimSpace(action))
	target = strings.ToLower(strings.TrimSpace(target))
	if action == "" {
		return false
	}
	for _, raw := range d.Preapprove {
		rule := strings.ToLower(strings.TrimSpace(raw))
		if rule == "" {
			continue
		}
		if rule == "*" || rule == "all" {
			return true
		}
		act, scope, hasScope := strings.Cut(rule, ":")
		act = strings.TrimSpace(act)
		if act != action {
			continue
		}
		if !hasScope {
			return true
		}
		scope = strings.TrimSpace(scope)
		if scope == "*" || scope == "all" {
			return true
		}
		if target == "" {
			continue
		}
		for _, item := range strings.Split(scope, ",") {
			item = strings.TrimSpace(item)
			if item != "" && (item == "*" || item == "all" || item == target) {
				return true
			}
		}
	}
	return false
}

// SprintDirectives returns a copy of the active directives recorded on sprintID.
func SprintDirectives(sprintID int64) ([]SprintDirective, error) {
	dir, err := sprintStoreDir()
	if err != nil {
		return nil, err
	}
	q, err := loadWeaveQueue(dir)
	if err != nil {
		return nil, err
	}
	s := findWeaveStory(q, sprintID)
	if s == nil {
		return nil, fmt.Errorf("sprint #%d not found", sprintID)
	}
	return activeSprintDirectives(s), nil
}

// SprintPreapproves reports the first active directive on sprintID that
// pre-approves action for target.
func SprintPreapproves(sprintID int64, action, target string) (SprintDirective, bool) {
	directives, err := SprintDirectives(sprintID)
	if err != nil {
		return SprintDirective{}, false
	}
	for _, d := range directives {
		if d.MatchesPreapprove(action, target) {
			return d, true
		}
	}
	return SprintDirective{}, false
}

func activeSprintDirectives(s *weaveStory) []SprintDirective {
	if s == nil || len(s.Directives) == 0 {
		return []SprintDirective{}
	}
	out := make([]SprintDirective, len(s.Directives))
	for i, d := range s.Directives {
		d.Preapprove = append([]string(nil), d.Preapprove...)
		out[i] = d
	}
	return out
}

func nextSprintDirectiveID(s *weaveStory) string {
	for _, d := range s.Directives {
		if n, err := strconv.ParseInt(d.ID, 10, 64); err == nil && n > s.NextDirectiveID {
			s.NextDirectiveID = n
		}
	}
	s.NextDirectiveID++
	for findSprintDirectiveIndex(s, strconv.FormatInt(s.NextDirectiveID, 10)) >= 0 {
		s.NextDirectiveID++
	}
	return strconv.FormatInt(s.NextDirectiveID, 10)
}

func findSprintDirectiveIndex(s *weaveStory, ref string) int {
	if s == nil {
		return -1
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return -1
	}
	trimmedHash := strings.TrimPrefix(ref, "#")
	trimmedD := ""
	if len(trimmedHash) > 1 && (trimmedHash[0] == 'd' || trimmedHash[0] == 'D') {
		if _, err := strconv.ParseInt(trimmedHash[1:], 10, 64); err == nil {
			trimmedD = trimmedHash[1:]
		}
	}
	for i, d := range s.Directives {
		if d.ID == ref || d.ID == trimmedHash || (trimmedD != "" && d.ID == trimmedD) {
			return i
		}
	}
	return -1
}

func sprintDirectiveDisplayID(id string) string {
	if _, err := strconv.ParseInt(id, 10, 64); err == nil {
		return "#" + id
	}
	return id
}

func normalizeSprintPreapprovals(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	seen := map[string]bool{}
	var out []string
	for _, entry := range raw {
		if strings.ContainsAny(entry, "\r\n") {
			return nil, sprintInvalidArg{fmt.Errorf("--preapprove must not contain newlines")}
		}
		entry = strings.TrimSpace(entry)
		if entry == "" {
			return nil, sprintInvalidArg{fmt.Errorf("--preapprove value must be non-empty (for example defer:p2,p3)")}
		}
		if !seen[entry] {
			seen[entry] = true
			out = append(out, entry)
		}
	}
	return out, nil
}

func resolveSprintDirectiveActor(cmd *cobra.Command, byFlag string) (string, error) {
	if cmd != nil && cmd.Flags().Changed("by") {
		by := strings.TrimSpace(byFlag)
		if by == "" || strings.ContainsAny(by, "\r\n") {
			return "", sprintInvalidArg{fmt.Errorf("--by must be a non-empty single-line actor name")}
		}
		return by, nil
	}
	if by := strings.TrimSpace(byFlag); by != "" {
		if strings.ContainsAny(by, "\r\n") {
			return "", sprintInvalidArg{fmt.Errorf("--by must be a non-empty single-line actor name")}
		}
		return by, nil
	}
	if who, ok := weaveConductorIdentity(""); ok {
		return who, nil
	}
	if u := strings.TrimSpace(principalLocalUser()); u != "" && u != "unknown" {
		return u, nil
	}
	return "operator", nil
}

func sprintDirectiveAddedNote(d SprintDirective) string {
	var b strings.Builder
	fmt.Fprintf(&b, "added directive %s (by %s", sprintDirectiveDisplayID(d.ID), d.By)
	if len(d.Preapprove) > 0 {
		fmt.Fprintf(&b, "; preapprove: %s", strings.Join(d.Preapprove, "; "))
	}
	fmt.Fprintf(&b, "): %s", d.Text)
	return b.String()
}

func sprintDirectiveRemovedNote(d SprintDirective, reason string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "removed directive %s", sprintDirectiveDisplayID(d.ID))
	if r := strings.TrimSpace(reason); r != "" {
		fmt.Fprintf(&b, " — %s", r)
	}
	fmt.Fprintf(&b, "\n  was (by %s", d.By)
	if len(d.Preapprove) > 0 {
		fmt.Fprintf(&b, "; preapprove: %s", strings.Join(d.Preapprove, "; "))
	}
	fmt.Fprintf(&b, "): %s", d.Text)
	return b.String()
}

func formatSprintDirectiveBullet(d SprintDirective) string {
	meta := sprintDirectiveDisplayID(d.ID)
	if by := strings.TrimSpace(d.By); by != "" {
		meta += " by " + by
	}
	if len(d.Preapprove) > 0 {
		meta += "; preapprove: " + strings.Join(d.Preapprove, "; ")
	}
	text := strings.ReplaceAll(strings.TrimSpace(d.Text), "\n", "\n    ")
	return fmt.Sprintf("[%s] %s", meta, text)
}

func formatSprintDirectivesSection(sprintID int64, directives []SprintDirective) string {
	if len(directives) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%ssprint #%d — active operator rules):", sprintDirectivesBlockHeaderPrefix, sprintID)
	for _, d := range directives {
		b.WriteString("\n- ")
		b.WriteString(formatSprintDirectiveBullet(d))
	}
	return b.String()
}

func stripSprintDirectivesSection(s string) string {
	if !strings.Contains(s, sprintDirectivesBlockHeaderPrefix) {
		return s
	}
	idx := strings.Index(s, sprintDirectivesBlockHeaderPrefix)
	if idx < 0 {
		return s
	}
	start := idx
	for start > 0 && s[start-1] == '\n' {
		start--
	}
	rest := s[idx:]
	lines := strings.Split(rest, "\n")
	consumed := 0
	for i, line := range lines {
		if i == 0 {
			consumed += len(line)
			continue
		}
		if strings.HasPrefix(line, "- [") || strings.HasPrefix(line, "    ") {
			consumed += 1 + len(line)
			continue
		}
		break
	}
	after := rest[consumed:]
	if start == 0 {
		return strings.TrimLeft(after, "\n")
	}
	return s[:start] + after
}

// applySprintDirectives idempotently replaces or appends the sprint's active
// directives block on base. When directives is empty and base carries no prior
// directives block, base is returned unchanged byte-for-byte.
func applySprintDirectives(base string, sprintID int64, directives []SprintDirective) string {
	if len(directives) == 0 && !strings.Contains(base, sprintDirectivesBlockHeaderPrefix) {
		return base
	}
	clean := stripSprintDirectivesSection(base)
	if len(directives) == 0 {
		return clean
	}
	block := formatSprintDirectivesSection(sprintID, directives)
	if strings.TrimSpace(clean) == "" {
		return block
	}
	if strings.HasSuffix(clean, "\n\n") {
		return clean + block
	}
	if strings.HasSuffix(clean, "\n") {
		return clean + "\n" + block
	}
	return clean + "\n\n" + block
}

func applySprintDirectivesForID(base string, sprintID int64) string {
	if sprintID <= 0 {
		return base
	}
	dir, err := sprintStoreDir()
	if err != nil {
		return base
	}
	q, err := loadWeaveQueue(dir)
	if err != nil {
		return base
	}
	s := findWeaveStory(q, sprintID)
	if s == nil {
		return base
	}
	return applySprintDirectives(base, s.ID, s.Directives)
}

func findSprintByIssueLink(board *weaveQueue, reg *issue.Issue) *weaveStory {
	if board == nil || reg == nil {
		return nil
	}
	if reg.SprintID != "" {
		for _, s := range board.Stories {
			if s != nil && s.UUID == reg.SprintID {
				return s
			}
		}
	}
	if reg.Sprint > 0 {
		return findWeaveStory(board, reg.Sprint)
	}
	return nil
}

func findSprintByBodyTrace(board *weaveQueue, body string) *weaveStory {
	if board == nil || strings.TrimSpace(body) == "" {
		return nil
	}
	if trace, err := weaveBriefTrace(body); err == nil {
		if trace.SprintID != "" {
			for _, s := range board.Stories {
				if s != nil && s.UUID == trace.SprintID {
					return s
				}
			}
		}
		if trace.Sprint > 0 {
			if s := findWeaveStory(board, trace.Sprint); s != nil {
				return s
			}
		}
	}
	if m := inlineSprintTrailerRe.FindStringSubmatch(body); len(m) == 2 {
		if id, err := strconv.ParseInt(m[1], 10, 64); err == nil && id > 0 {
			return findWeaveStory(board, id)
		}
	}
	return nil
}

func applySprintDirectivesForWeaveAdd(root, body string, reg *issue.Issue) string {
	dir, err := sprintStoreDir()
	if err != nil {
		return body
	}
	board, err := loadWeaveQueue(dir)
	if err != nil || len(board.Stories) == 0 {
		return body
	}
	s := findSprintByIssueLink(board, reg)
	if s == nil {
		s = findSprintByBodyTrace(board, body)
	}
	if s == nil {
		return body
	}
	return applySprintDirectives(body, s.ID, s.Directives)
}

func sprintForWeaveRun(root, queueDir string, it *weaveItem) *weaveStory {
	if it == nil {
		return nil
	}
	dir, err := sprintStoreDir()
	if err != nil {
		return nil
	}
	board, err := loadWeaveQueue(dir)
	if err != nil || len(board.Stories) == 0 {
		return nil
	}
	queueTag := ""
	if queueDir != "" {
		queueTag = filepath.Base(filepath.Clean(queueDir))
	}
	repoName := ""
	if root != "" {
		repoName = filepath.Base(filepath.Clean(root))
	}
	var linkedFallback *weaveStory
	for _, s := range board.Stories {
		if s == nil {
			continue
		}
		for _, r := range s.Runs {
			if r.ID != it.ID {
				continue
			}
			if !r.Born.IsZero() && !it.Created.IsZero() && !r.Born.Equal(it.Created) {
				continue
			}
			queueMatches := (r.Queue != "" && queueTag != "" && r.Queue == queueTag) ||
				(r.Queue == "" && repoName != "" && r.Repo == repoName)
			if !queueMatches {
				continue
			}
			if s.Column != "done" {
				return s
			}
			if linkedFallback == nil {
				linkedFallback = s
			}
		}
	}
	if linkedFallback != nil {
		return linkedFallback
	}
	if s := findSprintByBodyTrace(board, it.Body); s != nil {
		return s
	}
	if strings.TrimSpace(it.Register) != "" {
		if root != "" {
			if _, reg, err := findRegisterItem(root, it.Register); err == nil && reg != nil {
				if s := findSprintByIssueLink(board, reg); s != nil {
					return s
				}
			}
		}
		for _, s := range board.Stories {
			if s == nil {
				continue
			}
			for _, storyRoot := range sprintDeclaredStoryRoots(s) {
				story, err := todopkg.ResolveRef(todopkg.RepoStore(storyRoot), it.Register)
				if err == nil && story != nil && storyBelongsToSprint(story, s) {
					return s
				}
			}
		}
	}
	if it.ArenaSprint > 0 {
		return findWeaveStory(board, it.ArenaSprint)
	}
	return nil
}

func renderSprintShowDirectives(w io.Writer, s *weaveStory) {
	if s == nil || len(s.Directives) == 0 {
		return
	}
	fmt.Fprintln(w, "  ── directives (standing operator rules) ──")
	for _, d := range s.Directives {
		fmt.Fprintf(w, "  %s\n", formatSprintDirectiveBullet(d))
	}
}

func renderSprintTickDirectives(w io.Writer, directives []SprintDirective) {
	if len(directives) == 0 {
		return
	}
	fmt.Fprintln(w, "  ── DIRECTIVES: active standing operator rules ──")
	for _, d := range directives {
		fmt.Fprintf(w, "    %s\n", formatSprintDirectiveBullet(d))
	}
}

func newSprintDirectiveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "directive",
		Short: "Manage standing operator rules and pre-approvals on a sprint card",
		Long: "directive records standing operator rules and pre-approvals directly on a sprint card.\n\n" +
			"Active directives are shown by `sprint show` and `sprint tick`, appended to every\n" +
			"manager payload (`sprint take`, `sprint start`, `sprint instruct`), and appended\n" +
			"to every weave brief started for the sprint. Additions and removals are audited\n" +
			"on the sprint thread with no separate store.",
	}
	cmd.AddCommand(
		newSprintDirectiveAddCmd(),
		newSprintDirectiveListCmd(),
		newSprintDirectiveRmCmd(),
	)
	return cmd
}

func newSprintDirectiveAddCmd() *cobra.Command {
	var flags weaveOutputFlags
	var by, customID string
	var preapprove []string
	cmd := &cobra.Command{
		Use:     "add <sprint> <text>",
		Short:   "Record a standing operator directive (and optional --preapprove rule) on a sprint card",
		Example: "  bashy sprint directive add 576 \"ROOT CAUSE FIRST; never push\" --by operator --preapprove defer:p2,p3",
		Args:    cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			const op = "sprint directive add"
			mode := flags.mode()
			id, err := sprintArg(cmd, mode, op, args[0])
			if err != nil {
				return err
			}
			text := strings.TrimSpace(strings.Join(args[1:], " "))
			if text == "" {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, op, weavecli.ExitInvalidArg,
					fmt.Errorf("directive text is required")))
			}
			actor, err := resolveSprintDirectiveActor(cmd, by)
			if err != nil {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, op, weavecli.ExitInvalidArg, err))
			}
			normPre, err := normalizeSprintPreapprovals(preapprove)
			if err != nil {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, op, weavecli.ExitInvalidArg, err))
			}
			wantID := strings.TrimSpace(customID)
			if cmd.Flags().Changed("id") && (wantID == "" || strings.ContainsAny(wantID, " \t\r\n")) {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, op, weavecli.ExitInvalidArg,
					fmt.Errorf("--id must be non-empty with no whitespace")))
			}
			dir, err := weaveStoryDir(cmd, mode, op)
			if err != nil {
				return err
			}
			var added SprintDirective
			var all []SprintDirective
			lockErr := withWeaveQueueLock(dir, func(q *weaveQueue) error {
				s := findWeaveStory(q, id)
				if s == nil {
					return fmt.Errorf("sprint #%d not found", id)
				}
				dirID := wantID
				if dirID != "" {
					if findSprintDirectiveIndex(s, dirID) >= 0 {
						return sprintInvalidArg{fmt.Errorf("directive %q already exists on sprint #%d", dirID, id)}
					}
				} else {
					dirID = nextSprintDirectiveID(s)
				}
				added = SprintDirective{
					ID:         dirID,
					Text:       text,
					By:         actor,
					Preapprove: normPre,
					CreatedAt:  time.Now().UTC(),
				}
				s.Directives = append(s.Directives, added)
				weaveStoryAppend(s, actor, kindDecision, sprintDirectiveAddedNote(added))
				all = activeSprintDirectives(s)
				return nil
			})
			if lockErr != nil {
				code := weavecli.ExitGenericFail
				var invalid sprintInvalidArg
				if strings.Contains(lockErr.Error(), "not found") || errors.As(lockErr, &invalid) {
					code = weavecli.ExitInvalidArg
				}
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, op, code, lockErr))
			}
			if mode == weavecli.OutputJSON {
				return ec(emitOK(cmd.OutOrStdout(), mode, op, map[string]any{
					"sprint":     id,
					"ok":         true,
					"directive":  added,
					"directives": all,
				}))
			}
			pre := ""
			if len(added.Preapprove) > 0 {
				pre = "; preapprove: " + strings.Join(added.Preapprove, "; ")
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: sprint #%d +%s (by %s%s): %s\n",
				op, id, sprintDirectiveDisplayID(added.ID), added.By, pre, added.Text)
			return nil
		},
	}
	cmd.Flags().StringVar(&by, "by", "", "operator recording this directive")
	cmd.Flags().StringArrayVar(&preapprove, "preapprove", nil, "standing pre-approval rule, e.g. defer:p2,p3 (repeatable)")
	cmd.Flags().StringVar(&customID, "id", "", "optional stable directive id (default: next number on the card)")
	flags.attach(cmd)
	return cmd
}

func newSprintDirectiveListCmd() *cobra.Command {
	var flags weaveOutputFlags
	cmd := &cobra.Command{
		Use:     "list <sprint>",
		Aliases: []string{"ls"},
		Short:   "List active standing directives on a sprint card",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			const op = "sprint directive list"
			mode := flags.mode()
			id, err := sprintArg(cmd, mode, op, args[0])
			if err != nil {
				return err
			}
			dir, err := weaveStoryDir(cmd, mode, op)
			if err != nil {
				return err
			}
			q, err := loadWeaveQueue(dir)
			if err != nil {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, op, weavecli.ExitGenericFail, err))
			}
			s := findWeaveStory(q, id)
			if s == nil {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, op, weavecli.ExitInvalidArg,
					fmt.Errorf("sprint #%d not found", id)))
			}
			directives := activeSprintDirectives(s)
			if mode == weavecli.OutputJSON {
				return ec(emitOK(cmd.OutOrStdout(), mode, op, map[string]any{
					"sprint":     id,
					"directives": directives,
				}))
			}
			out := cmd.OutOrStdout()
			if len(directives) == 0 {
				fmt.Fprintf(out, "sprint #%d: no active directives\n", id)
				return nil
			}
			fmt.Fprintf(out, "sprint #%d directives (%d active):\n", id, len(directives))
			for _, d := range directives {
				fmt.Fprintf(out, "  %s\n", formatSprintDirectiveBullet(d))
			}
			return nil
		},
	}
	flags.attach(cmd)
	return cmd
}

func newSprintDirectiveRmCmd() *cobra.Command {
	var flags weaveOutputFlags
	var by, reason string
	cmd := &cobra.Command{
		Use:     "rm <sprint> <directive>",
		Aliases: []string{"remove", "delete"},
		Short:   "Remove an active standing directive from a sprint card (preserving thread history)",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			const op = "sprint directive rm"
			mode := flags.mode()
			id, err := sprintArg(cmd, mode, op, args[0])
			if err != nil {
				return err
			}
			ref := strings.TrimSpace(args[1])
			if ref == "" {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, op, weavecli.ExitInvalidArg,
					fmt.Errorf("directive id is required")))
			}
			actor, err := resolveSprintDirectiveActor(cmd, by)
			if err != nil {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, op, weavecli.ExitInvalidArg, err))
			}
			dir, err := weaveStoryDir(cmd, mode, op)
			if err != nil {
				return err
			}
			var removed SprintDirective
			var remaining []SprintDirective
			lockErr := withWeaveQueueLock(dir, func(q *weaveQueue) error {
				s := findWeaveStory(q, id)
				if s == nil {
					return fmt.Errorf("sprint #%d not found", id)
				}
				idx := findSprintDirectiveIndex(s, ref)
				if idx < 0 {
					return sprintInvalidArg{fmt.Errorf("directive %q not found on sprint #%d", ref, id)}
				}
				removed = s.Directives[idx]
				s.Directives = append(s.Directives[:idx], s.Directives[idx+1:]...)
				weaveStoryAppend(s, actor, kindDecision, sprintDirectiveRemovedNote(removed, reason))
				remaining = activeSprintDirectives(s)
				return nil
			})
			if lockErr != nil {
				code := weavecli.ExitGenericFail
				var invalid sprintInvalidArg
				if strings.Contains(lockErr.Error(), "not found") || errors.As(lockErr, &invalid) {
					code = weavecli.ExitInvalidArg
				}
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, op, code, lockErr))
			}
			if mode == weavecli.OutputJSON {
				return ec(emitOK(cmd.OutOrStdout(), mode, op, map[string]any{
					"sprint":     id,
					"ok":         true,
					"removed":    removed,
					"directives": remaining,
				}))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: sprint #%d removed directive %s (%d active)\n",
				op, id, sprintDirectiveDisplayID(removed.ID), len(remaining))
			return nil
		},
	}
	cmd.Flags().StringVar(&by, "by", "", "operator removing this directive")
	cmd.Flags().StringVar(&reason, "reason", "", "optional reason recorded on the sprint thread")
	cmd.Flags().StringVarP(&reason, "message", "m", "", "alias for --reason")
	flags.attach(cmd)
	return cmd
}
