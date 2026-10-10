package skills

import (
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Skill CLI output pinned before the resource-kind core refactor (Sprint 406
// story e018942b); see pkg/fleet/cli_golden_test.go for the contract.
// Fixtures are in-memory (cliFixture), so the output is host-independent
// except for one token: an inapplicable skill's failing clause ends in the
// host's own os, which normalizeHostOS folds to <HOST_OS> (Sprint 9999 story
// dce759296904); see pkg/fleet normalizeGolden for the <ROOT>/<HOME> precedent.
//
//	go test ./pkg/skills -run TestSkillCLIGolden -update-golden
var updateGolden = flag.Bool("update-golden", false, "rewrite testdata/golden/*.golden from the current CLI output")

func TestSkillCLIGolden(t *testing.T) {
	f := cliFixture(t)
	for name, args := range map[string][]string{
		"list":              {"list"},
		"list-json":         {"list", "--json"},
		"list-all":          {"list", "--all"},
		"list-inapplicable": {"list", "--inapplicable"},
		"show":              {"show", "alpha-notes"},
		"show-reference":    {"show", "alpha-notes", "--reference"},
		"show-yaml":         {"show", "alpha-notes", "--yaml"},
		"show-json":         {"show", "alpha-notes", "--json"},
	} {
		got, stderr, err := f.run(args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, stderr)
		}
		got = normalizeHostOS(got)
		path := filepath.Join("testdata", "golden", "skill_"+name+".golden")
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

// normalizeHostOS replaces the one host-dependent token in skill CLI output:
// the "os=<GOOS>" the requires gate reports for the host it actually runs on.
// Only the exact current runtime.GOOS is folded, so a clause that named any
// other os would still differ from the golden and fail.
func normalizeHostOS(out string) string {
	return strings.ReplaceAll(out, ": os="+runtime.GOOS, ": os=<HOST_OS>")
}
