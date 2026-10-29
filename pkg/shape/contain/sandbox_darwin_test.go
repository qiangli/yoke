//go:build darwin

package contain

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSandboxAllowsOnlyTheResolvedRunnerUnderHome(t *testing.T) {
	clone := t.TempDir()
	home := t.TempDir()
	runner := filepath.Join(home, "installed-bashy")
	if err := os.WriteFile(runner, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), Spec{
		Dir: clone, Argv: []string{runner}, Level: LevelSandbox,
		Cap: "read", Effects: []string{"read"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("runner exit = %d", res.ExitCode)
	}

	argv, err := sandboxArgv(clone, []string{runner})
	if err != nil {
		t.Fatal(err)
	}
	profile := argv[2]
	if !containsLiteral(profile, runner) || containsSubpath(profile, home) {
		t.Fatalf("sandbox profile did not grant exactly the runner path:\n%s", profile)
	}
}

func containsLiteral(profile, path string) bool {
	return strings.Contains(profile, `(literal "`+path+`")`)
}

func containsSubpath(profile, path string) bool {
	return strings.Contains(profile, `(subpath "`+path+`")`)
}
