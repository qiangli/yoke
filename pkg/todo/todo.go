// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

// Package todo is the STEWARD'S task list — level 1 of the tracking hierarchy:
//
//	issue  — per repo, COMMITTED backlog (what is wrong/wanted). pkg/issue.
//	sprint — cross-repo, what a conductor is planning now.
//	todo   — per HOST/USER, personal + non-committed. THIS package.
//
// It fills the one level nothing owned: the steward's own running list of what it
// is doing across all repos and all threads — the equivalent of a human's todo-list
// app. A conductor/fixer keeps its own list under its own owner; a human uses the
// default. Same tool, one vocabulary.
//
// It reuses the issue register's record format and store verbatim (YAML-frontmatter
// markdown, content-addressed ids, resolve-by-prefix) — see pkg/issue — but rooted
// at ~/.bashy/todo/<owner>/ (home, not a repo; not committed) and with its own
// status vocabulary. Not a new model or app: the same one at a different scope.
package todo

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/issue"
	"github.com/qiangli/yoke/pkg/scope"
)

// Statuses — a personal task's lifecycle. Unlike the committed issue register (which
// delegates "in progress"/"done" to weave so there is one source of truth), a
// steward/human todo OWNS its own execution, so doing/done are first-class here.
const (
	StatusTodo     = "todo"     // not started
	StatusAssigned = "assigned" // delegated to an agent — in that agent's hands (auto-set)
	StatusDoing    = "doing"    // in progress by ME (the steward/human works it directly)
	StatusBlocked  = "blocked"  // waiting on something else
	StatusDone     = "done"     // completed
)

var statuses = []string{StatusTodo, StatusAssigned, StatusDoing, StatusBlocked, StatusDone}

var todoAgentCatalog = func() *fleet.Catalog { return fleet.New() }

func canonicalAssignee(name string) (string, error) {
	n := strings.TrimSpace(name)
	if n == "" {
		return "", nil
	}
	// An assignee is a principal that can OWN the item — an agent or a person.
	// It used to be agents only, which refused every human by construction while
	// the message board accepted the same name, so an assigned human was both
	// addressable and unassignable. See fleet.CanonicalPrincipal.
	canonical, _, err := todoAgentCatalog().ResolvePrincipal(n)
	switch {
	case errors.Is(err, fleet.ErrPrincipalAmbiguous):
		// Ambiguous is not unknown: the name is real and answers for more than
		// one principal, so the fix is to qualify it, never to register it again.
		return "", fmt.Errorf("todo: assignee %q is ambiguous — more than one registered principal answers to it; qualify it", n)
	case err != nil:
		return "", fmt.Errorf("todo: assignee %q owns nothing here — %s", n, fleet.UnknownPrincipalHint(n))
	}
	return canonical, nil
}

// ValidStatus reports whether s is a known todo status.
func ValidStatus(s string) bool { return slices.Contains(statuses, s) }

// Statuses returns the status vocabulary.
func Statuses() []string { return append([]string(nil), statuses...) }

// DefaultOwner is the steward's list — the host's on-shift agent.
const DefaultOwner = "steward"

// Root is the host-scoped base directory (~/.bashy/todo). BASHY_TODO_DIR overrides
// it (tests, non-standard homes).
func Root() (string, error) {
	if d := strings.TrimSpace(os.Getenv("BASHY_TODO_DIR")); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".bashy", "todo"), nil
}

// SanitizeOwner reduces an owner to one safe path segment (no traversal),
// defaulting to the steward's list when empty.
func SanitizeOwner(owner string) string {
	if s := scope.SanitizeSegment(owner); s != "" {
		return s
	}
	return DefaultOwner
}

// RepoSub is the committed per-repo todo list's location: docs/todo/, inside the repo
// and CHECKED IN. It is the formal, structured replacement for an ad-hoc TODO.md — a
// human- and git-visible directory of one file per item that travels with the clone,
// shows up in diffs, and is the single per-repo tracker (`bashy todo --repo`). Visible
// on purpose (not hidden under .bashy/), because it replaces the file a human reads.
const RepoSub = "docs/todo"

// UserStore returns the host-scoped personal store (~/.bashy/todo/<owner>/).
func UserStore(owner string) (*issue.Store, error) {
	root, err := Root()
	if err != nil {
		return nil, err
	}
	return &issue.Store{Root: root, Sub: SanitizeOwner(owner)}, nil
}

// RepoStore returns the committed per-repo store (<repoRoot>/.bashy/todo/).
func RepoStore(repoRoot string) *issue.Store {
	return &issue.Store{Root: repoRoot, Sub: RepoSub}
}

// FindGitRoot walks up from the current directory for a `.git` entry.
func FindGitRoot() (string, bool) { return scope.FindGitRoot() }

// ResolveStore picks the store via the shared scope resolver (the same rule kb
// uses). The DEFAULT (no flag) auto-detects: THIS repo's docs/todo/ inside a git
// repo, else the personal host list. --base-dir travels to another repo, --user
// forces the personal list, --repo forces the repo list. It returns the store
// and a short human label for the scope.
func ResolveStore(owner string, forceRepo, forceUser bool, baseDir string) (*issue.Store, string, error) {
	if owner == "" {
		owner = DefaultOwner
	}
	sc, err := scope.Resolve(scope.Options{
		RepoSub:   RepoSub,
		Owner:     owner,
		HostDir:   Root,
		ForceRepo: forceRepo,
		ForceUser: forceUser,
		BaseDir:   baseDir,
	})
	if err != nil {
		return nil, "", err
	}
	return &issue.Store{Root: sc.Root, Sub: sc.Sub}, sc.Label(), nil
}

// Add files a new todo item (status: todo) into the given store. It files via Save
// directly rather than issue.Add, because the committed issue REGISTER deliberately
// births every issue "open" (triage is a separate act) — a todo, personal or checked
// in, has no triage step, so it is born ready in its own vocabulary.
func Add(st *issue.Store, title, body, priority string, due *time.Time, recurring, assignee string) (*issue.Issue, error) {
	if strings.TrimSpace(title) == "" {
		return nil, fmt.Errorf("a title is required")
	}
	if err := ValidateCadence(recurring); err != nil {
		return nil, err
	}
	next := 1
	if m, err := MaxSeq(st); err == nil {
		next = m + 1
	}
	assignee, err := canonicalAssignee(assignee)
	if err != nil {
		return nil, err
	}
	status := StatusTodo
	if assignee != "" {
		status = StatusAssigned
	}
	it := &issue.Issue{
		ID:        issue.NewID(),
		Kind:      issue.KindTask,
		Seq:       next,
		Title:     title,
		Body:      body,
		Priority:  priority,
		Due:       due,
		Recurring: recurring,
		Assignee:  assignee,
		Status:    status,
		Created:   time.Now().UTC(),
	}
	if _, err := st.Save(it); err != nil {
		return nil, err
	}
	return it, nil
}

// PriorityRank maps a priority tier to a sort rank where LOWER is more urgent:
// p0 < p1 < p2 < p3 < unset. Used by `todo list --by-priority` so the most
// urgent tasks surface first; ties fall through to the running number (Seq).
func PriorityRank(p string) int {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "p0":
		return 0
	case "p1":
		return 1
	case "p2":
		return 2
	case "p3":
		return 3
	default:
		return 4 // unset / unrecognized sorts after every explicit tier
	}
}

// MaxSeq is the highest running number assigned in the store (0 if none).
func MaxSeq(st *issue.Store) (int, error) {
	items, err := st.List()
	if err != nil {
		return 0, err
	}
	max := 0
	for _, it := range items {
		if it.Seq > max {
			max = it.Seq
		}
	}
	return max, nil
}

// EnsureSeq backfills a stable running number onto any items that predate the Seq
// field, in creation order, so a store always shows consistent numbers. Idempotent —
// a no-op once every item has one.
func EnsureSeq(st *issue.Store) error {
	items, err := st.List()
	if err != nil {
		return err
	}
	max := 0
	var missing []*issue.Issue
	for _, it := range items {
		if it.Seq > max {
			max = it.Seq
		}
		if it.Seq == 0 {
			missing = append(missing, it)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i].Created.Before(missing[j].Created) })
	for _, it := range missing {
		max++
		it.Seq = max
		if _, err := st.Save(it); err != nil {
			return err
		}
	}
	return nil
}

// ResolveRef resolves a reference that may be a running NUMBER (Seq, e.g. "3") or a
// content-id PREFIX (git-style, e.g. "a1148df4"). A bare positive integer is tried as
// a number first — the short handle a human reads off `todo list`.
//
// The two namespaces overlap: a hex content-id prefix can be all decimal digits (e.g.
// "580789"), so it parses as an integer yet is really an id. When no task carries the
// requested Seq, fall back to resolving the ref as an id prefix before giving up —
// otherwise an all-digit id prefix is misrouted to a Seq lookup that can never match.
func ResolveRef(st *issue.Store, ref string) (*issue.Issue, error) {
	ref = strings.TrimSpace(ref)
	if n, err := strconv.Atoi(ref); err == nil && n > 0 {
		items, err := st.List()
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			if it.Seq == n {
				return it, nil
			}
		}
		if it, err := st.Resolve(ref); err == nil {
			return it, nil
		}
		return nil, fmt.Errorf("no task with number %d (run `bashy todo list`)", n)
	}
	return st.Resolve(ref)
}

// SetStatus moves an item to a new status (done stamps Closed).
func SetStatus(st *issue.Store, ref, status string) (*issue.Issue, error) {
	if !ValidStatus(status) {
		return nil, fmt.Errorf("unknown status %q (want one of: %s)", status, strings.Join(statuses, ", "))
	}
	it, err := ResolveRef(st, ref)
	if err != nil {
		return nil, err
	}
	if status == StatusDone && it.Sprint != 0 {
		return nil, fmt.Errorf("sprint story %s cannot close through generic todo; submit delivery evidence, then the manager runs `bashy sprint accept %d %s -m \"<verified evidence>\"`", it.ID, it.Sprint, it.ID)
	}
	if status == StatusAssigned {
		assignee, err := canonicalAssignee(it.Assignee)
		if err != nil {
			return nil, err
		}
		if assignee == "" {
			return nil, fmt.Errorf("todo: assigned status requires an owner (assignee) — pass --owner NAME from `bashy agent list`")
		}
		it.Assignee = assignee
	}

	// A recurring item reopens on completion INSTEAD of closing — but only
	// when its cadence is its own. An item bound to a sprint takes its cadence
	// from that sprint's cycle (`bashy sprint advance`), and reopening here
	// would erase the very completion the cycle record has to attest: this
	// branch never stamps Closed/ClosedBy, so the tenth iteration is
	// indistinguishable from the first. Reaching a success state through the
	// absence of evidence is the one thing the fleet-evidence invariant
	// forbids, so a sprint story closes honestly and `advance` reopens it.
	if status == StatusDone && it.Recurring != "" && it.Sprint == 0 {
		it.Status = StatusTodo
		base := time.Now().UTC()
		if it.Due != nil {
			base = *it.Due
		}
		if next, err := advanceCadence(base, it.Recurring); err == nil {
			it.Due = &next
		}
	} else {
		it.Status = status
		if status == StatusDone {
			now := time.Now().UTC()
			it.Closed = &now
		} else {
			it.Closed = nil
		}
	}
	if _, err := st.Save(it); err != nil {
		return nil, err
	}
	return it, nil
}

// Remove drops an item outright.
func Remove(st *issue.Store, ref string) (*issue.Issue, error) {
	it, err := ResolveRef(st, ref)
	if err != nil {
		return nil, err
	}
	return it, st.Remove(it)
}

// IsOverdue reports whether an item's due date is in the past. Done items are
// never overdue, and a nil due date is never overdue.
func IsOverdue(it *issue.Issue) bool {
	if it.Status == StatusDone || it.Due == nil {
		return false
	}
	return it.Due.Before(time.Now().UTC())
}

// List returns a store's items sorted by PRIORITY then sequence: most urgent
// first (p0 < p1 < p2 < p3 < unset), and within one priority by running number
// ascending (#1, #2, … — oldest first / creation order). Optionally filtered by
// status. The CLI's --reverse flips the whole order.
func List(st *issue.Store, status string) ([]*issue.Issue, error) {
	all, err := st.List()
	if err != nil {
		return nil, err
	}
	out := all
	if status != "" {
		out = out[:0:0]
		for _, it := range all {
			if it.Status == status {
				out = append(out, it)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if ri, rj := PriorityRank(out[i].Priority), PriorityRank(out[j].Priority); ri != rj {
			return ri < rj
		}
		return out[i].Seq < out[j].Seq
	})
	return out, nil
}

// SprintHandles answers the uuid and title of the sprint card numbered seq on
// THIS host's board, or ok=false when no board is linked in or the card does
// not exist. A package-variable seam, not an import: todo does not depend on
// sprint (see issue.Issue.Sprint), so a plain `todo` binary leaves it nil and
// files only the seq; the sprint-aware wiring sets it, and `todo add/edit
// --sprint N` then also writes sprint_id + sprint_title so the story carries
// its sprint to any host that checks the repo out.
var SprintHandles func(seq int64) (uuid, title string, ok bool)

// LinkSprint sets the story's sprint fields from seq: all three when the seam
// answers, the seq alone when it does not, and none when seq is 0 (unlink).
// The seq is a label scoped to the filer's host; the uuid is the identity.
func LinkSprint(it *issue.Issue, seq int64) {
	it.Sprint, it.SprintID, it.SprintTitle = seq, "", ""
	if seq == 0 || SprintHandles == nil {
		return
	}
	if uuid, title, ok := SprintHandles(seq); ok {
		it.SprintID, it.SprintTitle = uuid, title
	}
}

// CadenceSprint is the cadence of an item whose repetition is driven by a
// SPRINT CYCLE rather than by the clock: `bashy sprint advance` resets it, so
// there is deliberately no due date to advance.
const CadenceSprint = "default"

// ValidateCadence rejects a cadence nothing can interpret.
//
// It exists because an unparseable cadence used to be indistinguishable from
// an on-demand one: Add never checked the string, and SetStatus discarded
// advanceCadence's error, so `--recurring wekly` silently became "repeats, but
// never becomes due". A typo that quietly half-works is worse than one that
// fails, because nothing ever reports it.
func ValidateCadence(cadence string) error {
	c := strings.ToLower(strings.TrimSpace(cadence))
	if c == "" || c == CadenceSprint {
		return nil
	}
	if _, err := advanceCadence(time.Now().UTC(), c); err != nil {
		return fmt.Errorf("%w (want %q for a sprint-driven item, or daily|weekly|monthly, a duration like 24h, or a 5-field cron expression)", err, CadenceSprint)
	}
	return nil
}

func advanceCadence(from time.Time, cadence string) (time.Time, error) {
	c := strings.ToLower(strings.TrimSpace(cadence))
	switch c {
	case "daily":
		return from.AddDate(0, 0, 1), nil
	case "weekly":
		return from.AddDate(0, 0, 7), nil
	case "monthly":
		return from.AddDate(0, 1, 0), nil
	}
	if d, err := time.ParseDuration(cadence); err == nil {
		return from.Add(d), nil
	}
	sched, err := cron.ParseStandard(cadence)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid recurring cadence %q: %w", cadence, err)
	}
	return sched.Next(from), nil
}
