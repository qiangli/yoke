package nativepath

import (
	"reflect"
	"testing"
)

// A Windows block spells the variable "Path"; the shell's and POSIX's spelling
// is "PATH". The OS resolves the name case-insensitively, a Unix one does not.
func TestIsPathName(t *testing.T) {
	cases := []struct {
		name    string
		windows bool
		want    bool
	}{
		{"PATH", false, true},
		{"Path", false, false},
		{"path", false, false},
		{"PATH", true, true},
		{"Path", true, true},
		{"path", true, true},
		{"PATHEXT", true, false},
		{"XPATH", true, false},
	}
	for _, c := range cases {
		if got := IsPathName(c.name, c.windows); got != c.want {
			t.Errorf("IsPathName(%q, windows=%v) = %v, want %v", c.name, c.windows, got, c.want)
		}
	}
}

func TestLookupPathReadsTheSpellingTheOSHas(t *testing.T) {
	env := []string{"HOME=/h", `Path=C:\a;C:\b`, "FOO=1"}
	if v, ok := LookupPath(env, true); !ok || v != `C:\a;C:\b` {
		t.Errorf("windows LookupPath = %q, %v; want the Path= entry", v, ok)
	}
	if _, ok := LookupPath(env, false); ok {
		t.Errorf("unix LookupPath found Path=, but Unix names are case-sensitive")
	}
	if v, ok := LookupPath([]string{"PATH=/usr/bin"}, false); !ok || v != "/usr/bin" {
		t.Errorf("unix LookupPath = %q, %v", v, ok)
	}
	// Several case-variants: the FIRST is what GetEnvironmentVariable answers.
	dual := []string{`Path=C:\orig`, `PATH=C:\exported`}
	if v, _ := LookupPath(dual, true); v != `C:\orig` {
		t.Errorf("LookupPath over a dual block = %q, want the first entry", v)
	}
}

// The defect: editing PATH by the literal "PATH=" prefix misses "Path=" and
// appends a second entry, which os/exec's case-insensitive dedup then lets
// replace the real one. SetPath rewrites in place and leaves exactly one.
func TestSetPathRewritesInPlaceKeepingSpelling(t *testing.T) {
	env := []string{"HOME=/h", `Path=C:\a`, "FOO=1"}
	got := SetPath(env, true, `C:\a;C:\extra`)
	want := []string{"HOME=/h", `Path=C:\a;C:\extra`, "FOO=1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SetPath = %q, want %q", got, want)
	}
	if !reflect.DeepEqual(env, []string{"HOME=/h", `Path=C:\a`, "FOO=1"}) {
		t.Errorf("SetPath modified its input: %q", env)
	}
}

func TestSetPathDropsOtherCaseVariants(t *testing.T) {
	env := []string{`Path=C:\orig`, "FOO=1", `PATH=C:\exported`, `path=C:\third`}
	got := SetPath(env, true, `C:\one`)
	want := []string{`Path=C:\one`, "FOO=1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SetPath = %q, want %q", got, want)
	}
}

func TestSetPathAppendsWhenAbsent(t *testing.T) {
	got := SetPath([]string{"FOO=1"}, true, `C:\a`)
	if want := []string{"FOO=1", `PATH=C:\a`}; !reflect.DeepEqual(got, want) {
		t.Errorf("SetPath = %q, want %q", got, want)
	}
	// Off Windows "Path" is an unrelated variable and must survive untouched.
	got = SetPath([]string{"Path=/other"}, false, "/usr/bin")
	if want := []string{"Path=/other", "PATH=/usr/bin"}; !reflect.DeepEqual(got, want) {
		t.Errorf("unix SetPath = %q, want %q", got, want)
	}
}

func TestCollapse(t *testing.T) {
	single := []string{"FOO=1", `Path=C:\a`}
	if got := Collapse(single, true); !reflect.DeepEqual(got, single) {
		t.Errorf("Collapse changed a single-entry list: %q", got)
	}
	none := []string{"FOO=1"}
	if got := Collapse(none, true); !reflect.DeepEqual(got, none) {
		t.Errorf("Collapse invented PATH: %q", got)
	}
	dual := []string{`Path=C:\orig`, "FOO=1", `PATH=C:\exported`}
	got := Collapse(dual, true)
	if want := []string{`Path=C:\orig`, "FOO=1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Collapse = %q, want the first entry kept in place, %q", got, want)
	}
	// Unix: two differently-cased names are two variables.
	if got := Collapse(dual, false); !reflect.DeepEqual(got, dual) {
		t.Errorf("unix Collapse merged distinct variables: %q", got)
	}
}
