package shape

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/qiangli/yoke/pkg/shape/evidence"
	shapeverify "github.com/qiangli/yoke/pkg/shape/verify"
	"github.com/qiangli/yoke/pkg/src"
)

type passCheckRunner struct{}

func (passCheckRunner) RunCheck(context.Context, shapeverify.CheckRun) (shapeverify.Outcome, error) {
	return shapeverify.Outcome{Status: evidence.StatusPass}, nil
}

type fakeMemberDelegate struct {
	calls int
	edit  func(dir, task string) error
}

func (d *fakeMemberDelegate) Delegate(_ context.Context, req DelegateRequest) (DelegateResult, error) {
	d.calls++
	if d.edit != nil {
		if err := d.edit(req.Dir, req.Task); err != nil {
			return DelegateResult{}, err
		}
	}
	return DelegateResult{
		Agent:  "fake:model",
		Output: "COVERED:\n- applied wish\nNOT COVERED:\n- (none)\n",
	}, nil
}

func newScopedOwnerFixture(t *testing.T, manifest string) (Service, string, string, *fakeMemberDelegate) {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, "src", "site")
	if err := os.MkdirAll(filepath.Join(dir, "public"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "checks"), 0o700); err != nil {
		t.Fatal(err)
	}
	r, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "public", "index.html"), []byte("<h1>Initial</h1>\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "contract.bsh"), []byte("@effects(\"read\")\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "checks", "smoke.bsh"), []byte("test -f public/index.html\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "shaped.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	wt, err := r.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := wt.AddWithOptions(&gogit.AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	baseHash, err := wt.Commit("initial", &gogit.CommitOptions{
		Author: &object.Signature{Name: "owner", Email: "o@example.test", When: time.Now()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("pristine"), baseHash)); err != nil {
		t.Fatal(err)
	}
	del := &fakeMemberDelegate{
		edit: func(work, task string) error {
			body := "<h1>Updated</h1>\n"
			if strings.Contains(task, "banner") {
				body = "<h1>Banner</h1>\n"
			} else if strings.Contains(task, "footer") {
				body = "<h1>Footer</h1>\n"
			}
			return os.WriteFile(filepath.Join(work, "public", "index.html"), []byte(body), 0o600)
		},
	}
	svc := Service{
		App: App{
			Name:      "site",
			SourceDir: dir,
			Owner:     "o@example.test",
		},
		Options: Options{
			Home:      home,
			Delegate:  del,
			Verifier:  ProductionVerifier{Home: home, Runner: passCheckRunner{}},
			Activator: &ProductionActivator{Home: home},
			Track:     NewTracking(home),
		},
	}
	return svc, home, baseHash.String(), del
}

// TestScopedMemberAOCLifecycle exercises the R5a A/O/C acceptance case:
// O owns the app; A has edit (company + support), C has shape (contractor + support).
// C wishes, verifies, previews; O approves; O's version line records C as author.
// A's code wish demotes to a request on O's queue. Unscoped members view only
// (demote on wish, refused import); account and anonymous callers are refused.
func TestScopedMemberAOCLifecycle(t *testing.T) {
	const manifest = `{
		"scopes": {
			"edit": {"circles": ["company"], "groups": ["support"]},
			"shape": {"circles": ["contractor"], "groups": ["support"]}
		}
	}`
	svc, home, baseRev, del := newScopedOwnerFixture(t, manifest)
	ctx := context.Background()

	owner := Caller{Principal: "o@example.test", Actor: "owner-agent", RepresentedUser: "o@example.test", Tier: "owner"}
	memberA := Caller{Principal: "a@example.test", Actor: "site-agent", RepresentedUser: "a@example.test", Tier: "member", Circle: "company", Groups: []string{"support"}}
	memberC := Caller{Principal: "c@example.test", Actor: "site-agent", RepresentedUser: "c@example.test", Tier: "member", Circle: "contractor", Groups: []string{"support"}}
	memberView := Caller{Principal: "m@example.test", Actor: "site-agent", RepresentedUser: "m@example.test", Tier: "member", Circle: "guest", Groups: []string{"support"}}
	accountStranger := Caller{Principal: "visitor:v123", Actor: "site-agent", Tier: "account"}
	signedInAlias := Caller{Principal: "visitor:v456", Actor: "site-agent", Tier: "signed-in"}
	anonVisitor := Caller{Principal: "anonymous", Actor: "site-agent", Tier: "anonymous"}

	// 1. C wishes a code change -> builds candidate targeting O's line, with C as Principal/RepresentedUser.
	candC, err := svc.Wish(ctx, memberC, Wish{Request: "add a banner"})
	if err != nil {
		t.Fatalf("C wish: %v", err)
	}
	if candC.Status != Ready || candC.Principal != "c@example.test" || candC.RepresentedUser != "c@example.test" || candC.ActivationPrincipal != "o@example.test" {
		t.Fatalf("C candidate = %+v", candC)
	}
	if del.calls != 1 {
		t.Fatalf("delegate calls = %d, want 1", del.calls)
	}

	// C verifies and diffs/previews the candidate.
	verifiedC, err := svc.Verify(ctx, memberC, candC.ID)
	if err != nil || verifiedC.Status != Verified || verifiedC.Verdict == nil || !verifiedC.Verdict.Pass {
		t.Fatalf("C verify = %+v, %v", verifiedC, err)
	}
	diff, err := svc.Diff(ctx, memberC, candC.ID)
	if err != nil || !strings.Contains(diff, "+<h1>Banner</h1>") {
		t.Fatalf("C diff = %q, %v", diff, err)
	}

	// C cannot approve; A cannot see or verify C's candidate.
	if _, err := svc.Approve(ctx, memberC, candC.ID); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("C approve err = %v, want ErrUnauthorized", err)
	}
	if _, err := svc.Show(ctx, memberA, candC.ID); !os.IsNotExist(err) {
		t.Fatalf("A Show(C candidate) err = %v, want not exist", err)
	}

	// 2. A (edit scope only) makes a code wish -> demotes to a request on O's queue, no delegate run.
	reqA, err := svc.Wish(ctx, memberA, Wish{Request: "add dark mode toggle", Page: "/about"})
	if err != nil {
		t.Fatalf("A wish demotion: %v", err)
	}
	if reqA.Status != Requested || reqA.Kind != KindRequest || reqA.Rev != "" || reqA.Principal != "a@example.test" || reqA.RepresentedUser != "a@example.test" || reqA.ActivationPrincipal != "o@example.test" {
		t.Fatalf("A demoted request = %+v", reqA)
	}
	if del.calls != 1 {
		t.Fatalf("A demotion must not invoke delegate; calls = %d", del.calls)
	}

	// View-only member M: wish demotes to request on O's queue; direct code import refused.
	reqM, err := svc.Wish(ctx, memberView, Wish{Request: "change layout"})
	if err != nil || reqM.Status != Requested || reqM.Kind != KindRequest || reqM.ActivationPrincipal != "o@example.test" {
		t.Fatalf("M wish = %+v, %v", reqM, err)
	}
	importDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(importDir, "index.html"), []byte("x"), 0o600)
	if _, err := svc.Import(ctx, memberView, Wish{Request: "import"}, importDir); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("M import err = %v, want ErrUnauthorized", err)
	}

	// Account strangers and anonymous callers are refused and file nothing on O's queue.
	for _, stranger := range []Caller{accountStranger, signedInAlias, anonVisitor} {
		if _, err := svc.Wish(ctx, stranger, Wish{Request: "hack site"}); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("stranger %+v wish err = %v, want ErrUnauthorized", stranger, err)
		}
	}

	// O's queue shows C's candidate plus A's and M's requests; A sees only A's request.
	stO, err := svc.Status(ctx, owner)
	if err != nil {
		t.Fatalf("O status: %v", err)
	}
	if len(stO.Candidates) != 3 {
		t.Fatalf("O queue count = %d (%+v), want 3 (C candidate + A request + M request)", len(stO.Candidates), stO.Candidates)
	}
	stA, err := svc.Status(ctx, memberA)
	if err != nil || len(stA.Candidates) != 1 || stA.Candidates[0].ID != reqA.ID {
		t.Fatalf("A status = %+v, %v; want only A's own request", stA, err)
	}

	// 3. O approves C's candidate (including via a bare local CLI Service with Owner unset).
	cliSvc := Service{App: App{Name: "site", SourceDir: svc.App.SourceDir}, Options: svc.Options}
	approved, err := cliSvc.Approve(ctx, Caller{Principal: "o@example.test", Actor: "owner-cli", RepresentedUser: "o@example.test"}, candC.ID)
	if err != nil {
		t.Fatalf("O approve C: %v", err)
	}
	if approved.Status != Approved || approved.Principal != "c@example.test" || approved.RepresentedUser != "c@example.test" || approved.ActivationPrincipal != "o@example.test" {
		t.Fatalf("approved = %+v", approved)
	}

	// O's version line moved from baseRev to approved.Rev and records C as author; C has no separate line.
	histO, err := src.History(home, "site", "o@example.test")
	if err != nil {
		t.Fatalf("O history: %v", err)
	}
	if histO.Active != approved.Rev || histO.Base != baseRev || len(histO.Versions) != 1 {
		t.Fatalf("O history = %+v", histO)
	}
	if histO.Versions[0].Principal != "o@example.test" || histO.Versions[0].Author != "c@example.test" {
		t.Fatalf("version attribution = %+v, want Principal=o@example.test Author=c@example.test", histO.Versions[0])
	}
	histC, err := src.History(home, "site", "c@example.test")
	if err != nil || len(histC.Versions) != 0 || histC.Active != "" {
		t.Fatalf("C must have no separate version line: %+v, %v", histC, err)
	}
}

// TestConflictingMemberCandidatesReturnBothRevisions verifies Acceptance 3:
// when two scoped members build from the same base and O approves the first,
// approving the second fails with ErrConflict naming both the base and active revisions.
func TestConflictingMemberCandidatesReturnBothRevisions(t *testing.T) {
	const manifest = `{"scopes": {"shape": {"groups": ["support"]}}}`
	svc, _, baseRev, _ := newScopedOwnerFixture(t, manifest)
	ctx := context.Background()

	owner := Caller{Principal: "o@example.test", Actor: "owner", RepresentedUser: "o@example.test", Tier: "owner"}
	c1 := Caller{Principal: "c1@example.test", Actor: "agent", RepresentedUser: "c1@example.test", Tier: "member", Circle: "contractor", Groups: []string{"support"}}
	c2 := Caller{Principal: "c2@example.test", Actor: "agent", RepresentedUser: "c2@example.test", Tier: "member", Circle: "contractor", Groups: []string{"support"}}

	cand1, err := svc.Wish(ctx, c1, Wish{Request: "add banner"})
	if err != nil {
		t.Fatalf("c1 wish: %v", err)
	}
	cand2, err := svc.Wish(ctx, c2, Wish{Request: "add footer"})
	if err != nil {
		t.Fatalf("c2 wish: %v", err)
	}
	if cand1.BaseRev != baseRev || cand2.BaseRev != baseRev {
		t.Fatalf("base revs = %s, %s; want %s", cand1.BaseRev, cand2.BaseRev, baseRev)
	}

	approved1, err := svc.Approve(ctx, owner, cand1.ID)
	if err != nil {
		t.Fatalf("approve cand1: %v", err)
	}

	_, err = svc.Approve(ctx, owner, cand2.ID)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("approve stale cand2 err = %v, want ErrConflict", err)
	}
	if !strings.Contains(err.Error(), baseRev) || !strings.Contains(err.Error(), approved1.Rev) {
		t.Fatalf("conflict error %q must name both base (%s) and active (%s) revisions", err.Error(), baseRev, approved1.Rev)
	}
}

// TestPerPersonVersionsUnchangedWithoutOwner ensures ordinary consumer-shaped
// apps (Owner == "") keep independent per-principal version lines.
func TestPerPersonVersionsUnchangedWithoutOwner(t *testing.T) {
	s, alice, from, _, a := testService(t)
	ctx := context.Background()
	var activated []ActivateRequest
	a.run = func(req ActivateRequest) error {
		activated = append(activated, req)
		return nil
	}
	bob := Caller{Principal: "bob", Actor: "tester", RepresentedUser: "bob"}
	if s.ActivationPrincipal(alice.Principal) != "alice" || s.ActivationPrincipal(bob.Principal) != "bob" {
		t.Fatal("per-principal ActivationPrincipal changed")
	}
	ca, err := s.Import(ctx, alice, Wish{Request: "alice greeting"}, from)
	if err != nil {
		t.Fatal(err)
	}
	cb, err := s.Import(ctx, bob, Wish{Request: "bob greeting"}, from)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Approve(ctx, alice, ca.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Approve(ctx, bob, cb.ID); err != nil {
		t.Fatal(err)
	}
	if len(activated) != 2 || activated[0].Principal != "alice" || activated[1].Principal != "bob" {
		t.Fatalf("per-person activations = %+v", activated)
	}
}
