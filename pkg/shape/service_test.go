package shape

import (
	"context"
	"errors"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/qiangli/yoke/pkg/shape/state"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

type testVerifier struct {
	pass  bool
	calls int
}

func (v *testVerifier) Verify(context.Context, VerifyRequest) (Verdict, error) {
	v.calls++
	return Verdict{Pass: v.pass, Evidence: "test-evidence"}, nil
}

type testActivator struct {
	calls int
	run   func(ActivateRequest) error
}

func (a *testActivator) Activate(_ context.Context, req ActivateRequest) error {
	a.calls++
	if a.run != nil {
		return a.run(req)
	}
	return nil
}
func testService(t *testing.T) (Service, Caller, string, *testVerifier, *testActivator) {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, "src", "hello")
	r, e := gogit.PlainInit(dir, false)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(dir, "index.html"), []byte("Hello, World!"), 0600); e != nil {
		t.Fatal(e)
	}
	wt, _ := r.Worktree()
	wt.Add("index.html")
	if _, e = wt.Commit("base", &gogit.CommitOptions{Author: &object.Signature{Name: "fixture", Email: "fixture@example.invalid", When: time.Now()}}); e != nil {
		t.Fatal(e)
	}
	v := &testVerifier{pass: true}
	a := &testActivator{}
	s := Service{App: App{Name: "hello", SourceDir: dir}, Options: Options{Home: home, Verifier: v, Activator: a}}
	input := t.TempDir()
	os.WriteFile(filepath.Join(input, "index.html"), []byte("Hello, Shaping!"), 0600)
	return s, Caller{Principal: "alice", Actor: "tester", RepresentedUser: "alice"}, input, v, a
}
func TestServiceImportVerifyApprove(t *testing.T) {
	s, c, from, v, a := testService(t)
	ctx := context.Background()
	candidate, e := s.Import(ctx, c, Wish{Request: "change greeting"}, from)
	if e != nil {
		t.Fatal(e)
	}
	if candidate.Status != Ready {
		t.Fatal(candidate)
	}
	if b, _ := os.ReadFile(filepath.Join(from, "index.html")); string(b) != "Hello, Shaping!" {
		t.Fatal("source changed")
	}
	diff, e := s.Diff(ctx, c, candidate.ID)
	if e != nil || !strings.Contains(diff, "+Hello, Shaping!") {
		t.Fatalf("diff %q %v", diff, e)
	}
	if _, e = s.Verify(ctx, c, candidate.ID); e != nil {
		t.Fatal(e)
	}
	got, e := s.Approve(ctx, c, candidate.ID)
	if e != nil {
		t.Fatal(e)
	}
	if got.Status != Approved || v.calls != 2 || a.calls != 1 {
		t.Fatalf("%+v calls %d %d", got, v.calls, a.calls)
	}
	if _, e = s.Show(ctx, c, got.ID); e != nil {
		t.Fatal("approved identity lost", e)
	}
	if _, e = os.Stat(candidate.Workspace); !os.IsNotExist(e) {
		t.Fatal("workspace retained")
	}
}
func TestServiceFailCannotActivate(t *testing.T) {
	s, c, from, v, a := testService(t)
	v.pass = false
	ctx := context.Background()
	candidate, e := s.Import(ctx, c, Wish{Request: "change greeting"}, from)
	if e != nil {
		t.Fatal(e)
	}
	got, e := s.Approve(ctx, c, candidate.ID)
	if !errors.Is(e, ErrVerification) || got.Verdict == nil || got.Verdict.Pass || a.calls != 0 {
		t.Fatalf("%+v %v calls %d", got, e, a.calls)
	}
}
func TestServicePrincipalAndReject(t *testing.T) {
	s, c, from, _, _ := testService(t)
	ctx := context.Background()
	candidate, e := s.Import(ctx, c, Wish{Request: "change greeting"}, from)
	if e != nil {
		t.Fatal(e)
	}
	other := c
	other.Principal = "bob"
	if _, e = s.Show(ctx, other, candidate.ID); !os.IsNotExist(e) {
		t.Fatalf("wrong principal: %v", e)
	}
	if _, e = s.Reject(ctx, c, candidate.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Show(ctx, c, candidate.ID); !os.IsNotExist(e) {
		t.Fatalf("rejected candidate: %v", e)
	}
}
func TestServiceTrackingFailureBeforeGeneration(t *testing.T) {
	s, c, _, _, _ := testService(t)
	s.Options.Track = func(context.Context, *Candidate) error { return errors.New("tracking unavailable") }
	if _, e := s.Begin(context.Background(), c, Wish{Request: "change"}); e == nil {
		t.Fatal("tracking failure accepted")
	}
	got, e := s.Status(context.Background(), c)
	if e != nil {
		t.Fatal(e)
	}
	if len(got.Candidates) != 0 {
		t.Fatal("candidate created despite tracking failure")
	}
}
func TestServiceRejectRunningCannotRecreate(t *testing.T) {
	s, c, _, _, _ := testService(t)
	ctx := context.Background()
	candidate, e := s.Begin(ctx, c, Wish{Request: "change"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Reject(ctx, c, candidate.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Complete(ctx, c, candidate, nil); !os.IsNotExist(e) {
		t.Fatalf("completion after rejection: %v", e)
	}
	if _, e = s.Show(ctx, c, candidate.ID); !os.IsNotExist(e) {
		t.Fatal("rejected candidate recreated")
	}
}

func TestServiceInterruptedActivationFinalizesWithoutReactivation(t *testing.T) {
	s, caller, from, v, a := testService(t)
	ctx := context.Background()
	c, e := s.Import(ctx, caller, Wish{Request: "change greeting"}, from)
	if e != nil {
		t.Fatal(e)
	}
	work, e := gogit.PlainOpen(c.Workspace)
	if e != nil {
		t.Fatal(e)
	}
	live, e := gogit.PlainOpen(s.App.SourceDir)
	if e != nil {
		t.Fatal(e)
	}
	iter, e := work.Storer.IterEncodedObjects(plumbing.AnyObject)
	if e != nil {
		t.Fatal(e)
	}
	defer iter.Close()
	for {
		obj, e := iter.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		if _, e = live.Storer.SetEncodedObject(obj); e != nil {
			t.Fatal(e)
		}
	}
	c.Status = "activating"
	dir, _ := s.CandidateDir(c.Principal, c.ID)
	if e = SaveCandidate(dir, c); e != nil {
		t.Fatal(e)
	}
	store, e := state.OpenAt(s.Options.Home, c.App, c.Principal)
	if e != nil {
		t.Fatal(e)
	}
	if e = store.Write(state.State{ActiveRevision: c.Rev, LastKnownGood: c.BaseRev}); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Reject(ctx, caller, c.ID); !errors.Is(e, ErrConflict) {
		t.Fatalf("pending activation reject %v", e)
	}
	got, e := s.Approve(ctx, caller, c.ID)
	if e != nil {
		t.Fatal(e)
	}
	if got.Status != Approved || a.calls != 0 || v.calls != 0 {
		t.Fatalf("replayed activation %+v %d %d", got, a.calls, v.calls)
	}
	if _, e = s.Diff(ctx, caller, c.ID); e != nil {
		t.Fatal("durable approved diff", e)
	}
	if _, e = os.Stat(c.Workspace); !os.IsNotExist(e) {
		t.Fatal("candidate work not cleaned")
	}
}
func TestServiceInterruptedActivationBeforeSwitchCanRetry(t *testing.T) {
	s, caller, from, _, a := testService(t)
	ctx := context.Background()
	c, e := s.Import(ctx, caller, Wish{Request: "change greeting"}, from)
	if e != nil {
		t.Fatal(e)
	}
	c.Status = "activating"
	dir, _ := s.CandidateDir(c.Principal, c.ID)
	if e = SaveCandidate(dir, c); e != nil {
		t.Fatal(e)
	}
	got, e := s.Approve(ctx, caller, c.ID)
	if e != nil {
		t.Fatal(e)
	}
	if got.Status != Approved || a.calls != 1 {
		t.Fatalf("retry %+v %d", got, a.calls)
	}
}
func TestServiceProtectedChangeFails(t *testing.T) {
	s, caller, from, _, a := testService(t)
	s.App.Protected = []string{"index.html"}
	ctx := context.Background()
	c, e := s.Import(ctx, caller, Wish{Request: "change greeting"}, from)
	if e != nil {
		t.Fatal(e)
	}
	got, e := s.Approve(ctx, caller, c.ID)
	if !errors.Is(e, ErrVerification) || got.Verdict == nil || !strings.Contains(got.Verdict.Detail, "protected") || a.calls != 0 {
		t.Fatalf("protected change %+v %v", got, e)
	}
}

func TestServiceActivationSwitchThenErrorIsReconciled(t *testing.T) {
	s, caller, from, _, a := testService(t)
	ctx := context.Background()
	c, e := s.Import(ctx, caller, Wish{Request: "change greeting"}, from)
	if e != nil {
		t.Fatal(e)
	}
	a.run = func(req ActivateRequest) error {
		work, e := gogit.PlainOpen(req.CandidateDir)
		if e != nil {
			return e
		}
		live, e := gogit.PlainOpen(req.SourceDir)
		if e != nil {
			return e
		}
		iter, e := work.Storer.IterEncodedObjects(plumbing.AnyObject)
		if e != nil {
			return e
		}
		defer iter.Close()
		for {
			obj, e := iter.Next()
			if e == io.EOF {
				break
			}
			if e != nil {
				return e
			}
			if _, e = live.Storer.SetEncodedObject(obj); e != nil {
				return e
			}
		}
		st, e := state.OpenAt(s.Options.Home, req.App, req.Principal)
		if e != nil {
			return e
		}
		if e = st.Write(state.State{ActiveRevision: req.CandidateRev, LastKnownGood: req.BaseRev}); e != nil {
			return e
		}
		return errors.New("simulated failure after switch")
	}
	got, e := s.Approve(ctx, caller, c.ID)
	if e != nil {
		t.Fatal(e)
	}
	if got.Status != Approved || a.calls != 1 {
		t.Fatalf("%+v calls %d", got, a.calls)
	}
	again, e := s.Approve(ctx, caller, c.ID)
	if e != nil || again.Status != Approved || a.calls != 1 {
		t.Fatalf("retry %+v %v calls %d", again, e, a.calls)
	}
}
