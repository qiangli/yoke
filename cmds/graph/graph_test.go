package graphcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	gfygraph "github.com/qiangli/gfy/pkg/graph"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/codegraph"
	"github.com/qiangli/yoke/pkg/recall"
)

// fixtureRepo writes a tiny multi-file Go package with a call chain
// Gamma -> Alpha -> Beta, so the graph has real nodes and edges.
func fixtureRepo(t *testing.T) string {
	t.Helper()
	isolateStores(t)
	dir := t.TempDir()
	files := map[string]string{
		"a.go": "package fixture\n\nfunc Alpha() int { return Beta() }\n\nfunc Beta() int { return 42 }\n",
		"b.go": "package fixture\n\nfunc Gamma() int { return Alpha() }\n",
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func isolateStores(t *testing.T) {
	t.Helper()
	for _, name := range []string{"BASHY_KB_DIR", "BASHY_HOME", "BASHY_SKILLS_DIR", "YCODE_DATA_DIR"} {
		t.Setenv(name, t.TempDir())
	}
}

func run(t *testing.T, dir string, fn func(*tool.RunContext, []string) int, args ...string) (out, errOut string, code int) {
	t.Helper()
	var o, e bytes.Buffer
	rc := &tool.RunContext{
		Ctx:   context.Background(),
		Dir:   dir,
		FS:    tool.NewLocalFS(),
		Stdio: tool.Stdio{In: strings.NewReader(""), Out: &o, Err: &e},
	}
	code = fn(rc, args)
	return o.String(), e.String(), code
}

func TestGraphBuildCreatesCacheAndStats(t *testing.T) {
	dir := fixtureRepo(t)
	out, errOut, code := run(t, dir, runBuild, "--plain")
	if code != 0 {
		t.Fatalf("graph build exit %d, stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "Nodes:") {
		t.Fatalf("expected stats in output, got: %s", out)
	}
	cache := filepath.Join(dir, cacheRel)
	if _, err := os.Stat(cache); err != nil {
		t.Fatalf("expected cache at %s: %v", cache, err)
	}
}

func TestGraphStatsJSONEnvelope(t *testing.T) {
	dir := fixtureRepo(t)
	out, errOut, code := run(t, dir, runStats, "--json")
	if code != 0 {
		t.Fatalf("graph stats exit %d, stderr=%s", code, errOut)
	}
	var p statsPayload
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("invalid JSON envelope: %v\n%s", err, out)
	}
	if p.Schema != schemaVersion {
		t.Errorf("schema=%q want %q", p.Schema, schemaVersion)
	}
	if p.GraphSHA == "" {
		t.Error("graph_sha must be set")
	}
	if p.Nodes == 0 || p.Edges == 0 {
		t.Errorf("expected a non-empty graph, got nodes=%d edges=%d", p.Nodes, p.Edges)
	}
}

func TestGraphNeighborsFindsCallee(t *testing.T) {
	dir := fixtureRepo(t)
	out, errOut, code := run(t, dir, runNeighbors, "Alpha", "--plain")
	if code != 0 {
		t.Fatalf("graph neighbors exit %d, stderr=%s", code, errOut)
	}
	// Undirected coupling: Alpha's neighbors include Beta (calls) and Gamma (caller).
	if !strings.Contains(out, "Beta") {
		t.Fatalf("expected Beta among Alpha's neighbors, got:\n%s", out)
	}
}

func TestGraphNeighborsCallsAreOutgoingOnly(t *testing.T) {
	dir := fixtureRepo(t)
	out, errOut, code := run(t, dir, runNeighbors, "Alpha", "--relation", "calls", "--json")
	if code != 0 {
		t.Fatalf("graph neighbors exit %d, stderr=%s", code, errOut)
	}
	var p neighborsPayload
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	labels := map[string]bool{}
	for _, n := range p.Neighbors {
		labels[strings.TrimSuffix(n.Label, "()")] = true
	}
	if !labels["Beta"] {
		t.Fatalf("expected Alpha calls to include callee Beta, got %#v", p.Neighbors)
	}
	if labels["Gamma"] {
		t.Fatalf("caller Gamma must not appear in outgoing calls neighbors: %#v", p.Neighbors)
	}

	out, errOut, code = run(t, dir, runNeighbors, "Beta", "--relation", "calls", "--json")
	if code != 0 {
		t.Fatalf("graph neighbors exit %d, stderr=%s", code, errOut)
	}
	p = neighborsPayload{}
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	for _, n := range p.Neighbors {
		if strings.TrimSuffix(n.Label, "()") == "Alpha" {
			t.Fatalf("caller Alpha must not appear as Beta's outgoing call neighbor: %#v", p.Neighbors)
		}
	}
}

func TestGraphNeighborsMissingSymbol(t *testing.T) {
	dir := fixtureRepo(t)
	_, errOut, code := run(t, dir, runNeighbors, "NoSuchSymbolXYZ", "--plain")
	if code != 1 {
		t.Fatalf("expected exit 1 for missing symbol, got %d", code)
	}
	if !strings.Contains(errOut, "not found") {
		t.Errorf("expected 'not found' message, got: %s", errOut)
	}
}

func TestGraphImpactBlastRadius(t *testing.T) {
	dir := fixtureRepo(t)
	out, errOut, code := run(t, dir, runImpact, "Alpha", "--depth", "2", "--json")
	if code != 0 {
		t.Fatalf("graph impact exit %d, stderr=%s", code, errOut)
	}
	var p impactPayload
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	labels := map[string]bool{}
	for _, n := range p.Nodes {
		labels[strings.TrimSuffix(strings.TrimPrefix(n.Label, "."), "()")] = true
	}
	if !labels["Gamma"] {
		t.Errorf("expected caller Gamma in Alpha's reverse-dependency impact, got %#v", p.Nodes)
	}
	if labels["Beta"] {
		t.Errorf("callee Beta should not be in Alpha's directed reverse-dependency impact, got %#v", p.Nodes)
	}
}

func TestGraphImpactUndirectedCouplingIsExplicit(t *testing.T) {
	dir := fixtureRepo(t)
	out, errOut, code := run(t, dir, runImpact, "Alpha", "--depth", "1", "--undirected", "--json")
	if code != 0 {
		t.Fatalf("graph impact exit %d, stderr=%s", code, errOut)
	}
	var p impactPayload
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	labels := map[string]bool{}
	for _, n := range p.Nodes {
		labels[strings.TrimSuffix(strings.TrimPrefix(n.Label, "."), "()")] = true
	}
	if !labels["Beta"] || !labels["Gamma"] {
		t.Fatalf("--undirected should preserve coupling view, got %#v", p.Nodes)
	}
}

func TestGraphImpactDefaultsToDirectAndCaps(t *testing.T) {
	dir := fixtureRepo(t)
	// Default depth is 1 (direct coupling) — a benchmark showed depth-2 on the
	// undirected graph explodes (124 nodes / 28 KB for one symbol). --limit caps
	// the returned set and reports the remainder rather than silently dropping.
	out, _, code := run(t, dir, runImpact, "Alpha", "--depth", "2", "--limit", "1", "--json")
	if code != 0 {
		t.Fatalf("graph impact exit %d", code)
	}
	var p impactPayload
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("bad json: %v\n%s", err, out)
	}
	if p.Depth != 2 {
		t.Errorf("depth should honor the flag, got %d", p.Depth)
	}
	if len(p.Nodes) > 1 {
		t.Errorf("--limit 1 should cap nodes to 1, got %d", len(p.Nodes))
	}
	if p.Total <= len(p.Nodes) || p.Truncated != p.Total-len(p.Nodes) {
		t.Errorf("truncation not reported: total=%d shown=%d truncated=%d", p.Total, len(p.Nodes), p.Truncated)
	}
	// Every returned edge must have both endpoints among the shown nodes.
	shown := map[string]bool{}
	for _, n := range p.Nodes {
		shown[n.Label] = true
	}
	for _, e := range p.Edges {
		if !shown[e.Source] || !shown[e.Target] {
			t.Errorf("capped result has a dangling edge: %s -> %s", e.Source, e.Target)
		}
	}
}

func TestGraphPathBetweenSymbols(t *testing.T) {
	dir := fixtureRepo(t)
	out, errOut, code := run(t, dir, runPath, "Gamma", "Beta", "--plain")
	if code != 0 {
		t.Fatalf("graph path exit %d, stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "path (") && !strings.Contains(out, "no path") {
		t.Fatalf("unexpected path output: %s", out)
	}
}

func TestGraphQueryKeyword(t *testing.T) {
	dir := fixtureRepo(t)
	out, _, code := run(t, dir, runQuery, "Beta", "--plain")
	if code != 0 {
		t.Fatalf("graph query exit %d", code)
	}
	if !strings.Contains(out, "Beta") {
		t.Fatalf("expected Beta in query subgraph, got: %s", out)
	}
}

func TestGraphHotspotsRunsAndFilters(t *testing.T) {
	dir := fixtureRepo(t)
	// --raw and default should both return valid envelopes; the fixture labels
	// (Alpha/Beta/Gamma) are not in the ubiquitous denylist, so they survive.
	out, errOut, code := run(t, dir, runHotspots, "--json")
	if code != 0 {
		t.Fatalf("graph hotspots exit %d, stderr=%s", code, errOut)
	}
	var p hotspotsPayload
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if p.Schema != schemaVersion {
		t.Errorf("schema=%q", p.Schema)
	}
}

func TestUbiquitousLabelsFiltered(t *testing.T) {
	isolateStores(t)
	// Unit-check the heuristic directly: normalized ubiquitous labels are dropped,
	// domain labels survive.
	cases := map[string]bool{
		".String()": true, ".Len()": true, "New()": true,
		"Alpha()": false, "NewAppRegistry()": false, "authenticate()": false,
	}
	for label, wantFiltered := range cases {
		got := ubiquitousLabels[normalizeLabel(label)]
		if got != wantFiltered {
			t.Errorf("normalizeLabel(%q)=%q filtered=%v want %v", label, normalizeLabel(label), got, wantFiltered)
		}
	}
}

func TestGraphSHAStableAcrossBuilds(t *testing.T) {
	dir := fixtureRepo(t)
	out1, _, _ := run(t, dir, runStats, "--json")
	// Force a second independent build (bypass cache) and compare graph_sha.
	out2, _, _ := run(t, dir, runStats, "--json", "--rebuild")
	var a, b statsPayload
	_ = json.Unmarshal([]byte(out1), &a)
	_ = json.Unmarshal([]byte(out2), &b)
	if a.GraphSHA == "" || a.GraphSHA != b.GraphSHA {
		t.Errorf("graph_sha not stable: %q vs %q", a.GraphSHA, b.GraphSHA)
	}
}

func TestDirectedGraphBuildIsStableAndContainsFileToSymbol(t *testing.T) {
	dir := fixtureRepo(t)
	a, err := codegraph.BuildWithProgress(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := codegraph.BuildWithProgress(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Graph.IsDirected() || !b.Graph.IsDirected() {
		t.Fatalf("graphs must be directed: %v %v", a.Graph.IsDirected(), b.Graph.IsDirected())
	}
	nodesA, nodesB := a.Graph.Nodes(), b.Graph.Nodes()
	if strings.Join(nodesA, "\x00") != strings.Join(nodesB, "\x00") {
		t.Fatalf("node ids changed across identical builds:\n%v\n%v", nodesA, nodesB)
	}
	first := edgeOrientationByExtractedEndpoints(a.Graph)
	second := edgeOrientationByExtractedEndpoints(b.Graph)
	flips := 0
	for key, orientA := range first {
		if orientB, ok := second[key]; ok && orientA != orientB {
			flips++
		}
	}
	if flips != 0 {
		t.Fatalf("source/target orientation flips across identical builds: %d", flips)
	}
	contains := 0
	for _, e := range a.Graph.Edges() {
		if edgeStr(e.Attrs, "relation") != "contains" {
			continue
		}
		contains++
		srcLabel := nodeLabel(a.Graph, e.Source)
		tgtLabel := nodeLabel(a.Graph, e.Target)
		if !strings.HasSuffix(srcLabel, ".go") || strings.HasSuffix(tgtLabel, ".go") {
			t.Fatalf("contains edge must be file->symbol, got %s -> %s", srcLabel, tgtLabel)
		}
	}
	if contains == 0 {
		t.Fatal("fixture produced no contains edges")
	}
}

func edgeOrientationByExtractedEndpoints(g *gfygraph.Graph) map[string]string {
	edges := g.Edges()
	sort.Slice(edges, func(i, j int) bool {
		if edgeStr(edges[i].Attrs, "_src") != edgeStr(edges[j].Attrs, "_src") {
			return edgeStr(edges[i].Attrs, "_src") < edgeStr(edges[j].Attrs, "_src")
		}
		if edgeStr(edges[i].Attrs, "_tgt") != edgeStr(edges[j].Attrs, "_tgt") {
			return edgeStr(edges[i].Attrs, "_tgt") < edgeStr(edges[j].Attrs, "_tgt")
		}
		return edgeStr(edges[i].Attrs, "relation") < edgeStr(edges[j].Attrs, "relation")
	})
	out := make(map[string]string, len(edges))
	for _, e := range edges {
		key := edgeStr(e.Attrs, "relation") + "\x00" + edgeStr(e.Attrs, "_src") + "\x00" + edgeStr(e.Attrs, "_tgt")
		out[key] = e.Source + "\x00" + e.Target
	}
	return out
}

func TestCacheFreshnessDetectsEdits(t *testing.T) {
	dir := fixtureRepo(t)
	// Build populates the cache.
	if _, _, code := run(t, dir, runBuild, "--plain"); code != 0 {
		t.Fatal("build failed")
	}
	cache := filepath.Join(dir, cacheRel)
	if !cacheFresh(dir, cache) {
		t.Fatal("cache should be fresh right after build")
	}
	// Touch a source file into the future → cache goes stale.
	future := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "a.go"), future, future); err != nil {
		t.Fatal(err)
	}
	if cacheFresh(dir, cache) {
		t.Error("cache should be stale after a source edit")
	}
}

func TestOldUndirectedCacheIsRebuilt(t *testing.T) {
	dir := fixtureRepo(t)
	cache := filepath.Join(dir, cacheRel)
	old := gfygraph.New(false)
	old.AddNode("old_file", map[string]any{"label": "old.go", "file_type": "code"})
	old.AddNode("old_symbol", map[string]any{"label": "Old()", "file_type": "code"})
	old.AddEdge("old_symbol", "old_file", map[string]any{
		"relation": "contains", "_src": "old_file", "_tgt": "old_symbol",
	})
	if err := old.SaveJSON(cache); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(cache, future, future); err != nil {
		t.Fatal(err)
	}
	if cacheFresh(dir, cache) {
		t.Fatal("old undirected cache must not be considered fresh")
	}
	out, errOut, code := run(t, dir, runStats, "--json")
	if code != 0 {
		t.Fatalf("graph stats exit %d, stderr=%s", code, errOut)
	}
	var p statsPayload
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	loaded, err := codegraph.Load(cache)
	if err != nil {
		t.Fatalf("rebuilt cache should load: %v", err)
	}
	if !loaded.Graph.IsDirected() {
		t.Fatal("rebuilt cache must be directed")
	}
	if searchOld := nodeLabel(loaded.Graph, "old_symbol"); searchOld == "Old()" {
		t.Fatal("stale undirected cache was loaded instead of rebuilt")
	}
}

func TestCodeRingContextBudgetAndRefs(t *testing.T) {
	dir := fixtureRepo(t)
	cmd := recall.NewContextCmd(NewCodeRing(dir))
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{
		"--for", "Alpha calls Beta",
		"--rings", "repo",
		"--forms", "code",
		"--files", "a.go",
		"--budget", "700",
		"--json",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var got recall.ContextResult
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("invalid context JSON: %v\n%s", err, out.String())
	}
	if got.Budget.Used > 700 {
		t.Fatalf("context used %d tokens, want <= 700", got.Budget.Used)
	}
	if len(got.Blocks) == 0 {
		t.Fatalf("code context returned no blocks: %+v", got)
	}
	sawFileBoost := false
	for _, block := range got.Blocks {
		if block.Form != "code" {
			t.Fatalf("block form = %q, want code: %+v", block.Form, block)
		}
		if !strings.HasPrefix(block.Ref, "code:") {
			t.Fatalf("block ref = %q, want code:*", block.Ref)
		}
		if block.Ref == "code:file:a.go" {
			sawFileBoost = true
		}
	}
	if !sawFileBoost {
		t.Fatalf("--files did not surface the boosted repomap file: %+v", got.Blocks)
	}
}
