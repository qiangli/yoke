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
	//     go.mod (requires example.com/b)
	//     nested/
	//       go.mod
	//   b/
	//     go.mod
	//     nested/
	//       go.mod
	//   c/
	//     go.mod (not required by a)
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

	writeMod := func(dir, content string) {
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	writeMod(dirA, `module example.com/a

go 1.27

require (
	example.com/b v1.0.0
)
`)
	writeMod(dirANested, `module example.com/a/nested

go 1.27
`)
	writeMod(dirB, `module example.com/b

go 1.27
`)
	writeMod(dirBNested, `module example.com/b/nested

go 1.27
`)
	writeMod(dirC, `module example.com/c

go 1.27
`)

	goWorkContent := `go 1.27

// Top-level comment
use ./a // single-line use for a with inline comment

use (
	./b // block use for b
	"c" // inline comment
	./a/nested
	./b/nested
	./missing_dir // nonexistent use dir
)
`
	if err := os.WriteFile(filepath.Join(umbrella, "go.work"), []byte(goWorkContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// 1. Claim from a: includes root a first, nested module a/nested (inside root),
	// and required sibling b. Sibling c is excluded (not required by a),
	// b/nested is excluded (nested of sibling b, not required by a),
	// and missing_dir is excluded (nonexistent).
	wantA := []string{dirA, dirANested, dirB}
	gotA := RepoMembers(dirA)
	if !reflect.DeepEqual(gotA, wantA) {
		t.Fatalf("RepoMembers(dirA) = %v, want %v", gotA, wantA)
	}

	// ProjectRoots exported wrapper check
	if gotPR := ProjectRoots(dirA); !reflect.DeepEqual(gotPR, wantA) {
		t.Fatalf("ProjectRoots(dirA) = %v, want %v", gotPR, wantA)
	}

	// 2. Claim from b: includes root b first, nested module b/nested (inside root),
	// but excludes a and c (not required by b).
	wantB := []string{dirB, dirBNested}
	gotB := RepoMembers(dirB)
	if !reflect.DeepEqual(gotB, wantB) {
		t.Fatalf("RepoMembers(dirB) = %v, want %v", gotB, wantB)
	}

	// 3. Claim from c: includes root c first, no nested modules, excludes a and b.
	wantC := []string{dirC}
	gotC := RepoMembers(dirC)
	if !reflect.DeepEqual(gotC, wantC) {
		t.Fatalf("RepoMembers(dirC) = %v, want %v", gotC, wantC)
	}
}

func TestRepoMembers_IndirectRequire(t *testing.T) {
	umbrella := t.TempDir()

	dirA := filepath.Join(umbrella, "a")
	dirB := filepath.Join(umbrella, "b")
	dirC := filepath.Join(umbrella, "c")

	for _, d := range []string{dirA, dirB, dirC} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	writeMod := func(dir, content string) {
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// a requires b directly and c indirectly
	writeMod(dirA, `module example.com/a

go 1.27

require (
	example.com/b v1.0.0
	example.com/c v1.0.0 // indirect
)
`)
	writeMod(dirB, `module example.com/b

go 1.27
`)
	writeMod(dirC, `module example.com/c

go 1.27
`)

	goWorkContent := `go 1.27

use (
	./a
	./b
	./c
)
`
	if err := os.WriteFile(filepath.Join(umbrella, "go.work"), []byte(goWorkContent), 0o644); err != nil {
		t.Fatal(err)
	}

	wantA := []string{dirA, dirB, dirC}
	gotA := RepoMembers(dirA)
	if !reflect.DeepEqual(gotA, wantA) {
		t.Fatalf("RepoMembers(dirA) = %v, want %v", gotA, wantA)
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

	writeMod := func(dir, content string) {
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	writeMod(repo1, `module example.com/repo1

go 1.27

require (
	example.com/repo2 v1.0.0
	example.com/repo3 v1.0.0
)
`)
	writeMod(repo2, `module example.com/repo2

go 1.27
`)
	writeMod(repo3, `module example.com/repo3

go 1.27
`)

	goWorkContent := `go 1.27

// Block form with comments and quoted paths
use (
	./repo2 // second repo
	"repo3"
)

// Single line form
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
