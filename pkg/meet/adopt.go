package meet

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

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
	for _, e := range events {
		if e.Kind == "action" {
			add(e.Text, sourceAgreed)
		}
	}
	if syn != nil {
		for _, a := range syn.Actions {
			add(a, sourceProposed)
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

// adoptActions files one story per selected action. Already-adopted actions are
// reported, never duplicated. With write=false nothing is saved (preview).
func adoptActions(room string, st *issue.Store, selected []agreedAction, specs map[string]adoptSpec, by string, sprint int64, write bool) ([]adoptResult, error) {
	existing, err := adoptedStories(st, room)
	if err != nil {
		return nil, err
	}
	inBatch := map[string]bool{}
	for _, a := range selected {
		inBatch[a.ID] = true
	}
	// Validate everything before saving anything: a half-adopted batch is the
	// duplicate-prone state this feature exists to avoid.
	for _, a := range selected {
		if _, done := existing[a.ID]; done {
			continue
		}
		sp := specs[a.ID]
		if len(sp.Accept) == 0 {
			return nil, fmt.Errorf("meet: action %s needs at least one --accept criterion before it can be adopted", a.ID)
		}
		for _, d := range sp.Depends {
			if _, ok := existing[d]; ok || inBatch[d] {
				continue
			}
			if _, err := todo.ResolveRef(st, d); err != nil {
				return nil, fmt.Errorf("meet: action %s depends on %q, which is neither an adopted action nor a known story", a.ID, d)
			}
		}
	}
	var out []adoptResult
	pending := append([]agreedAction(nil), selected...)
	// Adopt dependencies first so a dependent story can name the story ID.
	for progress := true; len(pending) > 0 && progress; {
		progress = false
		var next []agreedAction
		for _, a := range pending {
			if it, done := existing[a.ID]; done {
				out = append(out, adoptResult{Action: a, Story: it.ID, Note: "already adopted"})
				progress = true
				continue
			}
			sp := specs[a.ID]
			var deps []string
			ready := true
			for _, d := range sp.Depends {
				if it, ok := existing[d]; ok {
					deps = append(deps, it.ID)
				} else if inBatch[d] {
					ready = false
				} else if it, err := todo.ResolveRef(st, d); err == nil {
					deps = append(deps, it.ID)
				}
			}
			if !ready {
				next = append(next, a)
				continue
			}
			progress = true
			if sp.Owner == "" {
				sp.Owner = a.Owner
			}
			r := adoptResult{Action: a, Created: true}
			if write {
				it, err := todo.Add(st, a.Task, storyBody(room, a, sp, deps, by), "", nil, "", "")
				if err != nil {
					return out, err
				}
				if sprint != 0 {
					todo.LinkSprint(it, sprint)
				}
				it.Kind = issue.KindTask
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
				if _, err := st.Save(it); err != nil {
					return out, err
				}
				existing[a.ID] = it
				r.Story = it.ID
			}
			out = append(out, r)
		}
		pending = next
	}
	if len(pending) > 0 {
		return out, fmt.Errorf("meet: dependency cycle among actions in %s", room)
	}
	return out, nil
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
func reconcileAdopted(room string, st *issue.Store, current []agreedAction, write bool) ([]reconcileFinding, error) {
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
	for _, id := range ids {
		it := adopted[id]
		if todo.IsClosed(it.Status) || stale[it.ID] != "" {
			continue
		}
		for _, d := range extStrings(it, extDepends) {
			if _, ok := stale[d]; ok {
				findings = append(findings, reconcileFinding{Story: it.ID, Action: id, Reason: "depends on story " + d + ", whose decision changed"})
				break
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
func recordOutcome(st *issue.Store, ref string, evidence []string, post func(room, text string) error) (*issue.Issue, bool, error) {
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
	if err := post(room, text); err != nil {
		return it, false, err
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
			"Secretary-proposed actions are never adopted unless named. Re-running is safe: the\n" +
			"action ID is stable, so an adopted action never yields a second story.\n" +
			"--reconcile reports (with --confirm, requests) reconciliation for stories whose action changed.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			room, err := resolveMeeting(args[0])
			if err != nil {
				return err
			}
			state, err := loadState(room)
			if err != nil {
				return err
			}
			events, _ := readTranscript(room)
			current := agreedActions(room, events, loadSynthesis(room))
			store, _, err := todo.ResolveStore("", repo, user, baseDir)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if reconcile {
				findings, err := reconcileAdopted(room, store, current, confirm)
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
					if confirm && f.Requested {
						if _, err := record(state, "note", by, "", fmt.Sprintf("reconcile: story %s (action %s) — %s. Please confirm, amend or close it.", f.Story, f.Action, f.Reason)); err != nil {
							return err
						}
					}
				}
				if !confirm && len(findings) > 0 {
					fmt.Fprintln(w, "preview only; re-run with --confirm to mark the stories and ask the room")
				}
				return nil
			}
			adopted, err := adoptedStories(store, room)
			if err != nil {
				return err
			}
			if len(ids) == 0 {
				for _, a := range current {
					stateWord := "not adopted"
					if it, ok := adopted[a.ID]; ok {
						stateWord = "adopted as " + it.ID
					}
					fmt.Fprintf(w, "%s  [%s] %s — %s\n", a.ID, a.Source, a.Text, stateWord)
				}
				if len(current) == 0 {
					fmt.Fprintln(w, "(no actions in this room)")
				}
				fmt.Fprintln(w, "preview only; name actions with --action ID and --accept ID=CRITERION, then --confirm to adopt")
				return nil
			}
			byID := map[string]agreedAction{}
			for _, a := range current {
				byID[a.ID] = a
			}
			var selected []agreedAction
			for _, id := range ids {
				a, ok := byID[id]
				if !ok {
					return fmt.Errorf("meet: no action %s in room %s", id, room)
				}
				selected = append(selected, a)
			}
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
			specs := map[string]adoptSpec{}
			for _, a := range selected {
				sp := adoptSpec{Depends: dp[a.ID], Accept: ac[a.ID]}
				if len(ow[a.ID]) > 0 {
					sp.Owner = ow[a.ID][0]
				}
				specs[a.ID] = sp
			}
			if sprint == 0 && todo.DefaultSprint != nil {
				if sprint, err = todo.DefaultSprint(); err != nil {
					return err
				}
			}
			if sprint != 0 && todo.PrepareSprint != nil && confirm {
				if err := todo.PrepareSprint(store, sprint); err != nil {
					return err
				}
			}
			res, err := adoptActions(room, store, selected, specs, by, sprint, confirm)
			for _, r := range res {
				switch {
				case r.Note != "":
					fmt.Fprintf(w, "%s: %s (story %s)\n", r.Action.ID, r.Note, r.Story)
				case confirm:
					fmt.Fprintf(w, "%s: adopted as story %s\n", r.Action.ID, r.Story)
				default:
					fmt.Fprintf(w, "%s: would adopt %q\n", r.Action.ID, r.Action.Task)
				}
			}
			if err != nil {
				return err
			}
			if !confirm {
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
	f.StringVar(&by, "by", "operator", "who is adopting (recorded on the story and in the room)")
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
			it, posted, err := recordOutcome(store, args[0], evidence, func(room, text string) error {
				state, err := loadState(room)
				if err != nil {
					return err
				}
				_, err = record(state, "note", by, "", text)
				return err
			})
			if err != nil {
				return err
			}
			if posted {
				fmt.Fprintf(cmd.OutOrStdout(), "returned story %s to room %s\n", it.ID, extString(it, extRoom))
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "story %s was already returned to its room\n", it.ID)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringArrayVar(&evidence, "evidence", nil, "evidence link (commit, run record, URL; repeatable)")
	f.BoolVar(&repo, "repo", false, "use the repo todo list")
	f.BoolVar(&user, "user", false, "use the personal todo list")
	f.StringVar(&baseDir, "base-dir", "", "todo list base directory")
	f.StringVar(&by, "by", "operator", "who is reporting")
	return cmd
}
