// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package src_test

// S525.A10 proof: the installed CLI activates, restores and undoes through
// the REAL production verify-seal-gate path with no test-only wiring and no
// hand-minted evidence. Each activation seals through the trusted verifier
// first — the same verify call the production approve handler makes — and
// the CLI activates through the production gate; the CLI restore and undo
// verify the target themselves through the shared function the web route
// uses, so there is never anything to pre-seal. Three CLI activations, a
// CLI restore by activation id that moves the active revision, a
// schema-incompatible and a verifier-failing restore that both refuse with
// the reason and leave the active revision alone, and a CLI undo that
// returns to the pre-restore version.

import (
	"context"
	"runtime"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/qiangli/yoke/pkg/shape/state"
	"github.com/qiangli/yoke/pkg/shape/verify"
	"github.com/qiangli/yoke/pkg/src"
)

// alwaysPassGate is history setup only: it records a version on the line
// without judging it, the way a test-only activator would. It seals
// nothing, so every restore under test still verifies for real.
func alwaysPassGate() state.ActivationGate {
	return state.GateFunc(func(candidate, base, principal string) (state.EvidenceRef, error) {
		return state.EvidenceRef{Verdict: "pass", EvidenceID: "ev-setup"}, nil
	})
}

// s525a10gSeal seals the passing record the production gate reads by running
// the trusted verifier over the pair — the same verify-then-activate
// sequence the production approve handler runs. Test files may drive the
// verifier; only pkg/shape/verify seals records in non-test code.
func s525a10gSeal(t *testing.T, home, dest, candidate, base, principal string) {
	t.Helper()
	rep, err := verify.Run(context.Background(), verify.Options{
		Home: home, RepoDir: dest, BaseSHA: base, CandidateSHA: candidate,
		Principal: principal,
		Runner:    verify.TrustedRunner{Bin: "/bin/sh", Effects: []string{"read"}},
	})
	if err != nil {
		t.Fatalf("verify %s: %v", candidate, err)
	}
	if !rep.Passed() {
		t.Fatalf("verify %s refused: %s", candidate, rep.Record.Reason)
	}
}

func TestS525A10ProductionGateCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture units run under /bin/sh")
	}
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	dest, v1, v2, v3, _, narrow, _, broken := s525a10Fixture(t, home)
	const principal = s525a10Alice
	const app = s525a10App

	runCLI := func(args ...string) (string, error) {
		cmd := src.NewSrcCmd()
		var out strings.Builder
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		err := cmd.Execute()
		return out.String(), err
	}

	r, err := gogit.PlainOpen(dest)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := r.Reference(plumbing.ReferenceName("refs/heads/pristine"), true)
	if err != nil {
		t.Fatalf("pristine ref: %v", err)
	}
	pristine := ref.Hash().String()

	// Three activations through the binary-wired CLI, each sealed by the
	// trusted verifier the way a production approval seals its candidate.
	for _, step := range []struct{ rev, base string }{
		{v1, pristine}, {v2, v1}, {v3, v2},
	} {
		s525a10gSeal(t, home, dest, step.rev, step.base, principal)
		if out, err := runCLI("activate", app, step.rev, "--principal", principal); err != nil ||
			!strings.Contains(out, "activated") {
			t.Fatalf("activate CLI = %q, %v", out, err)
		}
	}
	if st := s525a10State(t, home, principal); st.ActiveRevision != v3 {
		t.Fatalf("after 3 activations active = %s, want %s", st.ActiveRevision, v3)
	}

	historyOf := func() src.VersionHistory {
		t.Helper()
		h, err := src.History(home, app, principal)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	tipOf := func() string {
		t.Helper()
		return historyOf().Tip
	}

	// The restore names the version-line activation commit from `src
	// history`, not the revision: history lists both and the shared
	// restore resolves either to the version's revision.
	var activationOfV1 string
	for _, e := range historyOf().Entries {
		if e.Revision == v1 {
			activationOfV1 = e.Activation
		}
	}
	if activationOfV1 == "" || activationOfV1 == v1 {
		t.Fatalf("no activation commit recorded for v1: %+v", historyOf().Entries)
	}

	// Restore succeeds through the CLI gate and moves the active revision.
	tipBefore := tipOf()
	if out, err := runCLI("restore", app, activationOfV1, "--user", principal); err != nil ||
		!strings.Contains(out, "restored") {
		t.Fatalf("restore CLI = %q, %v", out, err)
	}
	st := s525a10State(t, home, principal)
	if st.ActiveRevision != v1 || st.LastKnownGood != v3 {
		t.Fatalf("after restore state = %+v, want active v1 with last-known-good v3", st)
	}
	restoredTip := tipOf()
	if restoredTip == tipBefore {
		t.Fatal("restore did not append a forward commit to the principal line")
	}

	// Undo returns to the pre-restore version through the same gate.
	if out, err := runCLI("undo", app, "--user", principal); err != nil ||
		!strings.Contains(out, "restored") {
		t.Fatalf("undo CLI = %q, %v", out, err)
	}
	if st := s525a10State(t, home, principal); st.ActiveRevision != v3 {
		t.Fatalf("after undo active = %s, want %s", st.ActiveRevision, v3)
	}

	// Record the narrow and broken versions on the line so the refusals
	// below name real history: narrow seals clean through the verifier
	// (its schema range is not the verifier's business) and activates
	// through the CLI, then v3 comes back the same way so the current
	// data schema is 5 again. Broken can never seal — the frozen check
	// fails its tree — so it joins the line through a direct activation;
	// the restores under test stay fully real either way.
	s525a10gSeal(t, home, dest, narrow, v3, principal)
	if out, err := runCLI("activate", app, narrow, "--principal", principal); err != nil ||
		!strings.Contains(out, "activated") {
		t.Fatalf("activate narrow CLI = %q, %v", out, err)
	}
	s525a10gSeal(t, home, dest, v3, narrow, principal)
	if out, err := runCLI("activate", app, v3, "--principal", principal); err != nil ||
		!strings.Contains(out, "activated") {
		t.Fatalf("activate v3 CLI = %q, %v", out, err)
	}
	passGate := alwaysPassGate()
	if _, err := src.Activate(home, app, broken, principal, src.ActivateOptions{Gate: passGate}); err != nil {
		t.Fatalf("record broken: %v", err)
	}
	if st := s525a10State(t, home, principal); st.ActiveRevision != broken {
		t.Fatalf("after recording broken active = %s, want %s", st.ActiveRevision, broken)
	}
	s525a10gSeal(t, home, dest, v3, broken, principal)
	if out, err := runCLI("activate", app, v3, "--principal", principal); err != nil ||
		!strings.Contains(out, "activated") {
		t.Fatalf("activate v3 CLI = %q, %v", out, err)
	}
	refusalTip := tipOf()

	// A schema-incompatible target refuses with the reason; nothing moves.
	if out, err := runCLI("restore", app, narrow, "--user", principal); err == nil ||
		!strings.Contains(err.Error(), "schema") {
		t.Fatalf("schema-incompatible restore = %q, %v; want a schema refusal", out, err)
	}
	if st := s525a10State(t, home, principal); st.ActiveRevision != v3 {
		t.Fatalf("schema-refused restore moved active to %s", st.ActiveRevision)
	}
	if tipOf() != refusalTip {
		t.Fatal("schema-refused restore appended to the version line")
	}

	// A target the verifier fails refuses with the reason; nothing moves.
	if out, err := runCLI("restore", app, broken, "--user", principal); err == nil ||
		!strings.Contains(strings.ToLower(err.Error()+out), "refus") {
		t.Fatalf("verifier-failing restore = %q, %v; want a refusal", out, err)
	}
	if st := s525a10State(t, home, principal); st.ActiveRevision != v3 {
		t.Fatalf("refused restore moved active to %s", st.ActiveRevision)
	}
	if tipOf() != refusalTip {
		t.Fatal("refused restore appended to the version line")
	}
}
