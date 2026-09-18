package fleet

import (
	"bytes"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/spf13/cobra"
)

// bashy ships seeds, so "empty" here means a catalog built on a bare baseline
// FS with nothing mounted above it. In that state the listing is an empty
// table under its header plus a one-line rod hint on stderr — and exit 0,
// because an empty roster is a state, not a fault.
func TestEmptyRingListsHeaderAndHintsWithoutFailing(t *testing.T) {
	for _, key := range []string{"BASHY_FLEET_DIR", "BASHY_MODELS_PATH", "BASHY_AGENTS_PATH", "BASHY_MODELS_DIR", "BASHY_AGENTS_DIR"} {
		t.Setenv(key, "")
	}
	root := t.TempDir()
	for _, tc := range []struct {
		kind   string
		cmd    *cobra.Command
		header string
	}{
		{KindModel, NewModelsCmd(WithRoot(root), WithoutCloudOverlay(), WithBaselineFS(fstest.MapFS{})), "NAME  BAND  KIND"},
		{KindAgent, NewAgentsCmd(WithRoot(root), WithoutCloudOverlay(), WithBaselineFS(fstest.MapFS{})), "NAME  NICK  BAND"},
	} {
		var out, errOut bytes.Buffer
		tc.cmd.SetOut(&out)
		tc.cmd.SetErr(&errOut)
		tc.cmd.SetArgs([]string{"list"})
		if err := tc.cmd.Execute(); err != nil {
			t.Fatalf("%s list on an empty ring: %v", tc.kind, err)
		}
		if lines := strings.Split(strings.TrimSpace(out.String()), "\n"); len(lines) != 1 || !strings.HasPrefix(lines[0], tc.header) {
			t.Errorf("%s list stdout = %q, want the bare header", tc.kind, out.String())
		}
		hint := strings.TrimSpace(errOut.String())
		if strings.Count(hint, "\n") != 0 || !strings.HasPrefix(hint, "hint:") {
			t.Fatalf("%s list stderr = %q, want one hint line", tc.kind, errOut.String())
		}
		for _, want := range []string{"bashy " + tc.kind + " add", "bashy " + tc.kind + " sync", nounPathEnv[tc.kind+"s"]} {
			if !strings.Contains(hint, want) {
				t.Errorf("%s hint %q does not mention %q", tc.kind, hint, want)
			}
		}
	}
}

// A populated ring gets no hint: the hint is for an operator with nothing,
// not noise under every listing.
func TestPopulatedRingListsWithoutTheHint(t *testing.T) {
	c, root := store(t)
	if err := c.SaveModel(Model{Name: "local-only", Band: 1}); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	cmd := NewModelsCmd(WithRoot(root), WithoutCloudOverlay())
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"list"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "local-only") || strings.Contains(errOut.String(), "hint:") {
		t.Fatalf("stdout = %q, stderr = %q", out.String(), errOut.String())
	}
}
