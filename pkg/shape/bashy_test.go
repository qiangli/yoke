package shape

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestBashyTargetRecordsWithoutActivation(t *testing.T) {
	home := t.TempDir()
	act := &testActivator{}
	ver := &testVerifier{pass: true}
	s := Service{App: BashyApp(), Options: Options{Home: home, Verifier: ver, Activator: act}}
	c := Caller{Principal: "alice", Actor: "tester", RepresentedUser: "alice"}
	ctx := context.Background()

	if _, e := s.Wish(ctx, c, Wish{Request: "  "}); e == nil {
		t.Fatal("empty request accepted")
	}
	got, e := s.Wish(ctx, c, Wish{Request: "ls --json drops mtime", Reproduction: "bashy ls --json /tmp", Expected: "mtime field present", Evidence: "output lacks mtime"})
	if e != nil {
		t.Fatal(e)
	}
	if got.Status != Reported || got.AppKind != BashyKind || got.Scope != "host" || got.Rev != "" || got.Workspace != "" {
		t.Fatalf("record %+v", got)
	}
	if got.Reproduction != "bashy ls --json /tmp" || got.Expected != "mtime field present" || got.Evidence != "output lacks mtime" {
		t.Fatalf("fields %+v", got)
	}

	st, e := s.Status(ctx, c)
	if e != nil {
		t.Fatal(e)
	}
	if st.App != BashyTarget || st.Host == nil || st.Host.Kind != BashyKind || len(st.Candidates) != 1 || st.Candidates[0].ID != got.ID || st.Candidates[0].Expected != got.Expected {
		t.Fatalf("status %+v", st)
	}
	if hs, e := s.HostStatus(ctx, c); e != nil || hs.Kind != BashyKind {
		t.Fatalf("host status %+v %v", hs, e)
	}

	if _, e = s.Approve(ctx, c, got.ID); !errors.Is(e, ErrBashyActivation) {
		t.Fatalf("approve: %v", e)
	}
	if _, e = s.Verify(ctx, c, got.ID); !errors.Is(e, ErrBashyActivation) {
		t.Fatalf("verify: %v", e)
	}
	if _, e = s.Import(ctx, c, Wish{Request: "x"}, t.TempDir()); !errors.Is(e, ErrBashyActivation) {
		t.Fatalf("import: %v", e)
	}
	if _, e = s.Submit(ctx, c, got.ID, SubmitOptions{}); !errors.Is(e, ErrBashyActivation) {
		t.Fatalf("submit: %v", e)
	}
	if _, e = s.Push(ctx, "", got.ID, false); !errors.Is(e, ErrBashyActivation) {
		t.Fatalf("push: %v", e)
	}
	if act.calls != 0 || ver.calls != 0 {
		t.Fatalf("activator %d verifier %d", act.calls, ver.calls)
	}

	if _, e = s.Reject(ctx, c, got.ID); e != nil {
		t.Fatal(e)
	}
	dir, _ := s.CandidateDir(c.Principal, got.ID)
	if _, e = os.Stat(dir); !os.IsNotExist(e) {
		t.Fatalf("rejected record remains: %v", e)
	}
	if _, e = os.Stat(filepath.Join(home, "shape-work", BashyTarget)); e != nil {
		t.Fatalf("record not in the shape store: %v", e)
	}
}

// A registered app named bashy with a real source keeps the ordinary
// lifecycle (source-tier promotion); only the sourceless built-in records.
func TestRegisteredBashyAppIsNotRecordOnly(t *testing.T) {
	if (Service{App: App{Name: BashyTarget, SourceDir: t.TempDir()}}).isBashy() {
		t.Fatal("registered bashy app treated as record-only target")
	}
	if !(Service{App: BashyApp()}).isBashy() {
		t.Fatal("built-in bashy target not record-only")
	}
}
