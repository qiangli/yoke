package lenscmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// wsHome lays out a weave state root under a temp HOME with one workspace,
// and returns the workspace dir and a different cwd that must never be read.
func wsHome(t *testing.T) (ws, cwd string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	ws = filepath.Join(home, ".bashy", "weave", "repo-0000abcd", "workspaces", "issue-7")
	if err := os.MkdirAll(filepath.Join(ws, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "src", "a.txt"), []byte("in workspace\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cwd = t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "src", "host-only.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return ws, cwd
}

func TestWorkspaceScopeReadsWorkspaceNotCwd(t *testing.T) {
	_, cwd := wsHome(t)
	for _, id := range []string{"issue-7", "repo-0000abcd%2Fissue-7"} {
		out, errOut, code := runTool(t, cwd, "", "-t", "graph", "dir:src@ws:"+id)
		if code != 0 {
			t.Fatalf("%s: code %d stderr %q", id, code, errOut)
		}
		if !strings.Contains(out, "a.txt") || strings.Contains(out, "host-only.txt") {
			t.Fatalf("%s: stream does not read the workspace:\n%s", id, out)
		}
		if !strings.Contains(out, `"dir:src/a.txt@ws:`) {
			t.Fatalf("%s: emitted refs lost the ws scope:\n%s", id, out)
		}
		paths, _, code := runTool(t, cwd, "", "-t", "paths", "dir:src@ws:"+id)
		if code != 0 || !strings.Contains(paths, "a.txt") {
			t.Fatalf("%s: paths view: code %d %q", id, code, paths)
		}
	}
}

func TestWorkspaceScopeFailsClosed(t *testing.T) {
	ws, cwd := wsHome(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ws, "out")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	// A second queue with the same workspace name makes the bare id ambiguous.
	other := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(ws))), "other-1234abcd", "workspaces", "issue-9")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	dup := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(ws))), "other-1234abcd", "workspaces", "issue-7")
	if err := os.MkdirAll(dup, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ op, want string }{
		{"dir:src@ws:issue-404", "no such weave workspace"},
		{"dir:src@ws:issue-7", "ambiguous workspace"},
		{"dir:src@ws:..", "workspace id must be"},
		{"dir:src@ws:repo-0000abcd%2F..", "workspace id must be"},
		{"dir:src@ws:a%2Fb%2Fc", "workspace id must be"},
		{"dir:..@ws:issue-9", "escapes workspace"},
		{"dir:out@ws:repo-0000abcd%2Fissue-7", "escapes workspace"},
		{"dir:out%2Fsecret@ws:repo-0000abcd%2Fissue-7", "escapes workspace"},
		{"dir:%2Fetc@ws:issue-9", "workspace-relative"},
		{"git:HEAD:README.md@ws:issue-9", "does not support @ws scope"},
		{"dir:src@peer-a", "not served locally"},
		{"dir:src@cloud:x", "not served locally"},
	} {
		out, errOut, code := runTool(t, cwd, "", "-t", "graph", tc.op)
		if code == 0 || out != "" || !strings.Contains(errOut, tc.want) {
			t.Errorf("%s: code %d out %q stderr %q, want failure %q", tc.op, code, out, errOut, tc.want)
		}
	}
}

func TestWorkspaceScopeIsNeverImplied(t *testing.T) {
	_, cwd := wsHome(t)
	out, errOut, code := runTool(t, cwd, "", "-t", "graph", "dir:src")
	if code != 0 || !strings.Contains(out, "host-only.txt") || strings.Contains(out, "@ws:") {
		t.Fatalf("unscoped pull must read cwd: code %d stderr %q\n%s", code, errOut, out)
	}
}

// sandboxHome lays out a weave state root under a temp HOME with one sandbox,
// and returns the sandbox dir and a different cwd that must never be read.
func sandboxHome(t *testing.T) (sb, cwd string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	sb = filepath.Join(home, ".bashy", "weave", "repo-0000abcd", "sandboxes", "box-1")
	if err := os.MkdirAll(filepath.Join(sb, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sb, "src", "a.txt"), []byte("in sandbox\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cwd = t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "src", "host-only.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return sb, cwd
}

func TestSandboxScope(t *testing.T) {
	t.Run("explicit selection", func(t *testing.T) {
		_, cwd := sandboxHome(t)
		for _, id := range []string{"box-1", "repo-0000abcd%2Fbox-1"} {
			out, errOut, code := runTool(t, cwd, "", "-t", "graph", "dir:src@sandbox:"+id)
			if code != 0 {
				t.Fatalf("%s: code %d stderr %q", id, code, errOut)
			}
			if !strings.Contains(out, "a.txt") || strings.Contains(out, "host-only.txt") {
				t.Fatalf("%s: stream does not read the sandbox:\n%s", id, out)
			}
			if !strings.Contains(out, `"dir:src/a.txt@sandbox:`) {
				t.Fatalf("%s: emitted refs lost the sandbox scope:\n%s", id, out)
			}
			paths, _, code := runTool(t, cwd, "", "-t", "paths", "dir:src@sandbox:"+id)
			if code != 0 || !strings.Contains(paths, "a.txt") {
				t.Fatalf("%s: paths view: code %d %q", id, code, paths)
			}
			tree, _, code := runTool(t, cwd, "", "-t", "tree", "dir:src@sandbox:"+id)
			if code != 0 || !strings.Contains(tree, "a.txt") {
				t.Fatalf("%s: tree view: code %d %q", id, code, tree)
			}
		}

		// Unscoped pull must read cwd, not sandbox, and never imply @sandbox scope.
		out, _, code := runTool(t, cwd, "", "-t", "graph", "dir:src")
		if code != 0 || !strings.Contains(out, "host-only.txt") || strings.Contains(out, "@sandbox:") {
			t.Fatalf("unscoped pull must read cwd: code %d\n%s", code, out)
		}
	})

	t.Run("ambiguity", func(t *testing.T) {
		sb, cwd := sandboxHome(t)
		otherRoot := filepath.Dir(filepath.Dir(filepath.Dir(sb)))
		dup1 := filepath.Join(otherRoot, "repo-0000abcd", "sandboxes", "box-dup")
		dup2 := filepath.Join(otherRoot, "other-1234abcd", "sandboxes", "box-dup")
		if err := os.MkdirAll(filepath.Join(dup1, "src"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dup2, "src"), 0o755); err != nil {
			t.Fatal(err)
		}

		// Bare name is ambiguous across the two queues.
		out, errOut, code := runTool(t, cwd, "", "-t", "graph", "dir:src@sandbox:box-dup")
		if code == 0 || out != "" || !strings.Contains(errOut, "ambiguous sandbox") || !strings.Contains(errOut, "qualify with <queue-tag>/box-dup") {
			t.Fatalf("bare ambiguous sandbox must fail: code %d out %q stderr %q", code, out, errOut)
		}

		// Explicit qualification resolves unambiguously.
		out, errOut, code = runTool(t, cwd, "", "-t", "graph", "dir:src@sandbox:repo-0000abcd%2Fbox-dup")
		if code != 0 {
			t.Fatalf("qualified sandbox must succeed: code %d stderr %q", code, errOut)
		}
		out, errOut, code = runTool(t, cwd, "", "-t", "graph", "dir:src@sandbox:other-1234abcd%2Fbox-dup")
		if code != 0 {
			t.Fatalf("qualified sandbox must succeed: code %d stderr %q", code, errOut)
		}
	})

	t.Run("missing sandbox", func(t *testing.T) {
		_, cwd := sandboxHome(t)
		for _, tc := range []struct{ op, want string }{
			{"dir:src@sandbox:box-404", "no such weave sandbox"},
			{"dir:src@sandbox:repo-0000abcd%2Fbox-404", "no such weave sandbox"},
			{"dir:src@sandbox:..", "sandbox id must be"},
			{"dir:src@sandbox:repo-0000abcd%2F..", "sandbox id must be"},
			{"dir:src@sandbox:a%2Fb%2Fc", "sandbox id must be"},
		} {
			out, errOut, code := runTool(t, cwd, "", "-t", "graph", tc.op)
			if code == 0 || out != "" || !strings.Contains(errOut, tc.want) {
				t.Errorf("%s: code %d out %q stderr %q, want failure %q", tc.op, code, out, errOut, tc.want)
			}
		}
	})

	t.Run("escape", func(t *testing.T) {
		sb, cwd := sandboxHome(t)
		outside := t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret-data\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(sb, "out")); err != nil {
			t.Skip("symlinks unavailable:", err)
		}

		// Symlink in place of the sandbox directory itself is refused.
		symbox := filepath.Join(filepath.Dir(sb), "symbox")
		if err := os.Symlink(outside, symbox); err != nil {
			t.Skip("symlinks unavailable:", err)
		}

		for _, tc := range []struct{ op, want string }{
			{"dir:..@sandbox:box-1", "escapes sandbox"},
			{"dir:out@sandbox:box-1", "escapes sandbox"},
			{"dir:out%2Fsecret@sandbox:box-1", "escapes sandbox"},
			{"dir:%2Fetc@sandbox:box-1", "sandbox-relative"},
			{"dir:src@sandbox:symbox", "no such weave sandbox"},
			{"dir:src@sandbox:repo-0000abcd%2Fsymbox", "no such weave sandbox"},
		} {
			out, errOut, code := runTool(t, cwd, "", "-t", "graph", tc.op)
			if code == 0 || out != "" || !strings.Contains(errOut, tc.want) {
				t.Errorf("%s: code %d out %q stderr %q, want failure %q", tc.op, code, out, errOut, tc.want)
			}
		}
	})

	t.Run("read-only dir behavior", func(t *testing.T) {
		_, cwd := sandboxHome(t)

		// Directory reading views are served.
		for _, view := range []string{"paths", "tree", "graph"} {
			out, errOut, code := runTool(t, cwd, "", "-t", view, "dir:src@sandbox:box-1")
			if code != 0 || !strings.Contains(out, "a.txt") {
				t.Fatalf("view %s: code %d stderr %q out %q", view, code, errOut, out)
			}
		}

		// Non-dir provider scheme is refused under @sandbox scope.
		out, errOut, code := runTool(t, cwd, "", "-t", "graph", "git:HEAD:README.md@sandbox:box-1")
		if code == 0 || !strings.Contains(errOut, "does not support @sandbox scope") {
			t.Fatalf("non-dir scheme: code %d out %q stderr %q", code, out, errOut)
		}

		// Remote scopes are refused.
		for _, scope := range []string{"peer-a", "cloud:box-1", "k8s:box-1"} {
			out, errOut, code := runTool(t, cwd, "", "-t", "graph", "dir:src@"+scope)
			if code == 0 || !strings.Contains(errOut, "not served locally") {
				t.Fatalf("remote scope %s: code %d out %q stderr %q", scope, code, out, errOut)
			}
		}
	})
}
