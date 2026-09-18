// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package reduce

import (
	"path/filepath"
	"testing"
)

func TestCanonicalizeHomeExactAndDescendant(t *testing.T) {
	home := filepath.Join(string(filepath.Separator), "Users", "alice")
	in := []byte("home=" + home + "\nlog=" + filepath.Join(home, "work", "run.log") + "\n")
	want := "home=$HOME\nlog=" + filepath.Join("$HOME", "work", "run.log") + "\n"
	if got := string(CanonicalizeHome(in, home)); got != want {
		t.Fatalf("CanonicalizeHome() = %q, want %q", got, want)
	}
}

func TestCanonicalizeHomeIsBoundarySafe(t *testing.T) {
	home := filepath.Join(string(filepath.Separator), "Users", "alice")
	lookalike := filepath.Join(string(filepath.Separator), "Users", "alice2", "work")
	embedded := filepath.Join(string(filepath.Separator), "mnt") + home
	in := []byte(lookalike + "\n" + embedded + "\nvalid=" + filepath.Join(home, "work") + "\n")
	want := lookalike + "\n" + embedded + "\nvalid=" + filepath.Join("$HOME", "work") + "\n"
	if got := string(CanonicalizeHome(in, home)); got != want {
		t.Fatalf("boundary handling = %q, want %q", got, want)
	}
}

func TestCanonicalizeHomeRejectsUnsafeHomes(t *testing.T) {
	in := []byte("diagnostic / fixture\n")
	for _, home := range []string{"", ".", "relative/home", string(filepath.Separator)} {
		t.Run(home, func(t *testing.T) {
			if got := CanonicalizeHome(in, home); string(got) != string(in) {
				t.Fatalf("unsafe home %q changed input to %q", home, got)
			}
		})
	}
}

func TestCanonicalizeHomePreservesLiteralToken(t *testing.T) {
	in := []byte("$HOME/work remains literal\n")
	if got := string(CanonicalizeHome(in, filepath.Join(string(filepath.Separator), "Users", "alice"))); got != string(in) {
		t.Fatalf("literal token changed: %q", got)
	}
}
