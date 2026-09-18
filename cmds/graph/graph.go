// Package graphcmd registers the AgentOS code-knowledge-graph feature as a
// single `graph` command with subcommands — `graph build`, `graph stats`,
// `graph neighbors`, `graph impact`, `graph path`, `graph hotspots`,
// `graph query` (the structural, gfy-backed code-graph reads over
// coreutils/pkg/codegraph) plus the contribution subcommands `graph note`,
// `graph link`, `graph observe`, `graph forget`, `graph recall`,
// `graph notes`, `graph pitfalls` (the durable per-repo "agentic wiki").
// One verb, grouped subcommands (see dhnt/docs/bashy-code-graph-agentic-feature.md).
//
// This package is deliberately NOT blank-imported by cmds/all: it pulls in gfy's
// document-parsing dependency graph, which must stay out of the bare coreutils
// multicall binary (and out of the lean `bash` drop-in). It is imported only by
// bashy's internal/agentos, so the verb is reachable at the `bashy graph …`
// front door and in-shell via the ExecHandler, while cmd/coreutils and cmd/bash
// stay gfy-free.
//
// The engine builds a fully structural graph (tree-sitter AST extraction →
// Louvain communities → degree analytics) with NO LLM and NO graph database — a
// mid-size repo graphs in well under a second. The optional NL/DQL layers live
// behind later phases; every verb here is deterministic and model-free.
package graphcmd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	gfygraph "github.com/qiangli/gfy/pkg/graph"
	"github.com/qiangli/gfy/pkg/search"

	"github.com/qiangli/coreutils/pkg/weavecli"
	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/codegraph"
)

// schemaVersion is the stable envelope tag agents key on; graph_sha lets a
// consumer cache results and detect staleness across invocations.
const schemaVersion = "bashy-graph-v1"

// cacheRel is a bashy-OWNED cache path, deliberately separate from ycode's
// .agents/ycode/graph.json so the two never contend for the same file / lock.
const cacheRel = ".agents/bashy/graph.json"

// subcmd is one `graph <name>` subcommand.
type subcmd struct {
	name     string
	synopsis string
	usage    string
	run      func(*tool.RunContext, []string) int
}

// subcommands is the dispatch table. It is populated by this file (the
// structural code-graph reads) and by contrib_verbs.go (the contribution
// layer), and read only at Run time — so init() order across the two files
// does not matter.
var subcommands []subcmd

func addSub(name, synopsis, usage string, run func(*tool.RunContext, []string) int) {
	subcommands = append(subcommands, subcmd{name, synopsis, usage, run})
}

func init() {
	addSub("build", "build/refresh the code knowledge graph and cache it",
		"graph build [path] [--rebuild] [--json]", runBuild)
	addSub("stats", "code-graph size: nodes/edges/communities/languages",
		"graph stats [path] [--rebuild] [--json]", runStats)
	addSub("neighbors", "direct neighbors (1-hop coupling) of a symbol",
		"graph neighbors <symbol> [path] [--relation R] [--json]", runNeighbors)
	addSub("impact", "reverse dependency blast radius (who depends on a target)",
		"graph impact <file|symbol> [path] [--depth N=1] [--limit N=40] [--undirected] [--json]", runImpact)
	addSub("path", "shortest path between two symbols in the code graph",
		"graph path <a> <b> [path] [--max-hops N] [--json]", runPath)
	addSub("hotspots", "most-connected entities (refactor/blast-radius centers)",
		"graph hotspots [path] [--top N] [--raw] [--json]", runHotspots)
	addSub("query", "keyword question → matching subgraph (model-free)",
		"graph query <question> [path] [--depth N=1] [--limit N=40] [--json]", runQuery)

	tool.Register(&tool.Tool{
		Name:     "graph",
		Synopsis: "knowledge graph: code + agentic wiki + execution (subcommands: build stats neighbors impact path hotspots query · note link observe forget recall notes pitfalls · history space reached learn evidence)",
		Usage: "graph <subcommand> [args]\n\n" +
			"code graph:    build stats neighbors impact path hotspots query\n" +
			"agentic wiki:  note link observe forget recall notes pitfalls\n" +
			"execution:     history space reached learn evidence\n\n" +
			"Run `graph help` for one-line synopses, or `graph <sub> ...`.",
		Run: runGraph,
	})
}

// runGraph dispatches `graph <subcommand>` to the matching subcommand. Both
// the `bashy graph …` front door and the in-shell ExecHandler call this with
// the raw argv tail, so subcommands keep parsing their own flags exactly as
// the former flat verbs did.
func runGraph(rc *tool.RunContext, args []string) int {
	if len(args) == 0 {
		printGraphHelp(rc)
		return 0
	}
	switch args[0] {
	case "help", "-h", "--help":
		printGraphHelp(rc)
		return 0
	}
	for _, sc := range subcommands {
		if sc.name == args[0] {
			return sc.run(rc, args[1:])
		}
	}
	fmt.Fprintf(rc.Err, "graph: unknown subcommand %q (run `graph help`)\n", args[0])
	return 2
}

// helpSections is the canonical help layout: the two layers in their
// documented order (code-graph reads, then the agentic-wiki contribution
// verbs), independent of init() append order across the two source files.
var helpSections = []struct {
	title string
	names []string
}{
	{"code graph", []string{"build", "stats", "neighbors", "impact", "path", "hotspots", "query"}},
	{"agentic wiki", []string{"note", "link", "observe", "forget", "recall", "notes", "pitfalls"}},
	{"execution", []string{"history", "space", "reached", "learn", "evidence"}},
}

func printGraphHelp(rc *tool.RunContext) {
	byName := make(map[string]subcmd, len(subcommands))
	for _, sc := range subcommands {
		byName[sc.name] = sc
	}
	fmt.Fprintln(rc.Out, "Usage: graph <subcommand> [args]")
	for _, sec := range helpSections {
		fmt.Fprintf(rc.Out, "\n%s:\n", sec.title)
		for _, n := range sec.names {
			if sc, ok := byName[n]; ok {
				fmt.Fprintf(rc.Out, "  %-11s %s\n", sc.name, sc.synopsis)
			}
		}
	}
}

// --- shared envelope ---

type header struct {
	Schema    string `json:"schema"`
	GraphSHA  string `json:"graph_sha"`
	Generated string `json:"generated_at"`
	Root      string `json:"root"`
}

func newHeader(root, sha string) header {
	return header{
		Schema:    schemaVersion,
		GraphSHA:  sha,
		Generated: time.Now().UTC().Format(time.RFC3339),
		Root:      root,
	}
}

func writeJSON(rc *tool.RunContext, v any) {
	enc := json.NewEncoder(rc.Out)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// --- load-or-build with mtime staleness ---

// sourceExt is the code-file extension set used only for the cheap staleness
// walk (a stat-only pass; no parsing). It mirrors the languages gfy detects.
var sourceExt = map[string]bool{
	".go": true, ".py": true, ".js": true, ".ts": true, ".jsx": true, ".tsx": true,
	".rs": true, ".java": true, ".c": true, ".h": true, ".cpp": true, ".cc": true,
	".rb": true, ".swift": true, ".kt": true, ".cs": true, ".scala": true,
	".php": true, ".lua": true, ".zig": true, ".ex": true, ".jl": true,
	".m": true, ".dart": true, ".vue": true, ".svelte": true,
}

// loadOrBuild returns the code graph for root: it loads a fresh cache when one
// exists and no source file is newer, otherwise it rebuilds and best-effort
// caches. forceRebuild skips the cache entirely; quiet suppresses build
// progress (used in --json mode so stderr stays clean).
func loadOrBuild(rc *tool.RunContext, root string, forceRebuild, quiet bool) (*codegraph.GraphContext, error) {
	cp := filepath.Join(root, cacheRel)
	if !forceRebuild && cacheFresh(root, cp) {
		if gc, err := codegraph.Load(cp); err == nil && gc != nil {
			return gc, nil
		}
	}
	var progress codegraph.ProgressFunc
	if !quiet {
		progress = func(msg string) { fmt.Fprintln(rc.Err, msg) }
	}
	gc, err := codegraph.BuildWithProgress(root, progress)
	if err != nil {
		return nil, err
	}
	if err := gc.Save(cp); err != nil && !quiet {
		fmt.Fprintf(rc.Err, "graph: cache write skipped: %v\n", err)
	}
	return gc, nil
}

func cacheFresh(root, cachePath string) bool {
	ci, err := os.Stat(cachePath)
	if err != nil {
		return false
	}
	if err := codegraph.CacheFresh(cachePath); err != nil {
		return false
	}
	return !newestSourceMTime(root).After(ci.ModTime())
}

func newestSourceMTime(root string) time.Time {
	var newest time.Time
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".agents", ".weave", "node_modules", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !sourceExt[strings.ToLower(filepath.Ext(p))] {
			return nil
		}
		if info, err := d.Info(); err == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	return newest
}

// sourceSHA fingerprints the SOURCE the graph is built from — sorted
// (relpath|size|mtime) over the code files under root — not the built graph.
// The graph is a pure function of source (that is the whole caching premise), and
// gfy's internal node-ids/edge-order are assigned non-deterministically at build
// time, so a graph-content hash is not reproducible run-to-run. A source
// fingerprint IS: identical source → identical key (cache hit), any source edit →
// a new key (staleness). This is exactly what `graph_sha` is for. Uses the same
// stat-only walk + skip set as cacheFresh, so it is cheap.
func sourceSHA(root string) string {
	type fe struct {
		rel  string
		size int64
		mod  int64
	}
	var files []fe
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".agents", ".weave", "node_modules", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !sourceExt[strings.ToLower(filepath.Ext(p))] {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			rel = p
		}
		files = append(files, fe{rel: rel, size: info.Size(), mod: info.ModTime().UnixNano()})
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	h := sha256.New()
	for _, f := range files {
		fmt.Fprintf(h, "%s\x1f%d\x1f%d\x00", f.rel, f.size, f.mod)
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// --- shared flag/arg helpers ---

func resolveRoot(rc *tool.RunContext, target string) string {
	if target == "" {
		if rc.Dir != "" {
			return rc.Dir
		}
		return "."
	}
	return rc.Path(target)
}

func nodeLabel(g *gfygraph.Graph, id string) string {
	if s, ok := g.NodeAttrs(id)["label"].(string); ok {
		return s
	}
	return id
}

func edgeStr(attrs map[string]any, key string) string {
	if v, ok := attrs[key].(string); ok {
		return v
	}
	return ""
}

func usageErr(rc *tool.RunContext, name, msg string) int {
	fmt.Fprintf(rc.Err, "%s: %s\n", name, msg)
	return 2
}

// --- graph build ---

type buildPayload struct {
	header
	Nodes       int      `json:"nodes"`
	Edges       int      `json:"edges"`
	Communities int      `json:"communities"`
	Files       int      `json:"files"`
	Languages   []string `json:"languages"`
	Cache       string   `json:"cache"`
}

func runBuild(rc *tool.RunContext, args []string) int {
	asJSON := weavecli.IsAgent()
	var target string
	for _, a := range args {
		switch a {
		case "--json", "--json=true":
			asJSON = true
		case "--json=false", "--plain":
			asJSON = false
		case "--rebuild": // graph build always rebuilds; flag accepted for symmetry
		default:
			if strings.HasPrefix(a, "-") {
				return usageErr(rc, "graph build", "unknown option "+a)
			}
			if target == "" {
				target = a
			}
		}
	}
	root := resolveRoot(rc, target)
	// graph build forces a fresh build (its whole purpose) and caches it.
	gc, err := loadOrBuild(rc, root, true, asJSON)
	if err != nil {
		fmt.Fprintf(rc.Err, "graph build: %v\n", err)
		return 1
	}
	cp := filepath.Join(root, cacheRel)
	if asJSON {
		writeJSON(rc, buildPayload{
			header:      newHeader(root, sourceSHA(root)),
			Nodes:       gc.Stats.NodeCount,
			Edges:       gc.Stats.EdgeCount,
			Communities: gc.Stats.CommunityCount,
			Files:       gc.Stats.FilesAnalyzed,
			Languages:   gc.Stats.Languages,
			Cache:       cp,
		})
		return 0
	}
	fmt.Fprintln(rc.Out, gc.GetGraphStats())
	fmt.Fprintf(rc.Out, "cached: %s (graph_sha %s)\n", cp, sourceSHA(root))
	return 0
}

// --- graph stats ---

type statsPayload struct {
	header
	Nodes       int      `json:"nodes"`
	Edges       int      `json:"edges"`
	Communities int      `json:"communities"`
	Files       int      `json:"files"`
	Languages   []string `json:"languages"`
	Extracted   int      `json:"extracted"`
	Inferred    int      `json:"inferred"`
	Ambiguous   int      `json:"ambiguous"`
}

func runStats(rc *tool.RunContext, args []string) int {
	asJSON := weavecli.IsAgent()
	rebuild := false
	var target string
	for _, a := range args {
		switch a {
		case "--json", "--json=true":
			asJSON = true
		case "--json=false", "--plain":
			asJSON = false
		case "--rebuild":
			rebuild = true
		default:
			if strings.HasPrefix(a, "-") {
				return usageErr(rc, "graph stats", "unknown option "+a)
			}
			if target == "" {
				target = a
			}
		}
	}
	root := resolveRoot(rc, target)
	gc, err := loadOrBuild(rc, root, rebuild, asJSON)
	if err != nil {
		fmt.Fprintf(rc.Err, "graph stats: %v\n", err)
		return 1
	}
	if asJSON {
		ex, inf, amb := confidenceCounts(gc)
		writeJSON(rc, statsPayload{
			header:      newHeader(root, sourceSHA(root)),
			Nodes:       gc.Stats.NodeCount,
			Edges:       gc.Stats.EdgeCount,
			Communities: gc.Stats.CommunityCount,
			Files:       gc.Stats.FilesAnalyzed,
			Languages:   gc.Stats.Languages,
			Extracted:   ex,
			Inferred:    inf,
			Ambiguous:   amb,
		})
		return 0
	}
	fmt.Fprintln(rc.Out, gc.GetGraphStats())
	return 0
}

func confidenceCounts(gc *codegraph.GraphContext) (extracted, inferred, ambiguous int) {
	for _, e := range gc.Graph.Edges() {
		switch edgeStr(e.Attrs, "confidence") {
		case "EXTRACTED":
			extracted++
		case "INFERRED":
			inferred++
		case "AMBIGUOUS":
			ambiguous++
		}
	}
	return
}

// --- graph neighbors ---

type neighbor struct {
	Label      string `json:"label"`
	FileType   string `json:"file_type"`
	Relation   string `json:"relation"`
	Confidence string `json:"confidence"`
}

type neighborsPayload struct {
	header
	Node      string     `json:"node"`
	Neighbors []neighbor `json:"neighbors"`
}

func runNeighbors(rc *tool.RunContext, args []string) int {
	asJSON := weavecli.IsAgent()
	var symbol, target, relation string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json" || a == "--json=true":
			asJSON = true
		case a == "--json=false" || a == "--plain":
			asJSON = false
		case a == "--relation":
			if i+1 < len(args) {
				i++
				relation = args[i]
			}
		case strings.HasPrefix(a, "--relation="):
			relation = a[len("--relation="):]
		case strings.HasPrefix(a, "-") && a != "-":
			return usageErr(rc, "graph neighbors", "unknown option "+a)
		default:
			if symbol == "" {
				symbol = a
			} else if target == "" {
				target = a
			}
		}
	}
	if symbol == "" {
		return usageErr(rc, "graph neighbors", "missing <symbol>")
	}
	root := resolveRoot(rc, target)
	gc, err := loadOrBuild(rc, root, false, asJSON)
	if err != nil {
		fmt.Fprintf(rc.Err, "graph neighbors: %v\n", err)
		return 1
	}
	id := search.FindNode(gc.Graph, symbol)
	if id == "" {
		fmt.Fprintf(rc.Err, "graph neighbors: symbol not found: %s\n", symbol)
		return 1
	}
	var nbrs []neighbor
	for _, nb := range gc.Graph.Neighbors(id) {
		eAttrs := gc.Graph.EdgeAttrs(id, nb)
		rel := edgeStr(eAttrs, "relation")
		if relation != "" && rel != relation {
			continue
		}
		nAttrs := gc.Graph.NodeAttrs(nb)
		nbrs = append(nbrs, neighbor{
			Label:      edgeStr(nAttrs, "label"),
			FileType:   edgeStr(nAttrs, "file_type"),
			Relation:   rel,
			Confidence: edgeStr(eAttrs, "confidence"),
		})
	}
	if asJSON {
		writeJSON(rc, neighborsPayload{
			header:    newHeader(root, sourceSHA(root)),
			Node:      nodeLabel(gc.Graph, id),
			Neighbors: nbrs,
		})
		return 0
	}
	if len(nbrs) == 0 {
		fmt.Fprintln(rc.Err, "(no neighbors)")
		return 0
	}
	for _, n := range nbrs {
		fmt.Fprintf(rc.Out, "- %s (%s) [%s, %s]\n", n.Label, n.FileType, n.Relation, n.Confidence)
	}
	return 0
}

// --- graph impact ---

type sgNode struct {
	Label    string `json:"label"`
	FileType string `json:"file_type"`
}

type sgEdge struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	Relation string `json:"relation"`
}

type impactPayload struct {
	header
	Node      string   `json:"node"`
	Depth     int      `json:"depth"`
	Total     int      `json:"total"`               // full blast-radius size before capping
	Truncated int      `json:"truncated,omitempty"` // nodes omitted by --limit
	Nodes     []sgNode `json:"nodes"`
	Edges     []sgEdge `json:"edges"`
}

// defaultImpactLimit caps the returned node set so a benchmark showed the default
// query stays agent-lean: an uncapped depth-2 BFS on the undirected graph returned
// 124 nodes / 28 KB for a single symbol (worse than grep). Nearest-first BFS order
// means the cap keeps the most-impacted symbols; `total`/`truncated` report the
// rest. 0 = no cap.
const defaultImpactLimit = 40

func runImpact(rc *tool.RunContext, args []string) int {
	asJSON := weavecli.IsAgent()
	// Default depth 1 = direct reverse dependency. Depth 2+ can explode and is
	// opt-in. --undirected preserves the old coupling query explicitly.
	depth := 1
	limit := defaultImpactLimit
	undirected := false
	var symbol, target string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json" || a == "--json=true":
			asJSON = true
		case a == "--json=false" || a == "--plain":
			asJSON = false
		case a == "--undirected":
			undirected = true
		case a == "--depth":
			if i+1 < len(args) {
				i++
				depth = atoiDefault(args[i], depth)
			}
		case strings.HasPrefix(a, "--depth="):
			depth = atoiDefault(a[len("--depth="):], depth)
		case a == "--limit":
			if i+1 < len(args) {
				i++
				limit = atoiDefault(args[i], limit)
			}
		case strings.HasPrefix(a, "--limit="):
			limit = atoiDefault(a[len("--limit="):], limit)
		case strings.HasPrefix(a, "-") && a != "-":
			return usageErr(rc, "graph impact", "unknown option "+a)
		default:
			if symbol == "" {
				symbol = a
			} else if target == "" {
				target = a
			}
		}
	}
	if symbol == "" {
		return usageErr(rc, "graph impact", "missing <file|symbol>")
	}
	if depth < 1 {
		depth = 1
	}
	root := resolveRoot(rc, target)
	gc, err := loadOrBuild(rc, root, false, asJSON)
	if err != nil {
		fmt.Fprintf(rc.Err, "graph impact: %v\n", err)
		return 1
	}
	id := search.FindNode(gc.Graph, symbol)
	if id == "" {
		fmt.Fprintf(rc.Err, "graph impact: not found: %s\n", symbol)
		return 1
	}
	visited, edges := reverseBFS(gc.Graph, []string{id}, depth)
	if undirected {
		visited, edges = gc.Graph.ToUndirected().BFS([]string{id}, depth)
	}
	total := len(visited)
	truncated := 0
	if limit > 0 && len(visited) > limit {
		truncated = len(visited) - limit
		visited = visited[:limit] // BFS order = nearest-first, so we keep the most impacted
	}
	nodes, sgEdges := subgraphView(gc.Graph, visited, edges)
	if asJSON {
		writeJSON(rc, impactPayload{
			header:    newHeader(root, sourceSHA(root)),
			Node:      nodeLabel(gc.Graph, id),
			Depth:     depth,
			Total:     total,
			Truncated: truncated,
			Nodes:     nodes,
			Edges:     sgEdges,
		})
		return 0
	}
	fmt.Fprintf(rc.Out, "blast radius of %q within %d hop(s): %d symbols, %d edges\n",
		nodeLabel(gc.Graph, id), depth, total, len(sgEdges))
	for _, n := range nodes {
		fmt.Fprintf(rc.Out, "- %s (%s)\n", n.Label, n.FileType)
	}
	if truncated > 0 {
		fmt.Fprintf(rc.Out, "… +%d more (raise with --limit N or narrow with --depth 1)\n", truncated)
	}
	return 0
}

func reverseBFS(g *gfygraph.Graph, startNodes []string, depth int) (visited []string, edges []gfygraph.EdgeData) {
	incoming := make(map[string][]gfygraph.EdgeData)
	for _, e := range g.Edges() {
		incoming[e.Target] = append(incoming[e.Target], e)
	}
	for id := range incoming {
		sort.Slice(incoming[id], func(i, j int) bool {
			if incoming[id][i].Source != incoming[id][j].Source {
				return incoming[id][i].Source < incoming[id][j].Source
			}
			return incoming[id][i].Target < incoming[id][j].Target
		})
	}
	seen := make(map[string]bool)
	edgeSeen := make(map[string]bool)
	queue := make([]string, 0, len(startNodes))
	depthMap := make(map[string]int)
	for _, s := range startNodes {
		if g.HasNode(s) {
			queue = append(queue, s)
			seen[s] = true
			depthMap[s] = 0
		}
	}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		d := depthMap[current]
		if d >= depth {
			continue
		}
		for _, e := range incoming[current] {
			edgeKey := e.Source + "\x00" + e.Target
			if !edgeSeen[edgeKey] {
				edgeSeen[edgeKey] = true
				edges = append(edges, e)
			}
			if !seen[e.Source] {
				seen[e.Source] = true
				depthMap[e.Source] = d + 1
				queue = append(queue, e.Source)
			}
		}
	}
	visited = make([]string, 0, len(seen))
	for id := range seen {
		visited = append(visited, id)
	}
	sort.Strings(visited)
	return visited, edges
}

// subgraphView builds the node list from visited (kept) ids and only the edges
// whose BOTH endpoints are kept, so a capped result stays internally consistent.
func subgraphView(g *gfygraph.Graph, visited []string, edges []gfygraph.EdgeData) ([]sgNode, []sgEdge) {
	kept := make(map[string]bool, len(visited))
	for _, id := range visited {
		kept[id] = true
	}
	nodes := make([]sgNode, 0, len(visited))
	for _, nid := range visited {
		a := g.NodeAttrs(nid)
		nodes = append(nodes, sgNode{Label: edgeStr(a, "label"), FileType: edgeStr(a, "file_type")})
	}
	sgEdges := make([]sgEdge, 0, len(edges))
	for _, e := range edges {
		if !kept[e.Source] || !kept[e.Target] {
			continue
		}
		sgEdges = append(sgEdges, sgEdge{
			Source:   nodeLabel(g, e.Source),
			Target:   nodeLabel(g, e.Target),
			Relation: edgeStr(e.Attrs, "relation"),
		})
	}
	return nodes, sgEdges
}

// --- graph path ---

type pathPayload struct {
	header
	Source string   `json:"source"`
	Target string   `json:"target"`
	Hops   int      `json:"hops"`
	Path   []string `json:"path"`
}

func runPath(rc *tool.RunContext, args []string) int {
	asJSON := weavecli.IsAgent()
	maxHops := 6
	var a1, a2, target string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json" || a == "--json=true":
			asJSON = true
		case a == "--json=false" || a == "--plain":
			asJSON = false
		case a == "--max-hops":
			if i+1 < len(args) {
				i++
				maxHops = atoiDefault(args[i], maxHops)
			}
		case strings.HasPrefix(a, "--max-hops="):
			maxHops = atoiDefault(a[len("--max-hops="):], maxHops)
		case strings.HasPrefix(a, "-") && a != "-":
			return usageErr(rc, "graph path", "unknown option "+a)
		default:
			switch {
			case a1 == "":
				a1 = a
			case a2 == "":
				a2 = a
			case target == "":
				target = a
			}
		}
	}
	if a1 == "" || a2 == "" {
		return usageErr(rc, "graph path", "usage: graph path <a> <b> [path]")
	}
	root := resolveRoot(rc, target)
	gc, err := loadOrBuild(rc, root, false, asJSON)
	if err != nil {
		fmt.Fprintf(rc.Err, "graph path: %v\n", err)
		return 1
	}
	src := search.FindNode(gc.Graph, a1)
	tgt := search.FindNode(gc.Graph, a2)
	if src == "" {
		fmt.Fprintf(rc.Err, "graph path: not found: %s\n", a1)
		return 1
	}
	if tgt == "" {
		fmt.Fprintf(rc.Err, "graph path: not found: %s\n", a2)
		return 1
	}
	ids := gc.Graph.ShortestPath(src, tgt, maxHops)
	if ids == nil {
		if asJSON {
			writeJSON(rc, pathPayload{header: newHeader(root, sourceSHA(root)), Source: nodeLabel(gc.Graph, src), Target: nodeLabel(gc.Graph, tgt), Hops: -1, Path: []string{}})
			return 0
		}
		fmt.Fprintln(rc.Out, "no path found")
		return 1
	}
	labels := make([]string, len(ids))
	for i, nid := range ids {
		labels[i] = nodeLabel(gc.Graph, nid)
	}
	if asJSON {
		writeJSON(rc, pathPayload{
			header: newHeader(root, sourceSHA(root)),
			Source: labels[0],
			Target: labels[len(labels)-1],
			Hops:   len(labels) - 1,
			Path:   labels,
		})
		return 0
	}
	fmt.Fprintf(rc.Out, "path (%d hops): %s\n", len(labels)-1, strings.Join(labels, " → "))
	return 0
}

// --- graph hotspots ---

type hotspot struct {
	Rank   int    `json:"rank"`
	Label  string `json:"label"`
	Degree int    `json:"degree"`
}

type hotspotsPayload struct {
	header
	Hotspots []hotspot `json:"hotspots"`
}

// ubiquitousLabels are accessor/utility method names that dominate raw degree
// ranking (a bare `.String()`/`.Len()` is on nearly every type) but carry no
// architectural signal. graph hotspots drops them by default so real linchpins
// surface; --raw disables the filter (identical to gfy's god-nodes). This is a
// documented heuristic, not upstream behavior. Degree centrality is the cheap
// O(V) first-order god-object signal; betweenness/PageRank (a future --metric)
// add bottleneck/influence nuance at O(V·(V+E)).
var ubiquitousLabels = map[string]bool{
	"String": true, "Error": true, "Len": true, "Cap": true, "Bytes": true,
	"Append": true, "Reset": true, "Close": true, "Read": true, "Write": true,
	"Init": true, "Get": true, "Set": true, "contains": true, "Clone": true,
	"Equal": true, "Marshal": true, "Unmarshal": true, "MarshalJSON": true,
	"UnmarshalJSON": true, "Format": true, "Value": true, "Size": true,
	"Add": true, "Lock": true, "Unlock": true, "New": true,
}

func normalizeLabel(label string) string {
	s := strings.TrimPrefix(label, ".")
	s = strings.TrimSuffix(s, "()")
	return s
}

func runHotspots(rc *tool.RunContext, args []string) int {
	asJSON := weavecli.IsAgent()
	top, raw := 15, false
	var target string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json" || a == "--json=true":
			asJSON = true
		case a == "--json=false" || a == "--plain":
			asJSON = false
		case a == "--raw":
			raw = true
		case a == "--top":
			if i+1 < len(args) {
				i++
				top = atoiDefault(args[i], top)
			}
		case strings.HasPrefix(a, "--top="):
			top = atoiDefault(a[len("--top="):], top)
		case strings.HasPrefix(a, "-") && a != "-":
			return usageErr(rc, "graph hotspots", "unknown option "+a)
		default:
			if target == "" {
				target = a
			}
		}
	}
	if top <= 0 {
		top = 15
	}
	root := resolveRoot(rc, target)
	gc, err := loadOrBuild(rc, root, false, asJSON)
	if err != nil {
		fmt.Fprintf(rc.Err, "graph hotspots: %v\n", err)
		return 1
	}
	var spots []hotspot
	for _, gn := range gc.GodNodes {
		if !raw && ubiquitousLabels[normalizeLabel(gn.Label)] {
			continue
		}
		spots = append(spots, hotspot{Rank: len(spots) + 1, Label: gn.Label, Degree: gn.Degree})
		if len(spots) >= top {
			break
		}
	}
	if asJSON {
		writeJSON(rc, hotspotsPayload{header: newHeader(root, sourceSHA(root)), Hotspots: spots})
		return 0
	}
	if len(spots) == 0 {
		fmt.Fprintln(rc.Err, "(no hotspots)")
		return 0
	}
	for _, s := range spots {
		fmt.Fprintf(rc.Out, "%2d. %s (degree %d)\n", s.Rank, s.Label, s.Degree)
	}
	return 0
}

// --- graph query ---

type queryPayload struct {
	header
	Question  string   `json:"question"`
	Total     int      `json:"total"`
	Truncated int      `json:"truncated,omitempty"`
	Nodes     []sgNode `json:"nodes"`
	Edges     []sgEdge `json:"edges"`
}

func runQuery(rc *tool.RunContext, args []string) int {
	asJSON := weavecli.IsAgent()
	depth := 1 // depth 1 keeps the subgraph agent-lean; depth 2+ explodes (opt-in)
	limit := defaultImpactLimit
	var question, target string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json" || a == "--json=true":
			asJSON = true
		case a == "--json=false" || a == "--plain":
			asJSON = false
		case a == "--depth":
			if i+1 < len(args) {
				i++
				depth = atoiDefault(args[i], depth)
			}
		case strings.HasPrefix(a, "--depth="):
			depth = atoiDefault(a[len("--depth="):], depth)
		case a == "--limit":
			if i+1 < len(args) {
				i++
				limit = atoiDefault(args[i], limit)
			}
		case strings.HasPrefix(a, "--limit="):
			limit = atoiDefault(a[len("--limit="):], limit)
		case strings.HasPrefix(a, "-") && a != "-":
			return usageErr(rc, "graph query", "unknown option "+a)
		default:
			if question == "" {
				question = a
			} else if target == "" {
				target = a
			}
		}
	}
	if question == "" {
		return usageErr(rc, "graph query", "missing <question>")
	}
	if depth < 1 {
		depth = 1
	}
	root := resolveRoot(rc, target)
	gc, err := loadOrBuild(rc, root, false, asJSON)
	if err != nil {
		fmt.Fprintf(rc.Err, "graph query: %v\n", err)
		return 1
	}
	results := search.ScoreNodes(gc.Graph, question)
	if len(results) > 5 {
		results = results[:5]
	}
	if len(results) == 0 {
		if asJSON {
			writeJSON(rc, queryPayload{header: newHeader(root, sourceSHA(root)), Question: question, Nodes: []sgNode{}, Edges: []sgEdge{}})
			return 1
		}
		fmt.Fprintf(rc.Out, "no matching nodes for: %s\n", question)
		return 1
	}
	startNodes := make([]string, len(results))
	for i, r := range results {
		startNodes[i] = r.ID
	}
	visited, edges := gc.Graph.BFS(startNodes, depth)
	total := len(visited)
	truncated := 0
	if limit > 0 && len(visited) > limit {
		truncated = len(visited) - limit
		visited = visited[:limit]
	}
	nodes, sgEdges := subgraphView(gc.Graph, visited, edges)
	if asJSON {
		writeJSON(rc, queryPayload{
			header: newHeader(root, sourceSHA(root)), Question: question,
			Total: total, Truncated: truncated, Nodes: nodes, Edges: sgEdges,
		})
		return 0
	}
	fmt.Fprintf(rc.Out, "subgraph for %q: %d nodes, %d edges\n", question, total, len(sgEdges))
	for _, n := range nodes {
		fmt.Fprintf(rc.Out, "- %s (%s)\n", n.Label, n.FileType)
	}
	if truncated > 0 {
		fmt.Fprintf(rc.Out, "… +%d more (raise with --limit N)\n", truncated)
	}
	return 0
}

func atoiDefault(s string, def int) int {
	n := 0
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return def
	}
	return n
}
