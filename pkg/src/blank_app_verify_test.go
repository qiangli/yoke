// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package src_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/shape/verify"
)

// TestBlankAppFirstWishVerifies: an app made with `app new` is shaped like
// any other. Its blank base carries a contract and a smoke check, so the
// first wish (the page grows into a sign-up sheet) passes the trusted
// verifier against that base, and a candidate that drops the page fails it.
func TestBlankAppFirstWishVerifies(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("checks run under /bin/sh")
	}
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	cat := fleet.New(fleet.WithRoot(filepath.Join(home, "registry")), fleet.WithoutCloudOverlay())
	app, err := fleet.NewBlankShapedApp(cat, fleet.App{Name: "signup", Label: "Sign-up sheet"})
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

	candidate := func(msg string, edit func(dir string)) string {
		t.Helper()
		wt, err := repo.Worktree()
		if err != nil {
			t.Fatal(err)
		}
		edit(app.Source)
		if err := wt.AddWithOptions(&gogit.AddOptions{All: true}); err != nil {
			t.Fatal(err)
		}
		sha, err := wt.Commit(msg, &gogit.CommitOptions{Author: &object.Signature{Name: "genie", Email: "genie@localhost", When: time.Now()}})
		if err != nil {
			t.Fatal(err)
		}
		return sha.String()
	}
	run := func(cand string) verify.Report {
		t.Helper()
		rep, err := verify.Run(context.Background(), verify.Options{
			Home: home, RepoDir: app.Source, BaseSHA: base, CandidateSHA: cand,
			Principal: "alice",
			Runner:    verify.TrustedRunner{Bin: "/bin/sh", Effects: []string{"read"}},
		})
		if err != nil {
			t.Fatalf("verify %s: %v", cand, err)
		}
		return rep
	}

	page := filepath.Join(app.Source, "public", "index.html")
	wish := candidate("wish: a sign-up sheet for Saturday's class", func(dir string) {
		b, _ := os.ReadFile(page)
		grown := strings.Replace(string(b), "</main>",
			"  <form><input name=\"name\" placeholder=\"Name\"> <input name=\"email\" placeholder=\"Email\"> <button>Sign up</button></form>\n</main>", 1)
		if err := os.WriteFile(page, []byte(grown), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if rep := run(wish); !rep.Passed() {
		t.Fatalf("first wish on a blank app refused: %s", rep.Record.Reason)
	}

	broken := candidate("drops the page", func(dir string) {
		if err := os.Remove(page); err != nil {
			t.Fatal(err)
		}
	})
	if rep := run(broken); rep.Passed() {
		t.Fatal("a candidate that drops the blank app's page passed the verifier")
	}
}
