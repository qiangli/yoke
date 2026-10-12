package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The native engine's contract with host git: for every argv it serves,
// stdout and exit code are byte-identical to host git; for anything it
// cannot match it returns ErrUnsupported (the caller falls back to host
// git or fails loud). Callers diff these outputs to decide what to
// commit, clean or sync, so a silent divergence is a wrong decision.
//
// TestParityMatrix drives that contract across repository shapes that
// real checkouts take (submodules registered or not, exclude files,
// renames, conflicts, upstream tracking) and the read verbs those
// decisions are made from. A new routed verb or flag belongs here.

// parityCommands are the decision-making read argvs.
var parityCommands = [][]string{
	{"status"},
	{"status", "--short"},
	{"status", "-s"},
	{"status", "--porcelain"},
	{"status", "--porcelain=v1"},
	{"status", "-sb"},
	{"status", "--short", "--branch"},
	{"status", "--porcelain", "-b"},
	{"status", "--short", "-uall"},
	{"status", "--short", "-uno"},
	{"status", "--short", "--ignored"},
	{"status", "--short", "--", "."},
	{"diff"},
	{"diff", "--name-only"},
	{"diff", "--name-status"},
	{"diff", "--stat"},
	{"diff", "--quiet"},
	{"diff", "--exit-code"},
	{"diff", "--cached"},
	{"diff", "--cached", "--name-only"},
	{"diff", "--cached", "--name-status"},
	{"diff", "--cached", "--quiet"},
	{"diff", "--staged", "--name-only"},
	{"diff", "HEAD", "--name-only"},
	{"diff", "HEAD", "--name-status"},
	{"rev-parse", "HEAD"},
	{"rev-parse", "--verify", "HEAD"},
	{"rev-parse", "--abbrev-ref", "HEAD"},
	{"rev-parse", "--show-toplevel"},
	{"rev-parse", "--is-inside-work-tree"},
	{"rev-parse", "--git-dir"},
	{"ls-files"},
	{"ls-files", "-m"},
	{"ls-files", "-d"},
	{"ls-files", "-s"},
	{"ls-files", "--others"},
	{"ls-files", "-o", "--exclude-standard"},
	{"log"},
	{"show"},
	{"show", "HEAD~1"},
	{"diff", "--cached", "--no-renames", "HEAD"},
	{"log", "--oneline"},
	{"log", "--oneline", "-1"},
	{"log", "-1", "--format=%H"},
	{"rev-list", "--count", "HEAD"},
	{"branch"},
	{"branch", "--show-current"},
	{"for-each-ref"},
	{"rev-parse", "--short", "HEAD"},
	{"rev-parse", "HEAD~1"},
	{"merge-base", "HEAD", "HEAD"},
	{"merge-base", "--is-ancestor", "HEAD", "HEAD"},
	{"rev-list", "HEAD"},
	{"rev-list", "--count", "HEAD", "^HEAD"},
	{"log", "--format=%H %s"},
	{"log", "--format=%h %an %ad"},
	{"diff", "--cached", "--no-renames"},
	{"log", "--oneline", "--", "a.txt"},
	{"show", "-s", "--format=%H", "HEAD"},
	{"show", "--stat", "HEAD"},
	{"show", "HEAD"},
	{"show", "HEAD:a.txt"},
	{"remote"},
	{"remote", "-v"},
	{"branch", "-a"},
	{"branch", "-v"},
	{"grep", "-n", "a"},
	{"grep", "-l", "a"},
	{"blame", "a.txt"},
	{"cat-file", "-p", "HEAD"},
	{"cat-file", "-t", "HEAD"},
	{"config", "--get", "user.name"},
	{"symbolic-ref", "HEAD"},
}

type parityScenario struct {
	name  string
	build func(t *testing.T, root string) string // returns the repo dir to query
	// declines marks shapes the native model hands to host git (merge
	// conflicts, staged renames); elsewhere parityMustServe must be served
	// natively, so "decline everything" cannot pass the matrix.
	declines bool
}

// parityMustServe are the forms the native engine must answer itself on
// every ordinary repository shape.
var parityMustServe = map[string]bool{
	"status --short":            true,
	"status --porcelain":        true,
	"status --short -uall":      true,
	"diff --name-only":          true,
	"diff --quiet":              true,
	"diff --cached --name-only": true,
	"diff --cached --quiet":     true,
	"ls-files":                  true,
	"ls-files -m":               true,
	"rev-parse HEAD":            true,
	"rev-parse --short HEAD":    true,
	"log --oneline":             true,
}

func pgit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := osexec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("host git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func pwrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// hostRepo makes dir a repo with tracked a.txt, b.txt, dir/c.txt and
// "with space.txt" in one commit.
func pRepo(t *testing.T, dir string) string {
	t.Helper()
	pgit(t, filepath.Dir(dir), "init", "-q", "-b", "main", dir)
	pwrite(t, filepath.Join(dir, "a.txt"), "a\n")
	pwrite(t, filepath.Join(dir, "b.txt"), "b\n")
	pwrite(t, filepath.Join(dir, "dir", "c.txt"), "c\n")
	pwrite(t, filepath.Join(dir, "with space.txt"), "s\n")
	pgit(t, dir, "add", "-A")
	pgit(t, dir, "commit", "-q", "-m", "init")
	return dir
}

// hostSuperproject makes top/ with submodule "sub" pinned at the sub
// repo's first commit.
func hostSuperproject(t *testing.T, root string) string {
	t.Helper()
	sub := pRepo(t, filepath.Join(root, "subsrc"))
	top := pRepo(t, filepath.Join(root, "top"))
	pgit(t, top, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "sub")
	pgit(t, top, "commit", "-q", "-m", "add sub")
	return top
}

// writeFileQuoted adds untracked names host git C-quotes.
func writeFileQuoted(t *testing.T, r string) {
	t.Helper()
	pwrite(t, filepath.Join(r, "caf\u00e9.txt"), "c\n")
	if runtime.GOOS != "windows" { // NTFS rejects TAB and '"' in names
		pwrite(t, filepath.Join(r, "tab\tname.txt"), "t\n")
		pwrite(t, filepath.Join(r, "quote\"name.txt"), "q\n")
	}
}

func advanceSub(t *testing.T, top string) {
	t.Helper()
	s := filepath.Join(top, "sub")
	pwrite(t, filepath.Join(s, "new.txt"), "n\n")
	pgit(t, s, "add", "new.txt")
	pgit(t, s, "commit", "-q", "-m", "advance")
}

var parityScenarios = []parityScenario{
	{"clean", func(t *testing.T, root string) string {
		return pRepo(t, filepath.Join(root, "r"))
	}, false},
	{"mixed", func(t *testing.T, root string) string {
		r := pRepo(t, filepath.Join(root, "r"))
		pwrite(t, filepath.Join(r, "a.txt"), "a2\n") // unstaged modify
		pwrite(t, filepath.Join(r, "b.txt"), "b2\n") // staged modify
		pgit(t, r, "add", "b.txt")
		pwrite(t, filepath.Join(r, "added.txt"), "x\n") // staged add
		pgit(t, r, "add", "added.txt")
		os.Remove(filepath.Join(r, "dir", "c.txt")) // unstaged delete
		pwrite(t, filepath.Join(r, "untracked.txt"), "u\n")
		pwrite(t, filepath.Join(r, "newdir", "one.txt"), "1\n")
		pwrite(t, filepath.Join(r, "newdir", "deep", "two.txt"), "2\n")
		return r
	}, false},
	{"staged-delete-still-on-disk", func(t *testing.T, root string) string {
		r := pRepo(t, filepath.Join(root, "r"))
		pgit(t, r, "rm", "-q", "--cached", "with space.txt")
		writeFileQuoted(t, r)
		return r
	}, false},
	{"multiline-message", func(t *testing.T, root string) string {
		r := pRepo(t, filepath.Join(root, "r"))
		pwrite(t, filepath.Join(r, "a.txt"), "a2\n")
		pgit(t, r, "commit", "-q", "-am", "subject line\n\nbody one\n  indented two\n\ntrailer: x")
		return r
	}, false},
	{"core-abbrev-12", func(t *testing.T, root string) string {
		r := pRepo(t, filepath.Join(root, "r"))
		pgit(t, r, "config", "core.abbrev", "12")
		pwrite(t, filepath.Join(r, "a.txt"), "a2\n")
		pgit(t, r, "add", "a.txt")
		return r
	}, false},
	{"hunk-function-context", func(t *testing.T, root string) string {
		r := pRepo(t, filepath.Join(root, "r"))
		var src strings.Builder
		src.WriteString("package p\n\nfunc averyveryveryveryveryveryveryveryveryveryverylongfunctionname(argumentone, argumenttwo int) {\n")
		for i := 0; i < 12; i++ {
			fmt.Fprintf(&src, "\tx%d := %d\n", i, i)
		}
		src.WriteString("}\n\n_private := 1\n\nfunc second() {\n")
		for i := 0; i < 12; i++ {
			fmt.Fprintf(&src, "\ty%d := %d\n", i, i)
		}
		src.WriteString("}\n")
		pwrite(t, filepath.Join(r, "p.go"), src.String())
		pgit(t, r, "add", "p.go")
		pgit(t, r, "commit", "-q", "-m", "p")
		edited := strings.Replace(src.String(), "\tx9 := 9", "\tx9 := 99", 1)
		edited = strings.Replace(edited, "\ty10 := 10", "\ty10 := 100", 1)
		pwrite(t, filepath.Join(r, "p.go"), edited)
		pgit(t, r, "commit", "-q", "-am", "edit")
		pwrite(t, filepath.Join(r, "p.go"), strings.Replace(edited, "\ty3 := 3", "\ty3 := 33", 1))
		pgit(t, r, "add", "p.go")
		return r
	}, false},
	{"merged-history", func(t *testing.T, root string) string {
		r := pRepo(t, filepath.Join(root, "r"))
		pgit(t, r, "checkout", "-q", "-b", "side")
		pwrite(t, filepath.Join(r, "side.txt"), "s\n")
		pgit(t, r, "add", "side.txt")
		pgit(t, r, "commit", "-q", "-m", "side")
		pgit(t, r, "checkout", "-q", "main")
		pwrite(t, filepath.Join(r, "main.txt"), "m\n")
		pgit(t, r, "add", "main.txt")
		pgit(t, r, "commit", "-q", "-m", "main")
		pgit(t, r, "merge", "-q", "--no-edit", "side")
		return r
	}, true},
	{"both-staged-and-unstaged", func(t *testing.T, root string) string {
		r := pRepo(t, filepath.Join(root, "r"))
		pwrite(t, filepath.Join(r, "a.txt"), "a2\n")
		pgit(t, r, "add", "a.txt")
		pwrite(t, filepath.Join(r, "a.txt"), "a3\n")
		return r
	}, false},
	{"ignores", func(t *testing.T, root string) string {
		r := pRepo(t, filepath.Join(root, "r"))
		pwrite(t, filepath.Join(r, ".gitignore"), "*.log\nbuild/\n")
		pwrite(t, filepath.Join(r, "dir", ".gitignore"), "local.cfg\n")
		pgit(t, r, "add", ".gitignore", "dir/.gitignore")
		pgit(t, r, "commit", "-q", "-m", "ignores")
		pwrite(t, filepath.Join(r, ".git", "info", "exclude"), "# local\nsecret.json\n")
		pwrite(t, filepath.Join(r, "secret.json"), "{}\n")
		pwrite(t, filepath.Join(r, "x.log"), "l\n")
		pwrite(t, filepath.Join(r, "build", "out.bin"), "o\n")
		pwrite(t, filepath.Join(r, "dir", "local.cfg"), "c\n")
		pwrite(t, filepath.Join(r, "keep.txt"), "k\n")
		return r
	}, false},
	{"rename-staged", func(t *testing.T, root string) string {
		r := pRepo(t, filepath.Join(root, "r"))
		pgit(t, r, "mv", "a.txt", "renamed.txt")
		return r
	}, true},
	{"mode-change", func(t *testing.T, root string) string {
		r := pRepo(t, filepath.Join(root, "r"))
		if runtime.GOOS == "windows" {
			t.Skip("no executable bit on Windows checkouts (core.filemode=false)")
		}
		if err := os.Chmod(filepath.Join(r, "a.txt"), 0o755); err != nil {
			t.Fatal(err)
		}
		return r
	}, false},
	{"detached-head", func(t *testing.T, root string) string {
		r := pRepo(t, filepath.Join(root, "r"))
		pwrite(t, filepath.Join(r, "a.txt"), "a2\n")
		pgit(t, r, "commit", "-q", "-am", "second")
		pgit(t, r, "checkout", "-q", "--detach", "HEAD~1")
		return r
	}, false},
	{"upstream-ahead-behind", func(t *testing.T, root string) string {
		origin := pRepo(t, filepath.Join(root, "origin"))
		pgit(t, root, "clone", "-q", origin, "clone")
		c := filepath.Join(root, "clone")
		pwrite(t, filepath.Join(origin, "a.txt"), "theirs\n")
		pgit(t, origin, "commit", "-q", "-am", "theirs")
		pgit(t, c, "fetch", "-q")
		pwrite(t, filepath.Join(c, "b.txt"), "ours\n")
		pgit(t, c, "commit", "-q", "-am", "ours")
		return c
	}, false},
	{"merge-conflict", func(t *testing.T, root string) string {
		r := pRepo(t, filepath.Join(root, "r"))
		pgit(t, r, "checkout", "-q", "-b", "other")
		pwrite(t, filepath.Join(r, "a.txt"), "other\n")
		pgit(t, r, "commit", "-q", "-am", "other")
		pgit(t, r, "checkout", "-q", "main")
		pwrite(t, filepath.Join(r, "a.txt"), "main\n")
		pgit(t, r, "commit", "-q", "-am", "main")
		cmd := osexec.Command("git", "merge", "-q", "other")
		cmd.Dir = r
		_ = cmd.Run() // conflicts by design
		return r
	}, true},
	{"submodule-registered-moved", func(t *testing.T, root string) string {
		top := hostSuperproject(t, root)
		advanceSub(t, top)
		return top
	}, false},
	{"submodule-unregistered-moved", func(t *testing.T, root string) string {
		top := hostSuperproject(t, root)
		pgit(t, top, "config", "--remove-section", "submodule.sub")
		advanceSub(t, top)
		return top
	}, false},
	{"alternates-clone", func(t *testing.T, root string) string {
		// Objects borrowed through an absolute objects/info/alternates
		// path outside the repository (clone --reference, submodules).
		origin := pRepo(t, filepath.Join(root, "origin"))
		pgit(t, root, "clone", "-q", "--no-local", "--reference", origin, origin, "clone")
		return filepath.Join(root, "clone")
	}, false},
	{"submodule-via-alternates", func(t *testing.T, root string) string {
		sub := pRepo(t, filepath.Join(root, "subsrc"))
		top := pRepo(t, filepath.Join(root, "top"))
		pgit(t, top, "-c", "protocol.file.allow=always", "submodule", "add", "-q", "--reference", sub, "file://"+filepath.ToSlash(sub), "sub")
		pgit(t, top, "commit", "-q", "-m", "add sub")
		return top
	}, false},
	{"submodule-dirty-content", func(t *testing.T, root string) string {
		top := hostSuperproject(t, root)
		pwrite(t, filepath.Join(top, "sub", "a.txt"), "dirty\n")
		pwrite(t, filepath.Join(top, "sub", "stray.txt"), "s\n")
		return top
	}, false},
}

func TestParityMatrix(t *testing.T) {
	if _, err := osexec.LookPath("git"); err != nil {
		t.Skip("no host git on PATH")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	ctx := context.Background()

	for _, sc := range parityScenarios {
		t.Run(sc.name, func(t *testing.T) {
			dir := sc.build(t, t.TempDir())
			for _, argv := range parityCommands {
				name := strings.Join(argv, " ")
				native, err := Exec(ctx, dir, argv)
				if errors.Is(err, ErrUnsupported) {
					if parityMustServe[name] && !sc.declines {
						t.Errorf("git %s: declined natively; it must be served", name)
					}
					continue // declined: the caller falls back to host git
				}
				if err != nil {
					t.Errorf("%s: native error %v", name, err)
					continue
				}
				host, err := runHost(ctx, dir, argv)
				if err != nil {
					t.Fatalf("%s: host: %v", name, err)
				}
				if native.Stdout != host.Stdout || native.ExitCode != host.ExitCode {
					t.Errorf("git %s diverges\n native (exit %d):\n%s\n host (exit %d):\n%s",
						name, native.ExitCode, native.Stdout, host.ExitCode, host.Stdout)
				}
			}
		})
	}
}
