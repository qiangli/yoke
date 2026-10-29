package src_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	ygit "github.com/qiangli/yoke/git"
	"github.com/qiangli/yoke/pkg/shape/state"
	"github.com/qiangli/yoke/pkg/src"
)

const (
	s525a8Alice = "device:alice"
	s525a8Bob   = "device:bob"
	s525a8Carol = "device:carol"
	s525a8App   = "demoapp"
)

func s525a8Gate() state.ActivationGate {
	return state.GateFunc(func(candidate, base, principal string) (state.EvidenceRef, error) {
		return state.EvidenceRef{Verdict: "pass", EvidenceID: "ev-test"}, nil
	})
}

func s525a8WriteFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func s525a8Commit(t *testing.T, dir, msg string) string {
	t.Helper()
	if _, err := ygit.Add(ygit.AddOptions{RepoPath: dir, All: true}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := ygit.Commit(ygit.CommitOptions{
		RepoPath:    dir,
		Message:     msg,
		AuthorName:  "s525a8",
		AuthorEmail: "s525a8@example.com",
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	rev, err := ygit.RevParse(ygit.RevParseOptions{RepoPath: dir})
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	return rev.Hash
}

func s525a8Shape(id, rev, origin string) string {
	return "schema_version: bashy-shape-feature-v1\nid: " + id + "\norigin: " + origin + "\nbase_app: demoapp\nrevision: " + rev + "\n"
}

func setupOfferingFixture(t *testing.T, home string) (appDir, baseRev string) {
	t.Helper()
	work := filepath.Join(t.TempDir(), "work")
	if _, err := ygit.Init(ygit.InitOptions{Path: work}); err != nil {
		t.Fatalf("init: %v", err)
	}
	s525a8WriteFile(t, work, "app.txt", "app-base\n")
	s525a8WriteFile(t, work, "shared.txt", "shared-base\n")
	baseRev = s525a8Commit(t, work, "p0 base release")

	appDir = filepath.Join(home, "src", s525a8App)
	if _, err := gogit.PlainClone(appDir, false, &gogit.CloneOptions{URL: work}); err != nil {
		t.Fatalf("clone: %v", err)
	}
	r, err := gogit.PlainOpen(appDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Storer.SetReference(plumbing.NewHashReference("refs/heads/pristine", plumbing.NewHash(baseRev))); err != nil {
		t.Fatalf("pristine: %v", err)
	}
	return appDir, baseRev
}

// TestS525A8OfferingOptInOutAndSync verifies the end-to-end acceptance story:
// alice offers to bob only;
// carol cannot list it;
// bob previews then adopts (active changes, data untouched);
// alice offers revision 2;
// bob receives it only after adopting again;
// a conflicting change by bob yields a review version with active unchanged;
// decline clears the notice;
// carol unchanged throughout.
func TestS525A8OfferingOptInOutAndSync(t *testing.T) {
	home := t.TempDir()
	appDir, _ := setupOfferingFixture(t, home)
	gate := s525a8Gate()

	// Setup initial private data for Bob and Alice
	bobStore, err := state.OpenAt(home, s525a8App, s525a8Bob)
	if err != nil {
		t.Fatal(err)
	}
	if err := bobStore.WriteData("user.txt", []byte("bob-private-data\n")); err != nil {
		t.Fatal(err)
	}

	aliceStore, err := state.OpenAt(home, s525a8App, s525a8Alice)
	if err != nil {
		t.Fatal(err)
	}
	if err := aliceStore.WriteData("secret.txt", []byte("alice-secret-token\n")); err != nil {
		t.Fatal(err)
	}

	// Alice creates revision 1 of feature "demo"
	s525a8WriteFile(t, appDir, "feature.txt", "alice-feature-v1\n")
	s525a8WriteFile(t, appDir, "shape.yaml", s525a8Shape("demo", "v1", s525a8Alice))
	rev1 := s525a8Commit(t, appDir, "alice v1")

	// Alice offers to bob only
	offer1, err := src.Offer(home, s525a8App, rev1, s525a8Alice, src.OfferOptions{
		To: []string{s525a8Bob},
	})
	if err != nil {
		t.Fatalf("offer1: %v", err)
	}
	if offer1.ID != "demo" {
		t.Fatalf("offer1 ID = %q, want demo", offer1.ID)
	}
	if offer1.Revision != rev1 {
		t.Fatalf("offer1 Revision = %q, want %q", offer1.Revision, rev1)
	}
	if offer1.All {
		t.Fatalf("offer1 All = true, want false (restricted)")
	}

	// Eligible principal (bob) gets ONE notice per offering revision (no duplicates on re-offer)
	bobNotices, err := src.Notices(home, s525a8App, s525a8Bob)
	if err != nil {
		t.Fatalf("bob notices: %v", err)
	}
	if len(bobNotices) != 1 || bobNotices[0].Revision != rev1 {
		t.Fatalf("expected 1 notice for rev1, got %#v", bobNotices)
	}

	// Re-offer same revision: no duplicate notice in Bob's state
	if _, err := src.Offer(home, s525a8App, rev1, s525a8Alice, src.OfferOptions{To: []string{s525a8Bob}}); err != nil {
		t.Fatalf("re-offer: %v", err)
	}
	bobNotices, err = src.Notices(home, s525a8App, s525a8Bob)
	if err != nil {
		t.Fatal(err)
	}
	if len(bobNotices) != 1 {
		t.Fatalf("duplicate notice delivered on re-offer: count = %d", len(bobNotices))
	}

	// Carol cannot list it
	carolOffers, err := src.ListOffers(home, s525a8App, s525a8Carol)
	if err != nil {
		t.Fatalf("carol list offers: %v", err)
	}
	if len(carolOffers) != 0 {
		t.Fatalf("carol listed restricted offer: %#v", carolOffers)
	}
	carolNotices, err := src.Notices(home, s525a8App, s525a8Carol)
	if err != nil {
		t.Fatal(err)
	}
	if len(carolNotices) != 0 {
		t.Fatalf("carol got notice: %#v", carolNotices)
	}

	// Bob can list it
	bobOffers, err := src.ListOffers(home, s525a8App, s525a8Bob)
	if err != nil {
		t.Fatalf("bob list offers: %v", err)
	}
	if len(bobOffers) != 1 || bobOffers[0].ID != "demo" {
		t.Fatalf("bob cannot see offer: %#v", bobOffers)
	}

	// Bob previews then adopts (active changes, data untouched)
	// Preview first: isolated workspace, active untouched
	prevRes, err := src.Adopt(home, s525a8App, "demo", s525a8Bob, src.AdoptOptions{
		Preview: true,
		Gate:    gate,
	})
	if err != nil {
		t.Fatalf("bob preview: %v", err)
	}
	if prevRes.PreviewDir == "" {
		t.Fatalf("preview dir empty")
	}
	featContent, err := os.ReadFile(filepath.Join(prevRes.PreviewDir, "feature.txt"))
	if err != nil || string(featContent) != "alice-feature-v1\n" {
		t.Fatalf("preview workspace content: %s (err: %v)", featContent, err)
	}
	stBob, err := bobStore.Read()
	if err != nil {
		t.Fatal(err)
	}
	if stBob.ActiveRevision != "" {
		t.Fatalf("bob active changed during preview: %s", stBob.ActiveRevision)
	}

	// Bob adopts: active changes, data untouched
	adoptRes, err := src.Adopt(home, s525a8App, "demo", s525a8Bob, src.AdoptOptions{
		Preview: false,
		Gate:    gate,
	})
	if err != nil {
		t.Fatalf("bob adopt: %v", err)
	}
	if adoptRes.Revision != rev1 {
		t.Fatalf("adopt revision = %s, want %s", adoptRes.Revision, rev1)
	}
	stBob, err = bobStore.Read()
	if err != nil {
		t.Fatal(err)
	}
	if stBob.ActiveRevision != rev1 {
		t.Fatalf("bob active revision = %s, want %s", stBob.ActiveRevision, rev1)
	}
	// Data untouched: bob's private data preserved
	bobData, err := bobStore.ReadData("user.txt")
	if err != nil || string(bobData) != "bob-private-data\n" {
		t.Fatalf("bob private data modified: %s (%v)", bobData, err)
	}
	// Adoption never copies creator credentials or data
	if _, err := bobStore.ReadData("secret.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("creator credentials leaked into adopter private data")
	}
	// Notice cleared after adoption
	bobNotices, _ = src.Notices(home, s525a8App, s525a8Bob)
	if len(bobNotices) != 0 {
		t.Fatalf("notice not cleared after adoption: %#v", bobNotices)
	}

	// Alice offers revision 2
	s525a8WriteFile(t, appDir, "feature.txt", "alice-feature-v2\n")
	s525a8WriteFile(t, appDir, "shape.yaml", s525a8Shape("demo", "v2", s525a8Alice))
	rev2 := s525a8Commit(t, appDir, "alice v2")

	offer2, err := src.Offer(home, s525a8App, rev2, s525a8Alice, src.OfferOptions{
		To: []string{s525a8Bob},
	})
	if err != nil {
		t.Fatalf("offer2: %v", err)
	}
	if offer2.Revision != rev2 {
		t.Fatalf("offer2 revision = %s, want %s", offer2.Revision, rev2)
	}

	// Adopters get a notice
	bobNotices, err = src.Notices(home, s525a8App, s525a8Bob)
	if err != nil {
		t.Fatal(err)
	}
	if len(bobNotices) != 1 || bobNotices[0].Revision != rev2 {
		t.Fatalf("expected 1 notice for rev2, got %#v", bobNotices)
	}

	// Bob receives it ONLY after adopting again (active revision does not move yet)
	stBob, _ = bobStore.Read()
	if stBob.ActiveRevision != rev1 {
		t.Fatalf("bob active changed before adopting again: got %s, want %s", stBob.ActiveRevision, rev1)
	}

	// Bob adopts again to receive rev2
	adoptRes2, err := src.Adopt(home, s525a8App, "demo", s525a8Bob, src.AdoptOptions{Gate: gate})
	if err != nil {
		t.Fatalf("bob adopt rev2: %v", err)
	}
	if adoptRes2.Revision != rev2 {
		t.Fatalf("adoptRes2 revision = %s, want %s", adoptRes2.Revision, rev2)
	}
	stBob, _ = bobStore.Read()
	if stBob.ActiveRevision != rev2 {
		t.Fatalf("bob active revision = %s, want %s", stBob.ActiveRevision, rev2)
	}
	bobNotices, _ = src.Notices(home, s525a8App, s525a8Bob)
	if len(bobNotices) != 0 {
		t.Fatalf("notice not cleared after adopt rev2")
	}

	// A conflicting change by bob yields a review version with active unchanged:
	// Bob makes his own conflicting change modifying shared.txt off rev2
	if _, err := ygit.Checkout(ygit.CheckoutOptions{RepoPath: appDir, Branch: "tmp-bob", Create: true}); err != nil {
		t.Fatal(err)
	}
	s525a8WriteFile(t, appDir, "shared.txt", "bob-modified-shared\n")
	bobConflictingRev := s525a8Commit(t, appDir, "bob conflict commit")
	if _, err := src.Activate(home, s525a8App, bobConflictingRev, s525a8Bob, src.ActivateOptions{Gate: gate}); err != nil {
		t.Fatalf("bob activate: %v", err)
	}
	stBob, _ = bobStore.Read()
	if stBob.ActiveRevision != bobConflictingRev {
		t.Fatalf("bob active = %s, want %s", stBob.ActiveRevision, bobConflictingRev)
	}

	// Alice creates revision 3 off rev2 that also modifies shared.txt (causing conflict)
	if _, err := ygit.Checkout(ygit.CheckoutOptions{RepoPath: appDir, Branch: "tmp-alice", Create: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := ygit.Reset(ygit.ResetOpts{RepoPath: appDir, Mode: ygit.ResetHard, Commit: rev2}); err != nil {
		t.Fatal(err)
	}
	s525a8WriteFile(t, appDir, "shared.txt", "alice-modified-shared-v3\n")
	s525a8WriteFile(t, appDir, "shape.yaml", s525a8Shape("demo", "v3", s525a8Alice))
	rev3 := s525a8Commit(t, appDir, "alice v3 conflicting")

	if _, err := src.Offer(home, s525a8App, rev3, s525a8Alice, src.OfferOptions{To: []string{s525a8Bob}}); err != nil {
		t.Fatalf("offer3: %v", err)
	}

	// Bob attempts to adopt rev3: conflict!
	adoptConflictRes, conflictErr := src.Adopt(home, s525a8App, "demo", s525a8Bob, src.AdoptOptions{Gate: gate})
	if conflictErr == nil {
		t.Fatalf("expected conflict error, got nil")
	}
	if !adoptConflictRes.Conflict {
		t.Fatalf("expected adoptConflictRes.Conflict = true")
	}
	if adoptConflictRes.ReviewRef == "" {
		t.Fatalf("expected non-empty ReviewRef on conflict")
	}

	// Active revision does NOT move
	stBob, _ = bobStore.Read()
	if stBob.ActiveRevision != bobConflictingRev {
		t.Fatalf("bob active changed on conflict: got %s, want %s", stBob.ActiveRevision, bobConflictingRev)
	}

	// Review version is recorded under bob's namespace
	histBob, err := src.History(home, s525a8App, s525a8Bob)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(histBob.Reviews) == 0 {
		t.Fatalf("expected review version in history for bob, got 0")
	}
	foundReview := false
	for _, rv := range histBob.Reviews {
		if rv.Ref == adoptConflictRes.ReviewRef {
			foundReview = true
			break
		}
	}
	if !foundReview {
		t.Fatalf("review ref %s not found in bob reviews: %#v", adoptConflictRes.ReviewRef, histBob.Reviews)
	}

	// Decline clears the notice:
	// Alice offers revision 4 to Bob
	s525a8WriteFile(t, appDir, "other.txt", "other-feature-v4\n")
	s525a8WriteFile(t, appDir, "shape.yaml", s525a8Shape("other", "v4", s525a8Alice))
	rev4 := s525a8Commit(t, appDir, "alice v4")
	if _, err := src.Offer(home, s525a8App, rev4, s525a8Alice, src.OfferOptions{To: []string{s525a8Bob}}); err != nil {
		t.Fatalf("offer4: %v", err)
	}
	bobNotices, _ = src.Notices(home, s525a8App, s525a8Bob)
	if len(bobNotices) == 0 {
		t.Fatalf("expected notice for offer4")
	}
	// Decline clears notice
	if err := src.Decline(home, s525a8App, "other", s525a8Bob); err != nil {
		t.Fatalf("decline: %v", err)
	}
	bobNoticesAfter, _ := src.Notices(home, s525a8App, s525a8Bob)
	for _, n := range bobNoticesAfter {
		if n.ID == "other" {
			t.Fatalf("notice for 'other' not cleared after decline")
		}
	}

	// Carol unchanged throughout
	carolStore, err := state.OpenAt(home, s525a8App, s525a8Carol)
	if err != nil {
		t.Fatal(err)
	}
	stCarol, err := carolStore.Read()
	if err != nil {
		t.Fatal(err)
	}
	if stCarol.ActiveRevision != "" {
		t.Fatalf("carol active revision mutated: %s", stCarol.ActiveRevision)
	}
	if len(stCarol.Adopted) != 0 {
		t.Fatalf("carol has adopted items: %#v", stCarol.Adopted)
	}
	if len(stCarol.Notices) != 0 {
		t.Fatalf("carol has notices: %#v", stCarol.Notices)
	}
	carolLineRef := src.VersionLineRef(s525a8Carol)
	rMain, _ := gogit.PlainOpen(appDir)
	if _, err := rMain.Reference(plumbing.ReferenceName(carolLineRef), true); !errors.Is(err, plumbing.ErrReferenceNotFound) {
		t.Fatalf("carol version line exists when it should not")
	}
}

// TestS525A8CLI tests the CLI verbs: offer, offers, adopt, decline
func TestS525A8CLI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	t.Setenv("BASHY_PRINCIPAL", s525a8Alice)
	appDir, _ := setupOfferingFixture(t, home)
	src.RegisterActivationGate(s525a8Gate())

	s525a8WriteFile(t, appDir, "cli-feat.txt", "cli-v1\n")
	s525a8WriteFile(t, appDir, "shape.yaml", s525a8Shape("clifeat", "v1", s525a8Alice))
	rev := s525a8Commit(t, appDir, "cli v1")

	// bashy src offer <app> <rev> --to <bob>
	cmd := src.NewSrcCmd()
	cmd.SetArgs([]string{"offer", s525a8App, rev, "--to", s525a8Bob})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cli offer: %v", err)
	}

	// bashy src offers <app> (as bob)
	t.Setenv("BASHY_PRINCIPAL", s525a8Bob)
	cmd = src.NewSrcCmd()
	cmd.SetArgs([]string{"offers", s525a8App})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cli offers as bob: %v", err)
	}

	// bashy src adopt <app> <offer> --preview
	cmd = src.NewSrcCmd()
	cmd.SetArgs([]string{"adopt", s525a8App, "clifeat", "--preview"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cli adopt preview: %v", err)
	}

	// bashy src adopt <app> <offer>
	cmd = src.NewSrcCmd()
	cmd.SetArgs([]string{"adopt", s525a8App, "clifeat"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cli adopt: %v", err)
	}

	// bashy src decline <app> <offer>
	cmd = src.NewSrcCmd()
	cmd.SetArgs([]string{"decline", s525a8App, "clifeat"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cli decline: %v", err)
	}
}
