package contain

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func runCat(t *testing.T, level Level, dir, file, cap string) (Result, string, error) {
	t.Helper()
	out, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	r, runErr := Run(context.Background(), Spec{
		Dir: dir, Argv: []string{"cat", file}, Level: level, Cap: cap,
		Stdout: out, Stderr: out,
	})
	b, _ := os.ReadFile(out.Name())
	return r, string(b), runErr
}

func TestDenialTable(t *testing.T) {
	t.Setenv("SHAPE_TEST_SECRET", "hunter2")
	clone := t.TempDir()
	outside := t.TempDir()
	secretFile := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secretFile, []byte("outside-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(clone, "in.txt")
	if err := os.WriteFile(inside, []byte("inside-ok"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("env scrubbed", func(t *testing.T) {
		env := strings.Join(Scrub(clone, nil, nil), "\n")
		if strings.Contains(env, "hunter2") || strings.Contains(env, "SHAPE_TEST_SECRET") {
			t.Fatalf("secret leaked into env: %s", env)
		}
		if !strings.Contains(strings.Join(Scrub(clone, []string{"SHAPE_TEST_SECRET"}, nil), "\n"), "SHAPE_TEST_SECRET=hunter2") {
			t.Fatal("explicit allowlist must pass the variable")
		}
	})

	t.Run("effect cap refusal", func(t *testing.T) {
		_, _, err := runCat(t, LevelProcess, clone, inside, "net")
		var ee *EffectError
		if !errors.As(err, &ee) {
			t.Fatalf("want EffectError, got %v", err)
		}
	})

	t.Run("declared effects stand in for the atlas lookup", func(t *testing.T) {
		// A host-installed interpreter has no atlas row, and an absent
		// declaration denies as unknown. Declaring effects says what is
		// about to run; it does not exempt it from the cap.
		out, err := os.CreateTemp(t.TempDir(), "out")
		if err != nil {
			t.Fatal(err)
		}
		defer out.Close()
		run := func(cap string, effects []string) (Result, error) {
			return Run(context.Background(), Spec{
				Dir: clone, Argv: []string{"cat", inside}, Level: LevelProcess,
				Cap: cap, Effects: effects, Stdout: out, Stderr: out,
			})
		}
		if _, err := run("read", []string{"net"}); err == nil {
			t.Fatal("declared effects outside the cap were allowed")
		}
		r, err := run("read,net", []string{"net"})
		if err != nil || r.ExitCode != 0 {
			t.Fatalf("r=%+v err=%v", r, err)
		}
	})

	t.Run("unsupported level is typed refusal", func(t *testing.T) {
		_, _, err := runCat(t, Level("vm"), clone, inside, "read")
		var ue *UnavailableError
		if !errors.As(err, &ue) || !errors.Is(err, ErrUnavailable) {
			t.Fatalf("want UnavailableError, got %v", err)
		}
	})

	t.Run("process level cannot stand in for sandbox", func(t *testing.T) {
		err := RequireSandbox(LevelProcess)
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("want ErrUnavailable, got %v", err)
		}
	})

	t.Run("process level runs with scrubbed env", func(t *testing.T) {
		r, out, err := runCat(t, LevelProcess, clone, inside, "read")
		if err != nil || r.ExitCode != 0 || out != "inside-ok" {
			t.Fatalf("r=%+v out=%q err=%v", r, out, err)
		}
	})

	t.Run("sandbox confines reads to the clone", func(t *testing.T) {
		if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
			_, _, err := runCat(t, LevelSandbox, clone, inside, "read")
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("want ErrUnavailable, got %v", err)
			}
			return
		}
		r, out, err := runCat(t, LevelSandbox, clone, inside, "read")
		if errors.Is(err, ErrUnavailable) {
			t.Skipf("sandbox backend absent: %v", err)
		}
		if err != nil || r.ExitCode != 0 || out != "inside-ok" {
			t.Fatalf("inside read: r=%+v out=%q err=%v", r, out, err)
		}
		r, out, err = runCat(t, LevelSandbox, clone, secretFile, "read")
		if err != nil {
			t.Fatal(err)
		}
		if r.ExitCode == 0 || bytes.Contains([]byte(out), []byte("outside-secret")) {
			t.Fatalf("outside file readable: r=%+v out=%q", r, out)
		}
	})
}
