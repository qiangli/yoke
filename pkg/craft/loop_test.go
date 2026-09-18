package craft

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/skills"
)

// THE LOOP, end to end, through the command surface rather than the API.
//
// Every piece of this is unit-tested somewhere else, and that is exactly why
// this file exists: the parts were right while the loop was open. A fact
// recorded by the shell's learning hook has to survive the trip through the
// fact store, the entity parser, the resolver and the renderer to reach the
// artifact an agent actually reads — and a fold recorded with no coordinate has
// to land at the coordinate compose reads back, or it is written and never
// seen again.

// craftCLI runs one craft command line against a private store, returning
// stdout and stderr separately — the split is load-bearing (compose puts the
// artifact on stdout and its provenance on stderr), so a test that merged them
// could not tell a rendered skill from a report about one.
func craftCLI(t *testing.T, store string, args ...string) (string, string, error) {
	t.Helper()
	root := NewCraftCmd(WithStoreDir(store), WithSkillOptions(skills.WithConfigDir(store)))
	var out, errs bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errs)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), errs.String(), err
}

// installSkill writes a dual-bundle skill (prose + canonical face) into a store.
func installSkill(t *testing.T, store, name, desc, canon string, meta map[string]string) {
	t.Helper()
	dir := filepath.Join(store, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("---\nname: " + name + "\ndescription: " + desc + "\n")
	if len(meta) > 0 {
		b.WriteString("metadata:\n")
		for k, v := range meta {
			b.WriteString("  " + k + ": \"" + v + "\"\n")
		}
	}
	b.WriteString("---\n\n# " + name + "\n")
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skill.dhnt"), []byte(canon+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoop_WhatTheShellLearnedReachesTheComposedSkill(t *testing.T) {
	store := t.TempDir()
	installSkill(t, store, "repo-health",
		"Verify a repository builds and its tests pass",
		goBuildTest,
		map[string]string{"check-build": "go build ./...", "check-tests": "go test ./..."})

	// 1. The shell's learning hook, verbatim: bashy's ExecHandler middleware
	//    calls exactly these two functions on a command that exited 0.
	x, usable := Extract([]string{"ssh", "-p", "2222", "-l", "svc-build", "remote-host"})
	if !usable {
		t.Fatal("the hook learned nothing from a fully-specified ssh invocation")
	}
	facts := OpenFacts(store)
	for _, f := range FactsFrom(x, "exec:ssh") {
		if err := facts.Record(f); err != nil {
			t.Fatalf("recording %s: %v", f.Key, err)
		}
	}

	// 2. It is visible as knowledge about the HOST, not about ssh. That binding
	//    is the whole point: knowledge filed under a command only helps rerun
	//    that command.
	out, _, err := craftCLI(t, store, "facts")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "host:remote-host") {
		t.Fatalf("`craft facts` does not know the host the hook learned about:\n%s", out)
	}
	out, _, err = craftCLI(t, store, "facts", "host:remote-host")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"svc-build", "2222", "exec:ssh"} {
		if !strings.Contains(out, want) {
			t.Errorf("`craft facts host:remote-host` is missing %q:\n%s", want, out)
		}
	}

	// 3. A fold recorded with NO coordinate must land where compose reads. This
	//    is the join the whole fold half depends on, and it fails silently: the
	//    write succeeds either way.
	if _, _, err := craftCLI(t, store, "fold",
		"discovery by name is unreliable in this environment; resolve the address first",
		"--evidence", "three consecutive lookup timeouts"); err != nil {
		t.Fatalf("fold with no --coordinate: %v", err)
	}
	out, _, err = craftCLI(t, store, "folds")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "(here)") {
		t.Fatalf("the fold did not land at this host's coordinate — it was written where nothing reads:\n%s", out)
	}

	// 4. The artifact carries both, and the second agent inherits what the
	//    first one learned without anyone writing it down.
	body, prov, err := craftCLI(t, store, "compose", "verify the repository builds",
		"--for", "host:remote-host")
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	if !strings.Contains(body, "svc-build") || !strings.Contains(body, "2222") {
		t.Errorf("the composed skill does not carry what the host learned:\n%s", body)
	}
	if !strings.Contains(body, "resolve the address first") {
		t.Errorf("the composed skill does not carry the fold that holds here:\n%s", body)
	}
	if !strings.Contains(prov, "facts=2") || !strings.Contains(prov, "folds=1") {
		t.Errorf("the provenance line miscounts what was folded in: %s", prov)
	}
	// Values are carried in the BODY and counted in the report. A count that
	// became a value would make every logged composition a leak.
	if strings.Contains(prov, "svc-build") {
		t.Errorf("the provenance line names a fact value: %s", prov)
	}
}

// A composed skill must not be answered by an unrelated capability. This is the
// same defect as `find`'s, one layer up and much worse: find prints a wrong
// row, compose prints a wrong SCRIPT, which is the thing that gets run.
func TestLoop_ComposeRefusesAnUnrelatedQuestion(t *testing.T) {
	store := t.TempDir()
	installSkill(t, store, "repo-health",
		"Verify a repository is healthy with one machine-verified, attested command",
		goBuildTest, map[string]string{"check-build": "go build ./...", "check-tests": "go test ./..."})

	body, _, err := craftCLI(t, store, "compose", "ssh into a machine")
	if err == nil {
		t.Fatalf("compose answered a question it has no skill for:\n%s", body)
	}
	if !strings.Contains(err.Error(), "no capability matches") {
		t.Errorf("the refusal does not say what is wrong: %v", err)
	}
}

// `learn` reports what it DID, which is not always what it was asked to do.
//
// Both halves were confident and wrong. Forgetting a key nothing was believed
// about printed "forgot <key>", so a typo read as a completed withdrawal. And a
// key outside the role vocabulary — `remote_user`, which this command's own
// example used to suggest — is stored but can never be offered to a command,
// silently, until the day somebody expects it to be.
func TestLearn_SaysWhatItActuallyDid(t *testing.T) {
	store := t.TempDir()

	_, errs, err := craftCLI(t, store, "learn", "host:remote-host", "user", "--forget")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(errs, "forgot") || !strings.Contains(errs, "nothing to forget") {
		t.Errorf("forgetting what was never believed reported: %q", strings.TrimSpace(errs))
	}

	_, errs, err = craftCLI(t, store, "learn", "host:remote-host", "remote_user", "svc-build")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errs, "not a role") {
		t.Errorf("a key that can never be offered was recorded without saying so: %q", strings.TrimSpace(errs))
	}

	_, errs, err = craftCLI(t, store, "learn", "host:remote-host", "user", "svc-build")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(errs, "not a role") {
		t.Errorf("a role key was reported as untransferable: %q", strings.TrimSpace(errs))
	}

	_, errs, err = craftCLI(t, store, "learn", "host:remote-host", "user", "--forget")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errs, "forgot") {
		t.Errorf("a believed fact was not reported as forgotten: %q", strings.TrimSpace(errs))
	}
}

// A cold store is a legitimate state, and every verb has to say so in words.
// An empty table reads as a bug, and a bare exit 0 reads as success — both send
// the reader looking for a failure that did not happen.
func TestLoop_ColdStoreAnswersInWords(t *testing.T) {
	store := t.TempDir()
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"find", "anything at all"}, "no capability matches"},
		{[]string{"facts"}, "nothing learned yet"},
		{[]string{"facts", "host:unknown-host"}, "nothing learned about"},
		{[]string{"folds"}, "nothing folded yet"},
		{[]string{"promote"}, "nothing has repeated"},
		{[]string{"history"}, "no recorded runs"},
		{[]string{}, "no recorded runs"},
	} {
		out, _, err := craftCLI(t, store, tc.args...)
		if err != nil {
			t.Errorf("craft %v on a cold store: %v", tc.args, err)
			continue
		}
		if !strings.Contains(out, tc.want) {
			t.Errorf("craft %v said %q, want it to say %q", tc.args, strings.TrimSpace(out), tc.want)
		}
	}
}
