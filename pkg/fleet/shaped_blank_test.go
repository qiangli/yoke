// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package fleet

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// TestNewBlankShapedApp: `app new` makes a shaped app from nothing — a
// managed checkout whose base revision is the blank page, its contract and
// its smoke check — registered exactly like `app add --type shaped`, so the
// first wish on it goes through the same verifier, versions and offers.
func TestNewBlankShapedApp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	cat := New(WithRoot(filepath.Join(home, "registry")), WithoutCloudOverlay())

	a, err := NewBlankShapedApp(cat, App{Name: "signup", Label: "Sign-up sheet"})
	if err != nil {
		t.Fatalf("new blank app: %v", err)
	}
	if !a.IsShaped() || a.Port != 0 {
		t.Fatalf("app = %+v; want a shaped app with no port", a)
	}
	want := filepath.Join(home, "src", "signup")
	if a.Source != want {
		t.Fatalf("source = %q, want the managed checkout %q", a.Source, want)
	}
	if got, ok := cat.App("signup"); !ok || got.Source != want || !got.IsShaped() {
		t.Fatalf("registry record = %+v, %v", got, ok)
	}

	for _, rel := range []string{"public/index.html", "contract.bsh", "checks/smoke.bsh"} {
		if _, err := os.Stat(filepath.Join(want, rel)); err != nil {
			t.Errorf("blank base lacks %s: %v", rel, err)
		}
	}
	page, _ := os.ReadFile(filepath.Join(want, "public", "index.html"))
	if !strings.Contains(string(page), "Sign-up sheet") || !strings.Contains(string(page), "data-bashy-blank") {
		t.Errorf("blank page does not carry the label and blank marker:\n%s", page)
	}
	contract, _ := os.ReadFile(filepath.Join(want, "contract.bsh"))
	if !strings.Contains(string(contract), "@effects(") {
		t.Errorf("blank contract declares no effect cap: %q", contract)
	}

	repo, err := gogit.PlainOpen(want)
	if err != nil {
		t.Fatalf("managed checkout is not a repository: %v", err)
	}
	if _, err := repo.Reference(plumbing.NewBranchReferenceName(shapedBaseBranch), true); err != nil {
		t.Fatalf("blank app has no %s base branch: %v", shapedBaseBranch, err)
	}

	// A second `app new` with the same name must not overwrite the app, and
	// the refusal is recognisable (the console picks a suffixed name then).
	if _, err := NewBlankShapedApp(cat, App{Name: "signup"}); !errors.Is(err, ErrAppExists) {
		t.Fatalf("a second app new with a taken name: err = %v, want ErrAppExists", err)
	}
	if _, err := NewBlankShapedApp(cat, App{Name: "signup-2"}); err != nil {
		t.Fatalf("a suffixed name is a new app: %v", err)
	}
	// The registry's own validation still applies, before anything is written.
	strict := New(WithRoot(filepath.Join(home, "registry")), WithoutCloudOverlay(),
		WithAppValidate(func(a App) error {
			if a.Name == "taken" {
				return os.ErrExist
			}
			return nil
		}))
	if _, err := NewBlankShapedApp(strict, App{Name: "taken"}); err == nil {
		t.Fatal("validation refusal ignored")
	}
	if _, statErr := os.Stat(filepath.Join(home, "src", "taken")); !os.IsNotExist(statErr) {
		t.Fatal("a refused app new left a managed checkout behind")
	}
	if _, err := NewBlankShapedApp(cat, App{Name: "../escape"}); err == nil {
		t.Fatal("an invalid name was accepted")
	}
}
