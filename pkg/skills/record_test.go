package skills

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"gopkg.in/yaml.v3"

	"github.com/qiangli/yoke/pkg/redact"
)

// recordFixtureDir is the checked-in folder of skill folders the round-trip
// tests enumerate through the ring loader, the way a host enumerates its
// embedded ring.
const recordFixtureDir = "testdata/record"

// fixtureSkills lists every skill the fixture ring serves, with its source.
func fixtureSkills(t *testing.T, srcs ...Source) map[string]Skill {
	t.Helper()
	cat := &Catalog{Sources: srcs}
	rows, err := cat.List(testProbes(t, map[string]string{"os": "linux", "arch": "arm64"}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("fixture ring is empty")
	}
	out := map[string]Skill{}
	for _, r := range rows {
		out[r.Name] = r.Skill
	}
	return out
}

// readTree returns every file under dir keyed by slash path, bytes verbatim.
func readTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		out[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sameTree(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// (1) Every fixture skill survives folder → RecordOf → MarshalRecord →
// ParseRecord → WriteFolder byte-identical, and the rebuilt folder
// re-derives the same identity and capability.
func TestRecordRoundTripsEveryFixtureSkill(t *testing.T) {
	src := DirSource(recordFixtureDir)
	scrub := redact.New()
	for name, sk := range fixtureSkills(t, src) {
		t.Run(name, func(t *testing.T) {
			rec, err := RecordFrom(&sk, src, scrub)
			if err != nil {
				t.Fatalf("RecordFrom: %v", err)
			}
			want := readTree(t, filepath.Join(recordFixtureDir, name))
			if !sameTree(rec.Files, want) {
				t.Fatalf("Files = %v, want the folder %v", keys(rec.Files), keys(want))
			}
			if rec.Kind != "skill" || rec.Name != sk.Name || rec.Description != sk.Description {
				t.Errorf("header: %+v", rec)
			}
			b, err := MarshalRecord(rec)
			if err != nil {
				t.Fatalf("MarshalRecord: %v", err)
			}
			if strings.Contains(string(b), "exported_at") || strings.Contains(string(b), "at:") {
				t.Errorf("record carries a timestamp:\n%s", b)
			}
			back, err := ParseRecord(b)
			if err != nil {
				t.Fatalf("ParseRecord: %v\n%s", err, b)
			}
			dst := filepath.Join(t.TempDir(), name)
			if err := WriteFolder(back, dst); err != nil {
				t.Fatalf("WriteFolder: %v", err)
			}
			if got := readTree(t, dst); !sameTree(got, want) {
				t.Errorf("rebuilt folder differs: %v vs %v", keys(got), keys(want))
			}
			// The rebuilt folder re-derives what the record only reported.
			again := fixtureSkills(t, DirSource(filepath.Dir(dst)))[name]
			rec2, err := RecordFrom(&again, DirSource(filepath.Dir(dst)), scrub)
			if err != nil {
				t.Fatal(err)
			}
			if rec2.Identity != rec.Identity || rec2.Capability != rec.Capability {
				t.Errorf("re-derived identity/capability moved: %q/%q vs %q/%q",
					rec2.Identity, rec2.Capability, rec.Identity, rec.Capability)
			}
			b2, _ := MarshalRecord(rec2)
			if string(b2) != string(b) {
				t.Errorf("re-marshalled record differs:\n%s\n---\n%s", b, b2)
			}
		})
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// The dual fixture pins the derived view: identity, capability, contract,
// bindings and requires all come from the folder, not from the caller.
func TestRecordDerivesContractAndBindings(t *testing.T) {
	src := DirSource(recordFixtureDir)
	sk := fixtureSkills(t, src)["dual"]
	rec, err := RecordFrom(&sk, src, redact.New())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rec.Identity, "h") || !strings.HasPrefix(rec.Capability, "k") {
		t.Errorf("identity %q capability %q", rec.Identity, rec.Capability)
	}
	if rec.Requires != "os == linux" {
		t.Errorf("requires = %q", rec.Requires)
	}
	if rec.Contract.Steps != 1 || strings.Join(rec.Contract.Ensure, ",") != "gereeni" ||
		strings.Join(rec.Contract.Effects, ",") != "read,write" {
		t.Errorf("contract = %+v", rec.Contract)
	}
	want := map[string]string{"check-tests": "go test ./...", "step-reada": "cat SKILL.md", "check": "test -f SKILL.md"}
	if !sameTree(rec.Bindings, want) {
		t.Errorf("bindings = %v (author: must not be a binding)", rec.Bindings)
	}
	if _, ok := rec.Files["scripts/check.sh"]; !ok || len(rec.Files) != 4 {
		t.Errorf("files = %v", keys(rec.Files))
	}
}

// (3) A skill without skill.dhnt has identity "" and capability "" — and
// the emitted YAML says so explicitly rather than omitting the keys.
func TestRecordWithoutDhntHasEmptyIdentity(t *testing.T) {
	src := DirSource(recordFixtureDir)
	sk := fixtureSkills(t, src)["prose"]
	rec, err := RecordFrom(&sk, src, redact.New())
	if err != nil {
		t.Fatal(err)
	}
	if rec.Identity != "" || rec.Capability != "" || rec.Contract.Steps != 0 ||
		len(rec.Contract.Ensure) != 0 || len(rec.Contract.Effects) != 0 || rec.Bindings != nil {
		t.Errorf("prose-only record invented a face: %+v", rec)
	}
	b, err := MarshalRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "identity: \"\"\n") || !strings.Contains(string(b), "capability: \"\"\n") {
		t.Errorf("empty identity/capability not emitted:\n%s", b)
	}
	// An INVALID face is the same honest answer: the file travels, the
	// identity does not.
	broken := fstest.MapFS{
		"broken/SKILL.md":   {Data: []byte("---\nname: broken\ndescription: bad face\n---\nbody\n")},
		"broken/skill.dhnt": {Data: []byte("not canonical CONTENT!\n")},
	}
	esrc := EmbedSource(broken, RingEmbedded)
	bsk := fixtureSkills(t, esrc)["broken"]
	brec, err := RecordFrom(&bsk, esrc, redact.New())
	if err != nil {
		t.Fatal(err)
	}
	if brec.Identity != "" || brec.Capability != "" || brec.Files["skill.dhnt"] != "not canonical CONTENT!\n" {
		t.Errorf("broken face: %+v", brec)
	}
}

// (2) Re-ordering the YAML keys of a marshalled record changes nothing that
// matters: the parse is by key, the folder is rebuilt from files alone, and
// the identity re-derived from that folder equals the original.
func TestRecordKeyOrderDoesNotChangeIdentity(t *testing.T) {
	src := DirSource(recordFixtureDir)
	sk := fixtureSkills(t, src)["dual"]
	rec, err := RecordFrom(&sk, src, redact.New())
	if err != nil {
		t.Fatal(err)
	}
	b, err := MarshalRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	// Reverse the top-level key order through a generic decode; yaml.v3
	// keeps a mapping node's key order as written.
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	m := doc.Content[0]
	for i, j := 0, len(m.Content)-2; i < j; i, j = i+2, j-2 {
		m.Content[i], m.Content[j] = m.Content[j], m.Content[i]
		m.Content[i+1], m.Content[j+1] = m.Content[j+1], m.Content[i+1]
	}
	shuffled, err := yaml.Marshal(&doc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(string(shuffled), "name:") {
		t.Fatalf("reorder did not take:\n%s", shuffled)
	}
	back, err := ParseRecord(shuffled)
	if err != nil {
		t.Fatalf("ParseRecord(reordered): %v", err)
	}
	// A record that LIES about its identity is corrected by the re-derive.
	back.Identity = "hdeadbeef"
	back.Capability = "kdeadbeef"
	dst := filepath.Join(t.TempDir(), "dual")
	if err := WriteFolder(back, dst); err != nil {
		t.Fatal(err)
	}
	dsrc := DirSource(filepath.Dir(dst))
	again := fixtureSkills(t, dsrc)["dual"]
	rec2, err := RecordFrom(&again, dsrc, redact.New())
	if err != nil {
		t.Fatal(err)
	}
	if rec2.Identity != rec.Identity || rec2.Capability != rec.Capability {
		t.Errorf("identity after reorder+reimport = %q/%q, want %q/%q",
			rec2.Identity, rec2.Capability, rec.Identity, rec.Capability)
	}
	b2, _ := MarshalRecord(rec2)
	if string(b2) != string(b) {
		t.Errorf("canonical emit moved:\n%s\n---\n%s", b, b2)
	}
}

// (4) Host identity in any file refuses the whole record, naming the path —
// and the message never quotes the value it found.
func TestRecordRefusesIdentityNamingPath(t *testing.T) {
	root := t.TempDir()
	dir := writeSkillDir(t, root, "leaky", "---\nname: leaky\ndescription: clean header\n---", "")
	if err := os.MkdirAll(filepath.Join(dir, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	// An email address is caught by shape on every host, so RecordOf — the
	// FromHost path — is exercised directly and stays hermetic.
	if err := os.WriteFile(filepath.Join(dir, "notes", "contact.md"), []byte("ping ops@example.com first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sk := fixtureSkills(t, DirSource(root))["leaky"]
	if sk.Dir == "" {
		t.Fatal("dir-backed skill has no Dir")
	}
	_, err := RecordOf(&sk)
	var ci *ErrCarriesIdentity
	if !errors.As(err, &ci) {
		t.Fatalf("err = %v, want ErrCarriesIdentity", err)
	}
	if ci.Path != "leaky/notes/contact.md" || !strings.Contains(err.Error(), "leaky/notes/contact.md") {
		t.Errorf("path = %q; err = %v", ci.Path, err)
	}
	if !strings.Contains(err.Error(), "email") || strings.Contains(err.Error(), "example.com") {
		t.Errorf("message must name the kind and never the value: %v", err)
	}

	// The explicit scrubber catches declared literals the same way.
	src := DirSource(recordFixtureDir)
	dual := fixtureSkills(t, src)["dual"]
	_, err = RecordFrom(&dual, src, redact.New(redact.WithHost("reference")))
	if !errors.As(err, &ci) || ci.Path != "dual/reference.md" || ci.Kinds[0] != "host" {
		t.Errorf("declared literal: %v", err)
	}
	// Nothing host-shaped in the fixture ring itself: the hermetic scrubber
	// still runs its pattern passes, and the clean fixture must pass them.
	if _, err := RecordFrom(&dual, src, redact.New()); err != nil {
		t.Errorf("clean fixture refused: %v", err)
	}
	// And an embedded skill has no folder for RecordOf to walk.
	if _, err := RecordOf(&Skill{Name: "x"}); err == nil || !strings.Contains(err.Error(), "RecordFrom") {
		t.Errorf("RecordOf(embedded) = %v", err)
	}
}

// (5) MarshalRecord is deterministic across calls and across the order the
// maps were populated in.
func TestMarshalRecordDeterministic(t *testing.T) {
	build := func(order []string) Record {
		rec := Record{Name: "det", Kind: "skill", Description: "d", Bindings: map[string]string{}, Files: map[string]string{}}
		for _, k := range order {
			rec.Files[k] = "content of " + k
			rec.Bindings["step-"+k] = "run " + k
		}
		rec.Files["SKILL.md"] = "---\nname: det\n---\n"
		return rec
	}
	a, err := MarshalRecord(build([]string{"a.md", "b.md", "c/d.sh"}))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := MarshalRecord(build([]string{"c/d.sh", "b.md", "a.md"}))
	c, _ := MarshalRecord(build([]string{"b.md", "c/d.sh", "a.md"}))
	if string(a) != string(b) || string(a) != string(c) {
		t.Errorf("emit depends on insertion order:\n%s\n---\n%s\n---\n%s", a, b, c)
	}
	if i, j := strings.Index(string(a), "name:"), strings.Index(string(a), "files:"); i < 0 || j < i {
		t.Errorf("key order not fixed:\n%s", a)
	}
	// Sorted file order inside the map, too.
	if i, j := strings.Index(string(a), "a.md:"), strings.Index(string(a), "c/d.sh:"); i < 0 || j < i {
		t.Errorf("files not sorted:\n%s", a)
	}
}

// Bytes are verbatim on the wire: CRLF, a trailing-space line, no trailing
// newline, and invalid UTF-8 all survive marshal → parse → write.
func TestRecordCarriesBytesVerbatim(t *testing.T) {
	fsys := fstest.MapFS{
		"raw/SKILL.md":       {Data: []byte("---\r\nname: raw\r\ndescription: crlf\r\n---\r\nline with trailing space \r\nno final newline")},
		"raw/blob.bin":       {Data: []byte{0x00, 0xff, 0xfe, 'a', '\n', 0x80}},
		"raw/sub/deep/x.txt": {Data: []byte("---\n...\n")},
	}
	src := EmbedSource(fsys, RingEmbedded)
	sk := fixtureSkills(t, src)["raw"]
	rec, err := RecordFrom(&sk, src, redact.New())
	if err != nil {
		t.Fatal(err)
	}
	b, err := MarshalRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseRecord(b)
	if err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	dst := filepath.Join(t.TempDir(), "raw")
	if err := WriteFolder(back, dst); err != nil {
		t.Fatal(err)
	}
	got := readTree(t, dst)
	for p, f := range fsys {
		rel := strings.TrimPrefix(p, "raw/")
		if got[rel] != string(f.Data) {
			t.Errorf("%s: %q, want %q", rel, got[rel], f.Data)
		}
	}
}

// ParseRecord is strict: unknown keys, a foreign kind, and a record with no
// SKILL.md are all refused loudly; WriteFolder refuses escaping paths and a
// populated destination.
func TestRecordStrictParseAndWriteGuards(t *testing.T) {
	good := "name: g\nkind: skill\ndescription: d\nidentity: \"\"\ncapability: \"\"\ncontract:\n  effects: []\n  ensure: []\n  steps: 0\nfiles:\n  SKILL.md: |\n    ---\n    name: g\n    ---\n"
	if _, err := ParseRecord([]byte(good)); err != nil {
		t.Fatalf("good record refused: %v", err)
	}
	bad := map[string]string{
		"unknown key":   good + "action: run\n",
		"unknown kind":  strings.Replace(good, "kind: skill", "kind: tool", 1),
		"missing kind":  strings.Replace(good, "kind: skill\n", "", 1),
		"no SKILL.md":   strings.Replace(good, "SKILL.md:", "README.md:", 1),
		"no name":       strings.Replace(good, "name: g\nkind", "kind", 1),
		"not a mapping": "- a\n- b\n",
	}
	for name, doc := range bad {
		if _, err := ParseRecord([]byte(doc)); err == nil {
			t.Errorf("%s: accepted\n%s", name, doc)
		}
	}

	rec, _ := ParseRecord([]byte(good))
	for _, rel := range []string{"../escape.md", "/abs.md", "a/../../b.md", ""} {
		r := rec
		r.Files = map[string]string{skillMarker: "x", rel: "y"}
		dst := filepath.Join(t.TempDir(), "g")
		if err := WriteFolder(r, dst); err == nil || !strings.Contains(err.Error(), "escapes") {
			t.Errorf("%q: err = %v", rel, err)
		}
		if _, err := os.Stat(dst); err == nil {
			t.Errorf("%q: wrote a partial folder", rel)
		}
	}
	// The wrong kind never reaches disk either.
	r := rec
	r.Kind = "tool"
	if err := WriteFolder(r, filepath.Join(t.TempDir(), "g")); err == nil {
		t.Error("foreign kind written")
	}
	// A populated destination is refused, an empty one is fine.
	dst := filepath.Join(t.TempDir(), "g")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteFolder(rec, dst); err != nil {
		t.Fatalf("empty dir refused: %v", err)
	}
	if err := WriteFolder(rec, dst); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Errorf("populated dir: err = %v", err)
	}
}
