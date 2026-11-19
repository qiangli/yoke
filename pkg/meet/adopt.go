package meet

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/issue"
	"github.com/qiangli/yoke/pkg/todo"
)

// Action adoption: turn an agreed Meet action into a tracked sprint story.
//
// There is no second store. The story is an ordinary todo item (the same
// docs/todo register `bashy todo` and `bashy sprint` use); the link back to the
// room rides in its extension frontmatter (meet_room / meet_action), which the
// issue package round-trips untouched. Four rules shape everything here:
//
//   - Adoption is explicit. `adopt` previews by default; --confirm writes. A
//     secretary-proposed action is listed but is never adopted by default — the
//     operator must name it. Adoption files a story in status todo and launches
//     nothing: naming an owner records who, it does not dispatch them.
//   - Adoption is idempotent. The action ID is a hash of the room and the
//     normalized action text, so the same agreed action maps to the same story
//     however often it is adopted.
//   - A changed decision is surfaced, not silently followed. --reconcile finds
//     adopted stories whose action no longer stands in the room and asks for
//     reconciliation on the story and in the room.
//   - A verified outcome returns to the room. `outcome` posts the story result
//     and its evidence links back to the originating transcript, once.

const (
	extRoom       = "meet_room"
	extAction     = "meet_action"
	extSource     = "meet_source"
	extAdoptedBy  = "meet_adopted_by"
	extDepends    = "meet_depends_on"
	extAccept     = "meet_acceptance"
	extReconcile  = "meet_reconcile"
	extEvidence   = "meet_evidence"
	extReportedAt = "meet_reported_at"

	labelAdopted   = "meet-action"
	labelReconcile = "needs-reconcile"

	sourceAgreed   = "agreed"   // a participant recorded it with /action
	sourceProposed = "proposed" // only the secretary summary carries it
)

// agreedAction is one candidate for adoption.
type agreedAction struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	Owner  string `json:"owner,omitempty"`
	Task   string `json:"task"`
	Source string `json:"source"`
}

// actionID is the stable identity of an action within a room.
func actionID(room, text string) string {
	norm := strings.ToLower(strings.Join(strings.Fields(text), " "))
	sum := sha256.Sum256([]byte(room + "\x00" + norm))
	return "act-" + hex.EncodeToString(sum[:])[:10]
}

// splitOwner reads the "owner: task" convention the /action command documents.
// Only a single-token prefix counts as an owner, so prose with a colon is not
// misread.
func splitOwner(text string) (owner, task string) {
	if head, rest, ok := strings.Cut(text, ":"); ok {
		head = strings.TrimSpace(head)
		if head != "" && !strings.ContainsAny(head, " \t") && strings.TrimSpace(rest) != "" {
			return head, strings.TrimSpace(rest)
		}
	}
	return "", strings.TrimSpace(text)
}

// agreedActions lists actions in order, tagging whether a participant recorded
// them or only the secretary proposed them.
func agreedActions(room string, events []Event, syn *Synthesis) []agreedAction {
	var out []agreedAction
	seen := map[string]bool{}
	add := func(text, source string) {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		id := actionID(room, text)
		if seen[id] {
			return
		}
		seen[id] = true
		owner, task := splitOwner(text)
		out = append(out, agreedAction{ID: id, Text: text, Owner: owner, Task: task, Source: source})
	}
	// A retraction withdraws an action: it must not stay a candidate, and a
	// secretary summary that still lists the same text must not revive it.
	retracted := map[string]bool{}
	for _, e := range events {
		if e.Kind == "retraction" && e.Retracts != "" {
			retracted[e.Retracts] = true
		}
	}
	withdrawn := map[string]bool{}
	for _, e := range events {
		if e.Kind == "action" && retracted[e.TS.Format(time.RFC3339Nano)] {
			withdrawn[actionID(room, e.Text)] = true
		}
	}
	for _, e := range events {
		if e.Kind == "action" && !retracted[e.TS.Format(time.RFC3339Nano)] {
			add(e.Text, sourceAgreed)
		}
	}
	if syn != nil {
		for _, a := range syn.Actions {
			if !withdrawn[actionID(room, a)] {
				add(a, sourceProposed)
			}
		}
	}
	return out
}

// adoptSpec is the operator's explicit terms for one action.
type adoptSpec struct {
	Owner   string
	Depends []string // action IDs or story refs
	Accept  []string // acceptance criteria
}

// adoptResult reports one action's outcome.
type adoptResult struct {
	Action  agreedAction `json:"action"`
	Story   string       `json:"story,omitempty"`
	Created bool         `json:"created"`
	Note    string       `json:"note,omitempty"`
}

func extString(it *issue.Issue, key string) string {
	if v, ok := it.Extra[key].(string); ok {
		return v
	}
	return ""
}

func extStrings(it *issue.Issue, key string) []string {
	switch v := it.Extra[key].(type) {
	case []string:
		return v
	case []any:
		var out []string
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func setExt(it *issue.Issue, key string, v any) {
	if it.Extra == nil {
		it.Extra = map[string]any{}
	}
	it.Extra[key] = v
}

// adoptedStories maps action ID → story for one room.
func adoptedStories(st *issue.Store, room string) (map[string]*issue.Issue, error) {
	all, err := st.List()
	if err != nil {
		return nil, err
	}
	out := map[string]*issue.Issue{}
	for _, it := range all {
		if extString(it, extRoom) == room && extString(it, extAction) != "" {
			out[extString(it, extAction)] = it
		}
	}
	return out, nil
}

func storyBody(room string, a agreedAction, sp adoptSpec, deps []string, by string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Adopted from Meet room `%s` (action `%s`, %s) by %s.\n\n", room, a.ID, a.Source, by)
	fmt.Fprintf(&b, "Agreed action: %s\n\n", a.Text)
	if sp.Owner != "" {
		fmt.Fprintf(&b, "Owner: %s\n\n", sp.Owner)
	}
	if len(deps) > 0 {
		b.WriteString("Dependencies:\n")
		for _, d := range deps {
			fmt.Fprintf(&b, "- %s\n", d)
		}
		b.WriteString("\n")
	}
	b.WriteString("Acceptance:\n")
	for _, c := range sp.Accept {
		fmt.Fprintf(&b, "- %s\n", c)
	}
	b.WriteString("\nThis story does not authorize execution by itself; the owner starts it through the normal sprint flow. A verified result returns to the room with `meet outcome`.\n")
	return b.String()
}

// storyIDFor is the story ID an action adopts into. It is derived, not random:
// two adopters racing on the same action compute the same ID, so the second can
// only find (or repair) the first's story, never file another beside it.
func storyIDFor(room, action string) string {
	sum := sha256.Sum256([]byte("meet-story\x00" + room + "\x00" + action))
	return hex.EncodeToString(sum[:])[:12]
}

// withStoreLock serializes the read-check-write sections that file or mark
// stories. The lock is a kernel flock on a stable file keyed by the register's
// path (kept out of the register itself, which is a committed directory), so it
// holds across processes (a CLI and a serving browser route) as well as goroutines.
func withStoreLock(st *issue.Store, fn func() error) error {
	dir, err := filepath.Abs(st.Dir())
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(dir))
	host, _ := os.Hostname()
	l, err := lockfile.AcquireWithin(filepath.Join(os.TempDir(), "yoke-meet-adopt-"+hex.EncodeToString(sum[:6])+".lock"), 30*time.Second, lockfile.Holder{
		Name: host, PID: os.Getpid(), Intent: "adopt meet actions", Since: time.Now(),
	})
	if err != nil {
		return fmt.Errorf("meet: waiting for the story register: %w", err)
	}
	defer func() { _ = l.Release() }()
	return fn()
}

// adoptActions files one story per selected action. Already-adopted actions are
// reported, never duplicated. With write=false nothing is saved (preview). The
// whole check-and-file runs under the register lock, so concurrent repeats of
// the same adoption converge on one story.
func adoptActions(room string, st *issue.Store, selected []agreedAction, specs map[string]adoptSpec, by string, sprint int64, write bool) ([]adoptResult, error) {
	if !write {
		return adoptActionsLocked(room, st, selected, specs, by, sprint, false)
	}
	var out []adoptResult
	err := withStoreLock(st, func() (err error) {
		out, err = adoptActionsLocked(room, st, selected, specs, by, sprint, true)
		return err
	})
	return out, err
}

func adoptActionsLocked(room string, st *issue.Store, selected []agreedAction, specs map[string]adoptSpec, by string, sprint int64, write bool) ([]adoptResult, error) {
	all, err := st.List()
	if err != nil {
		return nil, err
	}
	existing := map[string]*issue.Issue{} // linked stories by action ID
	byID := map[string]*issue.Issue{}     // every story by its own ID (finds an unlinked leftover)
	for _, it := range all {
		byID[it.ID] = it
		if extString(it, extRoom) == room && extString(it, extAction) != "" {
			existing[extString(it, extAction)] = it
		}
	}
	inBatch := map[string]bool{}
	for _, a := range selected {
		inBatch[a.ID] = true
	}
	// Validate everything before saving anything. Same-batch dependencies are
	// resolved to the derived story ID, so they need no prior creation and no
	// ordering: only a genuine cycle is refused.
	graph := map[string][]string{}
	for _, a := range selected {
		if _, done := existing[a.ID]; done {
			continue
		}
		sp := specs[a.ID]
		if len(sp.Accept) == 0 {
			return nil, fmt.Errorf("meet: action %s needs at least one --accept criterion before it can be adopted", a.ID)
		}
		for _, d := range sp.Depends {
			if _, ok := existing[d]; ok {
				continue
			}
			if inBatch[d] {
				if d == a.ID {
					return nil, fmt.Errorf("meet: action %s depends on itself", a.ID)
				}
				graph[a.ID] = append(graph[a.ID], d)
				continue
			}
			if _, err := todo.ResolveRef(st, d); err != nil {
				return nil, fmt.Errorf("meet: action %s depends on %q, which is neither an adopted action nor a known story", a.ID, d)
			}
		}
	}
	if cyc := findCycle(graph); cyc != "" {
		return nil, fmt.Errorf("meet: dependency cycle among actions in %s (through %s)", room, cyc)
	}
	var out []adoptResult
	for _, a := range selected {
		if it, done := existing[a.ID]; done {
			out = append(out, adoptResult{Action: a, Story: it.ID, Note: "already adopted"})
			continue
		}
		sp := specs[a.ID]
		var deps []string
		for _, d := range sp.Depends {
			switch {
			case existing[d] != nil:
				deps = append(deps, existing[d].ID)
			case inBatch[d]:
				deps = append(deps, storyIDFor(room, d))
			default:
				if it, err := todo.ResolveRef(st, d); err == nil {
					deps = append(deps, it.ID)
				}
			}
		}
		if sp.Owner == "" {
			sp.Owner = a.Owner
		}
		sid := storyIDFor(room, a.ID)
		r := adoptResult{Action: a, Created: true, Story: sid}
		if write {
			fill := func(it *issue.Issue) {
				it.ID = sid
				if sprint != 0 {
					todo.LinkSprint(it, sprint)
				}
				it.Kind = issue.KindTask
				it.Title = a.Task
				it.Body = storyBody(room, a, sp, deps, by)
				it.Labels = []string{labelAdopted}
				it.Assignee = sp.Owner
				it.Origin = "meet:" + room
				setExt(it, extRoom, room)
				setExt(it, extAction, a.ID)
				setExt(it, extSource, a.Source)
				setExt(it, extAdoptedBy, by)
				if len(deps) > 0 {
					setExt(it, extDepends, deps)
				}
				setExt(it, extAccept, sp.Accept)
			}
			if left := byID[sid]; left != nil {
				// A story with this derived ID but no link: a previous adoption
				// was interrupted. Complete it rather than filing a second one.
				fill(left)
				if _, err := st.Save(left); err != nil {
					return out, err
				}
				existing[a.ID] = left
				r.Note = "completed an interrupted adoption"
			} else {
				it, err := todo.AddWith(st, a.Task, "", "", nil, "", "", fill)
				if err != nil {
					return out, err
				}
				existing[a.ID] = it
				byID[sid] = it
			}
		}
		out = append(out, r)
	}
	return out, nil
}

// findCycle returns a node on a dependency cycle, or "".
func findCycle(g map[string][]string) string {
	const (
		visiting = 1
		done     = 2
	)
	state := map[string]int{}
	var visit func(n string) string
	visit = func(n string) string {
		state[n] = visiting
		for _, d := range g[n] {
			if state[d] == visiting {
				return d
			}
			if state[d] == 0 {
				if c := visit(d); c != "" {
					return c
				}
			}
		}
		state[n] = done
		return ""
	}
	keys := make([]string, 0, len(g))
	for k := range g {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if state[k] == 0 {
			if c := visit(k); c != "" {
				return c
			}
		}
	}
	return ""
}

// reconcileFinding names one adopted story whose room decision moved.
type reconcileFinding struct {
	Story       string `json:"story"`
	Action      string `json:"action"`
	Reason      string `json:"reason"`
	Replacement string `json:"replacement,omitempty"` // possible successor action ID
	Requested   bool   `json:"requested"`
}

func wordSet(s string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(strings.ToLower(s)) {
		m[strings.Trim(w, ".,;:!?()\"'`")] = true
	}
	return m
}

func similarity(a, b string) float64 {
	x, y := wordSet(a), wordSet(b)
	if len(x) == 0 || len(y) == 0 {
		return 0
	}
	n := 0
	for w := range x {
		if y[w] {
			n++
		}
	}
	return float64(n) / float64(len(x)+len(y)-n)
}

// reconcileAdopted finds open adopted stories whose action is no longer in the
// room (amended, withdrawn, or re-decided), plus open stories that depend on
// them. With write=true it marks each story and returns findings; the caller
// posts the room request for those newly Requested.
func reconcileAdopted(room string, st *issue.Store, current []agreedAction, write bool, notify func(reconcileFinding) error) (out []reconcileFinding, err error) {
	if !write {
		return reconcileAdoptedLocked(room, st, current, false, nil)
	}
	err = withStoreLock(st, func() (e error) {
		out, e = reconcileAdoptedLocked(room, st, current, true, notify)
		return e
	})
	return out, err
}

func reconcileAdoptedLocked(room string, st *issue.Store, current []agreedAction, write bool, notify func(reconcileFinding) error) ([]reconcileFinding, error) {
	adopted, err := adoptedStories(st, room)
	if err != nil {
		return nil, err
	}
	live := map[string]agreedAction{}
	for _, a := range current {
		live[a.ID] = a
	}
	stale := map[string]string{} // story ID → reason
	ids := make([]string, 0, len(adopted))
	for id := range adopted {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var findings []reconcileFinding
	staleIdx := map[string]int{}
	for _, id := range ids {
		it := adopted[id]
		if todo.IsClosed(it.Status) {
			continue
		}
		if _, ok := live[id]; ok {
			continue
		}
		f := reconcileFinding{Story: it.ID, Action: id, Reason: "the agreed action no longer stands in the room (amended or withdrawn)"}
		best := 0.5
		for _, a := range current {
			if _, taken := adopted[a.ID]; taken {
				continue
			}
			if s := similarity(it.Title, a.Task); s >= best {
				best, f.Replacement = s, a.ID
			}
		}
		stale[it.ID] = f.Reason
		staleIdx[it.ID] = len(findings)
		findings = append(findings, f)
	}
	// Work that depends on a changed decision is affected too, transitively.
	for changed := true; changed; {
		changed = false
		for _, id := range ids {
			it := adopted[id]
			if todo.IsClosed(it.Status) || stale[it.ID] != "" {
				continue
			}
			for _, d := range extStrings(it, extDepends) {
				if _, ok := stale[d]; ok {
					reason := "depends on story " + d + ", whose decision changed"
					stale[it.ID] = reason
					findings = append(findings, reconcileFinding{Story: it.ID, Action: id, Reason: reason})
					changed = true
					break
				}
			}
		}
	}
	for i := range findings {
		it := byStory(adopted, findings[i].Story)
		if it == nil {
			continue
		}
		findings[i].Requested = extString(it, extReconcile) == ""
		if !write || !findings[i].Requested {
			continue
		}
		// Ask the room BEFORE marking the story: a crash between the two leaves
		// the story unmarked, so a retry asks again (the notice is deduplicated by
		// its marker) instead of finding a mark and never telling the room.
		if notify != nil {
			if err := notify(findings[i]); err != nil {
				return findings, err
			}
			if afterNotify != nil {
				if err := afterNotify(); err != nil {
					return findings, err
				}
			}
		}
		setExt(it, extReconcile, findings[i].Reason)
		if !hasLabel(it, labelReconcile) {
			it.Labels = append(it.Labels, labelReconcile)
		}
		it.Body = strings.TrimRight(it.Body, "\n") + "\n\nReconciliation requested (" + time.Now().UTC().Format("2006-01-02") + "): " + findings[i].Reason + ". Confirm, amend or close this story; it is not silently re-planned."
		if findings[i].Replacement != "" {
			it.Body += " Possible successor action: " + findings[i].Replacement + "."
		}
		it.Body += "\n"
		if _, err := st.Save(it); err != nil {
			return findings, err
		}
	}
	return findings, nil
}

func byStory(m map[string]*issue.Issue, story string) *issue.Issue {
	for _, it := range m {
		if it.ID == story {
			return it
		}
	}
	return nil
}

func hasLabel(it *issue.Issue, l string) bool {
	for _, x := range it.Labels {
		if x == l {
			return true
		}
	}
	return false
}

// recordOutcome returns a verified story result to its room, exactly once. A
// story counts as verified only once it is closed done — sprint stories reach
// that through `bashy sprint accept`, not through a worker's own claim.
func recordOutcome(st *issue.Store, ref string, evidence []string, post func(room, text string) error) (it *issue.Issue, posted bool, err error) {
	err = withStoreLock(st, func() (e error) {
		it, posted, e = recordOutcomeLocked(st, ref, evidence, post)
		return e
	})
	return it, posted, err
}

func recordOutcomeLocked(st *issue.Store, ref string, evidence []string, post func(room, text string) error) (*issue.Issue, bool, error) {
	it, err := todo.ResolveRef(st, ref)
	if err != nil {
		return nil, false, err
	}
	room := extString(it, extRoom)
	if room == "" {
		return it, false, fmt.Errorf("meet: story %s was not adopted from a room", it.ID)
	}
	if it.Status != todo.StatusDone {
		return it, false, fmt.Errorf("meet: story %s is %s, not done — only a verified (accepted) result returns to the room", it.ID, it.Status)
	}
	if extString(it, extReportedAt) != "" {
		return it, false, nil
	}
	if len(evidence) == 0 {
		return it, false, fmt.Errorf("meet: an outcome needs at least one --evidence link")
	}
	text := fmt.Sprintf("outcome: action %s done — story %s %q. Evidence: %s", extString(it, extAction), it.ID, it.Title, strings.Join(evidence, ", "))
	// The post is deduplicated by its marker (see ReturnOutcome), so when the
	// save below fails after the post, a retry finds the notice and only marks.
	if err := post(room, text); err != nil {
		return it, false, err
	}
	if afterNotify != nil {
		if err := afterNotify(); err != nil {
			return it, false, err
		}
	}
	setExt(it, extEvidence, evidence)
	setExt(it, extReportedAt, time.Now().UTC().Format(time.RFC3339))
	if _, err := st.Save(it); err != nil {
		return it, false, err
	}
	return it, true, nil
}

func parseKV(items []string, what string) (map[string][]string, error) {
	out := map[string][]string{}
	for _, s := range items {
		k, v, ok := strings.Cut(s, "=")
		if !ok || strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("meet: %s wants ACTION=VALUE, got %q", what, s)
		}
		out[strings.TrimSpace(k)] = append(out[strings.TrimSpace(k)], strings.TrimSpace(v))
	}
	return out, nil
}

func newAdoptCmd() *cobra.Command {
	var (
		ids, owners, depends, accepts []string
		confirm, reconcile            bool
		repo, user                    bool
		baseDir, by                   string
		sprint                        int64
	)
	cmd := &cobra.Command{
		Use:   "adopt <room> [--action ID ... --accept ID=CRITERION --confirm]",
		Short: "preview and explicitly adopt agreed actions as linked sprint stories",
		Long: "Without --confirm this only lists the room's actions and their adoption state.\n" +
			"Adopting is explicit: name each action with --action and give it at least one\n" +
			"--accept ID=CRITERION; --owner ID=NAME and --depends ID=ACTION|STORY are optional.\n" +
			"--confirm needs the caller, under their own identity, to be the room's human or\n" +
			"convener, or the agent that is its organizer or sprint manager; the secretary,\n" +
			"another agent or an outsider is refused, and --by cannot name anyone else.\n" +
			"Secretary-proposed actions are never adopted unless named. Re-running is\n" +
			"safe: the action ID is stable, so an adopted action never yields a second story.\n" +
			"--reconcile reports (with --confirm, requests) reconciliation for stories whose action changed.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			state, err := roomOf(args[0])
			if err != nil {
				return err
			}
			store, _, err := todo.ResolveStore("", repo, user, baseDir)
			if err != nil {
				return err
			}
			// The confirming person is who is actually at the CLI, never a name the
			// flag supplies; the API then checks that person against the room.
			principal, err := callerPrincipal(by)
			if err != nil {
				return err
			}
			by = principal
			w := cmd.OutOrStdout()
			if reconcile {
				findings, err := ReconcileActions(state.ID, ReconcileRequest{Confirm: confirm, By: by, Store: store})
				if err != nil {
					return err
				}
				if len(findings) == 0 {
					fmt.Fprintln(w, "no adopted work is affected by a changed decision")
				}
				for _, f := range findings {
					fmt.Fprintf(w, "affected: story %s (%s) — %s", f.Story, f.Action, f.Reason)
					if f.Replacement != "" {
						fmt.Fprintf(w, "; possible successor %s", f.Replacement)
					}
					fmt.Fprintln(w)
				}
				if !confirm && len(findings) > 0 {
					fmt.Fprintln(w, "preview only; re-run with --confirm to mark the stories and ask the room")
				}
				return nil
			}
			req := AdoptRequest{Confirm: confirm, By: by, Sprint: sprint, Store: store}
			if len(ids) > 0 {
				ow, err := parseKV(owners, "--owner")
				if err != nil {
					return err
				}
				dp, err := parseKV(depends, "--depends")
				if err != nil {
					return err
				}
				ac, err := parseKV(accepts, "--accept")
				if err != nil {
					return err
				}
				for _, id := range ids {
					sp := ActionSpec{Action: id, Depends: dp[id], Accept: ac[id]}
					if len(ow[id]) > 0 {
						sp.Owner = ow[id][0]
					}
					req.Actions = append(req.Actions, sp)
				}
			}
			resp, err := AdoptActions(state.ID, req)
			if resp != nil {
				for _, a := range resp.Candidates {
					stateWord := "not adopted"
					if a.Adopted {
						stateWord = "adopted as " + a.Story
					}
					fmt.Fprintf(w, "%s  [%s] %s — %s\n", a.ID, a.Source, a.Text, stateWord)
				}
				if len(ids) == 0 {
					if len(resp.Candidates) == 0 {
						fmt.Fprintln(w, "(no actions in this room)")
					}
					fmt.Fprintln(w, "preview only; name actions with --action ID and --accept ID=CRITERION, then --confirm to adopt")
				}
				for _, r := range resp.Results {
					switch {
					case r.Note != "":
						fmt.Fprintf(w, "%s: %s (story %s)\n", r.Action.ID, r.Note, r.Story)
					case confirm:
						fmt.Fprintf(w, "%s: adopted as story %s\n", r.Action.ID, r.Story)
					default:
						fmt.Fprintf(w, "%s: would adopt %q as story %s\n", r.Action.ID, r.Action.Task, r.Story)
					}
				}
			}
			if err != nil {
				return err
			}
			if !confirm && len(ids) > 0 {
				fmt.Fprintln(w, "preview only; re-run with --confirm to adopt")
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringArrayVar(&ids, "action", nil, "action ID to adopt (repeatable; see the preview)")
	f.StringArrayVar(&owners, "owner", nil, "ACTION=NAME owner (default: the 'owner:' prefix of the action)")
	f.StringArrayVar(&depends, "depends", nil, "ACTION=ACTION|STORY dependency (repeatable)")
	f.StringArrayVar(&accepts, "accept", nil, "ACTION=CRITERION acceptance criterion (required; repeatable)")
	f.BoolVar(&confirm, "confirm", false, "write: adopt (or, with --reconcile, mark stories and ask the room)")
	f.BoolVar(&reconcile, "reconcile", false, "report adopted work affected by a changed decision")
	f.BoolVar(&repo, "repo", false, "use the repo todo list")
	f.BoolVar(&user, "user", false, "use the personal todo list")
	f.StringVar(&baseDir, "base-dir", "", "todo list base directory")
	f.StringVar(&by, "by", "", "must be the calling user if given; it cannot lend another person's authority")
	f.Int64Var(&sprint, "sprint", 0, "sprint number to file the stories under (default: the host default)")
	return cmd
}

func newOutcomeCmd() *cobra.Command {
	var evidence []string
	var repo, user bool
	var baseDir, by string
	cmd := &cobra.Command{
		Use:   "outcome <story> --evidence LINK [--evidence LINK ...]",
		Short: "return a verified adopted story's result, with evidence links, to its room",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, _, err := todo.ResolveStore("", repo, user, baseDir)
			if err != nil {
				return err
			}
			it, err := todo.ResolveRef(store, args[0])
			if err != nil {
				return err
			}
			room := extString(it, extRoom)
			if room == "" {
				return fmt.Errorf("meet: story %s was not adopted from a room", it.ID)
			}
			if by == "" {
				by = humanName()
			}
			story, posted, err := ReturnOutcome(room, OutcomeRequest{Story: it.ID, Evidence: evidence, By: by, Store: store})
			if err != nil {
				return err
			}
			if posted {
				fmt.Fprintf(cmd.OutOrStdout(), "returned story %s to room %s\n", story, room)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "story %s was already returned to its room\n", story)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringArrayVar(&evidence, "evidence", nil, "evidence link (commit, run record, URL; repeatable)")
	f.BoolVar(&repo, "repo", false, "use the repo todo list")
	f.BoolVar(&user, "user", false, "use the personal todo list")
	f.StringVar(&baseDir, "base-dir", "", "todo list base directory")
	f.StringVar(&by, "by", "", "who is reporting (default: the current user)")
	return cmd
}
