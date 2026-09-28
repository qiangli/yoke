package broker

// Sprint: #322; Story: #1121; Story-ID: a90938cf0338
//
// An identity names the CLI version that actually serves the binding. cligw
// launches a tool's declared binary (cli.binary), so the version probe must run
// that same executable — not whatever `<tool name>` resolves to on PATH. A
// benchmark that pins a CLI copy through `bashy tool set --binary` otherwise
// records the PATH version while the pinned copy answers every call.

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestToolVersionProbesTheDeclaredBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stand-in for a CLI")
	}
	dir := t.TempDir()
	pinned := filepath.Join(dir, "pinned-cli")
	if err := os.WriteFile(pinned, []byte("#!/bin/sh\necho '2.1.283 (Pinned CLI)'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// PATH holds no executable named after the tool: only the declared binary
	// can answer.
	t.Setenv("PATH", t.TempDir())
	v := &toolVersions{probe: func(tool string) []string {
		if tool != "pinnedtool" {
			t.Fatalf("probe asked for %q", tool)
		}
		return []string{pinned, "--version"}
	}}
	if got := v.get("pinnedtool"); got != "2.1.283 (Pinned CLI)" {
		t.Fatalf("version = %q, want the declared binary's version", got)
	}
}

func TestToolVersionWithoutProbeFallsBackToToolName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stand-in for a CLI")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plaincli"), []byte("#!/bin/sh\necho 'plain 1.0'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if got := (&toolVersions{}).get("plaincli"); got != "plain 1.0" {
		t.Fatalf("version = %q", got)
	}
}
