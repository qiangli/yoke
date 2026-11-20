package agentlaunch

import (
	"slices"
	"testing"
)

// Operator rule 2026-11-20: the command-line flag is the most specific
// setting, so an explicit --no-yolo (DenyUnsafe) beats containment, the
// environment override and a tool's own default.
func TestDenyUnsafeOutranksContainmentEnvAndToolDefault(t *testing.T) {
	inContainer := func() bool { return true }
	cases := []struct {
		name string
		tool string
		args []string
		env  string
		flag string
	}{
		{"container", "agy", []string{"--dangerously-skip-permissions", "--model", "m"}, "", "--dangerously-skip-permissions"},
		{"env override", "claude", []string{"--dangerously-skip-permissions"}, "1", "--dangerously-skip-permissions"},
		{"codex default", "codex", []string{"--dangerously-bypass-approvals-and-sandbox"}, "", "--dangerously-bypass-approvals-and-sandbox"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(UnsafeLaunchEnv, tc.env)
			kept, err := FinalizeArgs(tc.tool, tc.args, Options{Attended: true, Containerized: inContainer})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(kept, tc.flag) {
				t.Fatalf("without --no-yolo the %s default should keep %s: %v", tc.name, tc.flag, kept)
			}
			got, err := FinalizeArgs(tc.tool, tc.args, Options{Attended: true, DenyUnsafe: true, Containerized: inContainer})
			if err != nil {
				t.Fatal(err)
			}
			if slices.Contains(got, tc.flag) {
				t.Fatalf("--no-yolo must strip %s despite the %s: %v", tc.flag, tc.name, got)
			}
		})
	}
}

// An explicit --yolo is unchanged: it keeps the kill-switch.
func TestAllowUnsafeStillKeepsTheKillSwitch(t *testing.T) {
	got, err := FinalizeArgs("agy", []string{"--dangerously-skip-permissions"}, Options{Attended: true, AllowUnsafe: true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got, "--dangerously-skip-permissions") {
		t.Fatalf("--yolo lost the kill-switch: %v", got)
	}
}
