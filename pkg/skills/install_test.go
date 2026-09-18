package skills

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// A valid canonical face: cap {read, write}, contract {gereeni}.
const testCanon = "sokilili demo efefecato reada wurite fini enisure gereeni fini fini\n"

func writeSkillDir(t *testing.T, root, name, frontmatter, canon string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(frontmatter+"\n# body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if canon != "" {
		if err := os.WriteFile(filepath.Join(dir, "skill.dhnt"), []byte(canon), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestDhntInfoInCatalog(t *testing.T) {
	embedded := fstest.MapFS{
		"dual-good/SKILL.md":   {Data: []byte("---\nname: dual-good\ndescription: ok\n---\nbody\n")},
		"dual-good/skill.dhnt": {Data: []byte(testCanon)},
		"dual-bad/SKILL.md":    {Data: []byte("---\nname: dual-bad\ndescription: broken face\n---\nbody\n")},
		"dual-bad/skill.dhnt":  {Data: []byte("not canonical CONTENT!\n")},
	}
	cat := &Catalog{Sources: []Source{EmbedSource(embedded, RingEmbedded)}}
	ps := testProbes(t, map[string]string{"os": "linux", "arch": "arm64"}, nil)
	rows, err := cat.List(ps)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Listing{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	good := byName["dual-good"]
	if !good.Dhnt.Valid() || !strings.HasPrefix(good.Dhnt.Identity, "h") ||
		len(good.Dhnt.Contract) != 1 || good.Dhnt.Contract[0] != "gereeni" ||
		len(good.Dhnt.EffectCap) != 2 {
		t.Errorf("dual-good: %+v", good.Dhnt)
	}
	bad := byName["dual-bad"]
	if bad.Dhnt.Valid() || bad.Dhnt == nil || bad.Dhnt.Err == "" {
		t.Errorf("dual-bad: %+v", bad.Dhnt)
	}
	// An invalid canonical face degrades — the prose skill stays listed.
	if !bad.Verdict.Applicable {
		t.Errorf("dual-bad hidden: %+v", bad.Verdict)
	}
}

func TestAdmit(t *testing.T) {
	ps := testProbes(t,
		map[string]string{"os": "linux", "arch": "arm64"},
		map[string]string{"git": "2.49.0"},
	)
	src := t.TempDir()

	dir := writeSkillDir(t, src, "good-skill",
		"---\nname: good-skill\ndescription: fine\nmetadata:\n  requires: \"os=linux has=git\"\n---", testCanon)
	sk, err := loadSkillDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := admit(sk, ps)
	if !a.Valid || !a.Applicable || !strings.HasPrefix(a.Identity, "h") || a.ContextKey == "" {
		t.Errorf("good-skill admission: %+v", a)
	}

	// Missing description fails the gate.
	dir = writeSkillDir(t, src, "no-desc", "---\nname: no-desc\n---", "")
	sk, err = loadSkillDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if a := admit(sk, ps); a.Valid {
		t.Errorf("no-desc admitted: %+v", a)
	}

	// Broken canonical face fails the gate.
	dir = writeSkillDir(t, src, "bad-face", "---\nname: bad-face\ndescription: x\n---", "NOT canonical\n")
	sk, err = loadSkillDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if a := admit(sk, ps); a.Valid {
		t.Errorf("bad-face admitted: %+v", a)
	}

	// Unparsable requires fails the gate.
	dir = writeSkillDir(t, src, "bad-req", "---\nname: bad-req\ndescription: x\nmetadata:\n  requires: \"os=\"\n---", "")
	sk, err = loadSkillDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if a := admit(sk, ps); a.Valid {
		t.Errorf("bad-req admitted: %+v", a)
	}

	// Inapplicable-here is reported, NOT a validity failure.
	dir = writeSkillDir(t, src, "elsewhere", "---\nname: elsewhere\ndescription: x\nmetadata:\n  requires: \"os=plan9\"\n---", "")
	sk, err = loadSkillDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	a = admit(sk, ps)
	if !a.Valid || a.Applicable {
		t.Errorf("elsewhere admission: %+v", a)
	}
}

func TestCLIAddVerify(t *testing.T) {
	store := t.TempDir()
	f := &cobraRunner{t: t, opts: []Option{WithConfigDir(store)}}

	src := t.TempDir()
	dir := writeSkillDir(t, src, "port-check",
		"---\nname: port-check\ndescription: says hello to a port\nmetadata:\n  requires: \"os=linux,darwin,windows\"\n---", testCanon)

	stdout, _, err := f.run("add", dir)
	if err != nil {
		t.Fatalf("add: %v\n%s", err, stdout)
	}
	if !strings.Contains(stdout, "identity: h") || !strings.Contains(stdout, "installed: ") {
		t.Fatalf("add output: %q", stdout)
	}
	if _, err := os.Stat(filepath.Join(store, "port-check", "skill.dhnt")); err != nil {
		t.Fatalf("bundle not copied: %v", err)
	}

	// Installed skill shows up in list (ring local) and verifies.
	stdout, _, err = f.run("list", "--json", "--all")
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		if r["name"] == "port-check" {
			found = true
			if r["ring"] != "local" || r["identity"] == nil {
				t.Errorf("row: %+v", r)
			}
		}
	}
	if !found {
		t.Fatalf("port-check not listed: %s", stdout)
	}

	stdout, _, err = f.run("verify", "port-check")
	if err != nil {
		t.Fatalf("verify: %v\n%s", err, stdout)
	}
	if !strings.Contains(stdout, "valid: true") || !strings.Contains(stdout, "applicable: true") {
		t.Fatalf("verify output: %q", stdout)
	}

	// Re-add without --force refuses; with --force replaces.
	if _, _, err := f.run("add", dir); err == nil {
		t.Fatal("re-add without --force succeeded")
	}
	if _, _, err := f.run("add", dir, "--force"); err != nil {
		t.Fatalf("re-add --force: %v", err)
	}

	// Admission failure: nothing installed.
	bad := writeSkillDir(t, src, "bad-face", "---\nname: bad-face\ndescription: x\n---", "BROKEN\n")
	if _, _, err := f.run("add", bad); err == nil {
		t.Fatal("bad-face admitted")
	}
	if _, err := os.Stat(filepath.Join(store, "bad-face")); err == nil {
		t.Fatal("bad-face installed despite failing the gate")
	}

	// verify exit-fails on an inapplicable skill but still reports.
	np := writeSkillDir(t, src, "not-here", "---\nname: not-here\ndescription: x\nmetadata:\n  requires: \"os=plan9\"\n---", "")
	if _, _, err := f.run("add", np); err != nil {
		t.Fatalf("not-here add: %v", err)
	}
	out, _, err := f.run("verify", "not-here")
	if err == nil {
		t.Fatal("verify not-here exited 0")
	}
	if !strings.Contains(out, "applicable: false") {
		t.Fatalf("verify output: %q", out)
	}

	// URL sources are a loud not-yet.
	if _, _, err := f.run("add", "https://host.invalid/skill"); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("url add: %v", err)
	}
}

func TestCopyDirSkipsIrregular(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", filepath.Join(src, "escape")); err != nil {
		t.Skip("symlinks unavailable")
	}
	dst := filepath.Join(t.TempDir(), "out")
	if err := copyDir(src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "escape")); err == nil {
		t.Fatal("symlink copied")
	}
	if _, err := os.Stat(filepath.Join(dst, "SKILL.md")); err != nil {
		t.Fatal("regular file missing")
	}
}
