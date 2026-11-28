package src_test

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/shape/evidence"
	"github.com/qiangli/yoke/pkg/shape/verify"
	"github.com/qiangli/yoke/pkg/src"
)

// TestPRGateBindsTheSpecAsGraded: a spec-changing candidate (it edits the
// frozen check) merges only on evidence graded against its own spec under
// the owner's review of exactly that spec. Unreviewed evidence, and
// evidence graded by a recorded version's own spec (a restore, which is no
// owner review), are refused.
func TestPRGateBindsTheSpecAsGraded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in interpreter is a POSIX shell")
	}
	setup := func(t *testing.T) (string, string, src.PR, verify.SpecDelta) {
		t.Helper()
		home := t.TempDir()
		t.Setenv("BASHY_AGENT", "test-agent")
		t.Setenv("BASHY_PRINCIPAL", testPrincipal)
		_, newSHA := seedFixture(t, home)
		fetchAt(t, home, newSHA)
		dir, baseSHA := shapeBase(t, home)
		files := baseFiles()
		files["checks/smoke.bsh"] = baseCheck + "test -s out.txt\n"
		pr := candidatePR(t, home, "spec-change", files)
		delta, err := verify.ReadSpecDelta(dir, baseSHA, pr.HeadSHA, nil)
		if err != nil || len(delta.Changed) != 1 {
			t.Fatalf("spec delta = %+v, %v", delta, err)
		}
		return home, baseSHA, pr, delta
	}
	run := func(t *testing.T, home, baseSHA string, pr src.PR, spec string) verify.Report {
		t.Helper()
		rep, err := verify.Run(context.Background(), verify.Options{
			Home: home, RepoDir: destOf(home), BaseSHA: baseSHA, CandidateSHA: pr.HeadSHA,
			Principal: testPrincipal, Runner: interpreter{}, Spec: spec,
		})
		if err != nil {
			t.Fatalf("verify.Run: %v", err)
		}
		return rep
	}

	t.Run("reviewed", func(t *testing.T) {
		home, baseSHA, pr, delta := setup(t)
		rep := run(t, home, baseSHA, pr, delta.Candidate)
		if !rep.Passed() || rep.Record.SpecReview != delta.Candidate || rep.Record.Binding.Contract != delta.Candidate {
			t.Fatalf("reviewed record = %+v", rep.Record)
		}
		// Re-verifying the PR carries the owner's review forward.
		rec, err := src.VerifyPR(context.Background(), home, pr.ID, interpreter{})
		if err != nil || rec.Verdict != evidence.VerdictPass || rec.SpecReview != delta.Candidate {
			t.Fatalf("VerifyPR = %+v, %v", rec, err)
		}
		if _, err := src.MergePR(home, pr.ID); err != nil {
			t.Fatalf("MergePR of a reviewed spec change: %v", err)
		}
	})

	t.Run("unreviewed", func(t *testing.T) {
		home, baseSHA, pr, _ := setup(t)
		if rep := run(t, home, baseSHA, pr, ""); rep.Passed() {
			t.Fatalf("an unreviewed spec change passed: %+v", rep.Record)
		}
		rec, err := src.VerifyPR(context.Background(), home, pr.ID, interpreter{})
		if err != nil || rec.Verdict != evidence.VerdictFail {
			t.Fatalf("VerifyPR of an unreviewed spec change = %+v, %v", rec, err)
		}
		if _, err := src.MergePR(home, pr.ID); err == nil {
			t.Fatal("an unreviewed spec change merged")
		}
	})

	t.Run("mismatched", func(t *testing.T) {
		home, baseSHA, pr, delta := setup(t)
		// A review of another spec is refused by the verifier.
		if rep := run(t, home, baseSHA, pr, delta.Base); rep.Passed() {
			t.Fatalf("a review of another spec passed: %+v", rep.Record)
		}
		if _, err := src.MergePR(home, pr.ID); err == nil {
			t.Fatal("a mismatched review merged")
		}
		// A passing record graded by the version's own spec (the restore
		// path) is not an owner review: the gate refuses it.
		if rep := run(t, home, baseSHA, pr, verify.VersionSpec); !rep.Passed() || rep.Record.SpecReview != "" {
			t.Fatalf("version-spec record = %+v", rep.Record)
		}
		_, err := src.MergePR(home, pr.ID)
		if err == nil || !errors.Is(err, verify.ErrSpecUnreviewed) || !strings.Contains(err.Error(), "checks/smoke.bsh (changed)") {
			t.Fatalf("MergePR on version-spec evidence err = %v, want an unreviewed spec refusal", err)
		}
	})
}
