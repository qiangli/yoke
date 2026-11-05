package src_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/qiangli/yoke/pkg/fleet"
	shapegate "github.com/qiangli/yoke/pkg/shape/gate"
	"github.com/qiangli/yoke/pkg/shape/state"
	"github.com/qiangli/yoke/pkg/shape/verify"
	"github.com/qiangli/yoke/pkg/src"
)

func TestFirstActivationSealedUndoRetainsReleasePerPrincipal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	bin := "/bin/sh"
	if runtime.GOOS == "windows" {
		bin = os.Getenv("BASHY_BIN")
		if bin == "" {
			bin = "bashy"
		}
		var err error
		bin, err = exec.LookPath(bin)
		if err != nil {
			t.Fatalf("native fixture shell: %v", err)
		}
	}
	runner := verify.TrustedRunner{Bin: bin, Effects: []string{"read"}}
	cat := fleet.New(fleet.WithRoot(filepath.Join(home, "registry")), fleet.WithoutCloudOverlay())
	app, err := fleet.NewBlankShapedApp(cat, fleet.App{Name: "first-undo", Label: "First undo"})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := gogit.PlainOpen(app.Source)
	if err != nil {
		t.Fatal(err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	base := head.Hash().String()
	page := filepath.Join(app.Source, "public", "index.html")
	body, err := os.ReadFile(page)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(page, append(body, []byte("\n<!-- first accepted wish -->\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = wt.Add("public/index.html"); err != nil {
		t.Fatal(err)
	}
	sha, err := wt.Commit("first wish", &gogit.CommitOptions{Author: &object.Signature{Name: "fixture", Email: "fixture@example.invalid", When: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	read := func(principal string) state.State {
		t.Helper()
		store, e := state.OpenAt(home, app.Name, principal)
		if e != nil {
			t.Fatal(e)
		}
		st, e := store.Read()
		if e != nil {
			t.Fatal(e)
		}
		return st
	}
	for _, principal := range []string{"alice", "bob"} {
		proof, e := verify.Run(context.Background(), verify.Options{Home: home, RepoDir: app.Source, BaseSHA: base, CandidateSHA: sha.String(), Principal: principal, Runner: runner})
		if e != nil || !proof.Passed() {
			t.Fatalf("%s first wish proof: %v %+v", principal, e, proof.Record)
		}
		activated, e := src.Activate(home, app.Name, sha.String(), principal, src.ActivateOptions{ExpectedBase: base, Gate: shapegate.New(home, proof.Path)})
		if e != nil {
			t.Fatal(e)
		}
		if activated.Previous != base || read(principal).LastKnownGood != base {
			t.Fatalf("%s lost actual release: %+v %+v", principal, activated, read(principal))
		}
		history, e := src.History(home, app.Name, principal)
		if e != nil || len(history.Entries) != 1 {
			t.Fatalf("first activation history: %+v %v", history, e)
		}
	}
	undone, err := src.UndoSealed(context.Background(), home, app.Name, "alice", src.RestoreSealedOptions{Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	if undone.Revision != base || read("alice").ActiveRevision != base || read("alice").LastKnownGood != sha.String() {
		t.Fatalf("first undo: %+v %+v", undone, read("alice"))
	}
	if st := read("bob"); st.ActiveRevision != sha.String() || st.LastKnownGood != base {
		t.Fatalf("alice undo changed bob: %+v", st)
	}
	history, err := src.History(home, app.Name, "alice")
	if err != nil || len(history.Entries) != 2 {
		t.Fatalf("undo history: %+v %v", history, err)
	}
}
