package resources

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := NewCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestResourceYAMLImportExportIdentity(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BASHY_RESOURCES_DIR", root)
	if _, err := run(t, "add", "matrix-resource", "--kind", "path", "--title", "matrix", "/tmp/matrix"); err != nil {
		t.Fatal(err)
	}
	before, err := run(t, "show", "matrix-resource", "--yaml")
	if err != nil {
		t.Fatal(err)
	}
	cmd := NewCommand()
	cmd.SetIn(bytes.NewBufferString(before))
	cmd.SetArgs([]string{"add", "-f", "-"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("add -f -: %v", err)
	}
	after, err := run(t, "show", "matrix-resource", "--yaml")
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("resource YAML round-trip changed record:\n%s\n---\n%s", before, after)
	}

	path := filepath.Join(t.TempDir(), "resource.yaml")
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "add", "-f", path); err != nil {
		t.Fatalf("add -f FILE: %v", err)
	}
	if _, err := run(t, "list", "--json"); err != nil {
		t.Fatalf("list --json: %v", err)
	}
	if _, err := run(t, "verify", "matrix-resource"); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if _, err := run(t, "set", "matrix-resource", "--title", "updated"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if _, err := run(t, "rm", "matrix-resource"); err != nil {
		t.Fatalf("rm: %v", err)
	}
}

func TestSystemCommandJSON(t *testing.T) {
	out, err := run(t, "system", "--json", "--interval", "0")
	if err != nil {
		t.Fatalf("resources system --json: %v\n%s", err, out)
	}
	var sys System
	if err := json.Unmarshal([]byte(out), &sys); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if sys.SchemaVersion != SchemaVersion {
		t.Errorf("schema_version = %q, want %q", sys.SchemaVersion, SchemaVersion)
	}
	if sys.At.IsZero() {
		t.Error("envelope has no timestamp")
	}
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		if sys.Memory.TotalBytes == 0 {
			t.Errorf("memory total is zero (warnings %v)", sys.Warnings)
		}
	}
}

func TestSystemCommandTable(t *testing.T) {
	out, err := run(t, "system", "--interval", "0")
	if err != nil {
		t.Fatalf("resources system: %v\n%s", err, out)
	}
	for _, want := range []string{"SYSTEM RESOURCES", "CPU", "MEMORY", "DISK", "NETWORK", "GPU"} {
		if !strings.Contains(out, want) {
			t.Errorf("table view is missing the %s section:\n%s", want, out)
		}
	}
}

func TestSystemCommandRejectsNegativeInterval(t *testing.T) {
	if _, err := run(t, "system", "--interval", "-1s"); err == nil {
		t.Fatal("a negative sample interval must be a usage error")
	}
}

func TestBareCommandShowsHelp(t *testing.T) {
	out, err := run(t)
	if err != nil {
		t.Fatalf("bare resources: %v", err)
	}
	if !strings.Contains(out, "system") || !strings.Contains(out, "usage") {
		t.Errorf("help does not mention the resource subcommands:\n%s", out)
	}
}

func TestSystemMinFreeAlarmPrintedOnce(t *testing.T) {
	cmd := NewSystemCommand()
	var errBuf bytes.Buffer
	cmd.SetOut(&errBuf)
	cmd.SetErr(&errBuf)
	cmd.SetArgs([]string{"--interval", "0", "--min-free", "10000000G"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected failure below the floor")
	}
	// The host prints the returned error; the command itself must stay silent.
	if strings.Contains(errBuf.String(), "Error:") {
		t.Fatalf("cobra printed the error itself: %q", errBuf.String())
	}
	host := errBuf.String() + err.Error() + "\n"
	if n := strings.Count(host, "disk floor alarm:"); n != 1 {
		t.Fatalf("alarm appears %d times, want 1: %q", n, host)
	}
}
