package shape

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRelativizeWorkspace(t *testing.T) {
	ws := "/private/tmp/state/shape-work/hello-world/abc/def/work"
	cases := map[string]string{
		"Changed the greeting in [public/index.html](" + ws + "/public/index.html:6). Verified the diff.": "Changed the greeting in [public/index.html](public/index.html:6). Verified the diff.",
		"Edited /tmp/state/shape-work/hello-world/abc/def/work/public/app.js":                             "Edited public/app.js",
		"Ran the checks in " + ws: "Ran the checks in .",
		"No paths here":           "No paths here",
		"Other path /private/tmp/elsewhere/file.txt stays": "Other path /private/tmp/elsewhere/file.txt stays",
	}
	for in, want := range cases {
		if got := RelativizeWorkspace(in, ws); got != want {
			t.Errorf("RelativizeWorkspace(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

func TestRelativizeWorkspaceResolvesSymlinks(t *testing.T) {
	real := t.TempDir()
	work := filepath.Join(real, "work")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "ws")
	if err := os.Symlink(work, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatal(err)
	}
	in := "see " + filepath.ToSlash(resolved) + "/public/index.html"
	if got := RelativizeWorkspace(in, link); got != "see public/index.html" {
		t.Fatalf("got %q", got)
	}
}

func TestRelativizeCoverage(t *testing.T) {
	ws := "/var/state/work"
	c := Candidate{Workspace: ws,
		Coverage: &CoverageSummary{Covered: []string{"Changed " + ws + "/a.html"}, NotCovered: []string{"(none)"}},
		Next:     []string{"Also touch /private/var/state/work/b.html"}}
	relativizeCoverage(&c)
	if c.Coverage.Covered[0] != "Changed a.html" || c.Next[0] != "Also touch b.html" {
		t.Fatalf("covered=%q next=%q", c.Coverage.Covered[0], c.Next[0])
	}
}

// The delegate envelope's agent field is a resolved executable path under the
// operator's home; the recorded agent must be the configured tool:model.
func TestBashyDelegateRecordsConfiguredAgent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell script as the fake bashy")
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "bashy")
	script := "#!/bin/sh\nprintf '%s\\n' '{\"schema_version\":\"bashy-chat-v1\",\"agent\":\"/home/someone/.cache/bashy/bin/codex/1.0/bin/codex\",\"identity\":\"genie-delegate:codex-x\",\"exit_code\":0,\"output\":\"done\"}'\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := BashyDelegate{Bashy: fake, Agent: "codex:x"}.Delegate(context.Background(), DelegateRequest{Dir: dir, Task: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Agent != "codex:x" {
		t.Fatalf("agent = %q, want codex:x", res.Agent)
	}
	if strings.Contains(res.Agent+res.Identity, "/home/") {
		t.Fatalf("path leaked: %+v", res)
	}
	if res.Identity != "genie-delegate:codex-x" || res.Output != "done" {
		t.Fatalf("result = %+v", res)
	}
}
