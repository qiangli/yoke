package fleet

import "testing"

func detectCat(t *testing.T) *Catalog {
	t.Helper()
	return New(WithRoot(t.TempDir()))
}

func clearMarkers(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CODEX_SANDBOX", "CODEX_THREAD_ID",
		"GEMINI_CLI", "CURSOR_AGENT", "CURSOR_TRACE_ID", "GOOSE_TERMINAL",
		"OPENCODE_CLIENT", "CLINE_ACTIVE", "AGENT", "AI_AGENT",
	} {
		t.Setenv(k, "")
	}
}

// Every marker the hardcoded table carried still identifies its harness.
func TestDetectToolCoversTheLegacyMarkers(t *testing.T) {
	legacy := map[string]string{
		"CLAUDECODE": "claude", "CLAUDE_CODE_ENTRYPOINT": "claude",
		"CODEX_SANDBOX": "codex", "CODEX_THREAD_ID": "codex",
		"GEMINI_CLI":      "gemini",
		"GOOSE_TERMINAL":  "goose",
		"OPENCODE_CLIENT": "opencode",
		"CLINE_ACTIVE":    "cline",
	}
	for marker, want := range legacy {
		t.Run(marker, func(t *testing.T) {
			clearMarkers(t)
			t.Setenv(marker, "1")
			got, ok := detectCat(t).DetectTool()
			if !ok || got != want {
				t.Fatalf("%s → %q,%v; want %q", marker, got, ok, want)
			}
		})
	}
}

// The name-valued conventions carry the name directly and need no entry.
func TestDetectToolNameValuedConventions(t *testing.T) {
	clearMarkers(t)
	t.Setenv("AGENT", "amp")
	if got, ok := detectCat(t).DetectTool(); !ok || got != "amp" {
		t.Fatalf("AGENT=amp → %q,%v", got, ok)
	}
}

func TestDetectToolUnattributed(t *testing.T) {
	clearMarkers(t)
	if got, ok := detectCat(t).DetectTool(); ok {
		t.Fatalf("no markers set, but detected %q", got)
	}
}

// Teaching bashy a new harness is a registry entry, not a code change.
func TestDetectToolPicksUpANewlyRegisteredHarness(t *testing.T) {
	clearMarkers(t)
	root := t.TempDir()
	cat := New(WithRoot(root))
	if err := cat.SaveTool(Tool{
		Name: "newthing", Kind: ToolKindCLI,
		CLI: ToolCLI{Launch: ToolLaunch{EnvMarkers: []string{"NEWTHING_ACTIVE"}}},
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEWTHING_ACTIVE", "1")
	if got, ok := New(WithRoot(root)).DetectTool(); !ok || got != "newthing" {
		t.Fatalf("got %q,%v", got, ok)
	}
}

// The package-level DetectTool caches the marker INDEX, not the verdict: the
// index comes from the registry and is stable, the environment is the question.
func TestPackageDetectToolRereadsTheEnvironment(t *testing.T) {
	clearMarkers(t)
	if _, ok := DetectTool(); ok {
		t.Fatal("clean env detected a tool")
	}
	t.Setenv("CLAUDECODE", "1")
	if got, ok := DetectTool(); !ok || got != "claude" {
		t.Fatalf("after setting a marker: %q,%v — the verdict must not be cached", got, ok)
	}
	t.Setenv("CLAUDECODE", "")
	if _, ok := DetectTool(); ok {
		t.Fatal("marker cleared, but a tool is still reported")
	}
}
