// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package src_test

// S525.A10c proof: the installed CLI restores and undoes through the
// production evidence-bound gate with no test-only wiring. The test builds
// the src cobra command the way the binary does and never calls
// RegisterActivationGate: three CLI activations, a CLI restore that moves
// the active revision, a schema-incompatible and a gate-failing restore
// that both refuse with the reason and leave the active revision alone,
// and a CLI undo that returns to the pre-restore version.

import (
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/qiangli/yoke/pkg/shape/evidence"
	"github.com/qiangli/yoke/pkg/shape/verify"
	"github.com/qiangli/yoke/pkg/src"
)

// s525a10gSeal mints the sealed passing record the production gate reads:
// this exact candidate, base, principal and verifier build. Test files may
// mint records; only pkg/shape/verify may do so in non-test code.
func s525a10gSeal(t *testing.T, home, candidate, base, principal string) {
	t.Helper()
	rec := evidence.Record{
		Binding: evidence.Binding{
			Candidate: candidate,
			Base:      base,
			Verifier:  verify.BuildID(),
			Principal: principal,
		},
		Verdict: evidence.VerdictPass,
	}
	if _, err := evidence.Write(home, rec); err != nil {
		t.Fatalf("seal evidence for %s: %v", candidate, err)
	}
}

func TestS525A10ProductionGateCLI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	dest, v1, v2, v3, _, narrow, _ := s525a10Fixture(t, home)
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

	// Three activations through the binary-wired CLI, each with the sealed
	// evidence the production gate demands for its candidate/base pair.
	for _, step := range []struct{ rev, base string }{
		{v1, pristine}, {v2, v1}, {v3, v2},
	} {
		s525a10gSeal(t, home, step.rev, step.base, principal)
		if out, err := runCLI("activate", app, step.rev, "--principal", principal); err != nil ||
			!strings.Contains(out, "activated") {
			t.Fatalf("activate CLI = %q, %v", out, err)
		}
	}
	if st := s525a10State(t, home, principal); st.ActiveRevision != v3 {
		t.Fatalf("after 3 activations active = %s, want %s", st.ActiveRevision, v3)
	}

	tipOf := func() string {
		t.Helper()
		h, err := src.History(home, app, principal)
		if err != nil {
			t.Fatal(err)
		}
		return h.Tip
	}

	// Restore succeeds through the CLI gate and moves the active revision.
	s525a10gSeal(t, home, v1, tipOf(), principal)
	tipBefore := tipOf()
	if out, err := runCLI("restore", app, v1, "--user", principal); err != nil ||
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

	// A schema-incompatible target refuses with the reason; nothing moves.
	if out, err := runCLI("restore", app, narrow, "--user", principal); err == nil ||
		!strings.Contains(err.Error(), "schema") {
		t.Fatalf("schema-incompatible restore = %q, %v; want a schema refusal", out, err)
	}
	if st := s525a10State(t, home, principal); st.ActiveRevision != v1 {
		t.Fatalf("schema-refused restore moved active to %s", st.ActiveRevision)
	}
	if tipOf() != restoredTip {
		t.Fatal("schema-refused restore appended to the version line")
	}

	// A target with no sealed evidence refuses at the gate; nothing moves.
	if out, err := runCLI("restore", app, v2, "--user", principal); err == nil ||
		!strings.Contains(strings.ToLower(err.Error()+out), "refus") {
		t.Fatalf("gate-failing restore = %q, %v; want an evidence refusal", out, err)
	}
	if st := s525a10State(t, home, principal); st.ActiveRevision != v1 {
		t.Fatalf("gate-refused restore moved active to %s", st.ActiveRevision)
	}
	if tipOf() != restoredTip {
		t.Fatal("gate-refused restore appended to the version line")
	}

	// Undo returns to the pre-restore version through the same gate.
	s525a10gSeal(t, home, v3, tipOf(), principal)
	if out, err := runCLI("undo", app, "--user", principal); err != nil ||
		!strings.Contains(out, "restored") {
		t.Fatalf("undo CLI = %q, %v", out, err)
	}
	if st := s525a10State(t, home, principal); st.ActiveRevision != v3 {
		t.Fatalf("after undo active = %s, want %s", st.ActiveRevision, v3)
	}
}
