package contain

import (
	"context"
	"errors"
	"os"
	"os/exec"
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

	t.Run("default level is bashy guards, no sandbox needed", func(t *testing.T) {
		// No bwrap, no sandbox-exec, no podman on PATH — only cat: the
		// default run must not care, on any OS.
		cat, err := exec.LookPath("cat")
		if err != nil {
			t.Skip("no cat")
		}
		bin := t.TempDir()
		if err := os.Symlink(cat, filepath.Join(bin, filepath.Base(cat))); err != nil {
			t.Skip("symlink:", err)
		}
		t.Setenv("PATH", bin)
		r, out, err := runCat(t, "", clone, inside, "read")
		if err != nil || r.ExitCode != 0 || r.Level != LevelProcess || out != "inside-ok" {
			t.Fatalf("r=%+v out=%q err=%v", r, out, err)
		}
	})

	t.Run("process level runs with scrubbed env", func(t *testing.T) {
		r, out, err := runCat(t, LevelProcess, clone, inside, "read")
		if err != nil || r.ExitCode != 0 || out != "inside-ok" {
			t.Fatalf("r=%+v out=%q err=%v", r, out, err)
		}
	})
}

// fakeOCI stands in for bashy oci: it records the request and runs a host
// shell that prints the argv it was asked to launch.
type fakeOCI struct {
	calls int
	dir   string
	argv  []string
	env   []string
}

func (f *fakeOCI) Argv(dir string, argv, env []string, _ Spec) ([]string, error) {
	f.calls++
	f.dir, f.argv, f.env = dir, argv, env
	return append([]string{"/bin/sh", "-c", `printf '%s\n' "$@"`, "oci"}, argv...), nil
}

func TestSandboxRoutesToOCI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake launcher is /bin/sh")
	}
	t.Setenv("SHAPE_TEST_SECRET", "hunter2")
	clone := t.TempDir()
	out, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	oci := &fakeOCI{}
	r, err := Run(context.Background(), Spec{
		Dir: clone, Argv: []string{"/opt/host/bashy", "-c", "echo hi"}, Level: LevelSandbox,
		Cap: "read", Effects: []string{"read"}, OCI: oci, EnvSet: map[string]string{"X": "1"},
		Stdout: out, Stderr: out,
	})
	if err != nil || r.ExitCode != 0 || r.Level != LevelSandbox {
		t.Fatalf("r=%+v err=%v", r, err)
	}
	if oci.calls != 1 || strings.Join(oci.argv, " ") != "bashy -c echo hi" {
		t.Fatalf("oci calls=%d argv=%q", oci.calls, oci.argv)
	}
	env := strings.Join(oci.env, "\n")
	if strings.Contains(env, "hunter2") || !strings.Contains(env, "X=1") {
		t.Fatalf("container env not the scrubbed candidate env: %s", env)
	}
	b, _ := os.ReadFile(out.Name())
	if string(b) != "bashy\n-c\necho hi\n" {
		t.Fatalf("launcher output = %q", b)
	}

	t.Run("effect cap refuses before the container starts", func(t *testing.T) {
		oci := &fakeOCI{}
		_, err := Run(context.Background(), Spec{
			Dir: clone, Argv: []string{"bashy"}, Level: LevelSandbox,
			Cap: "read", Effects: []string{"net"}, OCI: oci,
		})
		var ee *EffectError
		if !errors.As(err, &ee) || oci.calls != 0 {
			t.Fatalf("err=%v calls=%d", err, oci.calls)
		}
	})

	t.Run("process level never reaches oci", func(t *testing.T) {
		oci := &fakeOCI{}
		if _, err := Run(context.Background(), Spec{
			Dir: clone, Argv: []string{"true"}, Level: LevelProcess,
			Cap: "read", Effects: []string{"read"}, OCI: oci,
		}); err != nil || oci.calls != 0 {
			t.Fatalf("err=%v calls=%d", err, oci.calls)
		}
	})
}

func TestBashyOCIArgv(t *testing.T) {
	dir := filepath.Join(string(filepath.Separator)+"clone", "c1")
	spec := Spec{Dir: dir, MaxRSSBytes: 1 << 20, MaxProcs: 8}
	env := []string{"HOME=" + dir, "PATH=/usr/bin:/bin", "BASHY_APP_DATA=" + filepath.Join(dir, "data"), "X=1"}

	t.Setenv(SandboxImageEnv, "")
	if _, err := (BashyOCI{}).Argv(dir, []string{"bashy"}, env, spec); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("missing image: want ErrUnavailable, got %v", err)
	}

	t.Setenv(SandboxImageEnv, "example.invalid/bashy:test")
	got, err := (BashyOCI{}).Argv(dir, []string{"bashy", "-c", "true"}, env, spec)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"bashy", "oci", "run", "--rm", "-i", "--network=none",
		"-v", dir + ":/work", "-w", "/work", "--memory", "1048576", "--pids-limit", "8",
		"-e", "HOME=/work", "-e", "BASHY_APP_DATA=/work/data", "-e", "X=1",
		"example.invalid/bashy:test", "bashy", "-c", "true"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv =\n%q\nwant\n%q", got, want)
	}
}
