package kb

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestDefaultsDirResolution(t *testing.T) {
	t.Setenv(DefaultsDirEnv, "/explicit/defaults")
	if got := DefaultsDir(); got != "/explicit/defaults" {
		t.Fatalf("env override: %q", got)
	}
	t.Setenv(DefaultsDirEnv, "")
	t.Setenv("BASHY_HOME", "/bh")
	if got, want := DefaultsDir(), filepath.Join("/bh", "src", "bashy", "kb", "defaults"); got != want {
		t.Fatalf("BASHY_HOME: %q, want %q", got, want)
	}
	t.Setenv("BASHY_HOME", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if got, want := DefaultsDir(), filepath.Join(home, ".bashy", "src", "bashy", "kb", "defaults"); got != want {
		t.Fatalf("HOME fallback: %q, want %q", got, want)
	}
}

func TestMergeDefaultsLocalWins(t *testing.T) {
	local := []*Page{{Slug: "a", Title: "local a"}}
	defs := []*Page{{Slug: "a", Title: "default a", Default: true}, {Slug: "b", Title: "default b", Default: true}}
	got := MergeDefaults(local, defs)
	if len(got) != 2 || got[0].Title != "local a" || got[1].Slug != "b" || !got[1].Default {
		t.Fatalf("merge = %+v", got)
	}
	if MergeDefaults(local, nil)[0] != local[0] {
		t.Fatal("no defaults must return local unchanged")
	}
}

// snapshot lists every file under dir with its size, to prove a read left
// the read-only store untouched.
func snapshot(t *testing.T, dir string) string {
	t.Helper()
	var rows []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			fi, _ := d.Info()
			rows = append(rows, p+":"+fi.ModTime().String()+":"+string(rune(fi.Size())))
		}
		return nil
	})
	sort.Strings(rows)
	return strings.Join(rows, "\n")
}

func kbOut(t *testing.T, args ...string) string {
	t.Helper()
	cmd := NewKBCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("kb %v: %v", args, err)
	}
	return out.String()
}

// The default store is read after the writable ring, labelled, shadowed by
// a local page with the same slug, and never written by a read.
func TestSearchAndShowReadDefaultsReadOnly(t *testing.T) {
	isolateKB(t)
	defDir := t.TempDir()
	t.Setenv(DefaultsDirEnv, defDir)
	defs := OpenRepo(defDir)
	for _, p := range []*Page{
		{Slug: "outpost-setup", Form: FormPage, Type: TypeRunbook, Status: StatusValidated,
			Title: "Set up outpost", Description: "install and pair outpost", Body: "default body: outpost register"},
		{Slug: "podman-machine", Form: FormPage, Type: TypeFact, Status: StatusValidated,
			Title: "One podman machine", Description: "podman machine per host", Body: "default podman body"},
	} {
		if err := defs.Write(p, "add"); err != nil {
			t.Fatal(err)
		}
	}
	local := t.TempDir()
	before := snapshot(t, defDir)

	out := kbOut(t, "--dir", local, "search", "outpost")
	if !strings.Contains(out, "outpost-setup") || !strings.Contains(out, "[default validated/runbook]") {
		t.Fatalf("search did not return the labelled default page:\n%s", out)
	}
	if got := kbOut(t, "--dir", local, "show", "outpost-setup"); !strings.Contains(got, "default body: outpost register") {
		t.Fatalf("show did not fall back to the default page:\n%s", got)
	}
	if after := snapshot(t, defDir); after != before {
		t.Fatalf("a read changed the default store:\nbefore:\n%s\nafter:\n%s", before, after)
	}

	// A local page with the same slug wins everywhere.
	if err := Open(local).Write(&Page{Slug: "outpost-setup", Form: FormPage, Type: TypeRunbook, Status: StatusCandidate,
		Title: "My outpost setup", Description: "local outpost notes", Body: "local body"}, "add"); err != nil {
		t.Fatal(err)
	}
	out = kbOut(t, "--dir", local, "search", "outpost")
	if strings.Contains(out, "[default") || !strings.Contains(out, "My outpost setup") {
		t.Fatalf("local page did not shadow the default:\n%s", out)
	}
	if got := kbOut(t, "--dir", local, "show", "outpost-setup"); !strings.Contains(got, "local body") {
		t.Fatalf("show did not prefer the local page:\n%s", got)
	}
	if got := kbOut(t, "--dir", local, "list"); !strings.Contains(got, "podman-machine") || strings.Count(got, "outpost-setup") != 1 {
		t.Fatalf("list should hold the local page once plus the other default:\n%s", got)
	}
}

// Without a fetched source pin there is no default store and nothing changes.
func TestNoDefaultsStoreIsANoop(t *testing.T) {
	isolateKB(t)
	t.Setenv(DefaultsDirEnv, filepath.Join(t.TempDir(), "absent"))
	if OpenDefaults() != nil || DefaultPages() != nil {
		t.Fatal("absent store must open as nil")
	}
	if _, err := os.Stat(DefaultsDir()); err == nil {
		t.Fatal("a read created the absent store")
	}
}
