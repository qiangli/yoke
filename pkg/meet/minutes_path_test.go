package meet

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --out MAY BE A DIRECTORY. The flag's help says "docs | kb | <path>", which
// invites one — and the path used to be handed straight to atomicWrite, which
// writes <path>.tmp and renames it onto <path>. Renaming a file onto an existing
// DIRECTORY fails, so the meeting died at its last step:
//
//	rename /…/scratchpad.tmp /…/scratchpad: file exists
//
// after every participant had spoken and been paid for. The minutes were lost at
// the exact moment they were finished.
func TestMinutesPathIntoADirectory(t *testing.T) {
	dir := t.TempDir()
	st := &State{
		ID:      "x",
		Topic:   "ycode is slow",
		Out:     dir,
		Created: time.Date(2026, 7, 14, 9, 30, 0, 0, time.UTC),
	}
	got := minutesPath(st)
	if got == dir {
		t.Fatalf("minutes path is the DIRECTORY itself (%s) — the rename will fail and the minutes are lost", got)
	}
	if filepath.Dir(got) != dir {
		t.Errorf("minutes should land inside the given directory: got %s, want inside %s", got, dir)
	}
	if !strings.HasSuffix(got, ".md") {
		t.Errorf("minutes should be a .md file: %s", got)
	}
}

// An explicit FILE path is still taken literally — that is the other half of the
// contract, and it must not regress.
func TestMinutesPathToAnExplicitFile(t *testing.T) {
	want := filepath.Join(t.TempDir(), "notes.md")
	st := &State{ID: "x", Topic: "t", Out: want, Created: time.Now()}
	if got := minutesPath(st); got != want {
		t.Errorf("explicit file path = %s, want %s", got, want)
	}
}
