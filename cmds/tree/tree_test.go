package treecmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/coreutils/tool"
	_ "github.com/qiangli/yoke/cmds/lens"
	"github.com/qiangli/yoke/pkg/lens/stream"
)

func TestTreeLocatorJSONDepth(t *testing.T) {
	root := buildTree(t)
	out, errOut, code := runTree(t, root, "--json", "--depth=1", "dir:.")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	g, err := stream.Decode(strings.NewReader(out), stream.DecodeOptions{})
	if err != nil {
		t.Fatalf("not a framed bashy-graph-v1 stream: %v\n%s", err, out)
	}
	if err := g.Verify(); err != nil {
		t.Fatal(err)
	}
	if g.Source != "." || g.Cursor.Binding.Provider != "dir" || g.Cursor.Binding.Query != "depth=1" || g.Cursor.Binding.Revision != g.Rev {
		t.Fatalf("header/cursor not bound to dir:. depth=1: source=%q cursor=%+v", g.Source, g.Cursor)
	}
	if len(g.Nodes) < 2 || len(g.Edges) == 0 {
		t.Fatalf("rows: %q", out)
	}
	for _, n := range g.Nodes {
		if n.Name == "b.go" {
			t.Fatalf("depth exceeded: %+v", n)
		}
	}
}

// Pull and push are the same bytes: `tree --json LOC` is what
// `lens -t graph LOC` pulls, so every lens view of the piped stream equals
// the view lens pulls itself. --agentic changes the walk, so it changes the
// cursor binding (and the content), never silently the same token.
func TestTreeStreamPullPushIdentity(t *testing.T) {
	root := buildTree(t)
	lens := tool.Lookup("lens")
	if lens == nil {
		t.Fatal("lens is not registered")
	}
	runLens := func(stdin string, args ...string) string {
		t.Helper()
		var o, e bytes.Buffer
		rc := &tool.RunContext{Ctx: context.Background(), Dir: root,
			Stdio: tool.Stdio{In: strings.NewReader(stdin), Out: &o, Err: &e}}
		if code := lens.Run(rc, args); code != 0 || e.Len() > 0 {
			t.Fatalf("lens %v: %d %s", args, code, e.String())
		}
		return o.String()
	}
	for _, depth := range []string{"-1", "0", "1"} {
		pushed, errOut, code := runTree(t, root, "--json", "--depth="+depth, "dir:sub")
		if code != 0 || errOut != "" {
			t.Fatalf("depth %s: %d %s", depth, code, errOut)
		}
		if again, _, _ := runTree(t, root, "--json", "--depth="+depth, "dir:sub"); again != pushed {
			t.Fatalf("depth %s: producer is not deterministic", depth)
		}
		if pulled := runLens("", "-t", "graph", "--depth="+depth, "dir:sub"); pulled != pushed {
			t.Fatalf("depth %s: tree --json differs from lens pull:\n%s\n---\n%s", depth, pushed, pulled)
		}
		for _, view := range []string{"paths", "tree", "edges"} {
			if a, b := runLens(pushed, "-t", view), runLens("", "-t", view, "--depth="+depth, "dir:sub"); a != b {
				t.Errorf("depth %s view %s: push %q != pull %q", depth, view, a, b)
			}
		}
	}
	plain, _, _ := runTree(t, root, "--json", ".")
	agentic, _, _ := runTree(t, root, "--json", "--agentic", "-a", ".")
	pg, err1 := stream.Decode(strings.NewReader(plain), stream.DecodeOptions{})
	ag, err2 := stream.Decode(strings.NewReader(agentic), stream.DecodeOptions{})
	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	if ag.Cursor.Binding.Query != "depth=-1&agentic=true" || ag.Cursor.Token == pg.Cursor.Token {
		t.Fatalf("--agentic cursor not bound to its own query: %+v vs %+v", ag.Cursor, pg.Cursor)
	}
	if strings.Contains(agentic, "node_modules") || !strings.Contains(plain, "node_modules") {
		t.Fatalf("--agentic did not filter the streamed walk")
	}
}

func TestTreeJSONRefusesSeveralLocators(t *testing.T) {
	root := buildTree(t)
	out, errOut, code := runTree(t, root, "--json", "dir:sub", "dir:.")
	if code != 2 || out != "" || !strings.Contains(errOut, "one locator") {
		t.Fatalf("want usage error, got %d %q %q", code, out, errOut)
	}
}

// Without --json the classic listing is untouched.
func TestTreeClassicUnchangedByStream(t *testing.T) {
	root := buildTree(t)
	out, _, code := runTree(t, root, "sub")
	if want := "sub\n├── b.go\n└── c.go\n\n0 directories, 2 files\n"; code != 0 || out != want {
		t.Fatalf("classic tree:\n%q\nwant\n%q", out, want)
	}
}

// buildTree lays out a fixture directory and returns its path.
func buildTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mk := func(p string) {
		full := filepath.Join(root, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte("x"), 0o644)
	}
	mk("a.go")
	mk("sub/b.go")
	mk("sub/c.go")
	mk(".hidden")
	mk("node_modules/dep/index.js")
	os.MkdirAll(filepath.Join(root, ".git"), 0o755)
	os.WriteFile(filepath.Join(root, ".gitignore"), []byte("ignored.txt\n"), 0o644)
	mk("ignored.txt")
	return root
}

func runTree(t *testing.T, dir string, args ...string) (out, errOut string, code int) {
	t.Helper()
	var o, e bytes.Buffer
	rc := &tool.RunContext{
		Ctx:   context.Background(),
		Dir:   dir,
		Stdio: tool.Stdio{In: strings.NewReader(""), Out: &o, Err: &e},
	}
	code = cmd.Run(rc, args)
	return o.String(), e.String(), code
}

func TestTreeDefaultHidesDotfiles(t *testing.T) {
	root := buildTree(t)
	out, _, _ := runTree(t, root, root)
	if strings.Contains(out, ".hidden") || strings.Contains(out, ".git\n") {
		t.Errorf("default tree must hide dot entries:\n%s", out)
	}
	if !strings.Contains(out, "a.go") || !strings.Contains(out, "└──") || !strings.Contains(out, "├──") {
		t.Errorf("expected box-drawing tree with a.go:\n%s", out)
	}
	if !strings.Contains(out, "directories,") || !strings.Contains(out, "files") {
		t.Errorf("expected a summary line:\n%s", out)
	}
}

func TestTreeAllShowsHidden(t *testing.T) {
	root := buildTree(t)
	out, _, _ := runTree(t, root, "-a", root)
	if !strings.Contains(out, ".hidden") {
		t.Errorf("-a must show hidden entries:\n%s", out)
	}
}

func TestTreeDepthLimit(t *testing.T) {
	root := buildTree(t)
	out, _, _ := runTree(t, root, "-L", "1", root)
	if !strings.Contains(out, "sub") {
		t.Errorf("-L1 should still list the sub dir:\n%s", out)
	}
	if strings.Contains(out, "b.go") {
		t.Errorf("-L1 must not descend into sub:\n%s", out)
	}
}

func TestTreeDirsOnly(t *testing.T) {
	root := buildTree(t)
	out, _, _ := runTree(t, root, "-d", root)
	if strings.Contains(out, "a.go") || strings.Contains(out, "b.go") {
		t.Errorf("-d must list directories only:\n%s", out)
	}
	if !strings.Contains(out, "sub") {
		t.Errorf("-d should still show the sub dir:\n%s", out)
	}
}

func TestTreeAgenticSkipsNoiseAndGitignore(t *testing.T) {
	root := buildTree(t)
	out, errOut, _ := runTree(t, root, "-a", "--agentic", root)
	if strings.Contains(out, "node_modules") {
		t.Errorf("--agentic must skip node_modules:\n%s", out)
	}
	if strings.Contains(out, "ignored.txt") {
		t.Errorf("--agentic must skip .gitignore'd files:\n%s", out)
	}
	if !strings.Contains(errOut, "skipped") {
		t.Errorf("--agentic should report what it hid: %q", errOut)
	}
	// Without --agentic, -a shows the noise.
	out2, _, _ := runTree(t, root, "-a", root)
	if !strings.Contains(out2, "node_modules") {
		t.Errorf("without --agentic, -a should show node_modules:\n%s", out2)
	}
}
