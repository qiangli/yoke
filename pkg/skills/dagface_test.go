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

// --list answers the tasks.md targets and the SAME effects --target itself
// audits — no ordering flake, no running anything — and an untasked skill
// answers nothing (exit 0), not an error: a caller that turns this into
// extra methods must leave a plain skill's methods alone.
func TestRunTargetList(t *testing.T) {
	store, work := t.TempDir(), t.TempDir()
	src := t.TempDir()
	dir := writeTasksSkill(t, src, "taskful",
		"---\nname: taskful\ndescription: has targets\n---", "", tasksFixture(work))
	f := &cobraRunner{t: t, opts: []Option{WithConfigDir(store)}}
	if _, _, err := f.run("add", dir); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := f.run("run", "taskful", "--list")
	if err != nil {
		t.Fatalf("--list: %v", err)
	}
	if !strings.Contains(stdout, `"name":"hello"`) || !strings.Contains(stdout, `"name":"boom"`) {
		t.Fatalf("stdout: %q", stdout)
	}
	if !strings.Contains(stdout, `"read"`) {
		t.Fatalf("missing implied read effect: %q", stdout)
	}
	// Running nothing: the target's own marker file is untouched.
	if _, err := os.Stat(filepath.Join(work, "made.txt")); err == nil {
		t.Fatal("--list ran a target")
	}
	// --list stands alone.
	if _, _, err := f.run("run", "taskful", "--list", "--target", "hello"); err == nil {
		t.Fatal("--list --target combined")
	}
	// An untasked skill answers nothing, not an error.
	plain := writeSkillDir(t, src, "no-tasks", "---\nname: no-tasks\ndescription: x\n---", "")
	if _, _, err := f.run("add", plain); err != nil {
		t.Fatal(err)
	}
	stdout, _, err = f.run("run", "no-tasks", "--list")
	if err != nil || strings.TrimSpace(stdout) != "" {
		t.Fatalf("untasked --list: stdout=%q err=%v", stdout, err)
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

func writeInlineTasksSkill(t *testing.T, root, name, frontmatter, canon, tasks string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := frontmatter + "\n\n" + tasks
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if canon != "" {
		if err := os.WriteFile(filepath.Join(dir, "skill.dhnt"), []byte(canon), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// Inline Tasks parity: a SKILL.md with a '## Tasks' section exposes the exact
// same targets, effects, and execution behavior as the tasks.md form.
func TestRunTargetInlineParity(t *testing.T) {
	store := t.TempDir()
	src := t.TempDir()
	workTasksMD, workInline := t.TempDir(), t.TempDir()

	// 1. Target listing parity between tasks.md and inline Tasks.
	tasksMDDir := writeTasksSkill(t, src, "task-md-skill",
		"---\nname: task-md-skill\ndescription: tasks.md form\n---", "", tasksFixture(workTasksMD))
	inlineDir := writeInlineTasksSkill(t, src, "inline-skill",
		"---\nname: inline-skill\ndescription: inline tasks form\n---", "", tasksFixture(workInline))

	f := &cobraRunner{t: t, opts: []Option{WithConfigDir(store)}}
	if _, _, err := f.run("add", tasksMDDir); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.run("add", inlineDir); err != nil {
		t.Fatal(err)
	}

	stdoutMD, _, err := f.run("run", "task-md-skill", "--list")
	if err != nil {
		t.Fatalf("tasks.md list: %v", err)
	}
	stdoutInline, _, err := f.run("run", "inline-skill", "--list")
	if err != nil {
		t.Fatalf("inline list: %v", err)
	}
	if stdoutMD != stdoutInline {
		t.Fatalf("listing mismatch:\ntasks.md form:\n%s\ninline form:\n%s", stdoutMD, stdoutInline)
	}

	// Declared effects listing parity
	workAuditedMD, workAuditedInline := t.TempDir(), t.TempDir()
	auditedMD := writeTasksSkill(t, src, "audited-md",
		"---\nname: audited-md\ndescription: declared effects\nmetadata:\n  check-tests: \"true\"\n---",
		"sokilili demo efefecato reada fini enisure gereeni fini fini\n",
		effectsFixture(workAuditedMD))
	auditedInline := writeInlineTasksSkill(t, src, "audited-inline",
		"---\nname: audited-inline\ndescription: declared effects\nmetadata:\n  check-tests: \"true\"\n---",
		"sokilili demo efefecato reada fini enisure gereeni fini fini\n",
		effectsFixture(workAuditedInline))
	if _, _, err := f.run("add", auditedMD); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.run("add", auditedInline); err != nil {
		t.Fatal(err)
	}

	stdoutAuditedMD, _, err := f.run("run", "audited-md", "--list")
	if err != nil {
		t.Fatalf("audited-md --list: %v", err)
	}
	stdoutAuditedInline, _, err := f.run("run", "audited-inline", "--list")
	if err != nil {
		t.Fatalf("audited-inline --list: %v", err)
	}
	if stdoutAuditedMD != stdoutAuditedInline {
		t.Fatalf("declared effects listing mismatch:\ntasks.md:\n%s\ninline:\n%s", stdoutAuditedMD, stdoutAuditedInline)
	}

	// 2. Uncontracted inline target execution
	stdout, _, err := f.run("run", "inline-skill", "--target", "hello")
	if err != nil {
		t.Fatalf("inline target hello: %v\n%s", err, stdout)
	}
	if !strings.Contains(stdout, "ok: true") {
		t.Fatalf("stdout: %q", stdout)
	}
	if _, err := os.Stat(filepath.Join(workInline, "made.txt")); err != nil {
		t.Fatal("inline target did not run")
	}
	if _, err := os.Stat(filepath.Join(store, "attest", "inline-skill.jsonl")); err == nil {
		t.Fatal("uncontracted target run attested")
	}

	// Failing target propagates
	if _, _, err := f.run("run", "inline-skill", "--target", "boom"); err == nil {
		t.Fatal("boom target exited 0")
	}

	// Unknown target errors with the inventory
	_, _, err = f.run("run", "inline-skill", "--target", "nope")
	if err == nil || !strings.Contains(err.Error(), "hello") {
		t.Fatalf("unknown target: %v", err)
	}

	// 3. Contracted inline target execution
	workContracted := t.TempDir()
	flagContracted := filepath.ToSlash(filepath.Join(workContracted, "made.txt"))
	contractedDir := writeInlineTasksSkill(t, src, "contracted-inline",
		"---\nname: contracted-inline\ndescription: contracted inline\nmetadata:\n  check-tests: \"test -f "+flagContracted+"\"\n---",
		"sokilili demo efefecato reada wurite fini enisure gereeni fini fini\n",
		tasksFixture(workContracted))
	if _, _, err := f.run("add", contractedDir); err != nil {
		t.Fatal(err)
	}
	stdout, _, err = f.run("run", "contracted-inline", "--target", "hello")
	if err != nil {
		t.Fatalf("contracted inline target: %v\n%s", err, stdout)
	}
	if !strings.Contains(stdout, "valid: true") || !strings.Contains(stdout, "outcome: target:hello") ||
		!strings.Contains(stdout, "effects: read write") {
		t.Fatalf("contracted receipt: %q", stdout)
	}
	if _, err := os.Stat(filepath.Join(store, "attest", "contracted-inline.jsonl")); err != nil {
		t.Fatal("contracted target run not attested")
	}

	// 4. Declared effects narrow the audit for inline tasks
	stdout, _, err = f.run("run", "audited-inline", "--target", "pure")
	if err != nil {
		t.Fatalf("audited-inline pure: %v\n%s", err, stdout)
	}
	if !strings.Contains(stdout, "effects: read\n") {
		t.Fatalf("receipt effects: %q", stdout)
	}
	_, _, err = f.run("run", "audited-inline", "--target", "checkup")
	if err == nil || !strings.Contains(err.Error(), "pre-flight") {
		t.Fatalf("audited-inline closure refusal: %v", err)
	}

	// 5. Embedded skill with inline Tasks in SKILL.md
	workEmbedded := t.TempDir()
	embedded := fstest.MapFS{
		"embed-inline/SKILL.md": {Data: []byte("---\nname: embed-inline\ndescription: embedded inline\n---\n\n" + tasksFixture(workEmbedded))},
	}
	fEmbed := &cobraRunner{t: t, opts: []Option{
		WithSource(EmbedSource(embedded, RingEmbedded)),
		WithConfigDir(t.TempDir()),
	}}
	stdout, _, err = fEmbed.run("run", "embed-inline", "--target", "hello")
	if err != nil {
		t.Fatalf("embedded inline target: %v\n%s", err, stdout)
	}
	if _, err := os.Stat(filepath.Join(workEmbedded, "made.txt")); err != nil {
		t.Fatal("embedded inline target did not run")
	}
}

// Both forms present: a SKILL.md with BOTH an inline '## Tasks' section AND
// metadata.tasks: (or bundled tasks.md) must refuse loudly, never merging silently.
func TestRunTargetBothFormsRefusal(t *testing.T) {
	store, work := t.TempDir(), t.TempDir()
	src := t.TempDir()

	// Form 1: Inline Tasks + metadata.tasks pointer
	bothMetaDir := writeInlineTasksSkill(t, src, "both-meta",
		"---\nname: both-meta\ndescription: has inline and meta pointer\nmetadata:\n  tasks: \"tasks.md\"\n---",
		"", tasksFixture(work))
	f := &cobraRunner{t: t, opts: []Option{WithConfigDir(store)}}
	if _, _, err := f.run("add", bothMetaDir); err != nil {
		t.Fatal(err)
	}

	_, _, err := f.run("run", "both-meta", "--target", "hello")
	if err == nil || !strings.Contains(err.Error(), "both") {
		t.Fatalf("want refusal for inline + metadata.tasks on --target, got err=%v", err)
	}
	_, _, err = f.run("run", "both-meta", "--list")
	if err == nil || !strings.Contains(err.Error(), "both") {
		t.Fatalf("want refusal for inline + metadata.tasks on --list, got err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(work, "made.txt")); err == nil {
		t.Fatal("refused target still ran")
	}

	// Form 2: Inline Tasks + bundled tasks.md file
	bothBundledDir := writeInlineTasksSkill(t, src, "both-bundled",
		"---\nname: both-bundled\ndescription: has inline and bundled tasks.md\n---",
		"", tasksFixture(work))
	if err := os.WriteFile(filepath.Join(bothBundledDir, "tasks.md"), []byte(tasksFixture(work)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.run("add", bothBundledDir, "--force"); err != nil {
		t.Fatal(err)
	}

	_, _, err = f.run("run", "both-bundled", "--target", "hello")
	if err == nil || !strings.Contains(err.Error(), "both") {
		t.Fatalf("want refusal for inline + bundled tasks.md on --target, got err=%v", err)
	}
	_, _, err = f.run("run", "both-bundled", "--list")
	if err == nil || !strings.Contains(err.Error(), "both") {
		t.Fatalf("want refusal for inline + bundled tasks.md on --list, got err=%v", err)
	}
}
