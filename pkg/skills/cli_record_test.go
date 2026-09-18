package skills

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/qiangli/yoke/pkg/redact"
)

// hermeticScrub packs records through an identity-free scrubber for the
// duration of a test: the fixtures name nothing about this machine, but a
// host whose username happened to be a substring of a fixture would otherwise
// turn a round-trip test red for a reason that is not in the test.
func hermeticScrub(t *testing.T) {
	t.Helper()
	prev := hostScrubber
	hostScrubber = func(...redact.Option) *redact.Scrubber { return redact.New() }
	t.Cleanup(func() { hostScrubber = prev })
}

// storeEnv pins every store this package or fleet could resolve to
// throwaway directories, so no test can reach the operator's real config.
func storeEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	store := filepath.Join(home, "skills")
	t.Setenv("BASHY_HOME", home)
	t.Setenv("BASHY_SKILLS_DIR", store)
	t.Setenv("BASHY_FLEET_DIR", filepath.Join(home, "fleet"))
	return store
}

// runIn is run with stdin supplied, for `add -`.
func (r *cobraRunner) runIn(stdin string, args ...string) (stdout, stderr string, err error) {
	cmd := NewSkillsCmd(r.opts...)
	var out, errb bytes.Buffer
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return out.String(), errb.String(), err
}

// recordRing is the embedded ring the record CLI tests serve: dual is a full
// bundle (SKILL.md + reference.md + a nested script + skill.dhnt, with
// bindings), prose is SKILL.md alone. Both pass the admission gate, which the
// checked-in testdata/record fixtures (a different requires dialect) do not.
var recordRing = fstest.MapFS{
	"dual/SKILL.md":       {Data: []byte("---\nname: dual\ndescription: A dual bundle with a canonical face and bindings.\nmetadata:\n  requires: \"os=linux,darwin,windows\"\n  check-tests: go test ./...\n  step-reada: cat SKILL.md\n  author: fixture\n---\n\n# dual\n\nProse face. The canonical face sits beside this file in skill.dhnt.\n")},
	"dual/reference.md":   {Data: []byte("# dual reference\n\nDeep companion.\n")},
	"dual/scripts/run.sh": {Data: []byte("#!/bin/sh\necho dual\n")},
	"dual/skill.dhnt":     {Data: []byte(testCanon)},
	"prose/SKILL.md":      {Data: []byte("---\nname: prose\ndescription: SKILL.md alone\n---\nPROSE BODY\n")},
}

// recordTree is a ring entry as the map readTree returns for a folder.
func recordTree(name string) map[string]string {
	out := map[string]string{}
	for p, f := range recordRing {
		if rel, ok := strings.CutPrefix(p, name+"/"); ok {
			out[rel] = string(f.Data)
		}
	}
	return out
}

// recordFixture serves recordRing from the embedded ring over a throwaway
// local store, packing records hermetically.
func recordFixture(t *testing.T) *cobraRunner {
	t.Helper()
	hermeticScrub(t)
	store := storeEnv(t)
	return &cobraRunner{t: t, opts: []Option{
		WithSource(EmbedSource(recordRing, RingEmbedded)),
		WithConfigDir(store),
	}}
}

// show --yaml → add - → show --yaml is the identity: the record that comes
// out of the local ring is byte-for-byte the record that went in, and the
// flagless show is still the SKILL.md bytes and nothing else.
func TestCLIShowRecordAddRoundTrip(t *testing.T) {
	f := recordFixture(t)
	for _, name := range []string{"dual", "prose"} {
		t.Run(name, func(t *testing.T) {
			plain, _, err := f.run("show", name)
			if err != nil {
				t.Fatal(err)
			}
			if plain != recordTree(name)["SKILL.md"] {
				t.Fatalf("flagless show is not the SKILL.md bytes:\n%s", plain)
			}

			rec, errOut, err := f.run("show", name, "--yaml")
			if err != nil {
				t.Fatal(err)
			}
			if errOut != "" {
				t.Errorf("show --yaml wrote to stderr: %q", errOut)
			}
			if !strings.HasPrefix(rec, "name: "+name+"\nkind: skill\n") {
				t.Fatalf("record header:\n%s", rec)
			}

			out, _, err := f.runIn(rec, "add", "-")
			if err != nil {
				t.Fatalf("add -: %v\n%s", err, out)
			}
			if !strings.Contains(out, "installed: ") {
				t.Fatalf("add output: %q", out)
			}
			// The rebuilt folder is the fixture folder, file for file.
			got := readTree(t, filepath.Join(DefaultStoreDir(), name))
			if want := recordTree(name); !sameTree(got, want) {
				t.Errorf("installed folder differs: %v vs %v", keys(got), keys(want))
			}

			again, _, err := f.run("show", name, "--yaml")
			if err != nil {
				t.Fatal(err)
			}
			if again != rec {
				t.Errorf("round-trip changed the record:\n%s\n---\n%s", rec, again)
			}
			// Now served from the local ring, shadowing the embedded copy.
			if _, errOut, _ := f.run("show", name); !strings.Contains(errOut, "ring=local") {
				t.Errorf("after add, show stderr = %q, want ring=local", errOut)
			}
		})
	}
}

func TestCLIShowRecordJSON(t *testing.T) {
	f := recordFixture(t)
	out, _, err := f.run("show", "dual", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("bad json: %v\n%s", err, out)
	}
	if got["kind"] != "skill" || got["name"] != "dual" {
		t.Errorf("json header: %v", got)
	}
	files, _ := got["files"].(map[string]any)
	if files["SKILL.md"] == nil || files["skill.dhnt"] == nil {
		t.Errorf("json files: %v", files)
	}
	// JSON is YAML: the JSON face feeds `add -` too.
	if _, _, err := f.runIn(out, "add", "-"); err != nil {
		t.Fatalf("add - (json): %v", err)
	}
	if _, _, err := f.run("show", "dual", "--reference", "--yaml"); err == nil {
		t.Error("--reference --yaml combined silently")
	}
}

func TestCLIAddRecordFile(t *testing.T) {
	f := recordFixture(t)
	rec, _, err := f.run("show", "dual", "--yaml")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "dual.yaml")
	if err := os.WriteFile(path, []byte(rec), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.run("add", path); err != nil {
		t.Fatalf("add file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(DefaultStoreDir(), "dual", "reference.md")); err != nil {
		t.Errorf("record files not written: %v", err)
	}
	// Re-add without --force refuses, exactly as a folder does.
	if _, _, err := f.run("add", path); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Errorf("re-add: %v", err)
	}

	// A file that is not a record is refused by name, not installed as a folder.
	junk := filepath.Join(t.TempDir(), "notes.txt")
	os.WriteFile(junk, []byte("just prose\n"), 0o644)
	if _, _, err := f.run("add", junk); err == nil || !strings.Contains(err.Error(), "record") {
		t.Errorf("junk add: %v", err)
	}

	// A record is admitted through the same gate as a folder: a broken
	// canonical face fails and nothing lands in the store.
	bad := strings.Replace(rec, "name: dual\n", "name: dual-bad\n", 1)
	bad = strings.Replace(bad, "sokilili", "BROKEN", 1)
	if _, _, err := f.runIn(bad, "add", "-"); err == nil || !strings.Contains(err.Error(), "admission gate") {
		t.Errorf("bad face: %v", err)
	}
	if _, err := os.Stat(filepath.Join(DefaultStoreDir(), "dual-bad")); err == nil {
		t.Error("dual-bad installed despite failing the gate")
	}

	// A record whose name would escape the store is refused before any write.
	if _, _, err := f.runIn(strings.Replace(rec, "name: dual\n", "name: ../dual\n", 1), "add", "-"); err == nil {
		t.Error("path-escaping name admitted")
	}
}

func TestCLIAddRecordRefusesHostIdentityBeforeWriting(t *testing.T) {
	f := recordFixture(t)
	host := "private-workstation.example"
	prev := hostScrubber
	hostScrubber = func(...redact.Option) *redact.Scrubber {
		return redact.New(redact.WithHost(host))
	}
	t.Cleanup(func() { hostScrubber = prev })

	rec := Record{
		Name: "leaky",
		Kind: RecordKind,
		Files: map[string]string{
			"SKILL.md": "---\nname: leaky\ndescription: leaky record\nmetadata:\n  step-run: ssh " + host + "\n---\n",
		},
	}
	b, err := MarshalRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.runIn(string(b), "add", "-"); err == nil || !strings.Contains(err.Error(), "carries host identity") {
		t.Fatalf("add error = %v, want identity refusal", err)
	}
	if _, err := os.Stat(filepath.Join(DefaultStoreDir(), "leaky")); !os.IsNotExist(err) {
		t.Fatalf("leaky record wrote to store: %v", err)
	}
}

func TestCLIExportYAML(t *testing.T) {
	f := recordFixture(t)
	dst := t.TempDir()
	out, _, err := f.run("export", "dual", "--yaml", "--to", dst)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dst, "dual.yaml")
	if !strings.Contains(out, "exported: "+path) {
		t.Errorf("export output: %q", out)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	shown, _, _ := f.run("show", "dual", "--yaml")
	if string(written) != shown {
		t.Error("export --yaml differs from show --yaml")
	}
	if _, err := os.Stat(filepath.Join(dst, "dual")); err == nil {
		t.Error("export --yaml also wrote the folder")
	}
	if _, _, err := f.run("export", "dual", "--yaml", "--to", dst); err == nil {
		t.Error("overwrote an existing record without --force")
	}
	if _, _, err := f.run("export", "dual", "--yaml", "--to", dst, "--force"); err != nil {
		t.Errorf("--force: %v", err)
	}
	if _, _, err := f.run("export", "dual", "--yaml", "--user"); err == nil {
		t.Error("--yaml --user accepted")
	}
}

// sync consumes both content shapes: a plain SKILL.md lands as today
// (<name>/SKILL.md verbatim), a record is unpacked into its full folder.
func TestCLISyncUnpacksRecords(t *testing.T) {
	f := recordFixture(t)
	rec, _, err := f.run("show", "dual", "--yaml")
	if err != nil {
		t.Fatal(err)
	}
	const plain = "---\nname: plain-org\ndescription: served as SKILL.md\nmetadata:\n  requires: os == linux\n---\nPLAIN BODY\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/skills" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string][]map[string]string{"skills": {
			{"name": "plain-org", "content": plain},
			{"name": "dual-org", "content": rec},
		}})
	}))
	defer srv.Close()

	out, _, err := f.run("sync", "--url", srv.URL, "--token", "t")
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if !strings.Contains(out, "2 pulled") || !strings.Contains(out, "1 unpacked") {
		t.Errorf("sync output: %q", out)
	}
	ring := CloudRingDir()
	if !strings.HasPrefix(ring, os.Getenv("BASHY_FLEET_DIR")) {
		t.Fatalf("cloud ring %s is outside the test fleet dir", ring)
	}
	if got := readTree(t, filepath.Join(ring, "plain-org")); got["SKILL.md"] != plain || len(got) != 1 {
		t.Errorf("plain entry: %v", got)
	}
	got := readTree(t, filepath.Join(ring, "dual-org"))
	if want := recordTree("dual"); !sameTree(got, want) {
		t.Errorf("record entry: %v, want %v", keys(got), keys(want))
	}
	// Mounted as the shared ring, the unpacked skill parses as the dual bundle
	// it was — canonical face and all (listed under its frontmatter name).
	shared := &cobraRunner{t: t, opts: []Option{
		WithSource(SharedDirSource(ring)), WithConfigDir(DefaultStoreDir()),
	}}
	list, _, err := shared.run("list", "--all", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(list, `"name":"dual"`) || !strings.Contains(list, `"dhnt":true`) ||
		!strings.Contains(list, `"name":"plain-org"`) {
		t.Errorf("shared ring listing: %s", list)
	}

	// --json carries the unpacked count.
	out, _, err = f.run("sync", "--url", srv.URL, "--token", "t", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"unpacked": 1`) {
		t.Errorf("sync --json: %s", out)
	}
}
