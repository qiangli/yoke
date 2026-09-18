package graphcmd

// The contribution subcommands — the write/read API over the durable per-repo
// store. Added to the shared `graph` dispatch table like the code-graph reads,
// reachable at the `bashy graph …` front door and in-shell. Model-free, no
// graph build required for writes (they are pure append ops), so contributing
// is cheap in a tight loop.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/coreutils/pkg/weavecli"
	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/execlog"
)

const contribSchema = "bashy-graph-contrib-v1"

func init() {
	addSub("note", "attach a note/fact to an entity (contribute knowledge)",
		"graph note <target> <text> [--confidence C] [--source S] [--json]", runNote)
	addSub("link", "assert a typed relationship between two entities",
		"graph link <src> <relation> <dst> [--json]", runLink)
	addSub("observe", "record an action outcome (build/test/run/execution/…)",
		"graph observe <kind> <target> [--outcome O] [--summary S] [--data k=v]… [--json]", runObserve)
	addSub("forget", "retract a contribution (by id, or --target/--episode)",
		"graph forget <id> | [--target X] [--episode E] [--json]", runForget)
	addSub("recall", "search contributed knowledge (notes/links/observations)",
		"graph recall [query] [--target X] [--kind K] [--outcome O] [--limit N] [--json]", runRecall)
	addSub("notes", "show everything contributed about one entity",
		"graph notes <target> [--json]", runNotesFor)
	addSub("pitfalls", "known failures recorded against an entity/goal (avoid these)",
		"graph pitfalls <target> [--json]", runPitfalls)
}

type contribEnvelope struct {
	Schema        string         `json:"schema"`
	Root          string         `json:"root"`
	Count         int            `json:"count"`
	Contributions []Contribution `json:"contributions"`
	// Observed carries claims DERIVED from the execution corpus, kept in their
	// own key rather than merged into Contributions. An authored note is
	// somebody's judgement and a derived pitfall is a count of what happened;
	// a consumer must be able to tell which kind it is acting on.
	Observed []execlog.Pitfall `json:"observed,omitempty"`
}

type ackEnvelope struct {
	Schema string `json:"schema"`
	Op     string `json:"op"`
	ID     string `json:"id"`
	Target string `json:"target,omitempty"`
}

func storeRoot(rc *tool.RunContext) string {
	start := resolveRoot(rc, "")
	wd, err := os.Getwd()
	if err == nil {
		if abs, absErr := filepath.Abs(start); absErr == nil {
			if chErr := os.Chdir(abs); chErr == nil {
				defer os.Chdir(wd)
				if dir, scopeErr := contribStoreDir(rc); scopeErr == nil {
					return dir
				}
			}
		}
	}
	if _, statErr := os.Stat(filepath.Join(start, ".git")); statErr == nil {
		return filepath.Join(start, "docs", "kb")
	}
	if root := findRepoRoot(start); root != start {
		return filepath.Join(root, "docs", "kb")
	}
	if dir, err := contribStoreDir(rc); err == nil {
		return dir
	}
	return filepath.Join(start, "docs", "kb")
}

func writeContribJSON(rc *tool.RunContext, root string, cs []Contribution) {
	writeJSON(rc, contribEnvelope{Schema: contribSchema, Root: root, Count: len(cs), Contributions: cs})
}

func ack(rc *tool.RunContext, asJSON bool, op, id, target string) {
	if asJSON {
		writeJSON(rc, ackEnvelope{Schema: contribSchema, Op: op, ID: id, Target: target})
		return
	}
	if target != "" {
		fmt.Fprintf(rc.Out, "%s %s (%s)\n", op, target, id)
	} else {
		fmt.Fprintf(rc.Out, "%s (%s)\n", op, id)
	}
}

// --- graph note ---

func runNote(rc *tool.RunContext, args []string) int {
	asJSON := weavecli.IsAgent()
	confidence, source := "ASSERTED", ""
	var target string
	var textParts []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json" || a == "--json=true":
			asJSON = true
		case a == "--json=false" || a == "--plain":
			asJSON = false
		case a == "--confidence":
			if i+1 < len(args) {
				i++
				confidence = args[i]
			}
		case strings.HasPrefix(a, "--confidence="):
			confidence = a[len("--confidence="):]
		case a == "--source":
			if i+1 < len(args) {
				i++
				source = args[i]
			}
		case strings.HasPrefix(a, "--source="):
			source = a[len("--source="):]
		case strings.HasPrefix(a, "-") && a != "-":
			return usageErr(rc, "graph note", "unknown option "+a)
		default:
			if target == "" {
				target = a
			} else {
				textParts = append(textParts, a)
			}
		}
	}
	if target == "" || len(textParts) == 0 {
		return usageErr(rc, "graph note", "usage: graph note <target> <text>")
	}
	text := strings.Join(textParts, " ")
	c := Contribution{
		ID: contribID("note", target, text), Op: "note", By: contribBy(rc),
		At: time.Now().UTC(), Source: source, Confidence: confidence,
		Episode: contribEpisode(rc), Target: target, TargetID: entityID(target), Text: text,
	}
	root := storeRoot(rc)
	if err := openStore(root).append(c); err != nil {
		fmt.Fprintf(rc.Err, "graph note: %v\n", err)
		return 1
	}
	ack(rc, asJSON, "note", c.ID, target)
	return 0
}

// --- graph link ---

func runLink(rc *tool.RunContext, args []string) int {
	asJSON := weavecli.IsAgent()
	var pos []string
	for _, a := range args {
		switch {
		case a == "--json" || a == "--json=true":
			asJSON = true
		case a == "--json=false" || a == "--plain":
			asJSON = false
		case strings.HasPrefix(a, "-") && a != "-":
			return usageErr(rc, "graph link", "unknown option "+a)
		default:
			pos = append(pos, a)
		}
	}
	if len(pos) != 3 {
		return usageErr(rc, "graph link", "usage: graph link <src> <relation> <dst>")
	}
	src, relation, dst := pos[0], pos[1], pos[2]
	c := Contribution{
		ID: contribID("link", src, relation, dst), Op: "link", By: contribBy(rc),
		At: time.Now().UTC(), Episode: contribEpisode(rc),
		Target: src, TargetID: entityID(src), Relation: relation, Dst: dst, DstID: entityID(dst),
	}
	root := storeRoot(rc)
	if err := openStore(root).append(c); err != nil {
		fmt.Fprintf(rc.Err, "graph link: %v\n", err)
		return 1
	}
	if asJSON {
		writeJSON(rc, ackEnvelope{Schema: contribSchema, Op: "link", ID: c.ID, Target: src})
		return 0
	}
	fmt.Fprintf(rc.Out, "link %s -%s-> %s (%s)\n", src, relation, dst, c.ID)
	return 0
}

// --- graph observe ---

func runObserve(rc *tool.RunContext, args []string) int {
	asJSON := weavecli.IsAgent()
	outcome, summary := "", ""
	data := map[string]string{}
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json" || a == "--json=true":
			asJSON = true
		case a == "--json=false" || a == "--plain":
			asJSON = false
		case a == "--outcome":
			if i+1 < len(args) {
				i++
				outcome = args[i]
			}
		case strings.HasPrefix(a, "--outcome="):
			outcome = a[len("--outcome="):]
		case a == "--summary":
			if i+1 < len(args) {
				i++
				summary = args[i]
			}
		case strings.HasPrefix(a, "--summary="):
			summary = a[len("--summary="):]
		case a == "--data":
			if i+1 < len(args) {
				i++
				addKV(data, args[i])
			}
		case strings.HasPrefix(a, "--data="):
			addKV(data, a[len("--data="):])
		case strings.HasPrefix(a, "-") && a != "-":
			return usageErr(rc, "graph observe", "unknown option "+a)
		default:
			pos = append(pos, a)
		}
	}
	if len(pos) < 2 {
		return usageErr(rc, "graph observe", "usage: graph observe <kind> <target> [--outcome O] [--summary S]")
	}
	kind, target := pos[0], pos[1]
	if outcome == "" {
		outcome = "note"
	}
	now := time.Now().UTC()
	if len(data) == 0 {
		data = nil
	}
	c := Contribution{
		ID: contribID("observe", kind, target, outcome, summary, now.Format(time.RFC3339Nano)),
		Op: "observe", By: contribBy(rc), At: now, Episode: contribEpisode(rc),
		Target: target, TargetID: entityID(target), Text: summary, Kind: kind, Outcome: outcome, Data: data,
	}
	root := storeRoot(rc)
	if err := openStore(root).append(c); err != nil {
		fmt.Fprintf(rc.Err, "graph observe: %v\n", err)
		return 1
	}
	ack(rc, asJSON, "observe", c.ID, target)
	return 0
}

func addKV(m map[string]string, kv string) {
	if i := strings.IndexByte(kv, '='); i > 0 {
		m[kv[:i]] = kv[i+1:]
	}
}

// --- graph forget ---

func runForget(rc *tool.RunContext, args []string) int {
	asJSON := weavecli.IsAgent()
	var id, target, episode string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json" || a == "--json=true":
			asJSON = true
		case a == "--json=false" || a == "--plain":
			asJSON = false
		case a == "--target":
			if i+1 < len(args) {
				i++
				target = args[i]
			}
		case strings.HasPrefix(a, "--target="):
			target = a[len("--target="):]
		case a == "--episode":
			if i+1 < len(args) {
				i++
				episode = args[i]
			}
		case strings.HasPrefix(a, "--episode="):
			episode = a[len("--episode="):]
		case strings.HasPrefix(a, "-") && a != "-":
			return usageErr(rc, "graph forget", "unknown option "+a)
		default:
			if id == "" {
				id = a
			}
		}
	}
	if id == "" && target == "" && episode == "" {
		return usageErr(rc, "graph forget", "usage: graph forget <id> | --target X | --episode E")
	}
	c := Contribution{
		ID: contribID("forget", id, target, episode, time.Now().UTC().Format(time.RFC3339Nano)),
		Op: "forget", By: contribBy(rc), At: time.Now().UTC(),
		ForgetID: id, ForgetTarget: target, ForgetEpisode: episode,
	}
	root := storeRoot(rc)
	if err := openStore(root).append(c); err != nil {
		fmt.Fprintf(rc.Err, "graph forget: %v\n", err)
		return 1
	}
	sel := id
	if sel == "" {
		sel = strings.TrimSpace(target + " " + episode)
	}
	ack(rc, asJSON, "forget", c.ID, sel)
	return 0
}

// --- reads ---

func runRecall(rc *tool.RunContext, args []string) int {
	asJSON := weavecli.IsAgent()
	var query, fTarget, fKind, fOutcome string
	limit := 20
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json" || a == "--json=true":
			asJSON = true
		case a == "--json=false" || a == "--plain":
			asJSON = false
		case a == "--target":
			if i+1 < len(args) {
				i++
				fTarget = args[i]
			}
		case strings.HasPrefix(a, "--target="):
			fTarget = a[len("--target="):]
		case a == "--kind":
			if i+1 < len(args) {
				i++
				fKind = args[i]
			}
		case strings.HasPrefix(a, "--kind="):
			fKind = a[len("--kind="):]
		case a == "--outcome":
			if i+1 < len(args) {
				i++
				fOutcome = args[i]
			}
		case strings.HasPrefix(a, "--outcome="):
			fOutcome = a[len("--outcome="):]
		case a == "--limit":
			if i+1 < len(args) {
				i++
				limit = atoiDefault(args[i], limit)
			}
		case strings.HasPrefix(a, "--limit="):
			limit = atoiDefault(a[len("--limit="):], limit)
		case strings.HasPrefix(a, "-") && a != "-":
			return usageErr(rc, "graph recall", "unknown option "+a)
		default:
			if query == "" {
				query = a
			}
		}
	}
	root := storeRoot(rc)
	live, err := openStore(root).live()
	if err != nil {
		fmt.Fprintf(rc.Err, "graph recall: %v\n", err)
		return 1
	}
	q := strings.ToLower(query)
	var hits []Contribution
	for _, c := range live {
		if fTarget != "" && c.Target != fTarget {
			continue
		}
		if fKind != "" && c.Kind != fKind {
			continue
		}
		if fOutcome != "" && c.Outcome != fOutcome {
			continue
		}
		if q != "" && !contribMatches(c, q) {
			continue
		}
		hits = append(hits, c)
	}
	// Most recent first.
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].At.After(hits[j].At) })
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	return renderContribs(rc, root, hits, asJSON, "(no contributions)")
}

func runNotesFor(rc *tool.RunContext, args []string) int {
	asJSON := weavecli.IsAgent()
	var target string
	for _, a := range args {
		switch {
		case a == "--json" || a == "--json=true":
			asJSON = true
		case a == "--json=false" || a == "--plain":
			asJSON = false
		case strings.HasPrefix(a, "-") && a != "-":
			return usageErr(rc, "graph notes", "unknown option "+a)
		default:
			if target == "" {
				target = a
			}
		}
	}
	if target == "" {
		return usageErr(rc, "graph notes", "usage: graph notes <target>")
	}
	root := storeRoot(rc)
	live, err := openStore(root).live()
	if err != nil {
		fmt.Fprintf(rc.Err, "graph notes: %v\n", err)
		return 1
	}
	var hits []Contribution
	for _, c := range live {
		// Everything about the target: notes/observes on it, links from OR to it.
		if c.Target == target || c.Dst == target {
			hits = append(hits, c)
		}
	}
	return renderContribs(rc, root, hits, asJSON, "(nothing recorded about "+target+")")
}

func runPitfalls(rc *tool.RunContext, args []string) int {
	asJSON := weavecli.IsAgent()
	var target string
	for _, a := range args {
		switch {
		case a == "--json" || a == "--json=true":
			asJSON = true
		case a == "--json=false" || a == "--plain":
			asJSON = false
		case strings.HasPrefix(a, "-") && a != "-":
			return usageErr(rc, "graph pitfalls", "unknown option "+a)
		default:
			if target == "" {
				target = a
			}
		}
	}
	if target == "" {
		return usageErr(rc, "graph pitfalls", "usage: graph pitfalls <target>")
	}
	root := storeRoot(rc)
	live, err := openStore(root).live()
	if err != nil {
		fmt.Fprintf(rc.Err, "graph pitfalls: %v\n", err)
		return 1
	}
	var hits []Contribution
	for _, c := range live {
		if c.Op == "observe" && c.Target == target && c.Outcome == "failure" {
			hits = append(hits, c)
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].At.After(hits[j].At) })

	// Authored contributions first, then what the corpus actually observed.
	//
	// They are kept SEPARATE rather than merged into one ranked list. An
	// authored note is somebody's judgement; a derived pitfall is a count of
	// what happened. Sorting them together would imply the two are comparable
	// evidence, and a reader could not tell which kind they were acting on.
	derived := derivedPitfalls(target)

	if asJSON {
		if hits == nil {
			hits = []Contribution{}
		}
		// Additive: `contributions`/`count` keep their existing meaning, so a
		// consumer written against the old envelope reads the same authored
		// notes it always did. `observed` is a new key it can ignore.
		writeJSON(rc, contribEnvelope{
			Schema: contribSchema, Root: root, Count: len(hits),
			Contributions: hits, Observed: derived,
		})
		return 0
	}
	if len(hits) == 0 && len(derived) == 0 {
		fmt.Fprintln(rc.Err, pitfallsEmptyNote(target, len(live)))
		return 0
	}
	for _, c := range hits {
		fmt.Fprintln(rc.Out, formatContrib(c))
	}
	for _, p := range derived {
		fmt.Fprintln(rc.Out, formatPitfall(p))
	}
	return 0
}

// pitfallsEmptyNote refuses to answer an empty search with a claim about the
// world.
//
// "(no known failures for X)" is a statement about X. What the tool actually
// knows is a statement about its own corpus, and the two are only the same if
// the corpus is complete — which it is not. The reader cannot tell "nothing
// ever failed" from "nobody wrote one down" or "the recorder is off" unless the
// tool says which, and reporting the first when the truth is the third is how a
// system reaches a confident conclusion from the ABSENCE of evidence.
func pitfallsEmptyNote(target string, corpus int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "no recorded failures for %s\n", target)
	fmt.Fprintf(&b, "  authored: %d contributions in this repo's wiki\n", corpus)

	// Both halves of the search report themselves. The authored side can be
	// empty because nobody wrote anything down; the observed side can be empty
	// because nothing failed, because recording was off, or because the days
	// that held the evidence were pruned. Collapsing those into one blank
	// answer is the absence-of-evidence failure this verb used to commit.
	_, cov, err := execlog.Promote(execStoreRoot(), execlog.PromoteDefaults())
	switch {
	case err != nil:
		fmt.Fprintf(&b, "  observed: unavailable (%v)\n", err)
	default:
		fmt.Fprintf(&b, "  observed: %d commands recorded over %d days; recording: %s\n",
			cov.Records, cov.Days, onOff(cov.Recording || recordingOn()))
		if cov.Pruned > 0 {
			fmt.Fprintf(&b, "            %d records were deleted by retention\n", cov.Pruned)
		}
		if cov.Lost > 0 {
			fmt.Fprintf(&b, "            %d records were lost unflushed\n", cov.Lost)
		}
	}

	d := execlog.PromoteDefaults()
	fmt.Fprintf(&b, "  bar: a failure promotes at %d sessions across %d days with no later success",
		d.MinEpisodes, d.MinDays)
	return b.String()
}

func onOff(b bool) string {
	if b {
		return "ON"
	}
	return "OFF"
}

func contribMatches(c Contribution, lowerQuery string) bool {
	for _, f := range []string{c.Target, c.Text, c.Relation, c.Dst, c.Kind, c.Outcome} {
		if f != "" && strings.Contains(strings.ToLower(f), lowerQuery) {
			return true
		}
	}
	return false
}

func renderContribs(rc *tool.RunContext, root string, hits []Contribution, asJSON bool, empty string) int {
	if asJSON {
		if hits == nil {
			hits = []Contribution{}
		}
		writeContribJSON(rc, root, hits)
		return 0
	}
	if len(hits) == 0 {
		fmt.Fprintln(rc.Err, empty)
		return 0
	}
	for _, c := range hits {
		fmt.Fprintln(rc.Out, formatContrib(c))
	}
	return 0
}

func formatContrib(c Contribution) string {
	switch c.Op {
	case "link":
		return fmt.Sprintf("link  %s -%s-> %s  [%s]", c.Target, c.Relation, c.Dst, c.ID)
	case "observe":
		txt := c.Text
		if txt != "" {
			txt = ": " + txt
		}
		return fmt.Sprintf("observe  %s %s [%s]%s  [%s]", c.Kind, c.Target, c.Outcome, txt, c.ID)
	default: // note
		by := c.By
		if by != "" {
			by = " by " + by
		}
		return fmt.Sprintf("note  %s: %s  [%s%s %s]", c.Target, c.Text, c.Confidence, by, c.ID)
	}
}
