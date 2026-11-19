package resources

import (
	"bytes"
	"testing"
)

func TestResourceQueueUsesClaimLedger(t *testing.T) {
	t.Setenv("BASHY_COORD_DIR", t.TempDir())
	cmd := NewCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"queue", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if out.String() != "[]\n" {
		t.Fatalf("queue = %q", out.String())
	}
}
