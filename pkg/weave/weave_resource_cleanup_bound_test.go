package weave

import (
	"errors"
	"testing"
)

// The byte count is only a report: a workspace too large to measure within the
// scan bound must still be reclaimed (reported with incomplete bytes), not left
// behind forever. A full umbrella clone (~79k files) hit the bound and made
// `sprint end` refuse to close (2026-09-30).
func TestWeaveResourceCleanupReclaimsPastTheScanBound(t *testing.T) {
	dir, repo, it := resourceCleanupFixture(t)
	prev := weaveArtifactScanMaxEntries
	weaveArtifactScanMaxEntries = 3
	t.Cleanup(func() { weaveArtifactScanMaxEntries = prev })

	if _, e := weaveArtifactBytes(it.Workspace); !errors.Is(e, errWeaveArtifactScanBound) {
		t.Fatalf("expected the scan-bound sentinel, got %v", e)
	}
	actions := weavePruneOwnedRun(dir, 1, repo)
	if len(actions) == 0 || !actions[0].Done {
		t.Fatalf("a workspace past the scan bound was not reclaimed: %+v", actions)
	}
	if actions[0].BytesComplete {
		t.Fatalf("bytes must be reported incomplete when the scan was bounded: %+v", actions[0])
	}
	if actions[0].Err != "" {
		t.Fatalf("a bounded measurement is not a cleanup error: %+v", actions[0])
	}
}
