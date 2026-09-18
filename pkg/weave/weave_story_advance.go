package weave

// THE CYCLE BOUNDARY — how ONE sprint serves many iterations.
//
// Some work is a procedure, not a project: a release, a dependency sweep, a
// quarterly audit. It has a fixed set of steps, it runs to a terminal result,
// and then it runs again. Filed as an ordinary sprint that is the wrong shape
// twice over — the card can never close, because its stories reopen by design,
// and re-creating an identical card each time makes the procedure a game of
// telephone, since a workaround typed into iteration N's story silently
// becomes iteration N+1's authored procedure.
//
// So the sprint is REUSED and the ITERATION is what ends. `start` and `stop`
// already draw that boundary — stop parks the workers, runs the gate, and
// writes the verdict into a box, and Boxes is a list precisely so the history
// survives. What was missing is only what happens BETWEEN one stop and the
// next start, and that is this file.
//
// # Three kinds of story, distinguished by fields that already exist
//
//	recurring (`recurring:` set)  the procedure's steps. Reset to todo here.
//	one-off   (no `recurring:`)   raised for THIS iteration. Must be closed.
//	cycle record (label `cycle`)  the audit trail. One per closed iteration.
//
// Nothing marks a sprint as recurring. A sprint that has recurring stories is
// one; a sprint that does not is untouched by this command. That is what lets
// a line be converted in either direction with `todo edit --recurring`, and
// ended at any time with the ordinary `sprint end`.
//
// # Why the record is an ITEM
//
// The sprint board lives at ~/.bashy/sprint — host-local, and therefore unable
// to hold an audit trail: work that others depend on may never anchor to one
// user's home directory. A cycle record is written as an ordinary item in the
// repo's committed docs/todo/ store, so it travels with the clone, diffs in a
// pull request, and is readable with `cat` by someone who has never run bashy.
//
// # Rod, not fish
//
// This command knows nothing about versions, tags, or naming. It runs the
// sprint's own advance script and records what that script printed. Semver
// belongs to the line that has versions; a period id belongs to the line that
// has periods; bashy owns exactly one counter, the cycle number, because that
// is its own index rather than anyone's domain.

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/coreutils/pkg/weavecli"
	"github.com/qiangli/yoke/pkg/issue"
	todopkg "github.com/qiangli/yoke/pkg/todo"
)

const (
	// cycleLabel marks an item as a cycle record rather than a unit of work.
	// A label, not a new field or a new store: `issue.Issue` already carries
	// Labels, and a record that reuses the item format stays greppable,
	// diffable, and readable by every tool that already understands an item.
	cycleLabel = "cycle"

	// cycleKey is the one manifest key the MECHANISM owns. Everything else in
	// a manifest comes from the sprint's own script.
	cycleKey = "cycle"

	// advanceScriptRel is where a sprint's advance script lives, relative to a
	// tracked repo root. Its stdout is `k=v` lines. A script rather than a
	// schema, so it can be read, run and tested without going through bashy.
	advanceScriptRel = ".bashy/sprint"

	manifestOpen  = "<!-- manifest -->"
	manifestClose = "<!-- /manifest -->"

	// advanceScriptTimeout bounds the script. It reaches the network in the
	// motivating case (resolving the next version from remote tags), so it
	// needs real time — but never unbounded time, because an agent driving
	// this has no way to notice a hang.
	advanceScriptTimeout = 5 * time.Minute
)

// cycleStory is one of a sprint's stories together with the store it came
// from. The root is not decoration: a sprint spans repos, and an item is only
// addressable through the store that holds it.
type cycleStory struct {
	root string
	it   *issue.Issue
}

// cycleCensus is the sprint's stories sorted into the three kinds above.
type cycleCensus struct {
	recurring  []cycleStory
	openOneOff []cycleStory
	doneOneOff []cycleStory
	records    []cycleStory
	roots      []string
}

func hasLabel(it *issue.Issue, want string) bool {
	for _, l := range it.Labels {
		if strings.EqualFold(strings.TrimSpace(l), want) {
			return true
		}
	}
	return false
}

// takeCensus reads every tracked store once and classifies this sprint's items.
func takeCensus(s *weaveStory) (*cycleCensus, error) {
	c := &cycleCensus{roots: sprintStoryRoots(s)}
	seen := map[string]bool{}
	for _, root := range c.roots {
		items, err := todopkg.List(todopkg.RepoStore(root), "")
		if err != nil {
			return nil, fmt.Errorf("stories in %s: %w", root, err)
		}
		for _, it := range items {
			if it.Sprint != s.ID {
				continue
			}
			key := root + "\x00" + it.ID
			if seen[key] {
				continue
			}
			seen[key] = true
			cs := cycleStory{root: root, it: it}
			switch {
			case hasLabel(it, cycleLabel):
				c.records = append(c.records, cs)
			case strings.TrimSpace(it.Recurring) != "":
				c.recurring = append(c.recurring, cs)
			case it.Status == todopkg.StatusDone:
				c.doneOneOff = append(c.doneOneOff, cs)
			default:
				c.openOneOff = append(c.openOneOff, cs)
			}
		}
	}
	return c, nil
}

// lastManifest returns the newest cycle record's values and its cycle number.
// The cycle number lives IN the manifest rather than in a field of its own, so
// a record is self-describing: read one file and you know which iteration it
// was, without consulting the sprint board that may not be on this host.
func (c *cycleCensus) lastManifest() (map[string]string, int) {
	best := map[string]string{}
	bestN := 0
	for _, r := range c.records {
		m := parseManifest(r.it.Body)
		n, _ := strconv.Atoi(m[cycleKey])
		if n >= bestN {
			bestN, best = n, m
		}
	}
	return best, bestN
}

// parseManifest reads the k=v block a cycle record carries. Sentinel HTML
// comments delimit it so the record stays readable prose in every markdown
// viewer while remaining exactly parseable here.
func parseManifest(body string) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(body))
	in := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == manifestOpen:
			in = true
			continue
		case line == manifestClose:
			in = false
			continue
		case !in:
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if k = strings.TrimSpace(k); k != "" {
			out[k] = strings.TrimSpace(v)
		}
	}
	return out
}

func renderManifest(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(manifestOpen + "\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, m[k])
	}
	b.WriteString(manifestClose + "\n")
	return b.String()
}

// findAdvanceScript looks for <root>/.bashy/sprint/<id>.advance across the
// tracked roots. Absent is not an error: a sprint whose iterations carry no
// values needs no script, and --set alone is a complete manifest.
func findAdvanceScript(roots []string, id int64) string {
	for _, root := range roots {
		p := filepath.Join(root, advanceScriptRel, fmt.Sprintf("%d.advance", id))
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// runAdvanceScript executes the sprint's own script and parses its stdout as
// `k=v`. Unlike a GATE — which judges another party's output and is therefore
// run with a stripped environment — this is the sprint's own code, and it must
// reach `bashy`, `git` and the network to do its job. It inherits the
// environment, plus the previous cycle's values so a bump needs no state of
// its own.
func runAdvanceScript(ctx context.Context, dir, command string, cycle int, prev map[string]string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, advanceScriptTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = dir
	env := append(os.Environ(),
		"SPRINT_CYCLE="+strconv.Itoa(cycle),
	)
	for k, v := range prev {
		env = append(env, "SPRINT_PREV_"+strings.ToUpper(k)+"="+v)
	}
	cmd.Env = env
	cmd.Stderr = os.Stderr

	raw, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("advance script timed out after %s: %s", advanceScriptTimeout, command)
		}
		return nil, fmt.Errorf("advance script failed: %s: %w", command, err)
	}

	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("advance script printed a line that is not k=v: %q", line)
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out, nil
}

// advanceResult is what one boundary crossing did, and what --json emits.
type advanceResult struct {
	Sprint   int64             `json:"sprint"`
	Cycle    int               `json:"cycle"`
	Manifest map[string]string `json:"manifest"`
	Record   string            `json:"record,omitempty"`
	Reset    []string          `json:"reset,omitempty"`
	Dropped  []string          `json:"dropped,omitempty"`
	Script   string            `json:"script,omitempty"`
	DryRun   bool              `json:"dry_run,omitempty"`
}

func newSprintAdvanceCmd() *cobra.Command {
	var flags weaveOutputFlags
	var sets, drops []string
	var execCmd, note string
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "advance <sprint>",
		Short: "Close one iteration of a recurring sprint and open the next",
		Long: `advance crosses the CYCLE BOUNDARY of a recurring sprint: it files the
iteration that just finished as a durable record, then resets the procedure's
stories so the same sprint can run again.

A sprint is recurring when it HAS recurring stories — there is no sprint type
to set, nothing to migrate, and no separate card per iteration. Three kinds of
story, told apart by fields that already exist:

  recurring (` + "`--recurring default`" + `)  the procedure's steps; reset to todo here
  one-off   (no ` + "`recurring:`" + `)         raised for THIS iteration; must be closed
  cycle record (label ` + "`cycle`" + `)        the audit trail this command writes

Convert a line in either direction with ` + "`todo edit <ref> --recurring`" + `, and end
it whenever you like with the ordinary ` + "`sprint end`" + `. Nothing here is special.

WHAT IT REFUSES, AND WHY. A running box means the iteration has not been
judged yet — ` + "`sprint stop`" + ` is what parks the workers and records the gate
verdict, and advancing over it would file an iteration whose result nobody
established. An OPEN one-off story means unfinished work would be carried
silently into the next iteration under the previous one's name; finish it,
or ` + "`--drop`" + ` it, which closes it as obsolete WITH the reason on the record.
Neither refusal can be forced, because both exist to stop a success state
being reached by the absence of evidence.

WHERE THE VALUES COME FROM. advance knows nothing about versions, tags or
naming. It runs the sprint's own script — ` + "`.bashy/sprint/<id>.advance`" + ` in a
tracked repo, or ` + "`--exec`" + ` — and records the ` + "`k=v`" + ` lines it prints. The
previous iteration's values arrive as $SPRINT_PREV_<KEY> and the new cycle
number as $SPRINT_CYCLE, so a bump keeps no state of its own. It is an
ordinary command: run it by hand to see what the next iteration would carry.
` + "`--set k=v`" + ` overrides any key; with no script, --set is the whole manifest.

THE RECORD IS AN ITEM, in the repo's committed docs/todo/ store — because the
sprint board is host-local and cannot be an audit trail. It travels with the
clone, diffs in a pull request, and reads fine under ` + "`cat`" + `.`,
		Example: "  bashy sprint advance 129 --dry-run\n" +
			"  bashy sprint advance 129\n" +
			"  bashy sprint advance 129 --set version=v0.9.5 --note \"hotfix line\"\n" +
			"  bashy sprint advance 129 --drop 42 --drop 43",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("sprint must be an integer: %q", args[0])
			}
			return runSprintAdvance(cmd, id, &flags, advanceOpts{
				sets: sets, drops: drops, exec: execCmd, note: note, dryRun: dryRun,
			})
		},
	}
	flags.attach(cmd)
	cmd.Flags().StringArrayVar(&sets, "set", nil, "manifest value k=v; overrides the advance script (repeatable)")
	cmd.Flags().StringArrayVar(&drops, "drop", nil, "close an unfinished one-off story as obsolete (repeatable)")
	cmd.Flags().StringVar(&execCmd, "exec", "", "command whose k=v stdout is the manifest, instead of the sprint's advance script")
	cmd.Flags().StringVar(&note, "note", "", "free text recorded on the cycle record")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what would happen; write nothing")
	return cmd
}

type advanceOpts struct {
	sets, drops []string
	exec, note  string
	dryRun      bool
}

func parseSets(sets []string) (map[string]string, error) {
	out := map[string]string{}
	for _, kv := range sets {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("--set wants k=v, got %q", kv)
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out, nil
}

func runSprintAdvance(cmd *cobra.Command, id int64, flags *weaveOutputFlags, o advanceOpts) error {
	mode := flags.mode()
	op := "sprint advance"

	overrides, err := parseSets(o.sets)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, op, weavecli.ExitInvalidArg, err))
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

	// PHASE A — read, validate, resolve. Deliberately outside the board lock:
	// the advance script reaches the network in the motivating case, and
	// holding a lock across that would block every other sprint command on
	// this host for as long as a remote takes to answer.
	if b := s.currentBox(); b.Running() {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, op, weavecli.ExitGenericFail,
			fmt.Errorf("sprint #%d is still running (%s) — `bashy sprint stop %d --gate '<cmd>'` first.\n"+
				"  stop is what parks the workers and records the gate verdict; advancing over a\n"+
				"  running box would file an iteration whose result nobody established.",
				id, b.Status(time.Now().UTC()), id)))
	}

	census, err := takeCensus(s)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, op, weavecli.ExitGenericFail, err))
	}
	if len(census.recurring) == 0 {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, op, weavecli.ExitInvalidArg,
			fmt.Errorf("sprint #%d has no recurring stories, so it has no cycle to advance.\n"+
				"  a sprint becomes recurring by having them:\n"+
				"    bashy todo edit <ref> --recurring %s", id, todopkg.CadenceSprint)))
	}

	dropped, err := resolveDrops(census, o.drops)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, op, weavecli.ExitInvalidArg, err))
	}
	if blocking := blockingOneOffs(census, dropped); len(blocking) > 0 {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, op, weavecli.ExitGenericFail,
			fmt.Errorf("sprint #%d has %d unfinished one-off %s:\n%s\n"+
				"  finish them, or `--drop <ref>` to close one as obsolete with the reason recorded.\n"+
				"  carrying one forward would file it under the next iteration's name.",
				id, len(blocking), plural(len(blocking), "story", "stories"), strings.Join(blocking, "\n"))))
	}

	prev, prevCycle := census.lastManifest()
	cycle := prevCycle + 1

	script := strings.TrimSpace(o.exec)
	scriptDir := ""
	if script == "" {
		if p := findAdvanceScript(census.roots, id); p != "" {
			script = "sh " + shellQuote(p)
			scriptDir = filepath.Dir(filepath.Dir(filepath.Dir(p))) // <root>/.bashy/sprint/x -> <root>
		}
	} else if len(census.roots) > 0 {
		scriptDir = census.roots[0]
	}

	manifest := map[string]string{}
	if script != "" {
		produced, err := runAdvanceScript(cmd.Context(), scriptDir, script, cycle, prev)
		if err != nil {
			return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, op, weavecli.ExitGenericFail, err))
		}
		manifest = produced
	}
	for k, v := range overrides {
		manifest[k] = v
	}
	// The mechanism owns exactly one key, and owns it LAST so neither the
	// script nor --set can make a record lie about which iteration it is.
	manifest[cycleKey] = strconv.Itoa(cycle)

	res := advanceResult{Sprint: id, Cycle: cycle, Manifest: manifest, Script: script, DryRun: o.dryRun}
	for _, cs := range census.recurring {
		res.Reset = append(res.Reset, storyLabel(cs))
	}
	for _, cs := range dropped {
		res.Dropped = append(res.Dropped, storyLabel(cs))
	}

	if o.dryRun {
		if mode == weavecli.OutputJSON {
			return ec(emitOK(cmd.OutOrStdout(), mode, op, res))
		}
		fmt.Fprint(cmd.OutOrStdout(), renderAdvance(&res))
		return nil
	}

	// PHASE B — write. Items first: the record is the evidence, and a crash
	// between the two must leave evidence WITHOUT a reset rather than a reset
	// with no evidence.
	recordID, err := writeCycleRecord(s, census, cycle, manifest, dropped, o.note)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, op, weavecli.ExitGenericFail, err))
	}
	res.Record = recordID

	for _, cs := range dropped {
		if err := closeAsObsolete(cs); err != nil {
			return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, op, weavecli.ExitGenericFail, err))
		}
	}
	if err := resetRecurring(census, cycle); err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, op, weavecli.ExitGenericFail, err))
	}

	// The thread note is the only board mutation, and it is bookkeeping: the
	// card itself gains no cycle state, which is what keeps a recurring sprint
	// an ordinary sprint that `end` can close at any time.
	_ = withWeaveQueueLock(dir, func(q *weaveQueue) error {
		if st := findWeaveStory(q, id); st != nil {
			st.Thread = append(st.Thread, weaveComment{
				At:     time.Now().UTC(),
				Author: "advance",
				Kind:   "system",
				Body: fmt.Sprintf("cycle %d filed as %s; %d recurring %s reset%s",
					cycle, recordID, len(census.recurring),
					plural(len(census.recurring), "story", "stories"), droppedSuffix(dropped)),
			})
			st.UpdatedAt = time.Now().UTC()
		}
		return nil
	})

	if mode == weavecli.OutputJSON {
		return ec(emitOK(cmd.OutOrStdout(), mode, op, res))
	}
	fmt.Fprint(cmd.OutOrStdout(), renderAdvance(&res))
	return nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func droppedSuffix(dropped []cycleStory) string {
	if len(dropped) == 0 {
		return ""
	}
	return fmt.Sprintf(", %d dropped", len(dropped))
}

func storyLabel(cs cycleStory) string {
	if cs.it.Seq > 0 {
		return fmt.Sprintf("#%d %s", cs.it.Seq, cs.it.Title)
	}
	return fmt.Sprintf("%s %s", cs.it.ID, cs.it.Title)
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// resolveDrops maps each --drop ref onto a one-off story of THIS sprint.
// A ref that names a recurring story is refused rather than silently ignored:
// dropping a procedure step is a different act from finishing an iteration,
// and quietly doing nothing would leave the caller believing it happened.
func resolveDrops(c *cycleCensus, refs []string) ([]cycleStory, error) {
	var out []cycleStory
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		var found *cycleStory
		for i := range c.openOneOff {
			if matchesRef(c.openOneOff[i].it, ref) {
				found = &c.openOneOff[i]
				break
			}
		}
		if found == nil {
			for _, cs := range c.recurring {
				if matchesRef(cs.it, ref) {
					return nil, fmt.Errorf("--drop %s names a RECURRING story (%q).\n"+
						"  drop closes an unfinished one-off; a procedure step is removed with\n"+
						"  `bashy todo edit %s --recurring \"\"` or `bashy todo rm %s`", ref, cs.it.Title, ref, ref)
				}
			}
			return nil, fmt.Errorf("--drop %s matches no open one-off story of this sprint", ref)
		}
		out = append(out, *found)
	}
	return out, nil
}

func matchesRef(it *issue.Issue, ref string) bool {
	if n, err := strconv.Atoi(strings.TrimPrefix(ref, "#")); err == nil && n > 0 {
		return it.Seq == n
	}
	return it.ID == ref || strings.HasPrefix(it.ID, ref)
}

func blockingOneOffs(c *cycleCensus, dropped []cycleStory) []string {
	isDropped := map[string]bool{}
	for _, d := range dropped {
		isDropped[d.it.ID] = true
	}
	var out []string
	for _, cs := range c.openOneOff {
		if !isDropped[cs.it.ID] {
			out = append(out, fmt.Sprintf("    %s [%s]", storyLabel(cs), cs.it.Status))
		}
	}
	return out
}

// writeCycleRecord files the closed iteration as an ordinary item. It is
// written to the store that holds the sprint's stories, so the record sits
// beside the work it attests rather than in a registry of its own.
func writeCycleRecord(s *weaveStory, c *cycleCensus, cycle int, manifest map[string]string, dropped []cycleStory, note string) (string, error) {
	if len(c.roots) == 0 {
		return "", fmt.Errorf("sprint #%d tracks no repo store — `bashy sprint track` first", s.ID)
	}
	root := c.roots[0]
	st := todopkg.RepoStore(root)

	now := time.Now().UTC()
	seq := 1
	if m, err := todopkg.MaxSeq(st); err == nil {
		seq = m + 1
	}
	it := &issue.Issue{
		ID:       issue.NewID(),
		Kind:     issue.KindTask,
		Seq:      seq,
		Title:    fmt.Sprintf("cycle %d — sprint #%d", cycle, s.ID),
		Labels:   []string{cycleLabel},
		Sprint:   s.ID,
		Status:   todopkg.StatusDone,
		Created:  now,
		Closed:   &now,
		ClosedBy: strings.TrimSpace(s.Owner),
		Body:     renderCycleBody(s, c, cycle, manifest, dropped, note, now),
	}
	if _, err := st.Save(it); err != nil {
		return "", fmt.Errorf("write cycle record in %s: %w", root, err)
	}
	return it.ID, nil
}

func renderCycleBody(s *weaveStory, c *cycleCensus, cycle int, manifest map[string]string, dropped []cycleStory, note string, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Cycle %d of sprint #%d (%s), closed %s.\n\n",
		cycle, s.ID, s.Title, now.Format(time.RFC3339))
	b.WriteString(renderManifest(manifest))

	b.WriteString("\n## Box\n\n")
	if box := s.lastBox(); box != nil {
		elapsed := box.Elapsed(now)
		if box.StoppedAt != nil {
			elapsed = box.StoppedAt.Sub(box.StartedAt)
		}
		fmt.Fprintf(&b, "planned %s, actual %s.\n", roundDur(box.Planned), roundDur(elapsed))
		switch {
		case !box.GateRan:
			// Absence of a gate is recorded as absence, never as a pass. The
			// box already refuses to close green without one; saying so here
			// keeps the record honest for a reader who never saw the refusal.
			b.WriteString("gate: NOT RUN — this iteration is unverified.\n")
		case box.GatePassed:
			fmt.Fprintf(&b, "gate: `%s` PASSED.\n", box.GateCmd)
		default:
			fmt.Fprintf(&b, "gate: `%s` FAILED.\n", box.GateCmd)
		}
	} else {
		b.WriteString("no time box was opened for this iteration.\n")
	}

	fmt.Fprintf(&b, "\n## Stories\n\nrecurring: %d\none-off done: %d\n", len(c.recurring), len(c.doneOneOff))
	if len(dropped) > 0 {
		fmt.Fprintf(&b, "dropped: %d\n", len(dropped))
		for _, d := range dropped {
			fmt.Fprintf(&b, "  - %s\n", storyLabel(d))
		}
	}
	if len(s.Runs) > 0 {
		fmt.Fprintf(&b, "\n## Runs\n\n")
		for _, r := range s.Runs {
			fmt.Fprintf(&b, "  - %s #%d\n", r.Repo, r.ID)
		}
	}
	if strings.TrimSpace(note) != "" {
		fmt.Fprintf(&b, "\n## Note\n\n%s\n", strings.TrimSpace(note))
	}
	return b.String()
}

func closeAsObsolete(cs cycleStory) error {
	cs.it.Status = todopkg.StatusDone
	now := time.Now().UTC()
	cs.it.Closed = &now
	cs.it.Resolution = "obsolete"
	if _, err := todopkg.RepoStore(cs.root).Save(cs.it); err != nil {
		return fmt.Errorf("drop %s: %w", cs.it.ID, err)
	}
	return nil
}

// resetRecurring returns the procedure's steps to todo for the next iteration.
//
// Clearing Weave is not tidiness: `weave add --from-todo` refuses any item
// that still carries a run id ("already in flight"), so without this the
// SECOND iteration cannot delegate a single story. The completed run keeps its
// own record; what is dropped is only the item's claim to still be in it.
func resetRecurring(c *cycleCensus, cycle int) error {
	for _, cs := range c.recurring {
		it := cs.it
		it.Status = todopkg.StatusTodo
		if strings.TrimSpace(it.Assignee) != "" {
			// An owner that survives the boundary is the whole point of a
			// standing assignment: routine delegation costs nothing, and a
			// conductor overrides it per iteration when it wants to.
			it.Status = todopkg.StatusAssigned
		}
		it.Closed = nil
		it.Resolution = ""
		it.ClosedBy = ""
		it.Weave = 0
		if _, err := todopkg.RepoStore(cs.root).Save(it); err != nil {
			return fmt.Errorf("reset %s: %w", it.ID, err)
		}
	}
	return nil
}

func renderAdvance(r *advanceResult) string {
	var b strings.Builder
	head := "sprint advance"
	if r.DryRun {
		head += " (dry run — nothing written)"
	}
	fmt.Fprintf(&b, "%s: sprint #%d → cycle %d\n", head, r.Sprint, r.Cycle)
	if r.Script != "" {
		fmt.Fprintf(&b, "  script   %s\n", r.Script)
	}
	keys := make([]string, 0, len(r.Manifest))
	for k := range r.Manifest {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	b.WriteString("  manifest\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "    %s=%s\n", k, r.Manifest[k])
	}
	if r.Record != "" {
		fmt.Fprintf(&b, "  record   %s\n", r.Record)
	}
	verb := "reset"
	if r.DryRun {
		verb = "would reset"
	}
	fmt.Fprintf(&b, "  %s %d\n", verb, len(r.Reset))
	for _, s := range r.Reset {
		fmt.Fprintf(&b, "    %s\n", s)
	}
	if len(r.Dropped) > 0 {
		fmt.Fprintf(&b, "  dropped %d (obsolete)\n", len(r.Dropped))
		for _, s := range r.Dropped {
			fmt.Fprintf(&b, "    %s\n", s)
		}
	}
	return b.String()
}
