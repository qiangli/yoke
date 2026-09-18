package fleet

import (
	"testing"
	"testing/fstest"
)

func TestResolveLaunchModelLongestCanonicalMatch(t *testing.T) {
	fsys := fstest.MapFS{
		"baseline/models/gpt-5.5.yaml":     {Data: []byte("name: gpt-5.5\nmodel: gpt-5.5\nband: 3\n")},
		"baseline/models/gpt-5.6-sol.yaml": {Data: []byte("name: gpt-5.6-sol\nmodel: gpt-5.6-sol\nband: 4\n")},
		"baseline/models/gemini.yaml":      {Data: []byte("name: gemini-3.1-pro-high\nmodel: gemini-3.1-pro-high\nband: 4\nids:\n  agy: Gemini 3.1 Pro (High)\n")},
	}
	cat := New(WithBaselineFS(fsys), WithoutLocalStore(), WithoutCloudOverlay())
	for _, tc := range []struct {
		name, tool, display, canonical string
		band                           int
	}{
		{"provider display", "agy", "Gemini 3.1 Pro (High)", "gemini-3.1-pro-high", 4},
		{"ambiguous prefix takes longest", "codex", "launch gpt-5.6-sol", "gpt-5.6-sol", 4},
		{"short exact", "codex", "gpt-5.5", "gpt-5.5", 3},
		{"unknown never guessed", "codex", "gpt-5.7-future", "gpt-5.7-future", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			band, canonical := resolveLaunchModel(cat, tc.tool, tc.display)
			if band != tc.band || canonical != tc.canonical {
				t.Fatalf("got (%d, %q), want (%d, %q)", band, canonical, tc.band, tc.canonical)
			}
		})
	}
}
