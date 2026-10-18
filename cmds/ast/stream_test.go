package astcmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/lens/stream"
)

const refsSrc = `package p

func Hello() string { return World() }

func World() string { return "w" }

type T struct{}

func (T) String() string { return Hello() }
`

func refsFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pkg", "a.go"), []byte(refsSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func runAstIn(t *testing.T, dir, stdin string, args ...string) (string, string, int) {
	t.Helper()
	var o, e bytes.Buffer
	rc := &tool.RunContext{Ctx: context.Background(), Dir: dir,
		Stdio: tool.Stdio{In: strings.NewReader(stdin), Out: &o, Err: &e}}
	code := runAst(rc, args)
	return o.String(), e.String(), code
}

func decode(t *testing.T, out string) *stream.Graph {
	t.Helper()
	g, err := stream.Decode(strings.NewReader(out), stream.DecodeOptions{})
	if err != nil {
		t.Fatalf("not a framed bashy-graph-v1 stream: %v\n%s", err, out)
	}
	if err := g.Verify(); err != nil {
		t.Fatal(err)
	}
	return g
}

// symbols/search/map --json are the stream; identity is relative to the
// operand, so the same source in two checkouts gives the same bytes.
func TestAstProducersEmitStream(t *testing.T) {
	a, b := refsFixture(t), refsFixture(t)
	for _, args := range [][]string{
		{"symbols", "pkg", "--json"},
		{"search", "ell", "pkg", "--json"},
		{"map", "pkg", "--json"},
	} {
		out, errOut, code := runAstIn(t, a, "", args...)
		if code != 0 {
			t.Fatalf("%v: %d %s", args, code, errOut)
		}
		if other, _, _ := runAstIn(t, b, "", args...); other != out {
			t.Fatalf("%v: bytes depend on the checkout location:\n%s\n---\n%s", args, out, other)
		}
		g := decode(t, out)
		if g.Source != "ast:pkg" || g.Cursor.Binding.Provider != "ast" || g.Cursor.Binding.Target != "ast:pkg" ||
			!strings.Contains("&"+g.Cursor.Binding.Query+"&", "&op="+args[0]+"&") {
			t.Fatalf("%v: header/cursor binding: %q %+v", args, g.Source, g.Cursor.Binding)
		}
		if strings.Contains(out, a) {
			t.Fatalf("%v: absolute checkout path leaked into the stream", args)
		}
		var hello string
		for _, n := range g.Nodes {
			if n.Name == "Hello" {
				hello = n.Ref
			}
		}
		if hello != "ast:pkg!/a.go!/Hello!/3" {
			t.Fatalf("%v: Hello ref %q", args, hello)
		}
		contained := false
		for _, e := range g.Edges {
			contained = contained || e.Rel == "contains" && e.Src == "ast:pkg!/a.go" && e.Dst == hello
		}
		if !contained {
			t.Fatalf("%v: file does not contain Hello: %+v", args, g.Edges)
		}
	}
	// search binds its pattern: two queries never share a cursor.
	x, _, _ := runAstIn(t, a, "", "search", "ell", "pkg", "--json")
	y, _, _ := runAstIn(t, a, "", "search", "Hel", "pkg", "--json")
	if gx, gy := decode(t, x), decode(t, y); gx.Rev != gy.Rev || gx.Cursor.Token == gy.Cursor.Token {
		t.Fatalf("same rows, different queries: rev %s/%s token %s/%s", gx.Rev, gy.Rev, gx.Cursor.Token, gy.Cursor.Token)
	}
}

// Without --json the classic grep-style lines are unchanged.
func TestAstClassicOutputUnchanged(t *testing.T) {
	dir := refsFixture(t)
	out, _, code := runAstIn(t, dir, "", "refs", "Hello", "pkg")
	if want := "a.go:3: references   Hello\n    in Hello()\na.go:9: references   Hello\n    in String()\n"; code != 0 || out != want {
		t.Fatalf("classic refs:\n%q\nwant\n%q", out, want)
	}
	out, _, _ = runAstIn(t, dir, "", "symbols", "pkg/a.go")
	if !strings.HasPrefix(out, filepath.Join(dir, "pkg", "a.go")+":3: func Hello() string\n") {
		t.Fatalf("classic symbols:\n%s", out)
	}
}

func TestAstRefsStream(t *testing.T) {
	dir := refsFixture(t)
	out, errOut, code := runAstIn(t, dir, "", "refs", "Hello", "pkg", "--json")
	if code != 0 {
		t.Fatalf("%d %s", code, errOut)
	}
	g := decode(t, out)
	var refs []string
	for _, e := range g.Edges {
		if e.Rel == "references" {
			if e.Dst != "ast:pkg!/Hello" {
				t.Fatalf("reference edge to %q", e.Dst)
			}
			refs = append(refs, e.Src)
		}
	}
	if strings.Join(refs, ",") != "ast:pkg!/a.go!/Hello!/3,ast:pkg!/a.go!/Hello!/9" {
		t.Fatalf("reference sites: %v", refs)
	}
	if q := g.Cursor.Binding.Query; q != "op=refs&symbol=Hello" {
		t.Fatalf("query %q", q)
	}
	// No references is still a complete (empty) answer under --json.
	out, _, code = runAstIn(t, dir, "", "refs", "Nope", "pkg", "--json")
	if code != 0 || len(decode(t, out).Nodes) != 2 {
		t.Fatalf("empty refs stream: %d\n%s", code, out)
	}
}

// Consumers take addresses in: `ast … --json | ast refs -` reads node refs
// (root and file nodes name no symbol), plain lines are symbols, and a
// graph: label resolves by its symbol name.
func TestAstRefsReadsAddresses(t *testing.T) {
	dir := refsFixture(t)
	direct, _, _ := runAstIn(t, dir, "", "refs", "Hello", "pkg", "--json")

	searched, _, _ := runAstIn(t, dir, "", "search", "Hell", "pkg", "--json")
	for name, stdin := range map[string]string{
		"stream":          searched,
		"filtered stream": filterNodes(searched, "Hello"),
		"plain":           "\nHello\n\n",
		"graph label":     "graph:/repo!/pkg/a.go!/Hello()\n",
		"queried target":  "ast:pkg!/Hello\n",
	} {
		out, errOut, code := runAstIn(t, dir, stdin, "refs", "-", "pkg", "--json")
		if code != 0 {
			t.Fatalf("%s: %d %s", name, code, errOut)
		}
		if out != direct {
			t.Errorf("%s: refs - differs from refs Hello:\n%s\n---\n%s", name, out, direct)
		}
	}

	// Two symbols union into one stream, bound to both.
	out, _, code := runAstIn(t, dir, "Hello\nWorld\n", "refs", "-", "pkg", "--json")
	if g := decode(t, out); code != 0 || g.Cursor.Binding.Query != "op=refs&symbol=Hello&symbol=World" {
		t.Fatalf("multi-symbol refs: %d %+v", code, g.Cursor)
	}
	// Classic text over stdin addresses.
	out, _, code = runAstIn(t, dir, "World\n", "refs", "-", "pkg")
	if code != 0 || out != "a.go:3: references   World\n    in Hello()\na.go:5: references   World\n    in World()\n" {
		t.Fatalf("classic refs -: %d %q", code, out)
	}

	for name, stdin := range map[string]string{
		"empty":      "",
		"only files": "ast:pkg\nast:pkg!/a.go\n",
		"bad stream": `{"t":"header","schema_version":"bashy-graph-v1"}` + "\n",
	} {
		if _, errOut, code := runAstIn(t, dir, stdin, "refs", "-", "pkg"); code != 2 || errOut == "" {
			t.Errorf("%s: want exit 2 with a diagnostic, got %d %q", name, code, errOut)
		}
	}
}

// filterNodes keeps only node records named name — what `jq` or `grep`
// leaves of a stream (no header, no trailer).
func filterNodes(s, name string) string {
	var b strings.Builder
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, `"t":"node"`) && strings.Contains(l, `"name":"`+name+`"`) {
			b.WriteString(l + "\n")
		}
	}
	return b.String()
}

func TestSymbolFromAddress(t *testing.T) {
	for in, want := range map[string]string{
		"Hello":                     "Hello",
		"ast:pkg":                   "",
		"ast:pkg!/a.go":             "",
		"ast:pkg!/Hello":            "Hello",
		"ast:pkg!/a.go!/Hello!/3":   "Hello",
		"graph:/r!/a.go!/Hello()":   "Hello",
		"graph:/r!/a.go!/.String()": "String",
		"graph:/r!/a.go":            "",
		"graph:/r":                  "",
		"pkg.Func":                  "pkg.Func",
	} {
		if got := symbolFromAddress(in); got != want {
			t.Errorf("symbolFromAddress(%q) = %q, want %q", in, got, want)
		}
	}
}
