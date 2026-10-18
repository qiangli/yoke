package graphcmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/lens"
	"github.com/qiangli/yoke/pkg/lens/stream"
)

func decodeStream(t *testing.T, out string) *stream.Graph {
	t.Helper()
	g, err := stream.Decode(strings.NewReader(out), stream.DecodeOptions{})
	if err != nil {
		t.Fatalf("not a bashy-graph-v1 stream: %v\n%s", err, out)
	}
	if err := g.Verify(); err != nil {
		t.Fatalf("producer stream does not match its rev: %v", err)
	}
	if g.Cursor == nil || g.Cursor.Binding.Provider != "graph" || g.Cursor.Binding.Target != g.Source {
		t.Fatalf("missing or unbound cursor trailer: %#v", g.Cursor)
	}
	return g
}

func streamLabels(g *stream.Graph) map[string]bool {
	labels := map[string]bool{}
	for _, n := range g.Nodes {
		labels[strings.TrimSuffix(strings.TrimPrefix(n.Name, "."), "()")] = true
	}
	return labels
}

func runIn(t *testing.T, dir, stdin string, fn func(*tool.RunContext, []string) int, args ...string) (string, string, int) {
	t.Helper()
	var o, e bytes.Buffer
	rc := &tool.RunContext{Ctx: context.Background(), Dir: dir, FS: tool.NewLocalFS(),
		Stdio: tool.Stdio{In: strings.NewReader(stdin), Out: &o, Err: &e}}
	code := fn(rc, args)
	return o.String(), e.String(), code
}

// The stream carries no wall clock and no gfy node ids, so an independent
// rebuild of the same source gives byte-identical output.
func TestNeighborsStreamIsByteDeterministicAcrossRebuilds(t *testing.T) {
	dir := fixtureRepo(t)
	a, errOut, code := run(t, dir, runNeighbors, "Alpha", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if err := os.RemoveAll(filepath.Join(dir, cacheRel)); err != nil {
		t.Fatal(err)
	}
	b, _, _ := run(t, dir, runNeighbors, "Alpha", "--json")
	if a != b {
		t.Fatalf("rebuild changed the stream:\n%s\n---\n%s", a, b)
	}
	if strings.Contains(a, "generated_at") {
		t.Fatalf("stream carries a wall-clock field:\n%s", a)
	}
	g := decodeStream(t, a)
	if !strings.HasPrefix(g.Source, "graph:") || len(g.Edges) == 0 {
		t.Fatalf("expected graph: source and edges, got %#v", g)
	}
	for _, n := range g.Nodes {
		if !strings.HasPrefix(n.Ref, g.Source+"!/") {
			t.Errorf("node ref %q is not addressed inside %q", n.Ref, g.Source)
		}
	}
}

// The operator example: graph neighbors X --json | lens -t mermaid, with no -f.
func TestNeighborsStreamPipesIntoLens(t *testing.T) {
	dir := fixtureRepo(t)
	out, _, code := run(t, dir, runNeighbors, "Alpha", "--relation", "calls", "--json")
	if code != 0 {
		t.Fatal(code)
	}
	res, err := lens.Convert(context.Background(), lens.Request{To: "mermaid", Data: []byte(out)})
	if err != nil {
		t.Fatal(err)
	}
	if res.From != "graph" || !strings.Contains(string(res.Data), "-->|calls|") || !strings.Contains(string(res.Data), "Beta") {
		t.Fatalf("from %q:\n%s", res.From, res.Data)
	}
}

// Consumers take addresses in: graph … --json | graph impact -.
func TestImpactTakesAddressesOnStdin(t *testing.T) {
	dir := fixtureRepo(t)
	direct, _, code := run(t, dir, runImpact, "Alpha", "--json")
	if code != 0 {
		t.Fatal(code)
	}
	want := decodeStream(t, direct)

	// One-node stream for Alpha, as a producer would emit it.
	alphaOnly, _, _ := run(t, dir, runNeighbors, "Alpha", "--relation", "no-such-relation", "--json")
	if g := decodeStream(t, alphaOnly); len(g.Nodes) != 1 {
		t.Fatalf("fixture expects Alpha alone, got %#v", g.Nodes)
	}
	var headerless strings.Builder // what `jq -c 'select(.t=="node")'` leaves
	for _, l := range strings.Split(alphaOnly, "\n") {
		if strings.HasPrefix(l, `{"t":"node"`) {
			headerless.WriteString(l + "\n")
		}
	}
	for name, in := range map[string]string{
		"framed stream":   alphaOnly,
		"filtered stream": headerless.String(),
		"plain address":   decodeStream(t, alphaOnly).Nodes[0].Ref + "\n",
		"plain symbol":    "\nAlpha\n",
	} {
		out, errOut, code := runIn(t, dir, in, runImpact, "-", "--json")
		if code != 0 {
			t.Fatalf("%s: exit %d: %s", name, code, errOut)
		}
		got := decodeStream(t, out)
		if got.Rev != want.Rev {
			t.Errorf("%s: impact via stdin differs from impact Alpha:\n%s\n---\n%s", name, out, direct)
		}
	}

	if _, errOut, code := runIn(t, dir, "Alpha\nNoSuchSymbolXYZ\n", runImpact, "-", "--json"); code != 1 || !strings.Contains(errOut, "NoSuchSymbolXYZ") {
		t.Fatalf("unknown address must fail naming it: exit %d %q", code, errOut)
	}
	if _, _, code := runIn(t, dir, "", runImpact, "-"); code != 2 {
		t.Fatalf("empty stdin is a usage error, got %d", code)
	}
}
