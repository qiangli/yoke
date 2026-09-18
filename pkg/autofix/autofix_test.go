package autofix

import (
	"runtime"
	"testing"
)

func TestSedDialect(t *testing.T) {
	if runtime.GOOS == "linux" {
		// On Linux GNU sed accepts -r, so autofix is a deliberate no-op.
		if _, _, ok := Adapt([]string{"sed", "-r", "s/a/b/", "f"}); ok {
			t.Fatal("linux: sed -r should not be adapted (GNU accepts it)")
		}
		return
	}
	cases := []struct {
		in       []string
		wantFlag string // the flag expected at the rewritten position, "" = no change
		wantOK   bool
	}{
		{[]string{"sed", "-r", "s/a/b/", "f"}, "-E", true},
		{[]string{"sed", "-nr", "s/a/b/p", "f"}, "-nE", true},
		{[]string{"sed", "--regexp-extended", "s/a/b/", "f"}, "-E", true},
		{[]string{"sed", "-E", "s/a/b/", "f"}, "", false},       // already portable
		{[]string{"sed", "-i", "-r", "s/a/b/", "f"}, "", false}, // WRITE — never adapt
		{[]string{"sed", "-ri", "s/a/b/", "f"}, "", false},      // combined write cluster
		{[]string{"sed", "s/a/b/", "f"}, "", false},             // no -r
	}
	for _, c := range cases {
		fixed, note, ok := Adapt(c.in)
		if ok != c.wantOK {
			t.Errorf("Adapt(%v) ok=%v want %v", c.in, ok, c.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if note == "" {
			t.Errorf("Adapt(%v) adapted but produced no note", c.in)
		}
		if fixed[1] != c.wantFlag {
			t.Errorf("Adapt(%v) => %v, want flag %q at [1]", c.in, fixed, c.wantFlag)
		}
	}
}

func TestAdaptUnknownCommandNoOp(t *testing.T) {
	if _, _, ok := Adapt([]string{"grep", "-P", "x", "f"}); ok {
		t.Fatal("grep is not in the table; must be a no-op")
	}
	if _, _, ok := Adapt(nil); ok {
		t.Fatal("empty argv must be a no-op")
	}
}
