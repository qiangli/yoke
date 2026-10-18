//go:build windows

package lens

// The Windows truth gate (opt-in): every cmd and pwsh render, executed by the
// real cmd.exe and pwsh into an echo-argv helper, reproduces the argv.
//
//	set LENS_WINDOWS_TRUTH=1
//	go test ./pkg/lens -run TestWindowsTruth -v
//
// The helper is this test binary itself: with LENS_ECHO_ARGV=1 TestMain
// prints os.Args[1:] as JSON (Go builds os.Args from the raw command line
// with the C-runtime rules, exactly what a native program sees).

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
)

func TestMain(m *testing.M) {
	if os.Getenv("LENS_ECHO_ARGV") == "1" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(os.Args[1:]); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestWindowsTruth(t *testing.T) {
	if os.Getenv("LENS_WINDOWS_TRUTH") == "" {
		t.Skip("set LENS_WINDOWS_TRUTH=1 on a Windows host with cmd.exe and pwsh")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "LENS_ECHO_ARGV=1")
	echoed := func(t *testing.T, c *exec.Cmd) []string {
		t.Helper()
		c.Env = env
		var out, errb bytes.Buffer
		c.Stdout, c.Stderr = &out, &errb
		if err := c.Run(); err != nil {
			t.Fatalf("%v: %v\nstderr: %s\nstdout: %s", c.Args, err, errb.String(), out.String())
		}
		var got []string
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("helper output %q: %v", out.String(), err)
		}
		return got
	}

	t.Run("cmd", func(t *testing.T) {
		args := hostileFor("cmd")
		for _, batch := range [][]string{args, {""}, {"%PATH%", "%CD%", "!PATH!"}} {
			argv := append([]string{exe}, batch...)
			line, err := renderCmd(argv)
			if err != nil {
				t.Fatal(err)
			}
			c := exec.Command("cmd.exe")
			c.SysProcAttr = &syscall.SysProcAttr{CmdLine: cmdLineFor(string(line))}
			if got := echoed(t, c); !slices.Equal(got, batch) {
				t.Errorf("cmd.exe\n got %q\nwant %q\nline %s", got, batch, line)
			}
		}
	})

	for _, shell := range []string{"pwsh"} {
		t.Run(shell, func(t *testing.T) {
			path, err := exec.LookPath(shell)
			if err != nil {
				t.Fatal("pwsh is required for the gate:", err)
			}
			args := hostileFor("pwsh")
			argv := append([]string{exe}, args...)
			script := filepath.Join(t.TempDir(), "line.ps1")
			if err := os.WriteFile(script, []byte("\xef\xbb\xbf"+renderPwsh(argv)+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			c := exec.Command(path, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script)
			if got := echoed(t, c); !slices.Equal(got, args) {
				t.Errorf("%s -File\n got %q\nwant %q", shell, got, args)
			}
		})
	}
}
