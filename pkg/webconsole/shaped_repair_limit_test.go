// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	shape "github.com/qiangli/yoke/pkg/shape/state"
	shapeverify "github.com/qiangli/yoke/pkg/shape/verify"
	"github.com/qiangli/yoke/pkg/src"
)

func repairIntPtr(n int) *int { return &n }

// repairLimitFixture mirrors repairFixture but takes any delegate and an
// optional shaped.json body committed with the broken base revision, so the
// app config path (repair.max_attempts) is exercised end to end.
func repairLimitFixture(t *testing.T, delegate Delegator, manifest string) (*shapedApp, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	source, _ := productionNotesSource(t, home)
	if manifest != "" {
		if err := os.WriteFile(filepath.Join(source, "shaped.json"), []byte(manifest), 0600); err != nil {
			t.Fatal(err)
		}
	}
	r, err := gogit.PlainOpen(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "api", "list.md"), []byte("```bsh\nexit 1\n```\n"), 0600); err != nil {
		t.Fatal(err)
	}
	wt, _ := r.Worktree()
	// Stage everything: shaped.json is new and untracked, which a bare
	// commit-all would leave out of the base revision.
	if err := wt.AddWithOptions(&gogit.AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	base, err := wt.Commit("broken list", &gogit.CommitOptions{All: true, Author: &object.Signature{Name: "fixture", Email: "test@example.test", When: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Storer.SetReference(plumbing.NewHashReference("refs/heads/pristine", base)); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Release(home, "notes", base.String(), "device:alice", src.ReleaseOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, principal := range []string{"device:alice", "device:bob"} {
		st, err := shape.OpenAt(home, "notes", principal)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Write(shape.State{ActiveRevision: base.String()}); err != nil {
			t.Fatal(err)
		}
	}
	opts := productionShapeOptions()
	opts.Home, opts.WorkRoot, opts.Runner = home, filepath.Join(t.TempDir(), "work"), []string{"/bin/sh"}
	opts.Delegate = delegate
	opts.Verifier = productionVerifier{home: home, runner: shapeverify.TrustedRunner{Bin: "/bin/sh", Effects: []string{"read"}}}
	opts.Activator.(*productionActivator).home = home
	return &shapedApp{name: "notes", repo: source, opts: opts}, base.String()
}

func repairLimitStore(t *testing.T, app *shapedApp, principal, base string) *incidentStore {
	t.Helper()
	st, err := app.store(principal)
	if err != nil {
		t.Fatal(err)
	}
	s, err := openIncidentsWithLimit(st, app.repairLimitForRev(base))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func activeRev(t *testing.T, app *shapedApp, principal string) string {
	t.Helper()
	st, err := shape.OpenAt(app.opts.Home, "notes", principal)
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	return s.ActiveRevision
}

// Zero means record and report only: the incident lands failed, the delegate
// is never called, and the previous version stays active.
func TestRepairMaxAttemptsZero(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses /bin/sh")
	}
	delegate := &repairShaper{mode: "fix"}
	app, base := repairLimitFixture(t, delegate, `{"repair": {"max_attempts": 0}}`)
	in, ok := app.recordIncident("device:alice", IncidentAPI, "api/list", 500, "unit list failed", "exit 1", base)
	if !ok {
		t.Fatal("zero-attempt incident was not recorded")
	}
	if in.State != IncidentFailed {
		t.Fatalf("zero-attempt incident state = %q, want failed", in.State)
	}
	app.repairIncident(context.Background(), "device:alice", in.ID)
	if len(delegate.tasks) != 0 {
		t.Fatalf("zero-attempt repair called the delegate %d times", len(delegate.tasks))
	}
	s := repairLimitStore(t, app, "device:alice", base)
	got, err := s.Get(in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != IncidentFailed || len(got.Attempts) != 0 {
		t.Fatalf("zero-attempt incident = %+v", got)
	}
	if activeRev(t, app, "device:alice") != base {
		t.Fatal("zero-attempt repair moved the active revision")
	}
	if _, err := s.Attempt(in.ID, RepairAttempt{Candidate: "c1", Result: "failed", At: time.Now()}); err == nil {
		t.Fatal("zero-attempt incident accepted an attempt")
	}
}

// The host default caps attempts when the app sets nothing: stop at N with
// the previous version active, and no further attempt.
func TestRepairMaxAttemptsHostDefault(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses /bin/sh")
	}
	for _, n := range []int{1, 2} {
		t.Run("", func(t *testing.T) {
			delegate := &repairShaper{mode: "fail"}
			app, base := repairLimitFixture(t, delegate, "")
			app.opts.RepairMaxAttempts = repairIntPtr(n)
			s := repairLimitStore(t, app, "device:alice", base)
			in, start, err := s.Observe(Incident{App: "notes", Kind: IncidentAPI, Path: "api/list", Status: 500, Message: "unit list failed", Evidence: "exit 1", BaseRev: base}, time.Now())
			if err != nil || !start {
				t.Fatalf("observe: %v %v", start, err)
			}
			app.repairIncident(context.Background(), "device:alice", in.ID)
			got, err := s.Get(in.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != IncidentFailed || len(got.Attempts) != n {
				t.Fatalf("incident = %+v, want failed with %d attempts", got, n)
			}
			if len(delegate.tasks) != n {
				t.Fatalf("delegate called %d times, want %d", len(delegate.tasks), n)
			}
			if activeRev(t, app, "device:alice") != base {
				t.Fatal("failed repair moved the active revision")
			}
			if _, err := s.Attempt(in.ID, RepairAttempt{Candidate: "extra", Result: "failed"}); err == nil {
				t.Fatal("exhausted incident accepted another attempt")
			}
		})
	}
}

// Unlimited keeps trying until a repair lands: a delegate that succeeds on
// attempt 4 ends activated with 4 attempts recorded.
type repairCountdown struct {
	calls     int
	succeedOn int
	tasks     []string
}

func (f *repairCountdown) Delegate(_ context.Context, req DelegateRequest) (DelegateResult, error) {
	f.calls++
	body := "```bsh\nexit 2\n```\n"
	if f.calls >= f.succeedOn {
		body = "```bsh\nprintf 'repaired\\n'\n```\n"
	}
	if err := os.WriteFile(filepath.Join(req.Dir, "api", "list.md"), []byte(body), 0600); err != nil {
		return DelegateResult{}, err
	}
	f.tasks = append(f.tasks, req.Task)
	return DelegateResult{Agent: "test:repair"}, nil
}

func TestRepairMaxAttemptsUnlimited(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses /bin/sh")
	}
	delegate := &repairCountdown{succeedOn: 4}
	app, base := repairLimitFixture(t, delegate, `{"repair": {"max_attempts": "unlimited"}}`)
	s := repairLimitStore(t, app, "device:alice", base)
	in, start, err := s.Observe(Incident{App: "notes", Kind: IncidentAPI, Path: "api/list", Status: 500, Message: "unit list failed", Evidence: "exit 1", BaseRev: base}, time.Now())
	if err != nil || !start {
		t.Fatalf("observe: %v %v", start, err)
	}
	app.repairIncident(context.Background(), "device:alice", in.ID)
	got, err := s.Get(in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != IncidentActivated || len(got.Attempts) != 4 {
		t.Fatalf("incident = %+v, want activated with 4 attempts", got)
	}
	if delegate.calls != 4 {
		t.Fatalf("delegate called %d times, want 4", delegate.calls)
	}
	if activeRev(t, app, "device:alice") == base {
		t.Fatal("activated repair left the broken revision active")
	}
}

// Resolution order is app config, else host default, else 2. Invalid app
// config warns and falls back to 2 — never unlimited by accident.
func TestRepairLimitResolution(t *testing.T) {
	newRepo := func(t *testing.T, manifest string) (*shapedApp, string) {
		t.Helper()
		dir := t.TempDir()
		r, err := gogit.PlainInit(dir, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("notes\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if manifest != "" {
			if err := os.WriteFile(filepath.Join(dir, "shaped.json"), []byte(manifest), 0600); err != nil {
				t.Fatal(err)
			}
		}
		wt, _ := r.Worktree()
		if err := wt.AddWithOptions(&gogit.AddOptions{All: true}); err != nil {
			t.Fatal(err)
		}
		rev, err := wt.Commit("base", &gogit.CommitOptions{All: true, Author: &object.Signature{Name: "t", Email: "t@example.test", When: time.Now()}})
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Storer.SetReference(plumbing.NewHashReference("refs/heads/pristine", rev)); err != nil {
			t.Fatal(err)
		}
		return &shapedApp{name: "notes", repo: dir, opts: productionShapeOptions()}, rev.String()
	}
	var warned bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&warned, nil)))
	defer slog.SetDefault(old)

	app, rev := newRepo(t, `{"repair": {"max_attempts": 5}}`)
	app.opts.RepairMaxAttempts = repairIntPtr(7)
	if got := app.repairLimitForRev(rev); got != 5 {
		t.Fatalf("app config lost to host default: %d", got)
	}
	app, rev = newRepo(t, "")
	app.opts.RepairMaxAttempts = repairIntPtr(1)
	if got := app.repairLimitForRev(rev); got != 1 {
		t.Fatalf("host default ignored: %d", got)
	}
	app, rev = newRepo(t, "")
	app.opts.RepairMaxAttempts = nil
	if got := app.repairLimitForRev(rev); got != DefaultMaxRepairAttempts {
		t.Fatalf("missing config = %d, want default %d", got, DefaultMaxRepairAttempts)
	}
	app, rev = newRepo(t, `{"repair": {"max_attempts": "lots"}}`)
	app.opts.RepairMaxAttempts = repairIntPtr(7)
	if got := app.repairLimitForRev(rev); got != DefaultMaxRepairAttempts {
		t.Fatalf("invalid config = %d, want default %d", got, DefaultMaxRepairAttempts)
	}
	if !strings.Contains(warned.String(), "repair.max_attempts") {
		t.Fatalf("invalid config logged no warning: %q", warned.String())
	}
	app, rev = newRepo(t, `{"repair": {"max_attempts": "unlimited"}}`)
	app.opts.RepairMaxAttempts = repairIntPtr(1)
	if got := app.repairLimitForRev(rev); got != RepairUnlimited {
		t.Fatalf("unlimited config = %d", got)
	}
}

// The serve flag parses a number or "unlimited"; anything else fails loudly.
func TestParseRepairMaxAttemptsFlag(t *testing.T) {
	if n, err := parseRepairMaxAttempts(""); err != nil || n != nil {
		t.Fatalf("empty = %v, %v", n, err)
	}
	if n, err := parseRepairMaxAttempts("3"); err != nil || n == nil || *n != 3 {
		t.Fatalf("3 = %v, %v", n, err)
	}
	if n, err := parseRepairMaxAttempts("0"); err != nil || n == nil || *n != 0 {
		t.Fatalf("0 = %v, %v", n, err)
	}
	if n, err := parseRepairMaxAttempts("unlimited"); err != nil || n == nil || *n != RepairUnlimited {
		t.Fatalf("unlimited = %v, %v", n, err)
	}
	for _, bad := range []string{"lots", "-1", "2.5", "Unlimited"} {
		if n, err := parseRepairMaxAttempts(bad); err == nil {
			t.Errorf("flag %q accepted as %v", bad, n)
		}
	}
}

// A cancelled context stops the loop before the delegate runs.
func TestRepairCancelledStopsLoop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses /bin/sh")
	}
	delegate := &repairShaper{mode: "fix"}
	app, base := repairLimitFixture(t, delegate, "")
	s := repairLimitStore(t, app, "device:alice", base)
	in, _, err := s.Observe(Incident{App: "notes", Kind: IncidentAPI, Path: "api/list", Status: 500, Message: "unit list failed", Evidence: "exit 1", BaseRev: base}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	app.repairIncident(ctx, "device:alice", in.ID)
	if len(delegate.tasks) != 0 {
		t.Fatalf("cancelled repair called the delegate %d times", len(delegate.tasks))
	}
}
