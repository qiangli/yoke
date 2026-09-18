// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package gate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// requirePOSIXShell skips a test that executes gate checks through /bin/sh when
// no such shell exists (Windows). Gate.Run defaults to /bin/sh and the checks
// below are POSIX shell scripts (true/false/echo…>&2); on a host without a
// POSIX shell there is nothing to execute them, so the exec-path assertions do
// not apply. The resolution/precedence tests, which do not exec, still run.
func requirePOSIXShell(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("gate execution requires a POSIX /bin/sh (absent on this platform)")
	}
}

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A project with NO gate is an error, never a pass. This is the whole point: a
// project that has not said what passing means has not passed. Returning success
// here is how a green check mark comes to mean nothing — which this project
// already lived through, with a CI conformance gate that reported green for ten
// merges without running.
func TestNoGateIsAnError(t *testing.T) {
	_, err := Resolve(t.TempDir(), "")
	if err == nil {
		t.Fatal("a project with no gate resolved successfully — it must be an error, not a pass")
	}
	if !errors.Is(err, ErrNoGate) {
		t.Fatalf("wrong error: %v", err)
	}
	// The message must tell you how to fix it, not just that you are wrong.
	// Compare slash-normalized: the message embeds an OS-native path (backslashes
	// on Windows), while DefinitionFile is written forward-slash.
	if !strings.Contains(filepath.ToSlash(err.Error()), DefinitionFile) {
		t.Fatalf("the error does not say how to define a gate: %v", err)
	}
}

// Every project already using weave must keep working with NO migration. The
// point of unifying is to stop breaking people, not to start.
func TestLegacyWeaveGateStillWorks(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, LegacyWeaveFile, "exit 0\n")

	def, err := Resolve(dir, "")
	if err != nil {
		t.Fatalf("a project with weave's existing suite-gate stopped working: %v", err)
	}
	if def.Source != LegacyWeaveFile {
		t.Fatalf("source = %q, want the legacy weave file", def.Source)
	}
}

// The new definition WINS over the legacy one, so a project can migrate by adding
// a file rather than by deleting one.
func TestNewDefinitionTakesPrecedence(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, LegacyWeaveFile, "exit 1\n") // the old, failing gate
	write(t, dir, DefinitionFile, "exit 0\n")  // the new one

	def, err := Resolve(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if def.Source != DefinitionFile {
		t.Fatalf("source = %q, want the new definition to win", def.Source)
	}
}

// A gate STOPS at the first failure. It is a decision, not a test report: the
// decision is made the moment one check fails, and running the rest wastes time an
// agent is waiting on while burying the one line that matters.
func TestStopsAtFirstFailure(t *testing.T) {
	requirePOSIXShell(t)
	dir := t.TempDir()
	write(t, dir, DefinitionFile, "true\nfalse\ntouch SHOULD_NOT_EXIST\n")

	def, err := Resolve(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), def, "/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	if res.Passed {
		t.Fatal("gate passed with a failing check")
	}
	if len(res.Checks) != 2 {
		t.Fatalf("ran %d checks, want 2 (it must stop at the first failure)", len(res.Checks))
	}
	if _, err := os.Stat(filepath.Join(dir, "SHOULD_NOT_EXIST")); err == nil {
		t.Fatal("the third check ran after a failure — the gate did not stop")
	}
}

// A failing check must carry its output, or an agent is told "it failed" with no
// way to know why and has to re-run the whole thing to find out.
func TestFailureCarriesOutput(t *testing.T) {
	requirePOSIXShell(t)
	dir := t.TempDir()
	write(t, dir, DefinitionFile, "echo the-real-reason >&2; exit 7\n")

	def, _ := Resolve(dir, "")
	res, err := Run(context.Background(), def, "/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	if res.Passed {
		t.Fatal("gate passed on exit 7")
	}
	c := res.Checks[0]
	if c.Exit != 7 {
		t.Fatalf("exit = %d, want 7 (the real code, not a flattened 1)", c.Exit)
	}
	if !strings.Contains(c.Output, "the-real-reason") {
		t.Fatalf("the failure did not carry its output: %q", c.Output)
	}
}

// A passing gate records that output existed without carrying the chatter.
func TestPassRecordsEvidenceWithoutCarryingIt(t *testing.T) {
	requirePOSIXShell(t)
	dir := t.TempDir()
	write(t, dir, DefinitionFile, "echo lots and lots of chatter\n")

	def, _ := Resolve(dir, "")
	res, _ := Run(context.Background(), def, "/bin/sh")
	if !res.Passed {
		t.Fatal("gate failed on a passing command")
	}
	if res.Checks[0].Output != "" {
		t.Fatalf("a passing check carried output: %q", res.Checks[0].Output)
	}
	if res.Checks[0].OutputBytes == 0 {
		t.Fatal("a passing check forgot that it produced evidence")
	}
}

func TestSilentZeroExitAbstains(t *testing.T) {
	requirePOSIXShell(t)
	dir := t.TempDir()
	write(t, dir, DefinitionFile, "exit 0\n")

	def, _ := Resolve(dir, "")
	res, err := Run(context.Background(), def, "/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	if res.Passed || res.Verdict != VerdictAbstained {
		t.Fatalf("silent exit 0 = passed %t, verdict %q; want abstention", res.Passed, res.Verdict)
	}
	if res.Checks[0].OutputBytes != 0 {
		t.Fatalf("silent check reported %d output bytes", res.Checks[0].OutputBytes)
	}
}

// This is the mechanism's own mutation test. The vacuous gate stays green when
// the structure-preserving marker mutation is present and therefore abstains.
// The real gate sees the same mutation, goes red, is restored, and only then
// earns green.
func TestMutationProbeRejectsVacuousGateAndProvesRealGate(t *testing.T) {
	requirePOSIXShell(t)
	for _, tc := range []struct {
		name    string
		command string
		proved  bool
	}{
		{name: "vacuous", command: "echo always-green", proved: false},
		{name: "real", command: "if test -f MUTATED; then echo mutation >&2; exit 9; else echo healthy; fi", proved: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, DefinitionFile, tc.command+"\n")
			def, err := Resolve(dir, "")
			if err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(dir, "MUTATED")
			def.Probe = &MutationProbe{
				Name: "marker appears",
				Mutate: func() (func() error, error) {
					if err := os.WriteFile(marker, []byte("mutation"), 0o644); err != nil {
						return nil, err
					}
					return func() error { return os.Remove(marker) }, nil
				},
			}

			res, err := Run(context.Background(), def, "/bin/sh")
			if err != nil {
				t.Fatal(err)
			}
			if res.Probe.Proved != tc.proved {
				t.Fatalf("probe proved = %t, want %t: %+v", res.Probe.Proved, tc.proved, res.Probe)
			}
			if res.Passed != tc.proved {
				t.Fatalf("gate passed = %t, want %t: %+v", res.Passed, tc.proved, res)
			}
			if !tc.proved && res.Verdict != VerdictAbstained {
				t.Fatalf("vacuous gate verdict = %q, want abstained", res.Verdict)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("mutation was not restored: %v", err)
			}
		})
	}
}

func TestOverrideAndEnvPrecedence(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, DefinitionFile, "exit 1\n")

	def, err := Resolve(dir, "exit 0")
	if err != nil {
		t.Fatal(err)
	}
	if def.Source != "--command" {
		t.Fatalf("an explicit --command must win: source = %q", def.Source)
	}

	t.Setenv("BASHY_GATE", "exit 0")
	def, err = Resolve(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if def.Source != "BASHY_GATE" {
		t.Fatalf("BASHY_GATE must beat the file: source = %q", def.Source)
	}
}
