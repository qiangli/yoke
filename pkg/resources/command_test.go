package resources

import (
	"bytes"
	"encoding/json"
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
