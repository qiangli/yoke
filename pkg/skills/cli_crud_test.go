package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// crudFixture: an embedded ring with one gated skill carrying unknown
// frontmatter keys, a comment, and a body, over a throwaway local store.
func crudFixture(t *testing.T) *cobraRunner {
	t.Helper()
	hermeticScrub(t)
	store := storeEnv(t)
	embedded := fstest.MapFS{
		"kept/SKILL.md":     {Data: []byte("---\nname: kept\ndescription: embedded original\n# a comment the author left\nmetadata:\n  requires: \"os=linux,darwin,windows\"\n  author: someone   # trailing\n  check-tests: go test ./...\nlicense: MIT\n---\n\n# kept\n\nBody line one.\n  indented, trailing spaces   \n")},
		"kept/reference.md": {Data: []byte("KEPT REFERENCE\n")},
	}
	return &cobraRunner{t: t, opts: []Option{
		WithSource(EmbedSource(embedded, RingEmbedded)),
		WithConfigDir(store),
	}}
}

// add <name> --description writes a minimal SKILL.md — name, description,
// requires, the body given — that `list` admits, and nothing else.
func TestCLIAddFromFlags(t *testing.T) {
	f := crudFixture(t)
	body := filepath.Join(t.TempDir(), "body.md")
	os.WriteFile(body, []byte("# fresh\n\nDo the thing.\n"), 0o644)
	out, _, err := f.run("add", "fresh", "--description", "made from flags", "--requires", "os=linux,darwin,windows", "--body", body)
	if err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	md, err := os.ReadFile(filepath.Join(DefaultStoreDir(), "fresh", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	want := "---\nname: fresh\ndescription: made from flags\nmetadata:\n  requires: os=linux,darwin,windows\n---\n# fresh\n\nDo the thing.\n"
	if string(md) != want {
		t.Errorf("SKILL.md:\n%s\nwant:\n%s", md, want)
	}
	list, _, err := f.run("list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(list, "fresh\n") {
		t.Errorf("list does not admit fresh: %q", list)
	}

	// No body flag: the file ends at the closing line — no template.
	if _, _, err := f.runIn("BODY FROM STDIN\n", "add", "bare", "--description", "no body", "--body", "-"); err != nil {
		t.Fatal(err)
	}
	if md, _ := os.ReadFile(filepath.Join(DefaultStoreDir(), "bare", "SKILL.md")); string(md) != "---\nname: bare\ndescription: no body\n---\nBODY FROM STDIN\n" {
		t.Errorf("stdin body: %q", md)
	}
	if _, _, err := f.run("add", "empty", "--description", "nothing after"); err != nil {
		t.Fatal(err)
	}
	if md, _ := os.ReadFile(filepath.Join(DefaultStoreDir(), "empty", "SKILL.md")); string(md) != "---\nname: empty\ndescription: nothing after\n---\n" {
		t.Errorf("bodiless: %q", md)
	}

	// Refusals: no description, a bad requires, a name that is a path.
	if _, _, err := f.run("add", "nodesc", "--requires", "os=linux"); err == nil || !strings.Contains(err.Error(), "--description") {
		t.Errorf("no description: %v", err)
	}
	if _, _, err := f.run("add", "badreq", "--description", "x", "--requires", "os == linux"); err == nil {
		t.Error("bad requires admitted")
	}
	if _, err := os.Stat(filepath.Join(DefaultStoreDir(), "badreq")); err == nil {
		t.Error("badreq installed despite the bad requires")
	}
	if _, _, err := f.run("add", "../escape", "--description", "x"); err == nil {
		t.Error("path-shaped name admitted")
	}
	// A description that would read as a non-string is quoted, not mangled.
	if _, _, err := f.run("add", "truthy", "--description", "true"); err != nil {
		t.Fatal(err)
	}
	if out, _, _ := f.run("show", "truthy", "--yaml"); !strings.Contains(out, "description: \"true\"") {
		t.Errorf("truthy description not quoted:\n%s", out)
	}
}

func TestCLIRm(t *testing.T) {
	f := crudFixture(t)
	// Embedded-only: refused with the shadow hint; nothing changes.
	_, _, err := f.run("rm", "kept")
	if err == nil || !strings.Contains(err.Error(), "embedded entries are immutable") || !strings.Contains(err.Error(), "shadow it with `skill add`") {
		t.Fatalf("embedded rm: %v", err)
	}
	if _, _, err := f.run("show", "kept"); err != nil {
		t.Errorf("embedded skill gone after refused rm: %v", err)
	}
	if _, _, err := f.run("rm", "missing"); err == nil {
		t.Error("rm missing succeeded")
	}

	// Shadow it, then rm unshadows rather than deletes.
	if _, _, err := f.run("add", "kept", "--description", "local shadow"); err != nil {
		t.Fatal(err)
	}
	if _, errOut, _ := f.run("show", "kept"); !strings.Contains(errOut, "ring=local") {
		t.Fatalf("shadow not in place: %q", errOut)
	}
	out, errOut, err := f.run("rm", "kept")
	if err != nil {
		t.Fatal(err)
	}
	if out != "removed skill kept\n" || !strings.Contains(errOut, "unshadowed") || !strings.Contains(errOut, "embedded") {
		t.Errorf("rm output: %q / %q", out, errOut)
	}
	if _, err := os.Stat(filepath.Join(DefaultStoreDir(), "kept")); err == nil {
		t.Error("local folder survived rm")
	}
	if show, errOut, _ := f.run("show", "kept"); !strings.Contains(show, "embedded original") || !strings.Contains(errOut, "ring=embedded") {
		t.Errorf("original not unshadowed: %q / %q", show, errOut)
	}

	// A local-only skill: removed outright, no unshadow note.
	if _, _, err := f.run("add", "solo", "--description", "local only"); err != nil {
		t.Fatal(err)
	}
	if _, errOut, err := f.run("rm", "solo"); err != nil || errOut != "" {
		t.Errorf("solo rm: %v / %q", err, errOut)
	}
	if _, _, err := f.run("show", "solo"); err == nil {
		t.Error("solo still resolves")
	}
}

// set materialises a lower-ring skill into the local store first, then edits
// the frontmatter keeping every key it was not asked about — unknown keys,
// comments, quoting — and the body byte for byte.
func TestCLISetMaterialisesAndPreserves(t *testing.T) {
	f := crudFixture(t)
	original, _, _ := f.run("show", "kept")
	out, errOut, err := f.run("set", "kept", "--description", "edited here", "--binding", "step-reada=cat SKILL.md", "--rm-binding", "check-tests")
	if err != nil {
		t.Fatalf("set: %v\n%s", err, out)
	}
	if out != "kept\n" || !strings.Contains(errOut, "note: copied kept from the embedded ring into the local store") {
		t.Errorf("set output: %q / %q", out, errOut)
	}
	md, err := os.ReadFile(filepath.Join(DefaultStoreDir(), "kept", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(md)
	for _, want := range []string{
		"description: edited here\n",
		"# a comment the author left\n",
		"requires: \"os=linux,darwin,windows\"\n", // quoting kept
		"author: someone # trailing\n",            // unknown key + its comment
		"step-reada: cat SKILL.md\n",
		"license: MIT\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("SKILL.md lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "check-tests") {
		t.Errorf("rm-binding left the key:\n%s", got)
	}
	// The body is the original's, byte for byte.
	_, origBody, _ := strings.Cut(original, "\n---\n")
	_, gotBody, _ := strings.Cut(got, "\n---\n")
	if gotBody != origBody {
		t.Errorf("body changed:\n%q\nwant\n%q", gotBody, origBody)
	}
	// The whole folder was materialised, not just SKILL.md.
	if _, err := os.Stat(filepath.Join(DefaultStoreDir(), "kept", "reference.md")); err != nil {
		t.Errorf("reference.md not materialised: %v", err)
	}
	// A second set is local already: no note.
	if _, errOut, err := f.run("set", "kept", "--description", "again"); err != nil || strings.Contains(errOut, "note:") {
		t.Errorf("second set: %v / %q", err, errOut)
	}
	// The embedded original is untouched — rm unshadows back to it.
	if _, _, err := f.run("rm", "kept"); err != nil {
		t.Fatal(err)
	}
	if back, _, _ := f.run("show", "kept"); back != original {
		t.Errorf("embedded original changed:\n%s", back)
	}
}

func TestCLISetRequiresValidatedAndGated(t *testing.T) {
	f := crudFixture(t)
	if _, _, err := f.run("add", "mine", "--description", "local", "--requires", "os=linux,darwin,windows"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(DefaultStoreDir(), "mine", "SKILL.md"))

	// A requires that does not parse is refused before anything is written.
	if _, _, err := f.run("set", "mine", "--requires", "os == plan9"); err == nil {
		t.Error("bad requires accepted")
	}
	after, _ := os.ReadFile(filepath.Join(DefaultStoreDir(), "mine", "SKILL.md"))
	if string(after) != string(before) {
		t.Errorf("store changed by a refused set:\n%s", after)
	}
	// Bad binding keys, bad --file specs, and SKILL.md via --file are refused.
	for _, args := range [][]string{
		{"set", "mine", "--binding", "verify-x=cmd"},
		{"set", "mine", "--binding", "check-tests"},
		{"set", "mine", "--rm-binding", "author"},
		{"set", "mine", "--file", "nope"},
		{"set", "mine", "--file", "../out=" + string(before)},
		{"set", "mine", "--file", "SKILL.md=" + string(before)},
		{"set", "mine", "--rm-file", "absent.md"},
	} {
		if _, _, err := f.run(args...); err == nil {
			t.Errorf("%v accepted", args)
		}
	}

	// An inapplicable requires is saved (reported on stderr), and --requires ""
	// drops the key; metadata left empty disappears with it.
	if _, errOut, err := f.run("set", "mine", "--requires", "os=plan9"); err != nil || !strings.Contains(errOut, "not applicable here") {
		t.Errorf("inapplicable set: %v / %q", err, errOut)
	}
	if _, _, err := f.run("set", "mine", "--requires", ""); err != nil {
		t.Fatal(err)
	}
	md, _ := os.ReadFile(filepath.Join(DefaultStoreDir(), "mine", "SKILL.md"))
	if string(md) != "---\nname: mine\ndescription: local\n---\n" {
		t.Errorf("after dropping requires: %q", md)
	}

	// --body and --file land in the folder; --rm-file takes one out.
	src := filepath.Join(t.TempDir(), "run.sh")
	os.WriteFile(src, []byte("#!/bin/sh\necho hi\n"), 0o644)
	if _, _, err := f.runIn("NEW BODY\n", "set", "mine", "--body", "-", "--file", "scripts/run.sh="+src); err != nil {
		t.Fatal(err)
	}
	md, _ = os.ReadFile(filepath.Join(DefaultStoreDir(), "mine", "SKILL.md"))
	if string(md) != "---\nname: mine\ndescription: local\n---\nNEW BODY\n" {
		t.Errorf("after --body: %q", md)
	}
	if data, err := os.ReadFile(filepath.Join(DefaultStoreDir(), "mine", "scripts", "run.sh")); err != nil || string(data) != "#!/bin/sh\necho hi\n" {
		t.Errorf("--file: %q %v", data, err)
	}
	if _, _, err := f.run("set", "mine", "--rm-file", "scripts/run.sh"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(DefaultStoreDir(), "mine", "scripts", "run.sh")); err == nil {
		t.Error("--rm-file left the file")
	}
	// Two stdin readers is a usage error, and the store is untouched.
	if _, _, err := f.runIn("x", "set", "mine", "--body", "-", "--file", "a.txt=-"); err == nil {
		t.Error("two stdin readers accepted")
	}
	if md2, _ := os.ReadFile(filepath.Join(DefaultStoreDir(), "mine", "SKILL.md")); string(md2) != string(md) {
		t.Error("store changed by a refused set")
	}
}

// WriteFrontmatter alone: every key and the body survive an edit that names
// none of them; a fresh metadata map is created when needed.
func TestWriteFrontmatterPreservesUnknownKeysAndBody(t *testing.T) {
	in := []byte("---\nname: x\ndescription: d\nweird: [1, 2]\nmetadata:\n  requires: os=linux\n  extra: keep me\n---\r\nbody with \r\n odd line endings\n")
	out, err := WriteFrontmatter(in, FrontmatterEdit{})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(in) {
		t.Errorf("no-op edit changed the file:\n%q\nwant\n%q", out, in)
	}
	desc := "new: colon"
	out, err = WriteFrontmatter([]byte("---\nname: y\n---\nB\n"), FrontmatterEdit{Description: &desc, SetMeta: map[string]string{"step-go": "go run ."}})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "---\nname: y\ndescription: 'new: colon'\nmetadata:\n  step-go: go run .\n---\nB\n" {
		t.Errorf("edit:\n%s", out)
	}
	sk, err := ParseFrontmatter(out)
	if err != nil || sk.Description != "new: colon" || sk.Meta["step-go"] != "go run ." {
		t.Errorf("re-parse: %+v %v", sk, err)
	}
	if _, err := WriteFrontmatter([]byte("no block\n"), FrontmatterEdit{}); err == nil {
		t.Error("no frontmatter accepted")
	}
}

func TestCLIEdit(t *testing.T) {
	f := crudFixture(t)
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "")
	// No editor: the materialised path is printed, and the copy is in place.
	out, errOut, err := f.run("edit", "kept")
	if err == nil || !strings.Contains(err.Error(), "$EDITOR") {
		t.Fatalf("edit without editor: %v", err)
	}
	want := filepath.Join(DefaultStoreDir(), "kept", "SKILL.md")
	if strings.TrimSpace(out) != want || !strings.Contains(errOut, "note: copied kept from the embedded ring") {
		t.Errorf("edit output: %q / %q", out, errOut)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("not materialised: %v", err)
	}
	if _, _, err := f.run("edit", "missing"); err == nil {
		t.Error("edit missing succeeded")
	}
}
