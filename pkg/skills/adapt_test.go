package skills

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestConfig(store string) *config {
	return &config{statics: map[string]string{}, cfgDir: store, cacheTTL: time.Hour}
}

// getSkill resolves a name through the config's catalog for the engine
// tests (unit level — no cobra).
func getSkill(t *testing.T, cfg *config, name string) (Skill, Source) {
	t.Helper()
	sk, src, ok := cfg.catalog().Get(name)
	if !ok {
		t.Fatalf("skill %q not found", name)
	}
	return sk, src
}

func TestParseRepairReply(t *testing.T) {
	canon, meta, err := parseRepairReply(
		"chatter\n<dhnt>sokilili demo fini</dhnt>\n<meta>\nstep-faceto: echo hi\ncheck-tests: true\nnot-a-binding: ignored\n</meta>\n")
	if err != nil || canon != "sokilili demo fini" {
		t.Fatalf("canon=%q err=%v", canon, err)
	}
	if meta["step-faceto"] != "echo hi" || meta["check-tests"] != "true" || len(meta) != 2 {
		t.Fatalf("meta=%v", meta)
	}
	if _, _, err := parseRepairReply("no blocks here"); err == nil {
		t.Fatal("missing <dhnt> not rejected")
	}
}

// The full contribution loop: baseline fails, the repair agent proposes
// a step that fixes the world, the fix is verified under the ORIGINAL
// contract, folded into the overlay, and the next plain run uses it.
func TestAdaptiveRepairAndReuse(t *testing.T) {
	store, work := t.TempDir(), t.TempDir()
	flag := filepath.ToSlash(filepath.Join(work, "flag.txt"))

	cfg := newTestConfig(store)
	src := t.TempDir()
	dir := writeSkillDir(t, src, "demo-heal",
		"---\nname: demo-heal\ndescription: heals\nmetadata:\n  check-tests: \"test -f "+flag+"\"\n---",
		"sokilili demo efefecato reada wurite fini enisure gereeni fini fini\n")
	f := &cobraRunner{t: t, opts: []Option{WithConfigDir(store)}}
	if _, _, err := f.run("add", dir); err != nil {
		t.Fatal(err)
	}

	calls := 0
	complete := func(prompt string) (string, error) {
		calls++
		if !strings.Contains(prompt, "Contract that must still hold: gereeni") ||
			!strings.Contains(prompt, "check-tests: test -f") {
			t.Fatalf("prompt missing context:\n%s", prompt)
		}
		return "<dhnt>sokilili demo efefecato reada wurite fini sotepo one faceto fini enisure gereeni fini fini</dhnt>\n" +
			"<meta>\nstep-faceto: echo healed > " + flag + "\n</meta>\n", nil
	}

	sk, source := getSkill(t, cfg, "demo-heal")
	ps, _ := cfg.probes(false)
	rec, outcome, err := adaptiveRun(cfg, sk, source, ps, work, io.Discard, complete, 2)
	if err != nil || outcome != OutcomeRepaired || !rec.Attest.Valid || calls != 1 {
		t.Fatalf("outcome=%s err=%v calls=%d attest=%+v", outcome, err, calls, rec.Attest)
	}
	// Overlay + learned bindings persisted.
	if m := loadBindings(store, "demo-heal"); m["step-faceto"] == "" {
		t.Fatalf("bindings not saved: %v", m)
	}
	if entries, _ := os.ReadDir(filepath.Join(store, "versions")); len(entries) != 1 {
		t.Fatalf("overlay not saved")
	}

	// Drift: remove the flag — the OVERLAY version re-creates it via the
	// folded arm on a plain (non-adaptive) run.
	if err := os.Remove(filepath.Join(work, "flag.txt")); err != nil {
		t.Fatal(err)
	}
	sk, source = getSkill(t, cfg, "demo-heal")
	rec2, _, err := runSkill(cfg, sk, source, ps, work, io.Discard)
	if err != nil || !rec2.Attest.Valid {
		t.Fatalf("overlay rerun: err=%v attest=%+v", err, rec2.Attest)
	}
	// And adaptiveRun reports it as the overlay outcome without calling
	// the completer again.
	if err := os.Remove(filepath.Join(work, "flag.txt")); err != nil {
		t.Fatal(err)
	}
	rec3, outcome3, err := adaptiveRun(cfg, sk, source, ps, work, io.Discard, complete, 2)
	if err != nil || outcome3 != OutcomeOverlay || !rec3.Attest.Valid || calls != 1 {
		t.Fatalf("reuse: outcome=%s err=%v calls=%d", outcome3, err, calls)
	}
}

// A model cannot pass by weakening the spec: the candidate's own
// contract/cap are discarded; the ORIGINAL is grafted on.
func TestAdaptiveRejectsSpecWeakening(t *testing.T) {
	store, work := t.TempDir(), t.TempDir()
	cfg := newTestConfig(store)
	src := t.TempDir()
	dir := writeSkillDir(t, src, "demo-weak",
		"---\nname: demo-weak\ndescription: x\nmetadata:\n  check-tests: \"false\"\n---",
		"sokilili demo efefecato reada wurite fini enisure gereeni fini fini\n")
	f := &cobraRunner{t: t, opts: []Option{WithConfigDir(store)}}
	if _, _, err := f.run("add", dir); err != nil {
		t.Fatal(err)
	}
	// Candidate drops the contract entirely and does nothing.
	complete := func(string) (string, error) {
		return "<dhnt>sokilili demo efefecato reada wurite fini fini</dhnt>", nil
	}
	sk, source := getSkill(t, cfg, "demo-weak")
	ps, _ := cfg.probes(false)
	_, outcome, err := adaptiveRun(cfg, sk, source, ps, work, io.Discard, complete, 1)
	if err == nil || outcome != OutcomeFailed {
		t.Fatalf("weakened candidate accepted: outcome=%s err=%v", outcome, err)
	}
	if entries, _ := os.ReadDir(filepath.Join(store, "versions")); len(entries) != 0 {
		t.Fatal("rejected candidate reached the overlay")
	}
}

// A model cannot escalate the blast radius: candidate steps need write,
// the base cap is read-only, pre-flight rejects before anything runs.
func TestAdaptiveRejectsCapEscalation(t *testing.T) {
	store, work := t.TempDir(), t.TempDir()
	cfg := newTestConfig(store)
	src := t.TempDir()
	dir := writeSkillDir(t, src, "demo-cap",
		"---\nname: demo-cap\ndescription: x\nmetadata:\n  check-tests: \"false\"\n---",
		"sokilili demo efefecato reada fini enisure gereeni fini fini\n")
	f := &cobraRunner{t: t, opts: []Option{WithConfigDir(store)}}
	if _, _, err := f.run("add", dir); err != nil {
		t.Fatal(err)
	}
	marker := filepath.ToSlash(filepath.Join(work, "escalate.txt"))
	complete := func(string) (string, error) {
		return "<dhnt>sokilili demo efefecato reada wurite fini sotepo one faceto fini enisure gereeni fini fini</dhnt>\n" +
			"<meta>\nstep-faceto: echo x > " + marker + "\n</meta>", nil
	}
	sk, source := getSkill(t, cfg, "demo-cap")
	ps, _ := cfg.probes(false)
	_, outcome, err := adaptiveRun(cfg, sk, source, ps, work, io.Discard, complete, 1)
	if err == nil || outcome != OutcomeFailed {
		t.Fatalf("cap escalation accepted: outcome=%s err=%v", outcome, err)
	}
	if _, statErr := os.Stat(filepath.Join(work, "escalate.txt")); statErr == nil {
		t.Fatal("escalating step actually ran — pre-flight did not stop it")
	}
}

func TestLearnGate(t *testing.T) {
	store := t.TempDir()
	f := &cobraRunner{t: t, opts: []Option{WithConfigDir(store)}}
	src := t.TempDir()

	// Passing skill is learned and stays.
	good := writeSkillDir(t, src, "learn-good",
		"---\nname: learn-good\ndescription: x\nmetadata:\n  check-tests: \"true\"\n---",
		"sokilili demo efefecato reada fini enisure gereeni fini fini\n")
	stdout, _, err := f.run("learn", good)
	if err != nil || !strings.Contains(stdout, "outcome: learned") {
		t.Fatalf("learn good: %v\n%s", err, stdout)
	}
	if _, err := os.Stat(filepath.Join(store, "learn-good", "SKILL.md")); err != nil {
		t.Fatal("learned skill missing from store")
	}

	// Failing skill is refused AND removed; the failed run stays attested.
	bad := writeSkillDir(t, src, "learn-bad",
		"---\nname: learn-bad\ndescription: x\nmetadata:\n  check-tests: \"false\"\n---",
		"sokilili demo efefecato reada fini enisure gereeni fini fini\n")
	if _, _, err := f.run("learn", bad); err == nil {
		t.Fatal("failing skill learned")
	}
	if _, err := os.Stat(filepath.Join(store, "learn-bad")); err == nil {
		t.Fatal("failed skill left in store")
	}
	if _, err := os.Stat(filepath.Join(store, "attest", "learn-bad.jsonl")); err != nil {
		t.Fatal("failed learn run not attested")
	}
}

func TestPromoteBundle(t *testing.T) {
	store, work := t.TempDir(), t.TempDir()
	flag := filepath.ToSlash(filepath.Join(work, "flag.txt"))
	cfg := newTestConfig(store)
	src := t.TempDir()
	dir := writeSkillDir(t, src, "demo-promote",
		"---\nname: demo-promote\ndescription: x\nmetadata:\n  check-tests: \"test -f "+flag+"\"\n---",
		"sokilili demo efefecato reada wurite fini enisure gereeni fini fini\n")
	f := &cobraRunner{t: t, opts: []Option{WithConfigDir(store)}}
	if _, _, err := f.run("add", dir); err != nil {
		t.Fatal(err)
	}
	complete := func(string) (string, error) {
		return "<dhnt>sokilili demo efefecato reada wurite fini sotepo one faceto fini enisure gereeni fini fini</dhnt>\n" +
			"<meta>\nstep-faceto: echo healed > " + flag + "\n</meta>", nil
	}
	sk, source := getSkill(t, cfg, "demo-promote")
	ps, _ := cfg.probes(false)
	if _, outcome, err := adaptiveRun(cfg, sk, source, ps, work, io.Discard, complete, 1); err != nil || outcome != OutcomeRepaired {
		t.Fatalf("repair: %s %v", outcome, err)
	}

	out := filepath.Join(t.TempDir(), "bundle")
	sk, source = getSkill(t, cfg, "demo-promote")
	if _, err := promoteBundle(cfg, sk, source, out); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"SKILL.md", "skill.dhnt", "bindings.json", "PROMOTION.md"} {
		if _, err := os.Stat(filepath.Join(out, name)); err != nil {
			t.Fatalf("bundle missing %s", name)
		}
	}
	promo, err := os.ReadFile(filepath.Join(out, "PROMOTION.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(promo)
	if !strings.Contains(text, "base identity: `"+sk.Dhnt.Identity+"`") ||
		!strings.Contains(text, "preserved from base: **true**") ||
		!strings.Contains(text, "step-faceto") ||
		!strings.Contains(text, "Reviewer checklist") {
		t.Fatalf("PROMOTION.md incomplete:\n%s", text)
	}
	// The promoted canonical is the FOLDED version (env-guarded arm).
	canon, err := os.ReadFile(filepath.Join(out, "skill.dhnt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(canon), "wuheni conitexuto") {
		t.Fatalf("promoted canonical not the folded version: %q", canon)
	}
}
