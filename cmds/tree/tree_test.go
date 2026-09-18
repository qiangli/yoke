package treecmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/coreutils/tool"
)

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
