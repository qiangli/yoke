package webconsole

import (
	"context"
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

type repairShaper struct {
	mode  string
	tasks []string
}

func (f *repairShaper) Delegate(_ context.Context, req DelegateRequest) (DelegateResult, error) {
	f.tasks = append(f.tasks, req.Task)
	p := filepath.Join(req.Dir, "api", "list.md")
	if f.mode == "add" {
		if err := os.WriteFile(filepath.Join(req.Dir, "api", "extra.md"), []byte("```bsh\nprintf ok\n```\n"), 0600); err != nil {
			return DelegateResult{}, err
		}
	} else if f.mode == "fail" {
		if err := os.WriteFile(p, []byte("```bsh\nexit 2\n```\n"), 0600); err != nil {
			return DelegateResult{}, err
		}
	} else {
		if err := os.WriteFile(p, []byte("```bsh\nprintf 'repaired\\n'\n```\n"), 0600); err != nil {
			return DelegateResult{}, err
		}
	}
	return DelegateResult{Agent: "test:repair"}, nil
}

func repairFixture(t *testing.T, mode string) (*shapedApp, *incidentStore, *repairShaper, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	source, _ := productionNotesSource(t, home)
	r, err := gogit.PlainOpen(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "api", "list.md"), []byte("```bsh\nexit 1\n```\n"), 0600); err != nil {
		t.Fatal(err)
	}
	wt, _ := r.Worktree()
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
	delegate := &repairShaper{mode: mode}
	opts := productionShapeOptions()
	opts.Home, opts.WorkRoot, opts.Runner = home, filepath.Join(t.TempDir(), "work"), []string{"/bin/sh"}
	opts.Delegate = delegate
	opts.Verifier = productionVerifier{home: home, runner: shapeverify.TrustedRunner{Bin: "/bin/sh", Effects: []string{"read"}}}
	opts.Activator.(*productionActivator).home = home
	app := &shapedApp{name: "notes", repo: source, opts: opts}
	st, _ := app.store("device:alice")
	incidents, err := openIncidents(st)
	if err != nil {
		t.Fatal(err)
	}
	return app, incidents, delegate, base.String()
}

func TestRepairPipeline(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses /bin/sh")
	}
	for _, tc := range []struct {
		mode, want string
		attempts   int
	}{{"fix", "activated", 1}, {"add", "refused", 1}, {"fail", "failed", 2}} {
		t.Run(tc.mode, func(t *testing.T) {
			app, incidents, delegate, base := repairFixture(t, tc.mode)
			in, start, err := incidents.Observe(Incident{App: "notes", Kind: IncidentAPI, Path: "api/list", Status: 500, Message: "unit list failed", Evidence: "exit 1", BaseRev: base}, time.Now())
			if err != nil || !start {
				t.Fatalf("observe: %v %v", start, err)
			}
			app.repairIncident(context.Background(), "device:alice", in.ID)
			got, err := incidents.Get(in.ID)
			if err != nil {
				t.Fatal(err)
			}
			if string(got.State) != tc.want || len(got.Attempts) != tc.attempts {
				t.Fatalf("incident: %+v", got)
			}
			// A failing replay is the candidate failing, not a new incident:
			// it must not record one (or start a nested repair).
			if all, _ := incidents.List(); len(all) != 1 {
				t.Fatalf("replay recorded incidents: %+v", all)
			}
			bob, _ := shape.OpenAt(app.opts.Home, "notes", "device:bob")
			bs, _ := bob.Read()
			if bs.ActiveRevision != base {
				t.Fatalf("bob changed to %s", bs.ActiveRevision)
			}
			alice, _ := shape.OpenAt(app.opts.Home, "notes", "device:alice")
			as, _ := alice.Read()
			if tc.want == "activated" {
				if as.ActiveRevision == base || got.Attempts[0].Version != "v1.0.1" {
					t.Fatalf("activation/version: %+v %+v", as, got.Attempts[0])
				}
				h, err := src.History(app.opts.Home, "notes", "device:alice")
				if err != nil || len(h.Entries) == 0 || h.Entries[0].Trigger != in.ID || h.Entries[0].Version != "v1.0.1" {
					t.Fatalf("history: %+v %v", h, err)
				}
				if _, err := src.UndoSealed(context.Background(), app.opts.Home, "notes", "device:alice", src.RestoreSealedOptions{Runner: shapeverify.TrustedRunner{Bin: "/bin/sh", Effects: []string{"read"}}}); err != nil {
					t.Fatalf("undo repair: %v", err)
				}
				as, _ = alice.Read()
				if as.ActiveRevision != base {
					t.Fatalf("undo active = %s, want %s", as.ActiveRevision, base)
				}
			} else if as.ActiveRevision != base {
				t.Fatalf("failed/refused repair activated: %+v", as)
			}
			if tc.want == "refused" {
				c, _, err := app.loadCandidate("device:alice", got.Attempts[0].Candidate)
				if err != nil || c.Kind != candKindRequest || c.Status != candReady {
					t.Fatalf("review candidate: %+v %v", c, err)
				}
			}
			if tc.want == "failed" && (len(delegate.tasks) != 2 || !strings.Contains(delegate.tasks[1], "Previous attempt failed:")) {
				t.Fatalf("retry briefs: %v", delegate.tasks)
			}
		})
	}
}
