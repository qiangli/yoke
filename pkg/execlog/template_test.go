// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package execlog

import "testing"

func opts() TemplateOpts {
	return TemplateOpts{
		RepoRoot: "/w/repo",
		HomeDir:  "/home/alice",
		TmpDir:   "/tmp",
	}
}

func TestTemplate(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want string
	}{
		{
			"plain package pattern survives — it IS the intent",
			[]string{"go", "test", "./..."},
			"go test ./...",
		},
		{
			"test selector is a pattern, package path is not",
			[]string{"go", "test", "-race", "-run", "TestAdvisor", "./internal/agentos/..."},
			"go test -race -run <PATTERN> ./internal/agentos/...",
		},
		{
			"commit messages collapse",
			[]string{"git", "commit", "-m", "fix: nil advisor in WireExec"},
			"git commit -m <TEXT>",
		},
		{
			"ssh port stays a port and is NOT mistaken for a password",
			[]string{"ssh", "-p", "2222", "user@remote.host"},
			"ssh -p <PORT> <USER>@<HOST>",
		},
		{
			"make is a bare target",
			[]string{"make", "test"},
			"make test",
		},
		{
			"attached numeric flag classes out",
			[]string{"bashy", "dag", "suites.md", "-j8"},
			"bashy dag suites.md -j<N>",
		},
		{
			"absolute repo path becomes repo-relative — this is what `about` uses",
			[]string{"cat", "/w/repo/internal/cli/main.go"},
			"cat ./internal/cli/main.go",
		},
		{
			"home path is identity and never survives",
			[]string{"ls", "/home/alice/secrets"},
			"ls <HOMEPATH>",
		},
		{
			"temp path would fragment the corpus to n=1",
			[]string{"sort", "-o", "/tmp/tmp.QUORPZIwqx/out.tsv", "/tmp/tmp.QUORPZIwqx/out.tsv"},
			"sort -o <TMPPATH> <TMPPATH>",
		},
		{
			"a random-looking dir fragments even outside TMPDIR",
			[]string{"cat", "/var/folders/vg/nlsn8n8x77n1xgg2/T/x"},
			"cat <TMPPATH>",
		},
		{
			"script bodies collapse",
			[]string{"bash", "-c", "make all && ./run.sh"},
			"bash -c <SCRIPT>",
		},
		{
			"image tags are versions",
			[]string{"podman", "run", "img:1.2.3"},
			"podman run img:<VER>",
		},
		{
			"subcommand depth stops at two",
			[]string{"kubectl", "get", "pods", "-n", "prod"},
			"kubectl get pods -n prod",
		},
		{
			"after -- everything is an operand",
			[]string{"bashy", "sandbox", "--", "-m", "x"},
			"bashy sandbox -- -m x",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Template(tc.argv, opts()); got != tc.want {
				t.Errorf("Template(%q)\n got %q\nwant %q", tc.argv, got, tc.want)
			}
		})
	}
}

// TestTemplateCollapses is the anti-fragmentation assertion: the whole graph
// depends on two invocations of the same intent producing ONE node.
func TestTemplateCollapses(t *testing.T) {
	pairs := [][2][]string{
		{
			{"git", "commit", "-m", "fix a"},
			{"git", "commit", "-m", "a completely different message"},
		},
		{
			{"ssh", "-p", "2222", "alice@host-a"},
			{"ssh", "-p", "9000", "bob@host-b"},
		},
		{
			{"cat", "/tmp/tmp.aaaaaaaa/x"},
			{"cat", "/tmp/tmp.bbbbbbbb/y"},
		},
	}
	for _, p := range pairs {
		a, b := Template(p[0], opts()), Template(p[1], opts())
		if a != b {
			t.Errorf("should collapse to one node:\n  %q -> %q\n  %q -> %q", p[0], a, p[1], b)
		}
	}
}

// TestTemplateDistinguishes is the other half: collapsing too much is the
// SILENT failure, so these must stay apart.
func TestTemplateDistinguishes(t *testing.T) {
	pairs := [][2][]string{
		{
			{"go", "test", "./internal/..."},
			{"go", "test", "./cmd/..."},
		},
		{
			{"git", "commit", "-m", "x"},
			{"git", "push"},
		},
		{
			{"make", "test"},
			{"make", "build"},
		},
	}
	for _, p := range pairs {
		a, b := Template(p[0], opts()), Template(p[1], opts())
		if a == b {
			t.Errorf("must NOT collapse: %q and %q both -> %q", p[0], p[1], a)
		}
	}
}

func TestTemplateDeterministic(t *testing.T) {
	argv := []string{"go", "test", "-run", "TestX", "./..."}
	first := Template(argv, opts())
	for i := 0; i < 50; i++ {
		if got := Template(argv, opts()); got != first {
			t.Fatalf("not deterministic: %q vs %q", got, first)
		}
	}
}
