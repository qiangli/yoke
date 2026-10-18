package fleet

import (
	"testing"
	"testing/fstest"
)

// Story #2367: on a development host `weave fleet --probe` smoked codex with the retired
// gpt-5.5 ("not supported when using Codex with a ChatGPT account") and agy
// with the retired agy-gemini3.1 binding ("model gemini-3.1-pro-high is not
// recognized"), because the first name-sorted binding was chosen whether or
// not it was retired. Both tools answered SMOKE-OK on their live bindings.
func TestSmokeArgvSkipsRetiredBindings(t *testing.T) {
	base := fstest.MapFS{
		"baseline/tools/smoke.yaml": &fstest.MapFile{Data: []byte("name: smoke\nkind: cli\ncli:\n  binary: smoke\n  launch:\n    exec: smoke --model {model} {prompt}\n")},
		"baseline/models/old.yaml":  &fstest.MapFile{Data: []byte("name: old\nmodel: old-target\nretired:\n  at: 2026-10-09\n  replaced_by: live\n")},
		"baseline/models/gone.yaml": &fstest.MapFile{Data: []byte("name: gone\nmodel: gone-target\n")},
		"baseline/models/live.yaml": &fstest.MapFile{Data: []byte("name: live\nmodel: live-target\n")},
		// Name-sorted ahead of the live binding, as on the host.
		"baseline/agents/a.yaml": &fstest.MapFile{Data: []byte("agents:\n  - name: smoke-a-retired-agent\n    tool: smoke\n    model: gone\n    retired:\n      at: 2026-10-09\n      replaced_by: smoke-live\n  - name: smoke-b-retired-model\n    tool: smoke\n    model: old\n  - name: smoke-live\n    tool: smoke\n    model: live\n")},
	}
	c := New(WithRoot(t.TempDir()), WithBaselineFS(base), WithoutCloudOverlay())
	argv, ok := c.SmokeArgv("smoke")
	if !ok {
		t.Fatal("no smoke argv")
	}
	want := []string{"smoke", "--model", "live-target", smokePrompt}
	if len(argv) != len(want) {
		t.Fatalf("argv = %q, want %q", argv, want)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("argv = %q, want %q", argv, want)
		}
	}
}
