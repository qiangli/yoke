package spacetime

import (
	"reflect"
	"strings"
	"testing"
)

// fakeResolver answers a namespace from a fixed table (hermetic — unit
// tests never LookPath or exec).
type fakeResolver struct {
	ns   string
	vals map[string]string
}

func (f fakeResolver) Namespace() string { return f.ns }
func (f fakeResolver) Eval(key string) (string, error) {
	if v, ok := f.vals[key]; ok {
		return v, nil
	}
	return "absent", nil
}

// testProbes builds a hermetic ProbeSet: fixed core values + fake tool
// namespace.
func testProbes(t *testing.T, core map[string]string, tools map[string]string) *ProbeSet {
	t.Helper()
	ps := DefaultProbes(NopCache())
	// Pin every core probe to the test's world; drop the ones not named.
	for _, name := range []string{"os", "arch", "os.release", "libc", "container", "tty", "elevated"} {
		ps.SetStatic(name, core[name])
	}
	for k, v := range core {
		ps.SetStatic(k, v)
	}
	ps.Register(fakeResolver{ns: "tool", vals: tools})
	return ps
}

func TestParseRequires(t *testing.T) {
	tests := []struct {
		in      string
		want    Requires
		wantErr bool
	}{
		{in: "", want: Requires{}},
		{in: "os=linux,darwin", want: Requires{Clauses: []Clause{{Key: "os", Op: OpAnyOf, Values: []string{"linux", "darwin"}}}}},
		{in: "tty", want: Requires{Clauses: []Clause{{Key: "tty", Op: OpBool}}}},
		{in: "go>=1.26", want: Requires{Clauses: []Clause{{Key: "go", Op: OpAtLeast, Values: []string{"1.26"}}}}},
		// The pressure-test five (umbrella doc §11.6a):
		{in: "has=git has=claude,codex,opencode,aider bashy>=0.9", want: Requires{Clauses: []Clause{
			{Key: "has", Op: OpAnyOf, Values: []string{"git"}},
			{Key: "has", Op: OpAnyOf, Values: []string{"claude", "codex", "opencode", "aider"}},
			{Key: "bashy", Op: OpAtLeast, Values: []string{"0.9"}},
		}}},
		{in: "has=python3,node", want: Requires{Clauses: []Clause{{Key: "has", Op: OpAnyOf, Values: []string{"python3", "node"}}}}},
		// Errors are loud:
		{in: "os=", wantErr: true},
		{in: "=linux", wantErr: true},
		{in: "OS=linux", wantErr: true},
		{in: "go>=", wantErr: true},
		{in: "has=git,", wantErr: true},
		{in: "not-a-clause!", wantErr: true},
	}
	for _, tt := range tests {
		got, err := ParseRequires(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseRequires(%q) err=%v wantErr=%v", tt.in, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
			t.Errorf("ParseRequires(%q) = %+v, want %+v", tt.in, got, tt.want)
		}
	}
}

func TestRequiresEval(t *testing.T) {
	ps := testProbes(t,
		map[string]string{"os": "linux", "arch": "arm64", "tty": "false", "container": "true", "bashy": "0.9.1"},
		map[string]string{"git": "2.49.0", "go": "1.24.3", "python3": "3.12.1", "claude": "present"},
	)
	tests := []struct {
		requires string
		ok       bool
		failing  string
	}{
		{"", true, ""},
		{"os=linux,darwin", true, ""},
		{"os=darwin", false, "os=darwin: os=linux"},
		{"has=git", true, ""},
		{"has=docker", false, "has=docker: absent"},
		// any-of across tools: claude present suffices.
		{"has=claude,codex,opencode,aider", true, ""},
		// repeated clause key = AND: git present AND an agent CLI present.
		{"has=git has=claude,codex", true, ""},
		{"has=docker has=git", false, "has=docker: absent"},
		{"go>=1.24", true, ""},
		{"go>=1.26", false, "go>=1.26: tool.go=1.24.3"},
		{"go>=1.24.3", true, ""},
		{"bashy>=0.9", true, ""},
		{"rustc>=1.0", false, "rustc>=1.0: tool.rustc=absent"},
		// presence-only tools fail version comparison.
		{"claude>=2.0", false, "claude>=2.0: tool.claude=present"},
		{"tty", false, "tty: tty=false"},
		{"container", true, ""},
		// unpopulated reserved namespace fails applicability, no error.
		{"mesh.paired", false, "mesh.paired: mesh.paired=absent"},
	}
	for _, tt := range tests {
		r, err := ParseRequires(tt.requires)
		if err != nil {
			t.Fatalf("ParseRequires(%q): %v", tt.requires, err)
		}
		v := r.Eval(ps)
		if v.Applicable != tt.ok {
			t.Errorf("Eval(%q).Applicable = %v, want %v (failing=%q)", tt.requires, v.Applicable, tt.ok, v.Failing)
		}
		if tt.failing != "" && v.Failing != tt.failing {
			t.Errorf("Eval(%q).Failing = %q, want %q", tt.requires, v.Failing, tt.failing)
		}
	}
}

func TestProbeRefs(t *testing.T) {
	r, err := ParseRequires("os=linux has=git,go go>=1.26 tty mesh.paired")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(r.ProbeRefs(), " ")
	want := "os tool.git tool.go tty mesh.paired"
	if got != want {
		t.Errorf("ProbeRefs = %q, want %q", got, want)
	}
}

func TestVersionAtLeast(t *testing.T) {
	tests := []struct {
		have, want string
		ok         bool
	}{
		{"1.26", "1.26", true},
		{"1.26.2", "1.26", true},
		{"1.26", "1.26.2", false},
		{"2", "1.99", true},
		{"1.24.3", "1.26", false},
		{"present", "1.0", false},
		{"absent", "1.0", false},
		{"", "1.0", false},
	}
	for _, tt := range tests {
		if got := versionAtLeast(tt.have, tt.want); got != tt.ok {
			t.Errorf("versionAtLeast(%q, %q) = %v, want %v", tt.have, tt.want, got, tt.ok)
		}
	}
}
