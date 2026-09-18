package kb

import (
	"strings"
	"testing"
)

func TestTransferCmd(t *testing.T) {
	home, cwd := writeFixtureStores(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Chdir(cwd)

	dir := t.TempDir()
	mustRun(t, dir, "add",
		"--type", "fact",
		"--title", "outpost orientation and operational rules",
		"--description", "outpost deploy upgrade binary host rules — WHEN operating a paired host",
		"--tags", "xfer:claude-memory,outpost")

	// A quoted, task-shaped topic arrives as ONE arg; transfer must tokenize
	// it through Terms() so partial matches still surface the page (the
	// retro verb's raw-args behavior is exactly what this pins against).
	out := mustRun(t, dir, "transfer", "deploy a new outpost binary to a host")
	for _, want := range []string{
		"kb transfer",
		"sources on this host",
		"claude-memory",
		"already transferred",
		"outpost-orientation-and-operational-rules",
		"xfer:<source>",
		"CANDIDATE",
		"knowledge-transfer",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("transfer output missing %q:\n%s", want, out)
		}
	}

	// No topic: still prints sources + checklist, hints at passing terms.
	out = mustRun(t, dir, "transfer")
	if !strings.Contains(out, "pass topic terms") {
		t.Errorf("topicless transfer missing hint:\n%s", out)
	}
	if !strings.Contains(out, "bashy skill show knowledge-transfer") {
		t.Errorf("transfer missing skill pointer:\n%s", out)
	}

	// Greenfield topic: related section says so rather than going silent.
	out = mustRun(t, dir, "transfer", "entirely unrelated quantum topic")
	if !strings.Contains(out, "greenfield topic") {
		t.Errorf("greenfield transfer missing marker:\n%s", out)
	}
}
