package skills

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type skillListEnvelope struct {
	SchemaVersion string `json:"schema_version"`
	Kind          string `json:"kind"`
	View          string `json:"view"`
	Items         []struct {
		Name       string `json:"name"`
		Ring       string `json:"ring"`
		Applicable bool   `json:"applicable"`
		Retired    *struct {
			Reason     string `json:"reason"`
			ReplacedBy string `json:"replaced_by"`
		} `json:"retired"`
	} `json:"items"`
}

func (f *cobraRunner) listEnvelope(args ...string) skillListEnvelope {
	f.t.Helper()
	out, errOut, err := f.run(append([]string{"list", "--json"}, args...)...)
	if err != nil {
		f.t.Fatalf("list --json %v: %v\n%s", args, err, errOut)
	}
	var env skillListEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		f.t.Fatalf("list --json %v is not an envelope: %v\n%s", args, err, out)
	}
	return env
}

func (e skillListEnvelope) names() string {
	var n []string
	for _, it := range e.Items {
		n = append(n, it.Name)
	}
	return strings.Join(n, ",")
}

func (f *cobraRunner) mustRun(args ...string) string {
	f.t.Helper()
	out, errOut, err := f.run(args...)
	if err != nil {
		f.t.Fatalf("%v: %v\n%s", args, err, errOut)
	}
	return out
}

func (f *cobraRunner) addLocalSkill(name string) {
	f.t.Helper()
	dir := filepath.Join(f.dir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: local\n---\nLOCAL BODY\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func TestSkillListJSONEnvelope(t *testing.T) {
	f := cliFixture(t)
	env := f.listEnvelope()
	if env.SchemaVersion != "bashy-registry-list-v1" || env.Kind != "skill" || env.View != "default" {
		t.Fatalf("envelope = %+v", env)
	}
	if env.names() != "alpha-notes" {
		t.Fatalf("default items = %q, want alpha-notes", env.names())
	}
}

func TestSkillListViewFlags(t *testing.T) {
	f := cliFixture(t)
	f.addLocalSkill("local-one")
	for _, c := range []struct {
		args []string
		view string
		want string
	}{
		{nil, "default", "alpha-notes,local-one"},
		{[]string{"--builtin"}, "builtin", "alpha-notes"},
		{[]string{"--custom"}, "custom", "local-one"},
		{[]string{"--active"}, "active", "alpha-notes,local-one"},
		{[]string{"--all"}, "all", "alpha-notes,local-one"},
		{[]string{"--retired"}, "retired", ""},
		{[]string{"--ring", "local"}, "custom", "local-one"},
	} {
		env := f.listEnvelope(c.args...)
		if env.View != c.view || env.names() != c.want {
			t.Errorf("list %v: view=%q items=%q, want view=%q items=%q", c.args, env.View, env.names(), c.view, c.want)
		}
	}
	if _, _, err := f.run("list", "--builtin", "--custom"); err == nil {
		t.Error("--builtin with --custom must be refused: the views are alternatives")
	}
}

// The old `list --all` meaning (include skills that do not apply here) is
// --inapplicable; --all is the same all-rings view every kind has.
func TestSkillListInapplicable(t *testing.T) {
	f := cliFixture(t)
	out := f.mustRun("list", "--inapplicable")
	if !strings.Contains(out, "alpha-notes") || !strings.Contains(out, "omega-nowhere\t# inapplicable: os=plan9: os=") {
		t.Fatalf("list --inapplicable = %q", out)
	}
	if all := f.mustRun("list", "--all"); strings.Contains(all, "omega-nowhere") {
		t.Fatalf("list --all must not include inapplicable skills: %q", all)
	}
	if env := f.listEnvelope("--inapplicable"); env.names() != "alpha-notes,omega-nowhere" {
		t.Fatalf("list --inapplicable --json items = %q", env.names())
	}
	if _, _, err := f.run("list", "--inapplicable", "--active"); err == nil {
		t.Error("--inapplicable with --active contradict and must be refused")
	}
}

func TestSkillRetireUnretireRoundTrip(t *testing.T) {
	f := cliFixture(t)
	if out := f.mustRun("retire", "alpha-notes", "--reason", "stale", "--replaced-by", "beta-notes"); !strings.Contains(out, "retired skill alpha-notes") {
		t.Fatalf("retire output = %q", out)
	}
	if got := strings.TrimSpace(f.mustRun("list")); got != "" {
		t.Fatalf("a retired skill must leave the default list, got %q", got)
	}
	env := f.listEnvelope("--retired")
	if env.View != "retired" || env.names() != "alpha-notes" {
		t.Fatalf("list --retired = %+v", env)
	}
	if r := env.Items[0].Retired; r == nil || r.Reason != "stale" || r.ReplacedBy != "beta-notes" {
		t.Fatalf("retirement not reported: %+v", env.Items[0].Retired)
	}
	if env := f.listEnvelope("--all"); env.names() != "" {
		t.Fatalf("--all must exclude retired skills, got %q", env.names())
	}
	// Retiring writes a sidecar in the local ring; the embedded skill itself
	// is untouched and the sidecar dir is never itself listed as a skill.
	if _, err := os.Stat(filepath.Join(f.dir, ".retired", "alpha-notes.yaml")); err != nil {
		t.Fatalf("no retirement sidecar: %v", err)
	}
	if out := f.mustRun("show", "alpha-notes"); !strings.Contains(out, "ALPHA BODY") {
		t.Fatalf("show must still resolve a retired skill: %q", out)
	}

	f.mustRun("unretire", "alpha-notes")
	if got := strings.TrimSpace(f.mustRun("list")); got != "alpha-notes" {
		t.Fatalf("unretired skill must return, got %q", got)
	}
	if env := f.listEnvelope("--retired"); env.names() != "" {
		t.Fatalf("--retired after unretire = %q", env.names())
	}
}

func TestSkillRetireUnknownAndUnsafeNames(t *testing.T) {
	f := cliFixture(t)
	for _, name := range []string{"no-such-skill", "../escape", "a/b"} {
		if _, _, err := f.run("retire", name); err == nil {
			t.Errorf("retire %q must fail", name)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(f.dir, ".retired")); len(entries) != 0 {
		t.Errorf("failed retire left sidecars: %v", entries)
	}
}

func TestRetiredSkillIsRefusedByExportAndRun(t *testing.T) {
	f := cliFixture(t)
	f.mustRun("retire", "alpha-notes", "--replaced-by", "beta-notes")
	dst := t.TempDir()

	_, _, err := f.run("export", "alpha-notes", "--to", dst)
	if err == nil || !strings.Contains(err.Error(), "retired") || !strings.Contains(err.Error(), "beta-notes") {
		t.Fatalf("export of a retired skill: err = %v", err)
	}
	if _, serr := os.Stat(filepath.Join(dst, "alpha-notes")); serr == nil {
		t.Error("export wrote a retired skill")
	}
	if _, _, err := f.run("export", "alpha-notes", "--to", dst, "--yaml"); err == nil {
		t.Error("export --yaml of a retired skill must fail")
	}
	if _, _, err := f.run("run", "alpha-notes"); err == nil || !strings.Contains(err.Error(), "retired") {
		t.Fatalf("run of a retired skill: err = %v", err)
	}

	f.mustRun("unretire", "alpha-notes")
	if _, _, err := f.run("export", "alpha-notes", "--to", dst); err != nil {
		t.Fatalf("export after unretire: %v", err)
	}
}

func TestRetiredSkillIsSkippedByProvisionAndApplicable(t *testing.T) {
	f := cliFixture(t)
	f.mustRun("retire", "alpha-notes")

	ws := t.TempDir()
	var log bytes.Buffer
	Provision(ws, []string{"alpha-notes"}, &log, f.opts...)
	if _, err := os.Stat(filepath.Join(ws, ".agents", "skills", "alpha-notes")); err == nil {
		t.Errorf("Provision exported a retired skill; log: %s", log.String())
	}
	if !strings.Contains(log.String(), "retired") {
		t.Errorf("Provision should say why it skipped: %q", log.String())
	}
	for _, a := range Applicable(f.opts...) {
		if a.Name == "alpha-notes" {
			t.Error("Applicable advertises a retired skill")
		}
	}
}
