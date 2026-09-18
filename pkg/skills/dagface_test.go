package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// tasksFixture is a real dag task file: targets are ### children of a
// `## Tasks` section (the dag parser's rule), bodies are fenced bash.
func tasksFixture(work string) string {
	flag := filepath.ToSlash(filepath.Join(work, "made.txt"))
	return "# demo pipeline\n\n## Tasks\n\n### hello\n\nWrites the marker.\n\n```bash\necho made > " + flag + "\n```\n\n### boom\n\nAlways fails.\n\n```bash\nexit 3\n```\n"
}

func writeTasksSkill(t *testing.T, root, name, frontmatter, canon, tasks string) string {
	t.Helper()
	dir := writeSkillDir(t, root, name, frontmatter, canon)
	if err := os.WriteFile(filepath.Join(dir, "tasks.md"), []byte(tasks), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Uncontracted skill with a tasks face: the target executes through the
// dag engine, plainly (no attestation — rung "tasks").
func TestRunTargetUncontracted(t *testing.T) {
	store, work := t.TempDir(), t.TempDir()
	src := t.TempDir()
	dir := writeTasksSkill(t, src, "taskful",
		"---\nname: taskful\ndescription: has targets\n---", "", tasksFixture(work))
	f := &cobraRunner{t: t, opts: []Option{WithConfigDir(store)}}
	if _, _, err := f.run("add", dir); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := f.run("run", "taskful", "--target", "hello")
	if err != nil {
		t.Fatalf("target hello: %v\n%s", err, stdout)
	}
	if !strings.Contains(stdout, "ok: true") {
		t.Fatalf("stdout: %q", stdout)
	}
	if _, err := os.Stat(filepath.Join(work, "made.txt")); err != nil {
		t.Fatal("target did not run")
	}
	// No attestation for the uncontracted rung.
	if _, err := os.Stat(filepath.Join(store, "attest", "taskful.jsonl")); err == nil {
		t.Fatal("uncontracted target run attested")
	}
	// A failing target propagates.
	if _, _, err := f.run("run", "taskful", "--target", "boom"); err == nil {
		t.Fatal("boom target exited 0")
	}
	// Unknown target errors with the inventory.
	_, _, err = f.run("run", "taskful", "--target", "nope")
	if err == nil || !strings.Contains(err.Error(), "hello") {
		t.Fatalf("unknown target: %v", err)
	}
	// --target without a tasks face is a clear error.
	plain := writeSkillDir(t, src, "no-tasks", "---\nname: no-tasks\ndescription: x\n---", "")
	if _, _, err := f.run("add", plain); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.run("run", "no-tasks", "--target", "hello"); err == nil || !strings.Contains(err.Error(), "tasks.md") {
		t.Fatalf("no-tasks: %v", err)
	}
	// --target + --adapt refuse to combine.
	if _, _, err := f.run("run", "taskful", "--target", "hello", "--adapt"); err == nil {
		t.Fatal("--target --adapt combined")
	}
}

// Contracted skill: the dag target runs AS the steps phase — contract
// evaluated, effects observed, receipt attested.
func TestRunTargetContracted(t *testing.T) {
	store, work := t.TempDir(), t.TempDir()
	flag := filepath.ToSlash(filepath.Join(work, "made.txt"))
	src := t.TempDir()
	dir := writeTasksSkill(t, src, "taskful",
		"---\nname: taskful\ndescription: contracted targets\nmetadata:\n  check-tests: \"test -f "+flag+"\"\n---",
		"sokilili demo efefecato reada wurite fini enisure gereeni fini fini\n",
		tasksFixture(work))
	f := &cobraRunner{t: t, opts: []Option{WithConfigDir(store)}}
	if _, _, err := f.run("add", dir); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := f.run("run", "taskful", "--target", "hello")
	if err != nil {
		t.Fatalf("target: %v\n%s", err, stdout)
	}
	if !strings.Contains(stdout, "valid: true") || !strings.Contains(stdout, "outcome: target:hello") ||
		!strings.Contains(stdout, "effects: read write") {
		t.Fatalf("receipt: %q", stdout)
	}
	if _, err := os.Stat(filepath.Join(store, "attest", "taskful.jsonl")); err != nil {
		t.Fatal("contracted target run not attested")
	}
}

// The effect cap still governs: a read-only cap refuses a target run at
// pre-flight (targets are read+write by convention).
func TestRunTargetCapRefusal(t *testing.T) {
	store, work := t.TempDir(), t.TempDir()
	src := t.TempDir()
	dir := writeTasksSkill(t, src, "narrow",
		"---\nname: narrow\ndescription: read-only cap\nmetadata:\n  check-tests: \"true\"\n---",
		"sokilili demo efefecato reada fini enisure gereeni fini fini\n",
		tasksFixture(work))
	f := &cobraRunner{t: t, opts: []Option{WithConfigDir(store)}}
	if _, _, err := f.run("add", dir); err != nil {
		t.Fatal(err)
	}
	_, _, err := f.run("run", "narrow", "--target", "hello")
	if err == nil || !strings.Contains(err.Error(), "pre-flight") {
		t.Fatalf("cap refusal: %v", err)
	}
	if _, err := os.Stat(filepath.Join(work, "made.txt")); err == nil {
		t.Fatal("refused target still ran")
	}
}

// Embedded skills (no on-disk dir) materialize tasks.md to a temp file.
func TestRunTargetEmbedded(t *testing.T) {
	work := t.TempDir()
	embedded := fstest.MapFS{
		"taskful/SKILL.md": {Data: []byte("---\nname: taskful\ndescription: embedded targets\n---\nbody\n")},
		"taskful/tasks.md": {Data: []byte(tasksFixture(work))},
	}
	f := &cobraRunner{t: t, opts: []Option{
		WithSource(EmbedSource(embedded, RingEmbedded)),
		WithConfigDir(t.TempDir()),
	}}
	stdout, _, err := f.run("run", "taskful", "--target", "hello")
	if err != nil {
		t.Fatalf("embedded target: %v\n%s", err, stdout)
	}
	if _, err := os.Stat(filepath.Join(work, "made.txt")); err != nil {
		t.Fatal("embedded target did not run")
	}
}

// effectsFixture: a Tasks section where every target declares Effects:,
// including a Requires edge (checkup depends on prep).
func effectsFixture(work string) string {
	flag := filepath.ToSlash(filepath.Join(work, "made.txt"))
	return "# pipeline\n\n## Tasks\n\n### prep\n\nWrites the marker.\nEffects: write\n\n```bash\necho made > " + flag + "\n```\n\n### checkup\n\nPure check.\nRequires: prep\nEffects: read\n\n```bash\ntest -f " + flag + "\n```\n\n### pure\n\nStandalone pure check.\nEffects: read\n\n```bash\ntrue\n```\n"
}

// Declared Effects: narrow the audit: a read-only cap ACCEPTS a target
// whose closure declares only read, and still REFUSES one whose closure
// includes write. Unknown atoms are loud.
func TestRunTargetDeclaredEffects(t *testing.T) {
	store, work := t.TempDir(), t.TempDir()
	src := t.TempDir()
	dir := writeTasksSkill(t, src, "audited",
		"---\nname: audited\ndescription: declared effects\nmetadata:\n  check-tests: \"true\"\n---",
		"sokilili demo efefecato reada fini enisure gereeni fini fini\n", // READ-ONLY cap
		effectsFixture(work))
	f := &cobraRunner{t: t, opts: []Option{WithConfigDir(store)}}
	if _, _, err := f.run("add", dir); err != nil {
		t.Fatal(err)
	}

	// pure declares read only → runs under the read-only cap.
	stdout, _, err := f.run("run", "audited", "--target", "pure")
	if err != nil {
		t.Fatalf("pure: %v\n%s", err, stdout)
	}
	if !strings.Contains(stdout, "effects: read\n") {
		t.Fatalf("receipt effects: %q", stdout)
	}

	// checkup's CLOSURE includes prep (write) → refused at pre-flight.
	_, _, err = f.run("run", "audited", "--target", "checkup")
	if err == nil || !strings.Contains(err.Error(), "pre-flight") {
		t.Fatalf("closure refusal: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(work, "made.txt")); statErr == nil {
		t.Fatal("refused closure still ran prep")
	}

	// Unknown atom is a loud error, not a guess (own skill: dag itself
	// also rejects the atom, which would poison sibling targets).
	weird := writeTasksSkill(t, src, "weird",
		"---\nname: weird\ndescription: x\nmetadata:\n  check-tests: \"true\"\n---",
		"sokilili demo efefecato reada fini enisure gereeni fini fini\n",
		"# w\n\n## Tasks\n\n### zap\n\nEffects: teleport\n\n```bash\ntrue\n```\n")
	if _, _, err := f.run("add", weird); err != nil {
		t.Fatal(err)
	}
	_, _, err = f.run("run", "weird", "--target", "zap")
	if err == nil || !strings.Contains(err.Error(), "teleport") {
		t.Fatalf("unknown atom: %v", err)
	}
}

// With a wide-enough cap, a declared closure runs and the receipt
// reports exactly the declared union.
func TestRunTargetDeclaredClosureRuns(t *testing.T) {
	store, work := t.TempDir(), t.TempDir()
	src := t.TempDir()
	dir := writeTasksSkill(t, src, "audited",
		"---\nname: audited\ndescription: declared effects\nmetadata:\n  check-tests: \"test -f "+filepath.ToSlash(filepath.Join(work, "made.txt"))+"\"\n---",
		"sokilili demo efefecato reada wurite fini enisure gereeni fini fini\n",
		effectsFixture(work))
	f := &cobraRunner{t: t, opts: []Option{WithConfigDir(store)}}
	if _, _, err := f.run("add", dir); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := f.run("run", "audited", "--target", "checkup")
	if err != nil {
		t.Fatalf("checkup: %v\n%s", err, stdout)
	}
	if !strings.Contains(stdout, "valid: true") || !strings.Contains(stdout, "effects: read write") {
		t.Fatalf("receipt: %q", stdout)
	}
	if _, err := os.Stat(filepath.Join(work, "made.txt")); err != nil {
		t.Fatal("dependency prep did not run")
	}
}

// The tasks POINTER: a skill with no bundled tasks.md executes a dag
// file the repo already has, via metadata `tasks: <repo-relative path>`.
func TestRunTargetPointer(t *testing.T) {
	store := t.TempDir()
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "ci"), 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.ToSlash(filepath.Join(repo, "pointer-ran.txt"))
	dagFile := "# ci\n\n## Tasks\n\n### hello\n\n```bash\necho ran > " + marker + "\n```\n"
	if err := os.WriteFile(filepath.Join(repo, "ci", "pipeline.md"), []byte(dagFile), 0o644); err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	dir := writeSkillDir(t, src, "pointerish",
		"---\nname: pointerish\ndescription: points at the repo pipeline\nmetadata:\n  tasks: \"ci/pipeline.md\"\n---", "")
	f := &cobraRunner{t: t, opts: []Option{WithConfigDir(store)}}
	if _, _, err := f.run("add", dir); err != nil {
		t.Fatal(err)
	}

	t.Chdir(repo)
	stdout, _, err := f.run("run", "pointerish", "--target", "hello")
	if err != nil {
		t.Fatalf("pointer run: %v\n%s", err, stdout)
	}
	if _, err := os.Stat(filepath.Join(repo, "pointer-ran.txt")); err != nil {
		t.Fatal("pointed target did not run")
	}

	// Escapes are refused loudly.
	for _, bad := range []string{"../outside.md", "/etc/anything.md"} {
		badDir := writeSkillDir(t, src, "escapee",
			"---\nname: escapee\ndescription: x\nmetadata:\n  tasks: \""+bad+"\"\n---", "")
		if _, _, err := f.run("add", badDir, "--force"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := f.run("run", "escapee", "--target", "hello"); err == nil {
			t.Fatalf("pointer escape %q allowed", bad)
		}
	}
}
