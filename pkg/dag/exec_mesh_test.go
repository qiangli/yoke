// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestMeshCommandArgs(t *testing.T) {
	if n, a := meshCommandArgs("ssh", "host-a", ""); n != "ssh" || !reflect.DeepEqual(a, []string{"host-a", "bash", "-s"}) {
		t.Fatalf("simple = %q %v", n, a)
	}
	if n, a := meshCommandArgs("ssh -p 2222 -i key", "big", ""); n != "ssh" || !reflect.DeepEqual(a, []string{"-p", "2222", "-i", "key", "big", "bash", "-s"}) {
		t.Fatalf("flags = %q %v", n, a)
	}
	if n, a := meshCommandArgs("outpost exec", "winbox", "none"); n != "outpost" || !reflect.DeepEqual(a, []string{"exec", "winbox"}) {
		t.Fatalf("none = %q %v", n, a)
	}
	if n, a := meshCommandArgs("ssh", "host-a", "pwsh -NoProfile -Command -"); n != "ssh" || !reflect.DeepEqual(a, []string{"host-a", "pwsh", "-NoProfile", "-Command", "-"}) {
		t.Fatalf("custom shell = %q %v", n, a)
	}
	if n, _ := meshCommandArgs("", "h", ""); n != "ssh" {
		t.Fatalf("empty default = %q", n)
	}
}

func TestMeshExecutorHostlessRunsLocal(t *testing.T) {
	// A target with no Host must run locally even under the mesh executor.
	dir := t.TempDir()
	out := new(bytes.Buffer)
	res := meshExecutor{Remote: "ssh"}.Execute(context.Background(),
		&Task{Name: "x", Body: "echo local-ran\n"},
		TaskIO{Dir: dir, Env: os.Environ(), Stdout: out, Stderr: new(bytes.Buffer)})
	if res.Status != StatusDone {
		t.Fatalf("status = %s (%v)", res.Status, res.Err)
	}
	if out.String() != "local-ran\n" {
		t.Errorf("hostless body did not run locally; out=%q", out.String())
	}
}

func TestMeshExecutorDispatchesToRemote(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake remote is a #!/bin/sh script — unix-only test harness")
	}
	// Fake "remote" transport that runs locally: it drops the host arg and execs
	// the rest (`bash -s`), so the body — fed on stdin — runs here. This exercises
	// the full dispatch path (argv build + stdin body + capture) hermetically.
	dir := t.TempDir()
	fake := filepath.Join(dir, "fakeremote")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nshift\nexec \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := new(bytes.Buffer)
	res := meshExecutor{Remote: fake}.Execute(context.Background(),
		&Task{Name: "remote", Host: "bigbox", Body: "echo ran-via-mesh on $(echo here)\n"},
		TaskIO{Dir: dir, Env: os.Environ(), Stdout: out, Stderr: new(bytes.Buffer)})
	if res.Status != StatusDone {
		t.Fatalf("status = %s (%v)", res.Status, res.Err)
	}
	if res.Host != "bigbox" {
		t.Errorf("host not recorded: %q", res.Host)
	}
	if !bytes.Contains(out.Bytes(), []byte("ran-via-mesh")) {
		t.Errorf("mesh body did not run; out=%q", out.String())
	}
}

func TestMeshExecutorRemoteShellNoneFeedsBodyDirectly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake remote is a shell script — unix-only test harness")
	}
	// Fake outpost transport: it expects only the host arg and consumes the body
	// from stdin itself. If dag appends "bash -s", this exits with usage failure.
	dir := t.TempDir()
	fake := filepath.Join(dir, "fakeoutpost")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n[ \"$#\" -eq 1 ] || { echo unexpected args: \"$@\" >&2; exit 64; }\nprintf 'host=%s\\n' \"$1\"\ncat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, errOut := new(bytes.Buffer), new(bytes.Buffer)
	res := meshExecutor{Remote: fake, RemoteShell: "none"}.Execute(context.Background(),
		&Task{Name: "remote", Host: "winbox", Body: "body-consumed-directly\n"},
		TaskIO{Dir: dir, Env: os.Environ(), Stdout: out, Stderr: errOut})
	if res.Status != StatusDone {
		t.Fatalf("status = %s (%v), stderr=%q", res.Status, res.Err, errOut.String())
	}
	if got := out.String(); got != "host=winbox\nbody-consumed-directly\n" {
		t.Fatalf("direct body feed output = %q", got)
	}
}
