package weave

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWeaveTestMainIsolatesInheritedStores(t *testing.T) {
	if os.Getenv("WEAVE_TEST_MAIN_PROBE") == "1" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"BASHY_HOME", "BASHY_SPRINT_DIR", "BASHY_ROOM_DIR", "BASHY_MB_DIR", "BASHY_KB_DIR", "BASHY_SKILLS_DIR"} {
			value := os.Getenv(key)
			if value == "" {
				continue
			}
			rel, err := filepath.Rel(home, value)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				t.Errorf("%s escaped private HOME", key)
			}
		}
		return
	}
	// Poison with a disposable directory, never the operator's actual state.
	inherited := t.TempDir()
	c := exec.Command(os.Args[0], "-test.run=^TestWeaveTestMainIsolatesInheritedStores$")
	c.Env = append(os.Environ(), "WEAVE_TEST_MAIN_PROBE=1")
	for _, key := range []string{"BASHY_HOME", "BASHY_SPRINT_DIR", "BASHY_ROOM_DIR", "BASHY_MB_DIR", "BASHY_KB_DIR", "BASHY_SKILLS_DIR"} {
		c.Env = append(c.Env, key+"="+inherited)
	}
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("TestMain leaked inherited stores: %v\n%s", err, out)
	}
}
