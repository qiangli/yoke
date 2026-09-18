package skills

import "testing"

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
// namespace. The probe engine itself is tested in pkg/spacetime; this
// mirrors its helper so the skills catalog tests stay hermetic too.
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
