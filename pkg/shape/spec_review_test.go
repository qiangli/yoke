package shape

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	shapeverify "github.com/qiangli/yoke/pkg/shape/verify"
	"github.com/qiangli/yoke/pkg/src"
)

// The spec-change fixture replays the shape of ClassGo sprint 733 S4: the
// baseline v1 renders the date line as "undefined" and its check S1 pins
// that; the wish "show the date range above the totals" needs v2, whose
// date line reads the week's dates, together with S2, the check that pins
// the new text.
const (
	specV1Date = "undefined"
	specV2Date = "2026-02-09 to 2026-02-15"
	specWish   = "show the date range above the totals"
)

func specCheck(date string) string {
	return "test \"$(cat public/date.txt)\" = \"" + date + "\"\n"
}

func specRunner(t *testing.T) shapeverify.Runner {
	t.Helper()
	bin := "/bin/sh"
	if runtime.GOOS == "windows" {
		bin = os.Getenv("BASHY_BIN")
		if bin == "" {
			bin = "bashy"
		}
		var err error
		if bin, err = exec.LookPath(bin); err != nil {
			t.Skipf("native fixture shell: %v", err)
		}
	}
	return shapeverify.TrustedRunner{Bin: bin, Effects: []string{"read"}}
}

func newSpecChangeFixture(t *testing.T) (Service, string, string) {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, "src", "summary")
	for _, d := range []string{"public", "checks"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"public/date.txt":   specV1Date + "\n",
		"contract.bsh":      "@effects(\"read\")\n",
		"checks/render.bsh": specCheck(specV1Date),
		"shaped.json":       `{"scopes": {"shape": {"circles": ["contractor"]}}}`,
	}
	for rel, body := range files {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(rel)), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := r.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := wt.AddWithOptions(&gogit.AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	base, err := wt.Commit("weekly summary: baseline", &gogit.CommitOptions{
		Author: &object.Signature{Name: "owner", Email: "o@example.test", When: time.Now()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("pristine"), base)); err != nil {
		t.Fatal(err)
	}
	runner := specRunner(t)
	del := &fakeMemberDelegate{edit: func(work, task string) error {
		// The contractor's fix changes the implementation and the
		// expectation the baseline's check pins: a spec change.
		if err := os.WriteFile(filepath.Join(work, "public", "date.txt"), []byte(specV2Date+"\n"), 0o600); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(work, "checks", "render.bsh"), []byte(specCheck(specV2Date)), 0o600)
	}}
	svc := Service{
		App: App{Name: "summary", SourceDir: dir, Owner: "o@example.test"},
		Options: Options{
			Home:      home,
			Delegate:  del,
			Verifier:  ProductionVerifier{Home: home, Runner: runner},
			Activator: &ProductionActivator{Home: home},
			Track:     NewTracking(home),
		},
	}
	return svc, home, base.String()
}

// TestSpecChangeTravelsWithTheRevision replays S4 end to end with no manual
// spec step: an unreviewed spec change is refused; the owner's review lets
// the candidate be graded against its own spec; approval activates code and
// spec together; undo grades the baseline against the baseline's own spec
// and serves it again; the ledger keeps every attempt and the spec digests.
func TestSpecChangeTravelsWithTheRevision(t *testing.T) {
	svc, home, base := newSpecChangeFixture(t)
	ctx := context.Background()
	owner := Caller{Principal: "o@example.test", Actor: "owner-agent", RepresentedUser: "o@example.test", Tier: "owner"}
	contractor := Caller{Principal: "c@example.test", Actor: "site-agent", RepresentedUser: "c@example.test", Tier: "member", Circle: "contractor"}

	cand, err := svc.Wish(ctx, contractor, Wish{Request: specWish})
	if err != nil || cand.Status != Ready || cand.Rev == "" {
		t.Fatalf("contractor wish = %+v, %v", cand, err)
	}

	// Unreviewed: the verifier refuses the spec change before running it.
	got, err := svc.Verify(ctx, contractor, cand.ID)
	if !errors.Is(err, ErrVerification) || got.Verdict == nil || got.Verdict.Pass ||
		!strings.Contains(got.Verdict.Detail, "spec change needs the owner's review") ||
		!strings.Contains(got.Verdict.Detail, "checks/render.bsh (changed)") {
		t.Fatalf("unreviewed verify = %+v, %v", got.Verdict, err)
	}
	if got.Verdict.SpecChange != "checks/render.bsh (changed)" || got.Verdict.Spec == "" {
		t.Fatalf("unreviewed verdict does not name the spec change: %+v", got.Verdict)
	}
	if _, err := svc.Approve(ctx, owner, cand.ID); !errors.Is(err, ErrVerification) {
		t.Fatalf("owner approve before review err = %v, want ErrVerification", err)
	}
	if active, _ := svc.active("o@example.test"); active != base {
		t.Fatalf("a refused spec change moved the line: active %s, want %s", active, base)
	}

	// Only the owner tier may review the spec change.
	if _, err := svc.ReviewSpec(ctx, contractor, cand.ID); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("contractor review err = %v, want ErrUnauthorized", err)
	}
	reviewed, err := svc.ReviewSpec(ctx, owner, cand.ID)
	if err != nil || reviewed.SpecReview == nil || reviewed.SpecReview.Spec != got.Verdict.Spec ||
		len(reviewed.SpecReview.Changed) != 1 || reviewed.SpecReview.By != "owner-agent" {
		t.Fatalf("owner review = %+v, %v", reviewed.SpecReview, err)
	}

	// Reviewed: graded against its own spec, and it passes.
	verified, err := svc.Verify(ctx, contractor, cand.ID)
	if err != nil || verified.Status != Verified || verified.Verdict.SpecFrom != shapeverify.SpecFromCandidate {
		t.Fatalf("reviewed verify = %+v, %v", verified.Verdict, err)
	}
	approved, err := svc.Approve(ctx, owner, cand.ID)
	if err != nil || approved.Status != Approved {
		t.Fatalf("owner approve = %+v, %v", approved, err)
	}
	if active, _ := svc.active("o@example.test"); active != cand.Rev {
		t.Fatalf("approval active %s, want %s", active, cand.Rev)
	}
	h, err := src.History(home, "summary", "o@example.test")
	if err != nil || len(h.Entries) != 1 || h.Entries[0].Spec != verified.Verdict.Spec {
		t.Fatalf("approved version does not carry its spec: %+v, %v", h.Entries, err)
	}

	// Undo: one step, no manual spec restore. The baseline is graded
	// against S1, the spec it carries, not against the active S2.
	runner := svc.Options.Verifier.(ProductionVerifier).Runner
	undone, err := src.UndoSealed(ctx, home, "summary", "o@example.test", src.RestoreSealedOptions{Runner: runner})
	if err != nil {
		t.Fatalf("undo after a spec-changing approval: %v", err)
	}
	if undone.Revision != base {
		t.Fatalf("undo served %s, want the baseline %s", undone.Revision, base)
	}
	if active, _ := svc.active("o@example.test"); active != base {
		t.Fatalf("undo active %s, want %s", active, base)
	}
	h, err = src.History(home, "summary", "o@example.test")
	if err != nil || len(h.Entries) != 2 || h.Entries[0].Revision != base || h.Entries[0].Spec == "" || h.Entries[0].Spec == h.Entries[1].Spec {
		t.Fatalf("undo history does not carry the baseline's own spec: %+v, %v", h.Entries, err)
	}

	l, err := svc.Ledger(owner, true, "", -1)
	if err != nil {
		t.Fatal(err)
	}
	var fix *Attempt
	for _, p := range l.Problems {
		for i := range p.Attempts {
			if p.Attempts[i].Candidate == cand.ID {
				fix = &p.Attempts[i]
			}
		}
	}
	if fix == nil || fix.Standing != StandingUndone || fix.Spec != verified.Verdict.Spec || !strings.Contains(fix.SpecReview, "checks/render.bsh (changed)") {
		t.Fatalf("ledger attempt = %+v in %+v", fix, l)
	}
}

// TestReviewSpecNeedsASpecChange refuses a review of a candidate that keeps
// the base spec, so a review is never a blanket pass.
func TestReviewSpecNeedsASpecChange(t *testing.T) {
	svc, _, _ := newSpecChangeFixture(t)
	svc.Options.Delegate = &fakeMemberDelegate{edit: func(work, _ string) error {
		return os.WriteFile(filepath.Join(work, "public", "date.txt"), []byte(specV1Date+"\n\n"), 0o600)
	}}
	ctx := context.Background()
	owner := Caller{Principal: "o@example.test", Actor: "owner-agent", RepresentedUser: "o@example.test", Tier: "owner"}
	cand, err := svc.Wish(ctx, owner, Wish{Request: "whitespace"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReviewSpec(ctx, owner, cand.ID); !errors.Is(err, ErrNoSpecChange) {
		t.Fatalf("review without a spec change err = %v, want ErrNoSpecChange", err)
	}
}
