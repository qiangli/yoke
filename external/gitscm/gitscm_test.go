package gitscm

import "testing"

func TestAppendGitWindowsEnvInteractiveLeavesPromptsAlone(t *testing.T) {
	t.Parallel()

	env := []string{"PATH=C:\\bin"}
	got := appendGitWindowsEnv(env, false)
	if len(got) != len(env) {
		t.Fatalf("interactive env should not be changed: %#v", got)
	}
	for i := range env {
		if got[i] != env[i] {
			t.Fatalf("env[%d]: want %q, got %q", i, env[i], got[i])
		}
	}
}

func TestAppendGitWindowsEnvNonInteractive(t *testing.T) {
	t.Parallel()

	got := appendGitWindowsEnv([]string{"PATH=C:\\bin"}, true)
	want := []string{"PATH=C:\\bin", "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never"}
	if len(got) != len(want) {
		t.Fatalf("wrong env length: want %d, got %d: %#v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("env[%d]: want %q, got %q", i, want[i], got[i])
		}
	}
}

func TestAppendGitWindowsEnvPreservesExplicitValues(t *testing.T) {
	t.Parallel()

	env := []string{"GIT_TERMINAL_PROMPT=1", "GCM_INTERACTIVE=always"}
	got := appendGitWindowsEnv(env, true)
	if len(got) != len(env) {
		t.Fatalf("explicit values should not be duplicated: %#v", got)
	}
	for i := range env {
		if got[i] != env[i] {
			t.Fatalf("env[%d]: want %q, got %q", i, env[i], got[i])
		}
	}
}

func TestGitPromptsNonInteractive(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		env       []string
		stdinTTY  bool
		stderrTTY bool
		want      bool
	}{
		{name: "interactive terminal", stdinTTY: true, stderrTTY: true, want: false},
		{name: "piped stdin", stdinTTY: false, stderrTTY: true, want: true},
		{name: "redirected stderr", stdinTTY: true, stderrTTY: false, want: true},
		{name: "agentic", env: []string{"BASHY_AGENTIC=1"}, stdinTTY: true, stderrTTY: true, want: true},
		{name: "explicit noninteractive", env: []string{"BASHY_GIT_NONINTERACTIVE=1"}, stdinTTY: true, stderrTTY: true, want: true},
		{name: "explicit interactive wins", env: []string{"BASHY_AGENTIC=1", "BASHY_GIT_INTERACTIVE=1"}, stdinTTY: false, stderrTTY: false, want: false},
		{name: "off values", env: []string{"BASHY_AGENTIC=0", "BASHY_GIT_NONINTERACTIVE=false"}, stdinTTY: true, stderrTTY: true, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := gitPromptsNonInteractive(tt.env, tt.stdinTTY, tt.stderrTTY); got != tt.want {
				t.Fatalf("want %v, got %v", tt.want, got)
			}
		})
	}
}
