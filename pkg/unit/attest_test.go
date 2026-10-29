// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package unit_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/dag"
	"github.com/qiangli/yoke/pkg/skills"
	"github.com/qiangli/yoke/pkg/unit"
)

// THE ACCEPTANCE OF STORY #1896, spelled as a test: a dag target receipt and
// a skill run receipt are sealed by real engines on both sides of the
// boundary, and they are ONE Go type (unit.Attestation) carrying ONE
// vocabulary — target, valid, checks, effects, clause.

// runDagTarget seals a real dag target receipt: a task file with the
// canonical contract grammar (Ensure:/Effects:), run through the dag engine.
func runDagTarget(t *testing.T, dir string) *dag.Attestation {
	t.Helper()
	md := "## Tasks\n\n### make\n" +
		"Generates: out.txt\nEnsure: file-exists out.txt\nEffects: write\n" +
		"```bash\necho hi > out.txt\n```\n"
	doc, err := dag.Parse(strings.NewReader(md), filepath.Join(dir, "tasks.md"))
	if err != nil {
		t.Fatalf("dag.Parse: %v", err)
	}
	g, err := dag.BuildGraph(doc)
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	eng := &dag.Engine{Graph: g, Dir: dir, Env: os.Environ(), Concurrency: 1, FailFast: true,
		Stdout: new(bytes.Buffer), Stderr: new(bytes.Buffer)}
	report, err := eng.Run(context.Background(), "make")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Results) != 1 || report.Results[0].Attestation == nil {
		t.Fatalf("report = %+v, want one attested target", report.Results)
	}
	return report.Results[0].Attestation
}

// runSkill seals a real skill run receipt: a contracted skill added to a
// real store and run through the skills CLI, then read back from the
// ring-1 JSONL the way every reader since P2 does.
func runSkill(t *testing.T, store string) skills.AttestRecord {
	t.Helper()
	src := t.TempDir()
	dir := filepath.Join(src, "demo-run")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	skillMd := "---\nname: demo-run\ndescription: one contract\nmetadata:\n  check-tests: \"true\"\n---\n# body\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(skillMd), 0o644); err != nil {
		t.Fatal(err)
	}
	// Canonical dhnt face: cap {read}, contract {gereeni} (bound to the
	// check-tests metadata command above).
	canon := "sokilili demo efefecato reada fini enisure gereeni fini fini\n"
	if err := os.WriteFile(filepath.Join(dir, "skill.dhnt"), []byte(canon), 0o644); err != nil {
		t.Fatal(err)
	}

	run := func(args ...string) string {
		t.Helper()
		cmd := skills.NewSkillsCmd(skills.WithConfigDir(store))
		var out, errb bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&errb)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("skills %s: %v\nstdout=%s\nstderr=%s", args, err, out.String(), errb.String())
		}
		return out.String()
	}
	run("add", dir)
	run("run", "demo-run", "--json")

	data, err := os.ReadFile(filepath.Join(store, "attest", "demo-run.jsonl"))
	if err != nil {
		t.Fatalf("stored receipt: %v", err)
	}
	var rec skills.AttestRecord
	if err := json.Unmarshal(bytes.TrimSpace(data), &rec); err != nil {
		t.Fatalf("stored receipt does not decode: %v (%s)", err, data)
	}
	return rec
}

// TestDagAndSkillReceiptsAreOneType is the story's headline: both engines
// seal their verdict in the one receipt type, so a dag target receipt and a
// skill run receipt sit in one []unit.Attestation — a line that compiles
// only because dag.Attestation is an alias of unit.Attestation and
// AttestRecord embeds it.
func TestDagAndSkillReceiptsAreOneType(t *testing.T) {
	dagReceipt := runDagTarget(t, t.TempDir()) // *dag.Attestation == *unit.Attestation
	rec := runSkill(t, t.TempDir())

	receipts := []unit.Attestation{*dagReceipt, rec.Attestation}

	d, s := receipts[0], receipts[1]
	if d.Target != "make" || !d.Valid {
		t.Errorf("dag receipt = %+v, want make/valid", d)
	}
	if len(d.Checks) != 1 || d.Checks[0].Expr != "file-exists out.txt" || !d.Checks[0].Pass {
		t.Errorf("dag checks = %+v", d.Checks)
	}
	if len(d.Effects) != 1 || d.Effects[0] != "write" {
		t.Errorf("dag effects = %v", d.Effects)
	}
	if s.Target != "demo-run" || !s.Valid {
		t.Errorf("skill receipt = %+v, want demo-run/valid", s)
	}
	if len(s.Checks) != 1 || s.Checks[0].Expr != "gereeni" || !s.Checks[0].Pass {
		t.Errorf("skill checks = %+v", s.Checks)
	}
	if len(s.Effects) != 1 || s.Effects[0] != "read" {
		t.Errorf("skill effects = %v", s.Effects)
	}
	// Identity and capability are stamped: the unit that ran, the promise
	// it was keeping. The record's own capability shadows the embedded one,
	// so it is the one `capability` key the stored JSON carries.
	if s.Identity != rec.Attest.Skill || !strings.HasPrefix(rec.Capability, "k") {
		t.Errorf("skill receipt identity/capability not stamped: %+v (record capability %q)", s, rec.Capability)
	}
	// The dhnt half is still there, wrapped not forked: the exact version
	// that ran and the tier it ran under.
	if !strings.HasPrefix(rec.Attest.Skill, "h") || rec.Attest.Tier != rec.Tier {
		t.Errorf("dhnt attestation lost: %+v (tier %s)", rec.Attest, rec.Tier)
	}
}

// TestReceiptJSONFixturesStillDecode locks the additive rule: receipts
// written before the shared vocabulary still decode, and a receipt written
// by both vocabularies at once decodes with both halves intact.
func TestReceiptJSONFixturesStillDecode(t *testing.T) {
	// A dag target receipt exactly as dag shipped it (runItem.attestation).
	const dagReceipt = `{"target":"make","valid":true,"checks":[{"expr":"file-exists out.txt","pass":true}],"effects":["write"]}`
	var d unit.Attestation
	if err := json.Unmarshal([]byte(dagReceipt), &d); err != nil {
		t.Fatalf("dag fixture: %v", err)
	}
	if d.Target != "make" || !d.Valid || len(d.Checks) != 1 || len(d.Effects) != 1 || d.Effects[0] != "write" {
		t.Errorf("dag fixture decoded wrong: %+v", d)
	}
	// ...and through the dag alias: one type, one decode.
	var viaDag dag.Attestation
	if err := json.Unmarshal([]byte(dagReceipt), &viaDag); err != nil {
		t.Fatalf("dag fixture via alias: %v", err)
	}
	if !reflect.DeepEqual(viaDag, d) {
		t.Errorf("alias decode drifted: %+v vs %+v", viaDag, d)
	}

	// A skill run receipt exactly as the ring-1 store shipped it before the
	// shared vocabulary (pkg/craft's legacy fixture shape): the dhnt
	// attestation under `attest`, no unified fields yet.
	const legacySkill = `{"at":"2026-07-01T00:00:00Z","name":"old","tier":"bashy@old","context_key":"c1","attest":{"Skill":"hold","Valid":true}}`
	var old skills.AttestRecord
	if err := json.Unmarshal([]byte(legacySkill), &old); err != nil {
		t.Fatalf("legacy skill fixture: %v", err)
	}
	if old.Name != "old" || old.Attest.Skill != "hold" || !old.Attest.Valid {
		t.Errorf("legacy skill fixture decoded wrong: %+v", old)
	}
	// The unified half is absent, not fabricated: Target is the "not
	// stamped" marker, and the verdict stays where it was written.
	if old.Attestation.Target != "" || old.Attestation.Valid {
		t.Errorf("legacy receipt invented unified fields: %+v", old.Attestation)
	}

	// A receipt written by both vocabularies at once: the wrapped dhnt
	// attestation unchanged, the unified fields added beside it, one
	// `capability` key (the record's own, shallower field).
	const both = `{"at":"2026-10-11T00:00:00Z","name":"demo","tier":"local","context_key":"ck","capability":"k0",` +
		`"attest":{"Skill":"h0","Tier":"local","Passed":["gereeni"],"Effects":[0],"Valid":true},` +
		`"target":"demo","valid":true,"checks":[{"expr":"gereeni","pass":true}],"effects":["read"]}`
	var rec skills.AttestRecord
	if err := json.Unmarshal([]byte(both), &rec); err != nil {
		t.Fatalf("dual-vocabulary fixture: %v", err)
	}
	if rec.Attest.Skill != "h0" || !rec.Attest.Valid || len(rec.Attest.Passed) != 1 {
		t.Errorf("dhnt half decoded wrong: %+v", rec.Attest)
	}
	if rec.Attestation.Target != "demo" || !rec.Attestation.Valid ||
		len(rec.Attestation.Checks) != 1 || rec.Attestation.Checks[0].Expr != "gereeni" ||
		len(rec.Attestation.Effects) != 1 || rec.Attestation.Effects[0] != "read" {
		t.Errorf("unified half decoded wrong: %+v", rec.Attestation)
	}
	if rec.Capability != "k0" {
		t.Errorf("capability key = %q, want the record's own k0", rec.Capability)
	}
}
