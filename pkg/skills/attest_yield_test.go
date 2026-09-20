package skills

import (
	"testing"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

// AttestYield is spelled as a literal in run.go so the package stays an import
// leaf, but its VALUE is the agentic-yield exit code contract owned by
// weavecli. This test pins the two together: if weavecli.ExitInputRequired
// ever moves, a yield would silently start reading back as pass or fail, so
// the drift is caught here rather than in the ledger.
func TestAttestYieldMatchesInputRequiredExitCode(t *testing.T) {
	if AttestYield != weavecli.ExitInputRequired {
		t.Fatalf("AttestYield = %d, want weavecli.ExitInputRequired = %d", AttestYield, weavecli.ExitInputRequired)
	}
}
