// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

// The execution layer's read surface: the agentic history, and the entity
// graph the host has learned from it.
//
// These live under `graph` rather than as top-level verbs because they are not
// a separate thing — they are the execution SUBGRAPH of the same knowledge
// graph the code and wiki layers already sit in, sharing its id space and its
// append-only-log → derived-view shape.
package graphcmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/coreutils/pkg/weavecli"
	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/craft"
	"github.com/qiangli/yoke/pkg/execlog"
	"github.com/qiangli/yoke/pkg/kb"
	"github.com/qiangli/yoke/pkg/skills"
	"github.com/qiangli/yoke/pkg/spacegraph"
)

const (
	historySchema = "bashy-history-v1"
	spaceSchema   = "bashy-space-v1"
)

func init() {
	addSub("history", "the agentic command history: every command, in order, with its outcome",
		"graph history [--episode E] [--cmd C] [--since D] [--failed] [--limit N] [--json]\n"+
			"               [--graph]  what usually follows what, and what fixes what\n"+
			"               [--forget --before D | --forget --episode E]", runHistory)
	addSub("space", "what this host has learned about its environment (entities)",
		"graph space [--kind host|endpoint|account|repo|path|net] [--json]", runSpace)
	addSub("reached", "which endpoints this host has reached, as whom, and when",
		"graph reached [--json]", runReached)
	addSub("learn", "turn observed failures into kb candidate pages (the durable claim)",
		"graph learn [--dry-run] [--json]", runLearn)
	addSub("evidence", "resolve a kb page's evidence pointer back to the raw records",
		"graph evidence <exec://...> [--json]", runEvidence)
}

// ---------------------------------------------------------------------------
// promote / evidence — the pipe from the stream to the store of record
// ---------------------------------------------------------------------------

// runLearn writes what the corpus supports into kb, as CANDIDATES.
//
// Named `learn` rather than `promote` on purpose. The other rung of this ladder
// — moving a page from candidate to validated, which requires evidence and human
// judgement — is `kb validate`, and its help text calls that act "promote". Two
// verbs both meaning promote, doing two different rungs of the same ladder, is a
// footgun in a surface an agent drives. `learn` also matches `skills learn`,
// which absorbs what an invocation taught in exactly the same sense.
//
// kb is the entry point: the page holds the claim, and an address that indexes
// back into the stream rather than a copy of it. This verb is the only thing
// that makes an observation durable — before it, `graph pitfalls` recomputed on
// every read and nothing was ever written down.
func runLearn(rc *tool.RunContext, args []string) int {
	asJSON := weavecli.IsAgent()
	dry := false
	for _, a := range args {
		switch {
		case a == "--json" || a == "--json=true":
			asJSON = true
		case a == "--json=false" || a == "--plain":
			asJSON = false
		case a == "--dry-run" || a == "-n":
			dry = true
		case strings.HasPrefix(a, "-") && a != "-":
			return usageErr(rc, "graph learn", "unknown option "+a)
		}
	}

	root := execStoreRoot()
	if dry {
		pitfalls, cov, err := execlog.Promote(root, execlog.PromoteDefaults())
		if err != nil {
			fmt.Fprintf(rc.Err, "graph learn: %v\n", err)
			return 1
		}
		cov.Recording = recordingOn()
		if asJSON {
			if pitfalls == nil {
				pitfalls = []execlog.Pitfall{}
			}
			return emitJSON(rc, map[string]any{
				"schema": historySchema, "would_promote": pitfalls, "coverage": cov,
			})
		}
		for _, p := range pitfalls {
			fmt.Fprintln(rc.Out, formatPitfall(p))
		}
		if len(pitfalls) == 0 {
			fmt.Fprintln(rc.Err, "nothing meets the bar yet")
		}
		fmt.Fprintln(rc.Err, formatCoverage(cov))
		return 0
	}

	store := kb.Open(kb.DefaultDir())
	res, err := execlog.PromoteToKB(root, store, execlog.PromoteDefaults())
	if err != nil {
		fmt.Fprintf(rc.Err, "graph learn: %v\n", err)
		return 1
	}
	res.Recording = recordingOn()

	if asJSON {
		return emitJSON(rc, map[string]any{"schema": historySchema, "result": res})
	}
	for _, s := range res.Written {
		fmt.Fprintf(rc.Out, "wrote    %s\n", s)
	}
	for _, s := range res.Updated {
		fmt.Fprintf(rc.Out, "updated  %s\n", s)
	}
	for _, s := range res.Skipped {
		// Not silent. A skip means a human took the page over, and a pipe that
		// swallowed that would look identical to one that had nothing to do.
		fmt.Fprintf(rc.Out, "skipped  %s (no longer a candidate — someone owns it)\n", s)
	}
	if len(res.Written)+len(res.Updated)+len(res.Skipped) == 0 {
		fmt.Fprintln(rc.Err, "nothing meets the bar yet")
	}
	fmt.Fprintln(rc.Err, formatCoverage(res.Coverage))
	return 0
}

// runEvidence resolves an address back to the records behind a claim.
//
// This is the drill-down that makes the pointer real: a page cites an address,
// and this is how you check it. When the records have aged out it says so —
// the claim survives its evidence, and a reader is entitled to know when it has.
func runEvidence(rc *tool.RunContext, args []string) int {
	asJSON := weavecli.IsAgent()
	var refStr string
	for _, a := range args {
		switch {
		case a == "--json" || a == "--json=true":
			asJSON = true
		case a == "--json=false" || a == "--plain":
			asJSON = false
		case strings.HasPrefix(a, "-") && a != "-":
			return usageErr(rc, "graph evidence", "unknown option "+a)
		default:
			if refStr == "" {
				refStr = a
			}
		}
	}
	if refStr == "" {
		return usageErr(rc, "graph evidence", "usage: graph evidence <exec://...>")
	}

	ref, err := execlog.ParseEvidence(refStr)
	if err != nil {
		fmt.Fprintf(rc.Err, "graph evidence: %v\n", err)
		return 2
	}

	recs, cov, err := ref.Resolve(execStoreRoot())
	cov.Recording = recordingOn()
	if err != nil {
		if errors.Is(err, execlog.ErrPruned) {
			// Exit 0: this is not a malfunction. The stream has a retention
			// policy, the claim outlived it, and saying so IS the answer.
			if asJSON {
				return emitJSON(rc, map[string]any{
					"schema": historySchema, "ref": refStr,
					"pruned": true, "coverage": cov,
				})
			}
			fmt.Fprintf(rc.Out, "evidence pruned — the claim stands, its records do not\n  %s\n", refStr)
			fmt.Fprintln(rc.Err, formatCoverage(cov))
			return 0
		}
		fmt.Fprintf(rc.Err, "graph evidence: %v\n", err)
		return 1
	}

	if asJSON {
		return emitJSON(rc, map[string]any{
			"schema": historySchema, "ref": refStr,
			"records": recs, "coverage": cov,
		})
	}
	for _, r := range recs {
		fmt.Fprintln(rc.Out, formatRecord(r))
	}
	fmt.Fprintln(rc.Err, formatCoverage(cov))
	return 0
}

// ---------------------------------------------------------------------------
// history
// ---------------------------------------------------------------------------

func runHistory(rc *tool.RunContext, args []string) int {
	asJSON := weavecli.IsAgent()
	var (
		q       execlog.Query
		forget  bool
		asGraph bool
		before  string
	)
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch {
		case a == "--json" || a == "--json=true":
			asJSON = true
		case a == "--json=false" || a == "--plain":
			asJSON = false
		case a == "--failed":
			q.Failed = true
		case a == "--forget":
			forget = true
		case a == "--graph":
			asGraph = true
		case a == "--episode":
			q.Episode = next()
		case a == "--cmd":
			q.Cmd = next()
		case a == "--before":
			before = next()
		case a == "--since":
			d, err := time.ParseDuration(next())
			if err != nil {
				return usageErr(rc, "graph history", "bad --since duration")
			}
			q.Since = time.Now().UTC().Add(-d)
		case a == "--limit":
			n, err := strconv.Atoi(next())
			if err != nil {
				return usageErr(rc, "graph history", "bad --limit")
			}
			q.Limit = n
		case strings.HasPrefix(a, "-") && a != "-":
			return usageErr(rc, "graph history", "unknown option "+a)
		}
	}

	root := execStoreRoot()
	if forget {
		return runHistoryForget(rc, root, q.Episode, before, asJSON)
	}
	if asGraph {
		return runHistoryGraph(rc, root, q, asJSON)
	}

	recs, cov, err := execlog.Read(root, q)
	if err != nil {
		fmt.Fprintf(rc.Err, "graph history: %v\n", err)
		return 1
	}
	cov.Recording = recordingOn()

	if asJSON {
		return emitJSON(rc, map[string]any{
			"schema": historySchema, "records": recs, "coverage": cov,
		})
	}
	for _, r := range recs {
		fmt.Fprintln(rc.Out, formatRecord(r))
	}
	// The coverage block goes to stderr ALWAYS, not only when the result is
	// empty. An answer drawn from a corpus is only as good as the corpus, and
	// the reader cannot judge that from the rows alone.
	fmt.Fprintln(rc.Err, formatCoverage(cov))
	return 0
}

// runHistoryGraph renders the derived `then` edges.
//
// Recoveries lead, because they are the only transitions worth more than their
// frequency: "when X breaks, Y fixes it" is actionable, while "cd is usually
// followed by ls" is a transcript.
func runHistoryGraph(rc *tool.RunContext, root string, q execlog.Query, asJSON bool) int {
	ts, cov, err := execlog.Transitions(root, q)
	if err != nil {
		fmt.Fprintf(rc.Err, "graph history --graph: %v\n", err)
		return 1
	}
	cov.Recording = recordingOn()

	if asJSON {
		if ts == nil {
			ts = []execlog.Transition{}
		}
		return emitJSON(rc, map[string]any{
			"schema": historySchema, "transitions": ts, "coverage": cov,
		})
	}

	var fixes, plain []execlog.Transition
	for _, t := range ts {
		if t.Recovered > 0 {
			fixes = append(fixes, t)
		} else {
			plain = append(plain, t)
		}
	}
	if len(fixes) > 0 {
		fmt.Fprintln(rc.Out, "recoveries (a failure, then something that worked):")
		for _, t := range fixes {
			fmt.Fprintf(rc.Out, "  %s\n    then %s   recovered %d/%d\n",
				t.Src, t.Dst, t.Recovered, t.N)
		}
	}
	if len(plain) > 0 {
		if len(fixes) > 0 {
			fmt.Fprintln(rc.Out)
		}
		fmt.Fprintln(rc.Out, "sequences:")
		for _, t := range plain {
			fmt.Fprintf(rc.Out, "  %-40s then %-40s n=%d ok=%d\n", t.Src, t.Dst, t.N, t.OK)
		}
	}
	fmt.Fprintln(rc.Err, formatCoverage(cov))
	return 0
}

func runHistoryForget(rc *tool.RunContext, root, episode, before string, asJSON bool) int {
	var (
		stones []execlog.Tombstone
		err    error
	)
	switch {
	case episode != "":
		stones, err = execlog.PruneEpisode(root, episode, time.Now().UTC())
	case before != "":
		d, perr := time.ParseDuration(before)
		if perr != nil {
			return usageErr(rc, "graph history --forget", "bad --before duration")
		}
		stones, err = execlog.Prune(root, execlog.PruneOpts{Before: time.Now().UTC().Add(-d)})
	default:
		return usageErr(rc, "graph history --forget",
			"usage: graph history --forget (--episode E | --before DURATION)")
	}
	if err != nil {
		// Reported, never swallowed. A prune that quietly did nothing while
		// claiming to have bounded the store is the trap it exists to close.
		fmt.Fprintf(rc.Err, "graph history --forget: %v\n", err)
		return 1
	}
	if asJSON {
		if stones == nil {
			stones = []execlog.Tombstone{}
		}
		return emitJSON(rc, map[string]any{"schema": historySchema, "pruned": stones})
	}
	total := 0
	for _, s := range stones {
		total += s.Records
		fmt.Fprintf(rc.Out, "pruned %s: %d records, %d bytes (%s)\n",
			s.Day, s.Records, s.Bytes, s.Reason)
	}
	if total == 0 {
		fmt.Fprintln(rc.Err, "nothing pruned (today's records are never removed — a live writer holds them)")
	}
	return 0
}

func formatRecord(r execlog.Record) string {
	exit := "  ?"
	switch {
	case !r.Observed:
		exit = "  ~" // ran-unobserved, never rendered as a success
	case r.Exit != nil && *r.Exit == 0:
		exit = "  ."
	case r.Exit != nil:
		exit = fmt.Sprintf("%3d", *r.Exit)
	}
	extra := ""
	if r.Opaque {
		extra = "  [OPAQUE — children not observed]"
	}
	if r.Truncated {
		extra += "  [argv truncated]"
	}
	return fmt.Sprintf("%s %s %6dms  %s%s",
		r.At.Format("01-02 15:04:05"), exit, r.DurationMs, r.Template, extra)
}

// formatCoverage states what the corpus can and cannot answer.
//
// This is the difference between "no failures found" and "the evidence was
// deleted last Tuesday", and without it those two read identically.
func formatCoverage(c execlog.Coverage) string {
	var b strings.Builder
	rec := "OFF"
	if c.Recording {
		rec = "ON"
	}
	fmt.Fprintf(&b, "corpus: %d records", c.Records)
	if !c.From.IsZero() {
		fmt.Fprintf(&b, ", %s .. %s", c.From.Format("2006-01-02"), c.To.Format("2006-01-02"))
	}
	fmt.Fprintf(&b, " (%d days); recording: %s", c.Days, rec)
	if c.Pruned > 0 {
		fmt.Fprintf(&b, "\npruned: %d records deleted by retention", c.Pruned)
	}
	if c.Lost > 0 {
		fmt.Fprintf(&b, "\nlost:   %d records stamped but never flushed (process died)", c.Lost)
	}
	if c.Malformed > 0 {
		fmt.Fprintf(&b, "\nmalformed: %d unreadable lines skipped", c.Malformed)
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// space / reached
// ---------------------------------------------------------------------------

func runSpace(rc *tool.RunContext, args []string) int {
	asJSON := weavecli.IsAgent()
	kind := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json" || a == "--json=true":
			asJSON = true
		case a == "--json=false" || a == "--plain":
			asJSON = false
		case a == "--kind":
			if i+1 < len(args) {
				i++
				kind = args[i]
			}
		case strings.HasPrefix(a, "-") && a != "-":
			return usageErr(rc, "graph space", "unknown option "+a)
		}
	}

	s := spacegraph.Open(spaceStoreDir())
	now := time.Now().UTC()
	nodes, err := s.Nodes(now)
	if err != nil {
		fmt.Fprintf(rc.Err, "graph space: %v\n", err)
		return 1
	}
	if kind != "" {
		var keep []spacegraph.Node
		for _, n := range nodes {
			if string(n.Kind) == kind {
				keep = append(keep, n)
			}
		}
		nodes = keep
	}

	if asJSON {
		if nodes == nil {
			nodes = []spacegraph.Node{}
		}
		return emitJSON(rc, map[string]any{
			"schema": spaceSchema, "nodes": nodes, "malformed": s.Malformed(),
		})
	}
	if len(nodes) == 0 {
		fmt.Fprintln(rc.Err, spaceEmptyNote(s))
		return 0
	}
	for _, n := range nodes {
		fmt.Fprintf(rc.Out, "%-34s %-9s out=%-3d in=%-3d  last=%s\n",
			n.ID, n.Kind, n.Out, n.In, n.Last.Format("2006-01-02"))
	}
	return 0
}

func runReached(rc *tool.RunContext, args []string) int {
	asJSON := weavecli.IsAgent()
	for _, a := range args {
		switch {
		case a == "--json" || a == "--json=true":
			asJSON = true
		case a == "--json=false" || a == "--plain":
			asJSON = false
		case strings.HasPrefix(a, "-") && a != "-":
			return usageErr(rc, "graph reached", "unknown option "+a)
		}
	}

	s := spacegraph.Open(spaceStoreDir())
	now := time.Now().UTC()
	all, err := s.Edges(now)
	if err != nil {
		fmt.Fprintf(rc.Err, "graph reached: %v\n", err)
		return 1
	}
	var hits []spacegraph.Edge
	for _, e := range all {
		if e.Rel == spacegraph.RelReached {
			hits = append(hits, e)
		}
	}

	if asJSON {
		if hits == nil {
			hits = []spacegraph.Edge{}
		}
		return emitJSON(rc, map[string]any{
			"schema": spaceSchema, "edges": hits, "malformed": s.Malformed(),
		})
	}
	if len(hits) == 0 {
		fmt.Fprintln(rc.Err, spaceEmptyNote(s))
		return 0
	}
	for _, e := range hits {
		via := ""
		if e.Via != "" {
			via = "  as " + strings.TrimPrefix(e.Via, string(craft.EntityAccount)+":")
		}
		// n and ok are shown SEPARATELY. An endpoint reached 40 times and
		// working twice is a different claim from one reached twice and working
		// twice, and a single success rate hides exactly that.
		fmt.Fprintf(rc.Out, "%-30s  n=%-3d ok=%-3d%s\n    first=%s  last=%s\n",
			strings.TrimPrefix(e.Dst, string(craft.EntityEndpoint)+":"),
			e.N, e.OK, via,
			e.First.Format("2006-01-02 15:04"), e.Last.Format("2006-01-02 15:04"))
	}
	return 0
}

// spaceEmptyNote refuses to let an empty graph read as "nothing is reachable".
//
// An empty result has at least three causes — nothing was recorded, the
// recorder is off, or the store is damaged — and they are indistinguishable
// unless the tool says which.
func spaceEmptyNote(s *spacegraph.Store) string {
	var b strings.Builder
	b.WriteString("no entities recorded")
	if !recordingOn() {
		b.WriteString(" — recording is OFF (set BASHY_EXECHIST=1)")
	} else {
		b.WriteString(" — recording is ON; nothing has been learned yet")
	}
	if n := s.Malformed(); n > 0 {
		fmt.Fprintf(&b, "\n%d unreadable lines were skipped in %s", n, s.Path())
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// derived pitfalls — what the corpus observed, as opposed to what a human typed
// ---------------------------------------------------------------------------

// derivedPitfalls folds the execution corpus into the claims its evidence
// supports, for one target.
//
// The target matches a COMMAND NAME or a template substring, because that is
// how the question actually arrives: an agent about to run `go test` asks about
// "go test", not about the exact canonical template it is going to produce.
//
// Errors are swallowed and yield nothing. That is safe here only because the
// caller renders a coverage note when both halves are empty — otherwise this
// would be the absence-of-evidence failure with extra steps.
func derivedPitfalls(target string) []execlog.Pitfall {
	all, _, err := execlog.Promote(execStoreRoot(), execlog.PromoteDefaults())
	if err != nil {
		return nil
	}
	t := strings.ToLower(strings.TrimSpace(target))
	var out []execlog.Pitfall
	for _, p := range all {
		if strings.EqualFold(p.Cmd, t) || strings.Contains(strings.ToLower(p.Template), t) {
			out = append(out, p)
		}
	}
	return out
}

// formatPitfall renders a derived claim WITH its evidence.
//
// The counts are not decoration. A claim backed by three episodes over two days
// and one backed by nine over six are different claims, and a reader who cannot
// see which is which has to trust the threshold instead of the evidence.
func formatPitfall(p execlog.Pitfall) string {
	var b strings.Builder
	fmt.Fprintf(&b, "observed  %s", p.Template)
	if p.Dimension != "" {
		fmt.Fprintf(&b, "  [%s]", p.Dimension)
	}
	fmt.Fprintf(&b, "\n          %d failures across %d sessions on %d days",
		p.Failures, p.Episodes, p.Days)
	if p.ExitClass != "" && p.ExitClass != "generic" {
		fmt.Fprintf(&b, ", %s", p.ExitClass)
	}
	fmt.Fprintf(&b, "\n          last %s", p.LastSeen.Format("2006-01-02 15:04"))
	if !p.LastSuccess.IsZero() {
		// Say when it last worked. A pitfall about a command that used to work
		// is a different problem from one that never has, and hiding the
		// success would make a regression look like a permanent property.
		fmt.Fprintf(&b, "; last worked %s (%d successes)",
			p.LastSuccess.Format("2006-01-02"), p.Successes)
	} else {
		b.WriteString("; never observed to work here")
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// shared
// ---------------------------------------------------------------------------

// recordingOn mirrors the middleware's gate so a read verb can say whether the
// silence it is reporting means "nothing happened" or "nothing was watching".
func recordingOn() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("BASHY_AGENTIC"))) {
	case "0", "false", "off", "no":
		return false
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("BASHY_EXECHIST"))) {
	case "0", "false", "off", "no":
		return false
	case "1", "true", "on", "yes":
		return true
	}
	return weavecli.IsAgentDriven()
}

// execStoreRoot is the exec history store, resolved by the package that
// owns it — the same ladder the writer uses.
func execStoreRoot() string { return execlog.DefaultRoot() }

// spaceStoreDir is the craft store, which is where the fact half of what a
// host knows already lives. The edges belong beside the facts because they are
// the same KIND of claim — identity-bearing, host-local, and with no export
// path by construction.
//
// Resolved by the catalog package, because that is where the WRITER
// (bashy's exec-history middleware) puts it. This function used to carry its
// own ladder ending in ~/.bashy/skills — a directory nothing ever wrote — so
// `graph space` and `graph reached` read an empty store on every host.
func spaceStoreDir() string { return skills.DefaultStoreDir() }

func emitJSON(rc *tool.RunContext, v any) int {
	enc := json.NewEncoder(rc.Out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(rc.Err, "graph: %v\n", err)
		return 1
	}
	return 0
}
