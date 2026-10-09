package principal

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
)

// Person CLI output pinned before the resource-kind core refactor (Sprint 406
// story e018942b); see pkg/fleet/cli_golden_test.go for the contract.
//
//	go test ./pkg/principal -run TestPeopleCLIGolden -update-golden
var updateGolden = flag.Bool("update-golden", false, "rewrite testdata/golden/*.golden from the current CLI output")

func TestPeopleCLIGolden(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BASHY_FLEET_DIR", root)
	t.Setenv("BASHY_PEOPLE_DIR", "")
	opts := []fleet.Option{fleet.WithRoot(root)}
	run := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		cmd := NewPeopleCmd(opts...)
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out.String())
		}
		return strings.ReplaceAll(out.String(), root, "<ROOT>")
	}
	run("add", "gt-person", "--display", "Golden Person", "--email", "gt@example.test",
		"--alias", "goldie", "--os-user", "gt-host=gtuser", "--host", "gt-host")
	for name, args := range map[string][]string{
		"list":        {"list"},
		"list-json":   {"list", "--json"},
		"show":        {"show", "gt-person"},
		"show-json":   {"show", "gt-person", "--json"},
		"schema-json": {"schema", "--json"},
	} {
		got := run(args...)
		path := filepath.Join("testdata", "golden", "person_"+name+".golden")
		if *updateGolden {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("missing golden %s (run with -update-golden): %v", path, err)
		}
		if string(want) != got {
			t.Errorf("%s changed. If intended, rerun with -update-golden and explain why in the commit.\n--- want\n%s\n--- got\n%s", path, want, got)
		}
	}
}
