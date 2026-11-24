package src_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/qiangli/yoke/pkg/shape/state"
	"github.com/qiangli/yoke/pkg/src"
)

func TestActivateRecordsMemberAuthorOnOwnerLine(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "src", "site")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	r, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	wt, err := r.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = wt.Add("index.html")
	baseHash, err := wt.Commit("base", &gogit.CommitOptions{
		Author: &object.Signature{Name: "owner", Email: "o@example.test", When: time.Now()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("pristine"), baseHash)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("member edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _ = wt.Add("index.html")
	candHash, err := wt.Commit("candidate", &gogit.CommitOptions{
		Author: &object.Signature{Name: "member", Email: "c@example.test", When: time.Now()},
	})
	if err != nil {
		t.Fatal(err)
	}

	gate := state.GateFunc(func(cand, base, principal string) (state.EvidenceRef, error) {
		return state.EvidenceRef{Verdict: "pass", EvidenceID: "ev-1"}, nil
	})
	res, err := src.Activate(home, "site", candHash.String(), "o@example.test", src.ActivateOptions{
		ExpectedBase: baseHash.String(),
		Gate:         gate,
		Request:      "update copy",
		Author:       "c@example.test",
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if res.Principal != "o@example.test" || res.Author != "c@example.test" {
		t.Fatalf("ActivateResult = %+v", res)
	}
	h, err := src.History(home, "site", "o@example.test")
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(h.Versions) != 1 || h.Versions[0].Principal != "o@example.test" || h.Versions[0].Author != "c@example.test" {
		t.Fatalf("History versions = %+v", h.Versions)
	}
	lines, err := src.PrincipalLines(dir)
	if err != nil || len(lines) != 1 || lines[0].Principal != "o@example.test" {
		t.Fatalf("PrincipalLines = %+v, %v", lines, err)
	}
}
