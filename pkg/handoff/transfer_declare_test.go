// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package handoff

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/policy/coord"
)

// cliEnv isolates one resume/handoff CLI run: its own HOME, room and store,
// run inside a fresh git repo so CaptureWork sees a real tree.
func cliEnv(t *testing.T) (dir, repo string) {
	t.Helper()
	ledger(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_HOME", home)
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	dir = t.TempDir()
	t.Setenv("BASHY_HANDOFF_DIR", dir)
	repo = newRepo(t)
	prev, _ := os.Getwd()
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
	return dir, repo
}

// runResume executes one `bashy resume` invocation and returns its combined
// output. err is the command's error, which refusal paths assert on.
func runResume(t *testing.T, args ...string) (string, error) {
	t.Helper()
	r := NewResumeCmd()
	var out bytes.Buffer
	r.SetOut(&out)
	r.SetErr(&out)
	r.SetArgs(args)
	err := r.Execute()
	return out.String(), err
}

// runHandoff executes one `bashy handoff` invocation.
func runHandoff(t *testing.T, args ...string) string {
	t.Helper()
	h := NewHandoffCmd()
	var out bytes.Buffer
	h.SetOut(&out)
	h.SetErr(&out)
	h.SetArgs(args)
	if err := h.Execute(); err != nil {
		t.Fatalf("handoff %v: %v\n%s", args, err, out.String())
	}
	return out.String()
}

// soleID returns the one record id in dir, failing when the store does not
// hold exactly one handoff.
func soleID(t *testing.T, dir string) string {
	t.Helper()
	recs, err := List(dir)
	if err != nil || len(recs) != 1 {
		t.Fatalf("records = %v, %v", recs, err)
	}
	return recs[0].ID
}

// Reconcile accepts only observed outcomes. Anything else — uncertain, empty,
// or an unrecognized status — is refused, so an unknown status can never be
// laundered into a safe retry.
func TestReconcileRejectsUnknownStatus(t *testing.T) {
	tr := Summarize(fixtureRecord())
	tr.DeclareEffect("push image", "may have pushed")
	for _, st := range []EffectStatus{EffectUncertain, "", "maybe", "in-flight", "APPLIED"} {
		if err := tr.ReconcileEffect("fx-1", st, "muse"); err == nil {
			t.Fatalf("reconcile as %q succeeded", st)
		}
	}
	// And an effect carrying an unknown status reads as unsafe: the gate
	// stays closed until it is reconciled to applied or absent.
	tr.Effects[0].Status = "mystery"
	if len(tr.UncertainEffects()) != 1 {
		t.Fatal("unknown effect status must read as uncertain")
	}
	if err := tr.VerifyRetrySafe(); err == nil {
		t.Fatal("gate open with an unknown effect status")
	}
	if err := tr.ReconcileEffect("fx-1", EffectApplied, "muse"); err != nil {
		t.Fatalf("reconcile as applied: %v", err)
	}
	if err := tr.VerifyRetrySafe(); err != nil {
		t.Fatalf("gate closed after reconcile: %v", err)
	}
}

// The digest covers the whole candidate: a moved diff, a changed untracked
// mode, or different untracked bytes (text or binary) is a different
// candidate. Evidence bound to one must not validate another.
func TestWorkDigestCoversDiffModeAndBinary(t *testing.T) {
	base := WorkDigest(fixtureRecord().Work)
	rediffed := fixtureRecord().Work
	rediffed.Diff += "diff --git a/pkg/handoff/extra.go b/pkg/handoff/extra.go\n"
	if WorkDigest(rediffed) == base {
		t.Fatal("changed diff must change the candidate digest")
	}
	remoded := fixtureRecord().Work
	remoded.Untracked[0].Mode = 0o755
	if WorkDigest(remoded) == base {
		t.Fatal("changed untracked mode must change the candidate digest")
	}
	binary := fixtureRecord().Work
	binary.Untracked = append(binary.Untracked, UntrackedFile{
		Path:    "big/blob.bin",
		Content: "\x00\x01\x02\xff\xfebinary\n",
		Mode:    0o644,
	})
	if WorkDigest(binary) == base {
		t.Fatal("added binary untracked content must change the candidate digest")
	}
	if WorkDigest(fixtureRecord().Work) != base {
		t.Fatal("identical work must digest identically")
	}
}

// The agent loop end to end through the CLI: create declares the transfer,
// declare names the effect, transfer shows it, reconcile records the observed
// outcome, and only then does the claim proceed.
func TestDeclareReconcileClaimCLI(t *testing.T) {
	dir, _ := cliEnv(t)
	runHandoff(t, "-m", "half-pushed work", "--next", "reconcile, then rerun tests")
	id := soleID(t, dir)

	out, err := runResume(t, "--declare-effect", "push staging image", "--detail", "push ran before the host died", id)
	if err != nil {
		t.Fatalf("declare: %v\n%s", err, out)
	}
	if !strings.Contains(out, "fx-1") {
		t.Fatalf("declare must name the effect id:\n%s", out)
	}
	rec, err := Load(filepath.Join(dir, id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Declared() || len(rec.Transfer.Effects) != 1 || rec.Transfer.Effects[0] != "fx-1" {
		t.Fatalf("record declaration not updated with the effect: %+v", rec.Transfer)
	}

	out, err = runResume(t, "--transfer", id)
	if err != nil {
		t.Fatalf("transfer: %v\n%s", err, out)
	}
	if !strings.Contains(out, "push staging image") || !strings.Contains(out, "GATE: CLOSED") {
		t.Fatalf("transfer brief must show the effect and the closed gate:\n%s", out)
	}

	if out, err := runResume(t, "--claim", id); err == nil {
		t.Fatalf("claim succeeded with an uncertain effect:\n%s", out)
	} else if !strings.Contains(err.Error(), "uncertain") {
		t.Fatalf("refusal must name the uncertain effect: %v", err)
	}

	if _, err := runResume(t, "--reconcile-effect", "fx-1", "--as", "maybe", id); err == nil {
		t.Fatal("reconcile as an unknown status succeeded")
	}
	if out, err := runResume(t, "--reconcile-effect", "fx-1", "--as", "absent", id); err != nil {
		t.Fatalf("reconcile: %v\n%s", out, err)
	}
	if out, err := runResume(t, "--claim", id); err != nil {
		t.Fatalf("claim after reconcile: %v\n%s", out, err)
	}
}

// A clean scope is declared explicitly through the CLI, opening the gate.
func TestNoExternalEffectsCLI(t *testing.T) {
	dir, _ := cliEnv(t)
	runHandoff(t, "-m", "local-only edit")
	id := soleID(t, dir)

	if out, err := runResume(t, "--no-external-effects", id); err != nil {
		t.Fatalf("no-external-effects: %v\n%s", out, err)
	}
	if out, err := runResume(t, "--claim", id); err != nil {
		t.Fatalf("claim with declared clean scope: %v\n%s", out, err)
	}
}

// The portable pair travels: record plus sidecar copied to a fresh directory
// (the scp export) recomposes with its uncertain effect intact, and still
// refuses the pickup.
func TestExportRoundTripPreservesEffects(t *testing.T) {
	dir, _ := cliEnv(t)
	runHandoff(t, "-m", "exported work")
	id := soleID(t, dir)
	if _, err := runResume(t, "--declare-effect", "notify channel", id); err != nil {
		t.Fatalf("declare: %v", err)
	}

	arrived := t.TempDir()
	for _, name := range []string{id + ".json", id + ".transfer.json"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("export file %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(arrived, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rec, err := Load(filepath.Join(arrived, id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	full, err := ComposeChecked(arrived, rec)
	if err != nil {
		t.Fatalf("exported pair refused: %v", err)
	}
	if len(full.Effects) != 1 || full.Effects[0].Status != EffectUncertain {
		t.Fatalf("uncertain effect lost in export: %+v", full.Effects)
	}
	if err := full.VerifyRetrySafe(); err == nil {
		t.Fatal("exported gate open with an uncertain effect")
	}

	// The record alone is not a whole export: the declared transfer refuses
	// composition rather than resuming with dropped effects.
	half := t.TempDir()
	b, err := os.ReadFile(filepath.Join(dir, id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(half, id+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	halfRec, err := Load(filepath.Join(half, id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ComposeChecked(half, halfRec); err == nil {
		t.Fatal("record without its declared sidecar composed without error")
	} else if !strings.Contains(err.Error(), "sidecar") {
		t.Fatalf("refusal must name the missing sidecar: %v", err)
	}
}

// A declared transfer whose sidecar vanished refuses the pickup and the
// brief — it must not proceed having silently dropped uncertain effects.
func TestMissingSidecarRefusesDeclared(t *testing.T) {
	dir, _ := cliEnv(t)
	runHandoff(t, "-m", "work with effects")
	id := soleID(t, dir)
	if _, err := runResume(t, "--declare-effect", "push image", id); err != nil {
		t.Fatalf("declare: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, id+".transfer.json")); err != nil {
		t.Fatal(err)
	}
	if out, err := runResume(t, "--claim", id); err == nil {
		t.Fatalf("claim succeeded without the declared sidecar:\n%s", out)
	} else if !strings.Contains(err.Error(), "sidecar") {
		t.Fatalf("refusal must name the missing sidecar: %v", err)
	}
	if out, err := runResume(t, "--transfer", id); err == nil {
		t.Fatalf("transfer rendered without the declared sidecar:\n%s", out)
	} else if !strings.Contains(err.Error(), "sidecar") {
		t.Fatalf("refusal must name the missing sidecar: %v", err)
	}
	if cur, _ := Load(filepath.Join(dir, id+".json")); cur.ResumedAt != nil {
		t.Fatal("refused pickup stamped a claim")
	}
}

// Corruption is refused, not absorbed: an unreadable sidecar, a sidecar that
// lost the declared effect, and a sidecar whose status turned unrecognized
// all keep the pickup closed.
func TestCorruptSidecarRefusesDeclared(t *testing.T) {
	dir, _ := cliEnv(t)
	runHandoff(t, "-m", "work with effects")
	id := soleID(t, dir)
	if _, err := runResume(t, "--declare-effect", "push image", id); err != nil {
		t.Fatalf("declare: %v", err)
	}
	sidecar := filepath.Join(dir, id+".transfer.json")

	good, err := os.ReadFile(sidecar)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(sidecar, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runResume(t, "--claim", id); err == nil {
		t.Fatal("claim succeeded with an unreadable sidecar")
	}

	// Valid JSON that dropped the declared effect: a scope mismatch.
	dropped := &Transfer{HandoffID: id}
	if _, err := SaveTransfer(dir, dropped); err != nil {
		t.Fatal(err)
	}
	if _, err := runResume(t, "--claim", id); err == nil {
		t.Fatal("claim succeeded with a scope-mismatched sidecar")
	} else if !strings.Contains(err.Error(), "effect") {
		t.Fatalf("refusal must name the scope mismatch: %v", err)
	}

	// Restore the effect but corrupt its status: unknown reads as unsafe.
	if err := os.WriteFile(sidecar, good, 0o644); err != nil {
		t.Fatal(err)
	}
	side, err := LoadTransfer(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	side.Effects[0].Status = "mystery"
	if _, err := SaveTransfer(dir, side); err != nil {
		t.Fatal(err)
	}
	if out, err := runResume(t, "--claim", id); err == nil {
		t.Fatalf("claim succeeded with an unrecognized effect status:\n%s", out)
	} else if !strings.Contains(err.Error(), "uncertain") {
		t.Fatalf("refusal must name the uncertain effect: %v", err)
	}
}

// Legacy handoffs (written before the transfer envelope: no declaration, no
// sidecar) stay takeable with an explicit legacy warning — nothing to lose,
// nothing refused.
func TestLegacyWithoutSidecarProceeds(t *testing.T) {
	dir, _ := cliEnv(t)
	now := time.Now().UTC()
	legacy := &Record{
		ID:         NewID(now, "/w/legacy"),
		CreatedAt:  now,
		Project:    Project{Name: "p", Primary: "/w/legacy", Roots: []string{"/w/legacy"}},
		Continuity: "older handoff",
		Work:       WorkingState{Repo: "/w/legacy", Clean: true},
		Dispatch:   Dispatch{Disposition: DispatchPark},
	}
	if _, err := Save(dir, legacy); err != nil {
		t.Fatal(err)
	}
	out, err := runResume(t, "--claim", legacy.ID)
	if err != nil {
		t.Fatalf("legacy claim refused: %v\n%s", err, out)
	}
	if !strings.Contains(out, "legacy") {
		t.Fatalf("legacy pickup must say it is legacy:\n%s", out)
	}

	// And declaring on a legacy record upgrades it: sidecar plus declaration.
	dir2, _ := cliEnv(t)
	now2 := time.Now().UTC()
	legacy2 := &Record{
		ID:         NewID(now2, "/w/legacy2"),
		CreatedAt:  now2,
		Project:    Project{Name: "p", Primary: "/w/legacy2", Roots: []string{"/w/legacy2"}},
		Continuity: "older handoff",
		Work:       WorkingState{Repo: "/w/legacy2", Clean: true},
		Dispatch:   Dispatch{Disposition: DispatchPark},
	}
	// cliEnv reset BASHY_HANDOFF_DIR to dir2; the record below lives there.
	if _, err := Save(dir2, legacy2); err != nil {
		t.Fatal(err)
	}
	if _, err := runResume(t, "--declare-effect", "push image", legacy2.ID); err != nil {
		t.Fatalf("declare on legacy: %v", err)
	}
	cur, err := Load(filepath.Join(dir2, legacy2.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if !cur.Declared() || len(cur.Transfer.Effects) != 1 {
		t.Fatalf("legacy record not upgraded with a declaration: %+v", cur.Transfer)
	}
	if out, err := runResume(t, "--claim", legacy2.ID); err == nil {
		t.Fatalf("claim succeeded with a newly declared uncertain effect:\n%s", out)
	}
}

// Cancellation stays visible and blocks the pickup: --claim refuses a
// cancelled handoff, and no new effects may be recorded against it.
func TestCancelledBlocksPickupCLI(t *testing.T) {
	dir, _ := cliEnv(t)
	runHandoff(t, "-m", "doomed work")
	id := soleID(t, dir)
	if _, err := runResume(t, "--cancel", id); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if out, err := runResume(t, "--claim", id); err == nil {
		t.Fatalf("claim succeeded on a cancelled handoff:\n%s", out)
	} else if !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("refusal must say cancelled: %v", err)
	}
	if out, err := runResume(t, "--declare-effect", "push image", id); err == nil {
		t.Fatalf("declare succeeded on a cancelled handoff:\n%s", out)
	} else if !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("refusal must say cancelled: %v", err)
	}
	out, err := runResume(t, "--transfer", id)
	if err != nil {
		t.Fatalf("transfer on a cancelled handoff: %v\n%s", err, out)
	}
	if !strings.Contains(out, "explicitly cancelled") {
		t.Fatalf("transfer must keep the cancellation visible:\n%s", out)
	}
}

// The crash window between the two halves of a declaration write is safe
// because the Record lands FIRST: a failure after the record write leaves a
// declared record whose sidecar is stale (the birth sidecar, zero effects),
// and every read path refuses that pair instead of resuming without the
// uncertain effect. Copying only the record (the portable record-only export)
// refuses too — it never degrades to a legacy warning.
func TestDeclareCrashWindowRefusesDeclared(t *testing.T) {
	dir, _ := cliEnv(t)
	runHandoff(t, "-m", "half-pushed work")
	id := soleID(t, dir)

	oldSidecar := declSidecarSave
	declSidecarSave = func(string, *Transfer) (string, error) {
		return "", errors.New("injected sidecar write failure")
	}
	_, declErr := runResume(t, "--declare-effect", "push staging image", id)
	declSidecarSave = oldSidecar
	if declErr == nil || !strings.Contains(declErr.Error(), "injected sidecar write failure") {
		t.Fatalf("declare must surface the injected failure: %v", declErr)
	}

	rec, err := Load(filepath.Join(dir, id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Declared() || len(rec.Transfer.Effects) != 1 || rec.Transfer.Effects[0] != "fx-1" {
		t.Fatalf("crash after the record write must leave the declaration on disk: %+v", rec.Transfer)
	}
	if _, err := ComposeChecked(dir, rec); err == nil {
		t.Fatal("stale sidecar composed without error after the crash window")
	} else if !strings.Contains(err.Error(), "effect") {
		t.Fatalf("refusal must name the scope mismatch: %v", err)
	}
	if out, err := runResume(t, "--claim", id); err == nil {
		t.Fatalf("claim succeeded from the crash window:\n%s", out)
	} else if !strings.Contains(err.Error(), "effect") {
		t.Fatalf("claim refusal must name the scope mismatch: %v", err)
	}
	if out, err := runResume(t, "--transfer", id); err == nil {
		t.Fatalf("transfer rendered from the crash window:\n%s", out)
	} else if !strings.Contains(err.Error(), "effect") {
		t.Fatalf("transfer refusal must name the scope mismatch: %v", err)
	}
	if cur, _ := Load(filepath.Join(dir, id+".json")); cur.ResumedAt != nil {
		t.Fatal("refused pickup stamped a claim")
	}

	// The record alone is not a whole export: the declared transfer refuses
	// composition rather than resuming with dropped effects.
	half := t.TempDir()
	b, err := os.ReadFile(filepath.Join(dir, id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(half, id+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	halfRec, err := Load(filepath.Join(half, id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ComposeChecked(half, halfRec); err == nil {
		t.Fatal("record without its declared sidecar composed without error")
	} else if !strings.Contains(err.Error(), "sidecar") {
		t.Fatalf("refusal must name the missing sidecar: %v", err)
	}
}

// The same ordering holds for an explicit clean scope: a failure after the
// record write leaves a declared no-external-effects record whose sidecar
// disagrees, and the pickup refuses instead of opening the gate on half an
// export.
func TestNoEffectsCrashWindowRefuses(t *testing.T) {
	dir, _ := cliEnv(t)
	runHandoff(t, "-m", "local-only edit")
	id := soleID(t, dir)

	oldSidecar := declSidecarSave
	declSidecarSave = func(string, *Transfer) (string, error) {
		return "", errors.New("injected sidecar write failure")
	}
	_, declErr := runResume(t, "--no-external-effects", id)
	declSidecarSave = oldSidecar
	if declErr == nil || !strings.Contains(declErr.Error(), "injected sidecar write failure") {
		t.Fatalf("no-external-effects must surface the injected failure: %v", declErr)
	}

	rec, err := Load(filepath.Join(dir, id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Declared() || !rec.Transfer.NoExternalEffects {
		t.Fatalf("crash after the record write must leave the scope declaration on disk: %+v", rec.Transfer)
	}
	if out, err := runResume(t, "--claim", id); err == nil {
		t.Fatalf("claim succeeded from the crash window:\n%s", out)
	} else if !strings.Contains(err.Error(), "sidecar") && !strings.Contains(err.Error(), "scope") {
		t.Fatalf("claim refusal must name the half export: %v", err)
	}
}

// Equal lengths and membership alone cannot prove a complete effect scope.
func TestDuplicateEffectIDsRefusePickup(t *testing.T) {
	dir, _ := cliEnv(t)
	runHandoff(t, "-m", "two external effects")
	id := soleID(t, dir)
	for _, op := range []string{"push image", "notify channel"} {
		if _, err := runResume(t, "--declare-effect", op, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runResume(t, "--reconcile-effect", "fx-1", "--as", "applied", id); err != nil {
		t.Fatal(err)
	}
	side, err := LoadTransfer(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	side.Effects[1] = side.Effects[0] // hide uncertain fx-2 with safe fx-1
	if _, err := SaveTransfer(dir, side); err != nil {
		t.Fatal(err)
	}
	if _, err := runResume(t, "--claim", id); err == nil {
		t.Fatal("duplicate sidecar IDs opened pickup")
	}
	rec, _ := stored(dir, id)
	if rec.ResumedAt != nil {
		t.Fatal("corrupt sidecar stamped ownership")
	}
	rec.Transfer.Effects[1] = rec.Transfer.Effects[0]
	if err := checkSidecar(rec, side); err == nil {
		t.Fatal("duplicate declaration accepted")
	}
}

// A safe pre-lock snapshot must not hide an effect declared before pickup.
func TestClaimRechecksEffectsUnderLock(t *testing.T) {
	dir, _ := cliEnv(t)
	runHandoff(t, "-m", "interrupted work")
	id := soleID(t, dir)
	stale, _ := stored(dir, id)
	if _, err := ComposeChecked(dir, stale); err != nil {
		t.Fatal(err)
	}
	if _, err := runResume(t, "--declare-effect", "notify channel", id); err != nil {
		t.Fatal(err)
	}
	applied := false
	if _, err := Claim(context.Background(), dir, stale, taker("replacement", "new"), func() error { applied = true; return nil }); err == nil || !strings.Contains(err.Error(), "uncertain") {
		t.Fatalf("stale snapshot claim = %v", err)
	}
	if applied {
		t.Fatal("unsafe claim applied work")
	}
	cur, _ := stored(dir, id)
	if cur.ResumedAt != nil || len(cur.Transfer.Effects) != 1 {
		t.Fatal("claim lost effect or stamped ownership")
	}
}

// Effect writers cannot enter between the final check and ownership stamp.
func TestClaimSerializesEffectMutations(t *testing.T) {
	dir, _ := cliEnv(t)
	runHandoff(t, "-m", "local work")
	id := soleID(t, dir)
	rec, _ := stored(dir, id)
	_, err := Claim(context.Background(), dir, rec, coord.Self(), func() error {
		for _, args := range [][]string{
			{"--declare-effect", "notify channel", id},
			{"--reconcile-effect", "fx-1", "--as", "applied", id},
			{"--no-external-effects", id},
		} {
			if _, err := runResume(t, args...); err == nil {
				t.Fatalf("mutation entered held pickup: %v", args)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	cur, _ := stored(dir, id)
	if cur.ResumedAt == nil || len(cur.Transfer.Effects) != 0 {
		t.Fatal("mutation changed held pickup")
	}
}
