package fleet

import "testing"

// A harness we recognize but never drive is skipped, not failed. A healthy
// host must not report an error for a tool it never intended to launch.
func TestDetectionOnlyToolsAreSkippedNotFailed(t *testing.T) {
	c, _ := store(t)
	for _, name := range []string{"goose", "cline", "gemini"} {
		chk := c.VerifyTool(name, Probes(nil))
		if !chk.Skipped {
			t.Errorf("%s: Skipped = false; a detection-only harness is not a failure (%+v)", name, chk)
		}
		if chk.OK {
			t.Errorf("%s: OK = true; it has no launch template", name)
		}
	}
}

// A function kit that shares the tool namespace is skipped too.
func TestNonCLIToolKindIsSkipped(t *testing.T) {
	c, _ := store(t)
	if err := c.SaveTool(Tool{Name: "aikit", Kind: ToolKindFunc}); err != nil {
		t.Fatal(err)
	}
	chk := c.VerifyTool("aikit", Probes(nil))
	if !chk.Skipped || chk.OK {
		t.Fatalf("check = %+v, want skipped", chk)
	}
}

// An installed, drivable tool is a candidate and must not be skipped.
func TestDrivableToolIsACandidate(t *testing.T) {
	c, _ := store(t)
	// `sh` is on every host this test runs on; the point is the shape of the
	// verdict, not which binary it names.
	if err := c.SaveTool(Tool{
		Name: "shellish", Kind: ToolKindCLI,
		CLI: ToolCLI{Binary: "sh", Launch: ToolLaunch{Exec: "sh --model {model} {prompt}"}},
	}); err != nil {
		t.Fatal(err)
	}
	chk := c.VerifyTool("shellish", Probes(nil))
	if chk.Skipped {
		t.Fatalf("a tool with a launch template is a candidate: %+v", chk)
	}
	if !chk.OK {
		t.Fatalf("sh should be drivable here: %+v", chk)
	}
}
