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

// Sandbox scope moved to the outpost Podman tier (cmds/lens sandboxscope.go):
// a sandbox is containers on a remote Podman-backed peer, never a local
// weave directory. Its contract (Dragon-local refused, Podman-backed scope
// read-only) is pinned by the TestTierSandbox* tests in sandboxscope_test.go.
