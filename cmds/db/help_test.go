package dbcmd

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/qiangli/coreutils/tool"
)

// TestHelpPrintsUsageToStdout pins the help contract: `db --help`, `db -h`
// and `db help` (and subcommand forms like `db sql --help`) print the
// command's usage text to stdout and exit 0 before any target resolution,
// so no database file is ever opened or created.
func TestHelpPrintsUsageToStdout(t *testing.T) {
	registered := tool.Lookup("db")
	if registered == nil {
		t.Fatal("db tool not registered")
	}
	// The op-position forms print the bare usage string; the subcommand
	// forms route through tool.Parse, which prefixes it with "Usage: ".
	wholeUsage := command.Usage + "\n"
	for _, tc := range []struct {
		args []string
		bare bool
	}{
		{args: []string{"--help"}, bare: true},
		{args: []string{"-h"}, bare: true},
		{args: []string{"help"}, bare: true},
		{args: []string{"sql", "--help"}},
		{args: []string{"sql", "-h"}},
	} {
		root := t.TempDir()
		var out, stderr bytes.Buffer
		rc := &tool.RunContext{Ctx: context.Background(), Dir: root, Env: os.Environ()}
		rc.Out = &out
		rc.Err = &stderr
		code := registered.Run(rc, tc.args)
		if code != 0 {
			t.Fatalf("db %v: exit=%d, want 0 (stderr=%q)", tc.args, code, stderr.String())
		}
		if tc.bare && out.String() != wholeUsage {
			t.Fatalf("db %v: stdout=%q, want the usage text", tc.args, out.String())
		}
		if !strings.Contains(out.String(), command.Usage) {
			t.Fatalf("db %v: stdout missing usage text:\n%s", tc.args, out.String())
		}
		if stderr.Len() != 0 {
			t.Fatalf("db %v: stderr=%q, want empty", tc.args, stderr.String())
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			names := make([]string, len(entries))
			for i, e := range entries {
				names[i] = e.Name()
			}
			t.Fatalf("db %v: help created files in %s: %v", tc.args, root, names)
		}
	}
}
