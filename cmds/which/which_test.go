package whichcmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/qiangli/coreutils/tool"
)

func runIn(t *testing.T, dir string, env []string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errb bytes.Buffer
	rc := &tool.RunContext{
		Ctx:   context.Background(),
		Dir:   dir,
		Env:   env,
		Stdio: tool.Stdio{In: strings.NewReader(""), Out: &out, Err: &errb},
	}
	code = cmd.Run(rc, args)
	return out.String(), errb.String(), code
}

// mkExec creates an executable named base in dir and returns the file
// name actually created (base+".exe" on Windows, where executability
// comes from the extension, not mode bits).
func mkExec(t *testing.T, dir, base string) string {
	t.Helper()
	name := base
	if runtime.GOOS == "windows" {
		name = base + ".exe"
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return name
}

func pathEnv(dirs ...string) []string {
	return []string{"PATH=" + strings.Join(dirs, string(os.PathListSeparator))}
}

func TestWhichFirstMatch(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	nameA := mkExec(t, dirA, "tool1")
	mkExec(t, dirB, "tool1")

	out, errb, code := runIn(t, t.TempDir(), pathEnv(dirA, dirB), "tool1")
	want := dirA + string(filepath.Separator) + nameA + "\n"
	if code != 0 || out != want || errb != "" {
		t.Errorf("which tool1 = (%q, %q, %d), want (%q, \"\", 0)", out, errb, code, want)
	}
}

func TestWhichAll(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	nameA := mkExec(t, dirA, "tool1")
	nameB := mkExec(t, dirB, "tool1")

	out, _, code := runIn(t, t.TempDir(), pathEnv(dirA, dirB), "-a", "tool1")
	want := dirA + string(filepath.Separator) + nameA + "\n" +
		dirB + string(filepath.Separator) + nameB + "\n"
	if code != 0 || out != want {
		t.Errorf("which -a tool1 = (%q, %d), want (%q, 0)", out, code, want)
	}
}

func TestWhichNotFound(t *testing.T) {
	out, errb, code := runIn(t, t.TempDir(), pathEnv(t.TempDir()), "no-such-tool")
	if code != 1 || out != "" || errb != "" {
		t.Errorf("which no-such-tool = (%q, %q, %d), want silent exit 1", out, errb, code)
	}
	// found + missing: prints the found one, still exits 1
	dirA := t.TempDir()
	nameA := mkExec(t, dirA, "tool1")
	out, _, code = runIn(t, t.TempDir(), pathEnv(dirA), "tool1", "no-such-tool")
	want := dirA + string(filepath.Separator) + nameA + "\n"
	if code != 1 || out != want {
		t.Errorf("which tool1 no-such-tool = (%q, %d), want (%q, 1)", out, code, want)
	}
}

func TestWhichUsesRunContextEnvNotProcessEnv(t *testing.T) {
	dirA := t.TempDir()
	mkExec(t, dirA, "tool1")
	// nil env: nothing on PATH, even though the process PATH is set
	out, _, code := runIn(t, t.TempDir(), nil, "tool1")
	if code != 1 || out != "" {
		t.Errorf("which with nil env = (%q, %d), want silent exit 1", out, code)
	}
}

func TestWhichNonExecutableSkipped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits do not gate executability on windows")
	}
	dirA, dirB := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(dirA, "tool1"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	nameB := mkExec(t, dirB, "tool1")
	out, _, code := runIn(t, t.TempDir(), pathEnv(dirA, dirB), "tool1")
	want := dirB + "/" + nameB + "\n"
	if code != 0 || out != want {
		t.Errorf("which skips non-exec: (%q, %d), want (%q, 0)", out, code, want)
	}
}

func TestWhichNameWithSeparator(t *testing.T) {
	parent := t.TempDir()
	if err := os.Mkdir(filepath.Join(parent, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	name := mkExec(t, filepath.Join(parent, "sub"), "tool1")
	// no PATH search: resolved against rc.Dir, printed as given
	operand := "sub/" + "tool1"
	wantLine := "sub/" + name // .exe appended on windows
	out, _, code := runIn(t, parent, nil, operand)
	if code != 0 || out != wantLine+"\n" {
		t.Errorf("which %s = (%q, %d), want (%q, 0)", operand, out, code, wantLine+"\n")
	}
}

func TestWhichWindowsPathext(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("PATHEXT is windows-only")
	}
	dirA := t.TempDir()
	if err := os.WriteFile(filepath.Join(dirA, "tool2.bat"), []byte("@echo off\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := append(pathEnv(dirA), "PATHEXT=.COM;.EXE;.BAT;.CMD")
	out, _, code := runIn(t, t.TempDir(), env, "tool2")
	want := dirA + `\` + "tool2.BAT\n"
	if code != 0 || out != want {
		t.Errorf("which tool2 = (%q, %d), want (%q, 0)", out, code, want)
	}
	// explicit extension matches the bare name
	out, _, code = runIn(t, t.TempDir(), env, "tool2.bat")
	want = dirA + `\` + "tool2.bat\n"
	if code != 0 || out != want {
		t.Errorf("which tool2.bat = (%q, %d), want (%q, 0)", out, code, want)
	}
}

func TestWhichErrors(t *testing.T) {
	_, errb, code := runIn(t, t.TempDir(), nil)
	if code != 2 || !strings.Contains(errb, "missing operand") {
		t.Errorf("no args: code=%d err=%q", code, errb)
	}
	_, errb, code = runIn(t, t.TempDir(), nil, "--frobnicate", "x")
	if code != 2 || !strings.Contains(errb, "frobnicate") || !strings.Contains(errb, "pure-Go") {
		t.Errorf("unknown flag: code=%d err=%q", code, errb)
	}
}

func TestWhichHelpAndVersion(t *testing.T) {
	out, _, code := runIn(t, t.TempDir(), nil, "--help")
	if code != 0 || !strings.Contains(out, "Usage: which") {
		t.Errorf("--help: code=%d out=%q", code, out)
	}
	out, _, code = runIn(t, t.TempDir(), nil, "--version")
	if code != 0 || !strings.Contains(out, "which") {
		t.Errorf("--version: code=%d out=%q", code, out)
	}
}
