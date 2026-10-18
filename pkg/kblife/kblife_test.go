package kblife

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

var (
	t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 = t0.Add(24 * time.Hour)
	t2 = t0.Add(48 * time.Hour)
	t3 = t0.Add(72 * time.Hour)
)

func TestValidityAt(t *testing.T) {
	cases := []struct {
		name string
		v    Validity
		at   time.Time
		want bool
	}{
		{"open both", Validity{}, t1, true},
		{"before from", Validity{ValidFrom: t1}, t0, false},
		{"at from (inclusive)", Validity{ValidFrom: t1}, t1, true},
		{"before to", Validity{ValidTo: t2}, t1, true},
		{"at to (exclusive)", Validity{ValidTo: t2}, t2, false},
		{"after to", Validity{ValidFrom: t0, ValidTo: t2}, t3, false},
	}
	for _, c := range cases {
		if got := c.v.At(c.at); got != c.want {
			t.Errorf("%s: At = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestValidityCheck(t *testing.T) {
	if err := (Validity{ValidFrom: t0, ValidTo: t1}).Check(); err != nil {
		t.Fatal(err)
	}
	if err := (Validity{ValidTo: t1}).Check(); err != nil {
		t.Fatal(err)
	}
	for _, v := range []Validity{{ValidFrom: t1, ValidTo: t1}, {ValidFrom: t2, ValidTo: t1}} {
		if err := v.Check(); err == nil {
			t.Errorf("Check(%v) = nil, want inverted-range error", v)
		}
	}
}

func TestSupersede(t *testing.T) {
	old := Validity{ValidFrom: t0}
	rep := Validity{}
	if err := Supersede(&old, &rep, t1); err != nil {
		t.Fatal(err)
	}
	if !old.ValidTo.Equal(t1) || !rep.ValidFrom.Equal(t1) {
		t.Fatalf("old=%v rep=%v", old, rep)
	}
}

func TestSupersedeErrorsMutateNothing(t *testing.T) {
	cases := []struct {
		name     string
		old, rep Validity
		at       time.Time
		want     string
	}{
		{"at before old.ValidFrom", Validity{ValidFrom: t2}, Validity{}, t1, "before the old record"},
		{"old already closed", Validity{ValidFrom: t0, ValidTo: t1}, Validity{}, t2, "already closed"},
		{"old closed exactly at", Validity{ValidFrom: t0, ValidTo: t1}, Validity{}, t1, "already closed"},
		{"replacement ends before at", Validity{ValidFrom: t0}, Validity{ValidTo: t1}, t2, "replacement record's valid_to"},
		{"replacement ends at at", Validity{ValidFrom: t0}, Validity{ValidTo: t1}, t1, "replacement record's valid_to"},
		{"old inverted", Validity{ValidFrom: t2, ValidTo: t1}, Validity{}, t3, "inverted"},
		{"replacement inverted", Validity{ValidFrom: t0}, Validity{ValidFrom: t3, ValidTo: t2}, t1, "inverted"},
		{"zero instant", Validity{ValidFrom: t0}, Validity{}, time.Time{}, "non-zero"},
	}
	for _, c := range cases {
		old, rep := c.old, c.rep
		err := Supersede(&old, &rep, c.at)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want containing %q", c.name, err, c.want)
		}
		if old != c.old || rep != c.rep {
			t.Errorf("%s: inputs mutated on error: old=%v rep=%v", c.name, old, rep)
		}
	}
	if err := Supersede(nil, &Validity{}, t1); err == nil {
		t.Error("nil old accepted")
	}
}

func TestNewTombstone(t *testing.T) {
	tb, err := NewTombstone("kb:My Page", "wrong", "agent-a", "ep1", t1)
	if err != nil {
		t.Fatal(err)
	}
	if tb.Ref != "kb:My Page" || tb.Reason != "wrong" || tb.By != "agent-a" || tb.Episode != "ep1" || !tb.At.Equal(t1) {
		t.Fatalf("tombstone = %+v", tb)
	}
	for _, c := range []struct{ ref, reason, by string }{
		{"", "r", "b"}, {"kb:x", "", "b"}, {"kb:x", "  ", "b"}, {"kb:x", "r", ""},
	} {
		if _, err := NewTombstone(c.ref, c.reason, c.by, "", t1); err == nil {
			t.Errorf("NewTombstone(%q,%q,%q) = nil error", c.ref, c.reason, c.by)
		}
	}
	if _, err := NewTombstone("kb:x", "r", "b", "", time.Time{}); err == nil {
		t.Error("zero instant accepted")
	}
}

func TestResolve(t *testing.T) {
	tomb := &Tombstone{Ref: "kb:x", Reason: "r", By: "b", At: t2}
	// A page superseded at t1: old [t0,t1), replacement [t1,∞).
	oldV := Validity{ValidFrom: t0, ValidTo: t1}
	repV := Validity{ValidFrom: t1}
	cases := []struct {
		name  string
		v     Validity
		by    string
		tomb  *Tombstone
		asOf  time.Time
		want  State
		shown bool
	}{
		{"open valid", Validity{}, "", nil, t1, StateValid, true},
		{"old as-of before supersede", oldV, "new", nil, t0, StateValid, true},
		{"old as-of at supersede", oldV, "new", nil, t1, StateSuperseded, false},
		{"old as-of after supersede", oldV, "new", nil, t2, StateSuperseded, false},
		{"replacement as-of before supersede", repV, "", nil, t0, StateNotYetValid, false},
		{"replacement as-of after supersede", repV, "", nil, t2, StateValid, true},
		{"legacy superseded no valid_to", Validity{}, "new", nil, t0, StateSuperseded, false},
		{"expired", Validity{ValidTo: t1}, "", nil, t2, StateExpired, false},
		{"as-of before forget", Validity{}, "", tomb, t1, StateValid, true},
		{"as-of at forget", Validity{}, "", tomb, t2, StateForgotten, false},
		{"as-of after forget", Validity{}, "", tomb, t3, StateForgotten, false},
		{"forgotten wins over superseded", oldV, "new", tomb, t3, StateForgotten, false},
	}
	for _, c := range cases {
		got := Resolve(c.v, c.by, c.tomb, c.asOf)
		if got != c.want {
			t.Errorf("%s: Resolve = %q, want %q", c.name, got, c.want)
		}
		if Visible(got) != c.shown {
			t.Errorf("%s: Visible(%q) = %v, want %v", c.name, got, Visible(got), c.shown)
		}
	}
}

// TestImportsStdlibOnly pins the package's reason to exist: a dbdoc-backed
// store must be able to import it without a cycle.
func TestImportsStdlibOnly(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", ".").Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	for _, line := range strings.Fields(string(out)) {
		if line != "github.com/qiangli/yoke/pkg/kblife" {
			t.Errorf("kblife depends on non-stdlib package %s", line)
		}
	}
}
