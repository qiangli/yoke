package skills

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"
)

// cliFixture builds a command over an in-memory embedded ring + a temp
// local ring. Requires clauses gate only on core probes (os), so the
// tests never LookPath or exec.
func cliFixture(t *testing.T) *cobraRunner {
	t.Helper()
	embedded := fstest.MapFS{
		"alpha-notes/SKILL.md":     {Data: []byte("---\nname: alpha-notes\ndescription: ungated\n---\nALPHA BODY\n")},
		"alpha-notes/reference.md": {Data: []byte("ALPHA REFERENCE\n")},
		"omega-nowhere/SKILL.md":   {Data: []byte("---\nname: omega-nowhere\ndescription: never applicable\nmetadata:\n  requires: \"os=plan9\"\n---\nOMEGA BODY\n")},
	}
	dir := t.TempDir()
	return &cobraRunner{t: t, opts: []Option{
		WithSource(EmbedSource(embedded, RingEmbedded)),
		WithConfigDir(dir),
	}}
}

type cobraRunner struct {
	t    *testing.T
	opts []Option
}

func (r *cobraRunner) run(args ...string) (stdout, stderr string, err error) {
	cmd := NewSkillsCmd(r.opts...)
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return out.String(), errb.String(), err
}

func TestCLIListDefaultGates(t *testing.T) {
	f := cliFixture(t)
	out, _, err := f.run("list")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "alpha-notes" {
		t.Fatalf("list = %q, want just alpha-notes", out)
	}
	// Bare `skills` == `skills list` (pre-cobra back-compat).
	bare, _, err := f.run()
	if err != nil {
		t.Fatal(err)
	}
	if bare != out {
		t.Fatalf("bare = %q, list = %q", bare, out)
	}
}

func TestCLIListAll(t *testing.T) {
	f := cliFixture(t)
	out, _, err := f.run("list", "--all")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "alpha-notes") ||
		!strings.Contains(out, "omega-nowhere\t# inapplicable: os=plan9: os=") {
		t.Fatalf("list --all = %q", out)
	}
}

func TestCLIListJSON(t *testing.T) {
	f := cliFixture(t)
	out, _, err := f.run("list", "--all", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("bad json: %v\n%s", err, out)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d", len(rows))
	}
}

// The coordinate `probe` prints and the one HostCoordinate returns must be the
// same string. They are the write and read ends of the same address: a fold
// filed under one and looked up under the other is written, stored, and never
// found again — with nothing to see but a store that answers nothing.
func TestHostCoordinateIsWhatProbePrints(t *testing.T) {
	dir := t.TempDir()
	r := &cobraRunner{t: t, opts: []Option{WithConfigDir(dir)}}
	out, _, err := r.run("probe", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		ContextKey string `json:"context_key"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("bad json: %v\n%s", err, out)
	}
	if want := HostCoordinate(WithConfigDir(dir)); got.ContextKey != want {
		t.Errorf("probe printed %s, HostCoordinate returned %s", got.ContextKey, want)
	}
}

// The reported coordinate must not move because a probe cache warmed up. It did:
// the first run keyed over the core probes and every later one over the core
// probes PLUS whatever the cache had since resolved, so a host's very first
// observation was always filed at an address it would never use again.
func TestProbeCoordinateSurvivesCacheWarmUp(t *testing.T) {
	dir := t.TempDir()
	r := &cobraRunner{t: t, opts: []Option{WithConfigDir(dir)}}
	key := func() string {
		out, _, err := r.run("probe", "--json")
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			ContextKey string `json:"context_key"`
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("bad json: %v\n%s", err, out)
		}
		return got.ContextKey
	}
	if cold, warm := key(), key(); cold != warm {
		t.Errorf("coordinate moved between runs: %s then %s", cold, warm)
	}
}

func TestCLIProbe(t *testing.T) {
	f := cliFixture(t)
	out, _, err := f.run("probe", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Probes     map[string]string `json:"probes"`
		ContextKey string            `json:"context_key"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("bad json: %v\n%s", err, out)
	}
	if got.Probes["os"] == "" || !strings.HasPrefix(got.ContextKey, "c") {
		t.Fatalf("probe = %+v", got)
	}
	// Text form ends with the context key line.
	txt, _, err := f.run("probe")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(txt, "os=") || !strings.Contains(txt, "context=c") {
		t.Fatalf("probe text = %q", txt)
	}
}

func TestCLIShowByteCompat(t *testing.T) {
	f := cliFixture(t)
	out, errOut, err := f.run("show", "alpha-notes")
	if err != nil {
		t.Fatal(err)
	}
	// stdout is the file, byte-identical; the verdict rides stderr.
	if !strings.HasSuffix(out, "ALPHA BODY\n") || strings.Contains(out, "ring=") {
		t.Fatalf("show stdout = %q", out)
	}
	if !strings.Contains(errOut, "ring=embedded") || !strings.Contains(errOut, "applicable") {
		t.Fatalf("show stderr = %q", errOut)
	}

	ref, refErr, err := f.run("show", "alpha-notes", "--reference")
	if err != nil {
		t.Fatal(err)
	}
	if ref != "ALPHA REFERENCE\n" || refErr != "" {
		t.Fatalf("show --reference = %q / %q", ref, refErr)
	}

	if _, _, err := f.run("show", "missing"); err == nil {
		t.Fatal("show missing did not error")
	}
}
