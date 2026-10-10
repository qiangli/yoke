// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package handoff

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRepoMembers_FakeUmbrella(t *testing.T) {
	umbrella := t.TempDir()

	// Structure:
	// umbrella/
	//   go.work
	//   a/
	//     nested/
	//   b/
	//     nested/
	//   c/
	dirA := filepath.Join(umbrella, "a")
	dirANested := filepath.Join(dirA, "nested")
	dirB := filepath.Join(umbrella, "b")
	dirBNested := filepath.Join(dirB, "nested")
	dirC := filepath.Join(umbrella, "c")

	for _, d := range []string{dirA, dirANested, dirB, dirBNested, dirC} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	goWorkContent := `go 1.27

// Top-level comment
use ./a // single-line use for a with inline comment

use (
	./b // block use for b
	"c" /* inline block comment */
	./a/nested
	./b/nested
	./missing_dir // nonexistent use dir
)
`
	if err := os.WriteFile(filepath.Join(umbrella, "go.work"), []byte(goWorkContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// 1. Claim from a: includes root a first, siblings b and c, nested module a/nested,
	// but excludes b/nested (nested of sibling) and missing_dir.
	wantA := []string{dirA, dirANested, dirB, dirC}
	gotA := RepoMembers(dirA)
	if !reflect.DeepEqual(gotA, wantA) {
		t.Fatalf("RepoMembers(dirA) = %v, want %v", gotA, wantA)
	}

	// ProjectRoots exported wrapper check
	if gotPR := ProjectRoots(dirA); !reflect.DeepEqual(gotPR, wantA) {
		t.Fatalf("ProjectRoots(dirA) = %v, want %v", gotPR, wantA)
	}

	// 2. Claim from b: includes root b first, siblings a and c, nested module b/nested,
	// but excludes a/nested.
	wantB := []string{dirB, dirA, dirBNested, dirC}
	gotB := RepoMembers(dirB)
	if !reflect.DeepEqual(gotB, wantB) {
		t.Fatalf("RepoMembers(dirB) = %v, want %v", gotB, wantB)
	}

	// 3. Claim from c: includes root c first, siblings a and b, no nested modules.
	wantC := []string{dirC, dirA, dirB}
	gotC := RepoMembers(dirC)
	if !reflect.DeepEqual(gotC, wantC) {
		t.Fatalf("RepoMembers(dirC) = %v, want %v", gotC, wantC)
	}
}

func TestRepoMembers_NoGoWork_FallbackToGoMod(t *testing.T) {
	tmp := t.TempDir()
	repoX := filepath.Join(tmp, "x")
	repoY := filepath.Join(tmp, "y")
	if err := os.MkdirAll(repoX, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(repoY, 0o755); err != nil {
		t.Fatal(err)
	}

	goModContent := `module example.com/x

go 1.27

replace (
	example.com/y => ../y
	example.com/missing => ../missing
)
`
	if err := os.WriteFile(filepath.Join(repoX, "go.mod"), []byte(goModContent), 0o644); err != nil {
		t.Fatal(err)
	}

	want := []string{repoX, repoY}
	got := RepoMembers(repoX)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RepoMembers(repoX) = %v, want %v", got, want)
	}
}

func TestRepoMembers_GoWorkSyntaxVariations(t *testing.T) {
	umbrella := t.TempDir()
	repo1 := filepath.Join(umbrella, "repo1")
	repo2 := filepath.Join(umbrella, "repo2")
	repo3 := filepath.Join(umbrella, "repo3")
	for _, d := range []string{repo1, repo2, repo3} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	goWorkContent := `
// Single-line block form
use ( ./repo2 )
use "repo3"
use ./repo1
`
	if err := os.WriteFile(filepath.Join(umbrella, "go.work"), []byte(goWorkContent), 0o644); err != nil {
		t.Fatal(err)
	}

	want := []string{repo1, repo2, repo3}
	got := RepoMembers(repo1)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RepoMembers(repo1) = %v, want %v", got, want)
	}
}

func TestRepoMembers_StandaloneEmpty(t *testing.T) {
	tmp := t.TempDir()
	alone := filepath.Join(tmp, "alone")
	if err := os.MkdirAll(alone, 0o755); err != nil {
		t.Fatal(err)
	}

	want := []string{alone}
	got := RepoMembers(alone)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RepoMembers(alone) = %v, want %v", got, want)
	}
}
