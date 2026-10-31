package src_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	ygit "github.com/qiangli/yoke/git"
	"github.com/qiangli/yoke/pkg/src"
)

const (
	s526b8App   = "demoapp"
	s526b8Alice = "device:alice"
)

// s526b8Tree is the shaped fixture: one unit with a flag and an argument,
// one public route, and two stored properties, so every surface kind the
// B6 compare reads is present.
func s526b8Tree(notesUnit, shaped string) map[string]string {
	return map[string]string{
		"app.txt":           "app-base\n",
		"shaped.json":       shaped,
		"api/notes.md":      notesUnit,
		"public/index.html": "<h1>notes</h1>\n",
	}
}

const (
	s526b8Notes  = "---\nflags: [--all]\nargs: [limit]\n---\n# notes\n\nlist the notes\n"
	s526b8Shaped = `{"properties":["title","body"]}` + "\n"
)

func s526b8Write(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func s526b8Commit(t *testing.T, dir, msg string) string {
	t.Helper()
	if _, err := ygit.Add(ygit.AddOptions{RepoPath: dir, All: true}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := ygit.Commit(ygit.CommitOptions{
		RepoPath: dir, Message: msg, AuthorName: "s526b8", AuthorEmail: "s526b8@example.com",
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	rev, err := ygit.RevParse(ygit.RevParseOptions{RepoPath: dir})
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	return rev.Hash
}

// s526b8Fixture clones a shaped work repo to home/src/demoapp with pristine
// at the base release and returns the checkout plus the base commit.
func s526b8Fixture(t *testing.T, home string) (dest, base string) {
	t.Helper()
	work := filepath.Join(t.TempDir(), "work")
	if _, err := ygit.Init(ygit.InitOptions{Path: work}); err != nil {
		t.Fatalf("init: %v", err)
	}
	s526b8Write(t, work, s526b8Tree(s526b8Notes, s526b8Shaped))
	base = s526b8Commit(t, work, "p0 base release")
	dest = filepath.Join(home, "src", s526b8App)
	if _, err := gogit.PlainClone(dest, false, &gogit.CloneOptions{URL: work}); err != nil {
		t.Fatalf("clone: %v", err)
	}
	r, err := gogit.PlainOpen(dest)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Storer.SetReference(plumbing.NewHashReference("refs/heads/pristine", plumbing.NewHash(base))); err != nil {
		t.Fatalf("pristine: %v", err)
	}
	return dest, base
}

var pseudoVersionRE = regexp.MustCompile(`^v0\.0\.0-\d{14}-[0-9a-f]{12}$`)

// TestS526B8SemverReleases is the focused S526.B8 proof on a shaped
// fixture: the first release is v1.0.0; a repair is v1.0.1; an additive
// change is v1.1.0; a rename is refused until it is released through the
// contract step as v2.0.0; an unreleased revision is named by a
// pseudo-version; and offerings publish releases only.
func TestS526B8SemverReleases(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	dest, base := s526b8Fixture(t, home)

	// An unreleased revision is named by a Go-style pseudo-version built
	// from the commit time and the first 12 hex digits of the commit.
	v, released, err := src.Version(home, s526b8App, base)
	if err != nil {
		t.Fatal(err)
	}
	if released || !pseudoVersionRE.MatchString(v) || !strings.HasSuffix(v, "-"+base[:12]) {
		t.Fatalf("unreleased version = %q (released=%v), want pseudo-version ending in %s", v, released, base[:12])
	}
	r, err := gogit.PlainOpen(dest)
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.CommitObject(plumbing.NewHash(base))
	if err != nil {
		t.Fatal(err)
	}
	if want := "v0.0.0-" + c.Committer.When.UTC().Format("20060102150405") + "-" + base[:12]; v != want {
		t.Fatalf("pseudo-version = %q, want %q", v, want)
	}

	// Offerings publish tagged releases only.
	if _, err := src.Offer(home, s526b8App, base, s526b8Alice, src.OfferOptions{All: true}); err == nil ||
		!strings.Contains(err.Error(), "not a released version") || !strings.Contains(err.Error(), v) {
		t.Fatalf("offer of an unreleased revision: err = %v, want refusal naming %s", err, v)
	}

	// First release: v1.0.0.
	first, err := src.Release(home, s526b8App, base, s526b8Alice, src.ReleaseOptions{})
	if err != nil {
		t.Fatalf("first release: %v", err)
	}
	if first.Version != "v1.0.0" || first.Kind != src.ReleaseInitial || first.Revision != base {
		t.Fatalf("first release = %+v, want v1.0.0 initial at %s", first, base)
	}
	if v, released, err = src.Version(home, s526b8App, base); err != nil || !released || v != "v1.0.0" {
		t.Fatalf("version after release = %q (released=%v, err=%v), want v1.0.0", v, released, err)
	}
	if v, _, err = src.Version(home, s526b8App, "v1.0.0"); err != nil || v != "v1.0.0" {
		t.Fatalf("the tag resolves as a revision: %q, %v", v, err)
	}
	if _, err := src.Release(home, s526b8App, base, s526b8Alice, src.ReleaseOptions{}); err == nil ||
		!strings.Contains(err.Error(), "already released as v1.0.0") {
		t.Fatalf("re-releasing the same revision: err = %v", err)
	}

	// A repair (same declared surface, body changed) -> v1.0.1.
	s526b8Write(t, dest, map[string]string{"api/notes.md": strings.Replace(s526b8Notes, "list the notes", "list the notes, fixed", 1)})
	repair := s526b8Commit(t, dest, "repair notes")
	rel, err := src.Release(home, s526b8App, repair, s526b8Alice, src.ReleaseOptions{Repair: true})
	if err != nil {
		t.Fatalf("repair release: %v", err)
	}
	if rel.Version != "v1.0.1" || rel.Kind != src.ReleasePatch || rel.Previous != "v1.0.0" || len(rel.Added) != 0 {
		t.Fatalf("repair release = %+v, want v1.0.1 patch after v1.0.0", rel)
	}

	// An additive change (a new flag and a new stored property) -> v1.1.0,
	// and the same change declared a repair is refused: a repair keeps the
	// declared surface.
	s526b8Write(t, dest, map[string]string{
		"api/notes.md": strings.Replace(s526b8Notes, "flags: [--all]", "flags: [--all, --pinned]", 1),
		"shaped.json":  `{"properties":["title","body","pinned"]}` + "\n",
	})
	additive := s526b8Commit(t, dest, "add pinned")
	if _, err := src.Release(home, s526b8App, additive, s526b8Alice, src.ReleaseOptions{Repair: true}); err == nil ||
		!strings.Contains(err.Error(), "repair keeps the declared surface") {
		t.Fatalf("additive change released as a repair: err = %v", err)
	}
	rel, err = src.Release(home, s526b8App, additive, s526b8Alice, src.ReleaseOptions{})
	if err != nil {
		t.Fatalf("additive release: %v", err)
	}
	if rel.Version != "v1.1.0" || rel.Kind != src.ReleaseMinor || rel.Previous != "v1.0.1" {
		t.Fatalf("additive release = %+v, want v1.1.0 minor after v1.0.1", rel)
	}
	if got := strings.Join(rel.Added, ","); got != "flag:notes --pinned,property:pinned" {
		t.Fatalf("added = %q", got)
	}

	// A rename (property title -> heading) is refused until it is released
	// through the contract step, which makes it v2.0.0.
	s526b8Write(t, dest, map[string]string{"shaped.json": `{"properties":["heading","body","pinned"]}` + "\n"})
	rename := s526b8Commit(t, dest, "rename title to heading")
	_, err = src.Release(home, s526b8App, rename, s526b8Alice, src.ReleaseOptions{})
	if err == nil || !strings.Contains(err.Error(), "release refused") ||
		!strings.Contains(err.Error(), "property:title was renamed or replaced by property:heading") ||
		!strings.Contains(err.Error(), `--contract "property:title"`) {
		t.Fatalf("rename without the contract step: err = %v", err)
	}
	if tags, _ := src.Releases(home, s526b8App); len(tags) != 3 || tags[0].Version != "v1.1.0" {
		t.Fatalf("a refused release tagged something: %+v", tags)
	}
	rel, err = src.Release(home, s526b8App, rename, s526b8Alice, src.ReleaseOptions{Contract: []string{"property:title"}})
	if err != nil {
		t.Fatalf("contract release: %v", err)
	}
	if rel.Version != "v2.0.0" || rel.Kind != src.ReleaseMajor || rel.Previous != "v1.1.0" ||
		strings.Join(rel.Contracted, ",") != "property:title" || strings.Join(rel.Added, ",") != "property:heading" {
		t.Fatalf("contract release = %+v, want v2.0.0 major dropping property:title", rel)
	}

	// The release list is newest version first and carries the kind; the
	// tag is annotated by the releasing principal.
	tags, err := src.Releases(home, s526b8App)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tg := range tags {
		got = append(got, tg.Version+":"+tg.Kind)
		if tg.When.IsZero() || tg.When.Location() != time.UTC {
			t.Fatalf("release %s has no UTC time: %v", tg.Version, tg.When)
		}
	}
	if want := "v2.0.0:major,v1.1.0:minor,v1.0.1:patch,v1.0.0:initial"; strings.Join(got, ",") != want {
		t.Fatalf("releases = %v, want %s", got, want)
	}

	// A released revision can be offered, and the offering carries the
	// version.
	off, err := src.Offer(home, s526b8App, rename, s526b8Alice, src.OfferOptions{All: true})
	if err != nil {
		t.Fatalf("offer of a release: %v", err)
	}
	if off.Version != "v2.0.0" || off.Revision != rename {
		t.Fatalf("offering = %+v, want v2.0.0 at %s", off, rename)
	}

	// History names every version: the release tag where released, a
	// pseudo-version otherwise, and lists the releases.
	gate := s525a8Gate()
	if _, err := src.Activate(home, s526b8App, additive, s526b8Alice, src.ActivateOptions{Gate: gate, Request: "pinned notes"}); err != nil {
		t.Fatal(err)
	}
	s526b8Write(t, dest, map[string]string{"app.txt": "app-unreleased\n"})
	unreleased := s526b8Commit(t, dest, "unreleased tweak")
	if _, err := src.Activate(home, s526b8App, unreleased, s526b8Alice, src.ActivateOptions{Gate: gate, Request: "tweak"}); err != nil {
		t.Fatal(err)
	}
	h, err := src.History(home, s526b8App, s526b8Alice)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Entries) != 2 || !pseudoVersionRE.MatchString(h.Entries[0].Version) || h.Entries[1].Version != "v1.1.0" {
		t.Fatalf("history versions = %+v", h.Entries)
	}
	if len(h.Releases) != 4 || h.Releases[0].Version != "v2.0.0" {
		t.Fatalf("history releases = %+v", h.Releases)
	}

	// Tags stay compatible with the tidy base (S526.B7): a released
	// revision is a supported version, so a tidy that keeps only the tip
	// (plus the last-known-good) squashes the unreleased v1.1.0 successor
	// but keeps v1.1.0 itself, and after the tidy the tag still names it
	// and history still shows it.
	for _, body := range []string{"app-unreleased-2\n", "app-unreleased-3\n"} {
		s526b8Write(t, dest, map[string]string{"app.txt": body})
		rev := s526b8Commit(t, dest, "unreleased tweak "+body)
		if _, err := src.Activate(home, s526b8App, rev, s526b8Alice, src.ActivateOptions{Gate: gate, Request: "tweak"}); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := src.PlanTidy(home, s526b8App, s526b8Alice, src.TidyPolicy{KeepLast: 1, KeepDays: 0})
	if err != nil {
		t.Fatal(err)
	}
	var kept, squashed []string
	for _, e := range plan.Kept {
		kept = append(kept, e.Version)
	}
	for _, e := range plan.Squashed {
		squashed = append(squashed, e.Revision)
	}
	if len(kept) != 3 || kept[2] != "v1.1.0" || strings.Join(squashed, ",") != unreleased {
		t.Fatalf("tidy plan kept %v squashed %v, want v1.1.0 kept and %s squashed", kept, squashed, unreleased[:7])
	}
	if err := src.ApplyTidy(home, plan); err != nil {
		t.Fatal(err)
	}
	if v, released, err := src.Version(home, s526b8App, "v1.1.0"); err != nil || !released || v != "v1.1.0" {
		t.Fatalf("after tidy, v1.1.0 = %q (released=%v, err=%v)", v, released, err)
	}
	h, err = src.History(home, s526b8App, s526b8Alice)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Entries) != 3 || h.Entries[2].Version != "v1.1.0" || h.Entries[2].Revision != additive || len(h.Releases) != 4 {
		t.Fatalf("history after tidy = %+v", h.Entries)
	}
}
