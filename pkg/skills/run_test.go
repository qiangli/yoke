package skills

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Canonical faces for the run tests. Commands bind through SKILL.md
// metadata, so the same face runs different commands per fixture.
const (
	// cap {read}, contract {gereeni}
	canonCheckOnly = "sokilili demo efefecato reada fini enisure gereeni fini fini\n"
	// cap {read, write}, one step (primitive porta), contract {gereeni}
	canonStep = "sokilili demo efefecato reada wurite fini sotepo one porota fini enisure gereeni fini fini\n"
	// contract only, NO effect cap — must be refused by pre-flight
	canonNoCap = "sokilili demo enisure gereeni fini fini\n"
)

func runFixture(t *testing.T, frontmatter, canon string) (*cobraRunner, string) {
	t.Helper()
	store := t.TempDir()
	src := t.TempDir()
	dir := writeSkillDir(t, src, "demo-run", frontmatter, canon)
	f := &cobraRunner{t: t, opts: []Option{WithConfigDir(store)}}
	if _, _, err := f.run("add", dir); err != nil {
		t.Fatalf("add: %v", err)
	}
	return f, store
}

func TestRunContractPasses(t *testing.T) {
	f, store := runFixture(t,
		"---\nname: demo-run\ndescription: passing contract\nmetadata:\n  requires: \"os=linux,darwin,windows\"\n  check-tests: \"true\"\n---", canonCheckOnly)
	stdout, stderr, err := f.run("run", "demo-run")
	if err != nil {
		t.Fatalf("run: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}
	if !strings.Contains(stdout, "valid: true") || !strings.Contains(stdout, "passed: gereeni") {
		t.Fatalf("receipt: %q", stdout)
	}
	// Attestation stored as JSONL in the ring-1 store.
	data, err := os.ReadFile(filepath.Join(store, "attest", "demo-run.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var rec AttestRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	if !rec.Attest.Valid || rec.ContextKey == "" || !strings.HasPrefix(rec.Attest.Skill, "h") {
		t.Fatalf("record: %+v", rec)
	}
}

func TestRunContractFails(t *testing.T) {
	f, store := runFixture(t,
		"---\nname: demo-run\ndescription: failing contract\nmetadata:\n  check-tests: \"false\"\n---", canonCheckOnly)
	stdout, _, err := f.run("run", "demo-run")
	if err == nil {
		t.Fatal("run exited 0 on a failed contract")
	}
	if !strings.Contains(stdout, "valid: false") || !strings.Contains(stdout, "failed: gereeni") {
		t.Fatalf("receipt: %q", stdout)
	}
	// A failed contract is still an attested run (Valid=false receipt).
	if _, err := os.Stat(filepath.Join(store, "attest", "demo-run.jsonl")); err != nil {
		t.Fatalf("failed run not attested: %v", err)
	}
}

func TestRunStepThenCheck(t *testing.T) {
	work := t.TempDir()
	f, _ := runFixture(t,
		"---\nname: demo-run\ndescription: step writes, check reads\nmetadata:\n  step-porota: \"echo made > "+filepath.ToSlash(filepath.Join(work, "made.txt"))+"\"\n  check-tests: \"test -f "+filepath.ToSlash(filepath.Join(work, "made.txt"))+"\"\n---", canonStep)
	stdout, stderr, err := f.run("run", "demo-run")
	if err != nil {
		t.Fatalf("run: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}
	if !strings.Contains(stdout, "valid: true") {
		t.Fatalf("receipt: %q", stdout)
	}
	if _, err := os.Stat(filepath.Join(work, "made.txt")); err != nil {
		t.Fatalf("step did not write: %v", err)
	}
	// Observed effects include write (the step ran).
	if !strings.Contains(stdout, "effects: read write") {
		t.Fatalf("effects line: %q", stdout)
	}
}

func TestRunPreflightRefusesUncappedEffects(t *testing.T) {
	f, store := runFixture(t,
		"---\nname: demo-run\ndescription: no cap declared\nmetadata:\n  check-tests: \"true\"\n---", canonNoCap)
	_, _, err := f.run("run", "demo-run")
	if err == nil || !strings.Contains(err.Error(), "pre-flight") {
		t.Fatalf("pre-flight did not refuse: %v", err)
	}
	// Refused before anything ran — nothing attested.
	if _, err := os.Stat(filepath.Join(store, "attest", "demo-run.jsonl")); err == nil {
		t.Fatal("refused run left an attestation")
	}
}

func TestRunRefusalsAndBindings(t *testing.T) {
	// Missing metadata command is a loud, named error.
	f, _ := runFixture(t,
		"---\nname: demo-run\ndescription: missing binding\n---", canonCheckOnly)
	_, _, err := f.run("run", "demo-run")
	if err == nil || !strings.Contains(err.Error(), "check-tests") {
		t.Fatalf("missing-binding error: %v", err)
	}

	// Prose-only skill cannot be machine-run.
	store := t.TempDir()
	src := t.TempDir()
	dir := writeSkillDir(t, src, "prose-only", "---\nname: prose-only\ndescription: no face\n---", "")
	f2 := &cobraRunner{t: t, opts: []Option{WithConfigDir(store)}}
	if _, _, err := f2.run("add", dir); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f2.run("run", "prose-only"); err == nil || !strings.Contains(err.Error(), "prose-only skill") {
		t.Fatalf("prose run: %v", err)
	}

	// Inapplicable skill refuses to run.
	dir = writeSkillDir(t, src, "not-here", "---\nname: not-here\ndescription: x\nmetadata:\n  requires: \"os=plan9\"\n  check-tests: \"true\"\n---", canonCheckOnly)
	if _, _, err := f2.run("add", dir); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f2.run("run", "not-here"); err == nil || !strings.Contains(err.Error(), "not applicable") {
		t.Fatalf("inapplicable run: %v", err)
	}
}

func TestRunJSONReceipt(t *testing.T) {
	f, _ := runFixture(t,
		"---\nname: demo-run\ndescription: json receipt\nmetadata:\n  check-tests: \"true\"\n---", canonCheckOnly)
	stdout, _, err := f.run("run", "demo-run", "--json")
	if err != nil {
		t.Fatalf("run --json: %v\n%s", err, stdout)
	}
	var rec AttestRecord
	if err := json.Unmarshal([]byte(stdout), &rec); err != nil {
		t.Fatalf("bad json: %v\n%s", err, stdout)
	}
	if !rec.Attest.Valid || len(rec.Attest.Passed) != 1 {
		t.Fatalf("record: %+v", rec)
	}
}
