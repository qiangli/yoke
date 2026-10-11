// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package coord

import (
	"path/filepath"
	"testing"
	"time"
)

// newProcess clears the per-process episode export, the way a harness that
// starts every tool call in a FRESH shell does (Claude Code runs each Bash call
// in a new process, so nothing Self exported survives to the next call).
func newProcess(t *testing.T) {
	t.Helper()
	t.Setenv(EpisodeEnv, "")
	t.Setenv("WEAVE_EPISODE", "")
}

func TestSelfDerivesStableEpisodeFromHarnessSession(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "f35f1737-f3d0-4ece-8f16-a6bd57404beb")

	newProcess(t)
	first := Self().Episode
	newProcess(t)
	second := Self().Episode

	if first == "" || first != second {
		t.Fatalf("two processes of one harness session got episodes %q and %q; want one stable episode", first, second)
	}

	t.Setenv("CLAUDE_CODE_SESSION_ID", "0b6b0b3c-0000-4000-8000-000000000001")
	newProcess(t)
	if other := Self().Episode; other == first {
		t.Fatalf("a different harness session reused episode %q", other)
	}
}

func TestSelfExplicitEpisodeWinsOverHarnessSession(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "f35f1737-f3d0-4ece-8f16-a6bd57404beb")
	t.Setenv(EpisodeEnv, "ep-explicit000001")
	if got := Self().Episode; got != "ep-explicit000001" {
		t.Fatalf("Episode = %q, want the explicit BASHY_EPISODE", got)
	}
}

// The reported bug (Sprint 9999 todo d83b030ce9da): a `git commit` run through
// bashy took the project claim, the next `git push` from the same agent session
// was refused as if another agent held the repo, and `claim release` dropped
// nothing — because every process minted a new episode.
func TestCommitClaimDoesNotBlockSameSessionsNextPushAndReleaseDropsIt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BASHY_COORD_DIR", dir)
	t.Setenv("BASHY_CLAIM_FORCE", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "f35f1737-f3d0-4ece-8f16-a6bd57404beb")
	roots := []string{filepath.Join(dir, "project")}

	newProcess(t) // `git commit`
	if err := Enforce(roots, "git commit -q -F -"); err != nil {
		t.Fatalf("first write: %v", err)
	}

	newProcess(t) // `git push`, a separate process of the same session
	if err := Enforce(roots, "git push origin dev"); err != nil {
		t.Fatalf("the same session's push was refused by its own commit claim: %v", err)
	}

	newProcess(t) // `bashy claim release`
	if err := Release(dir, Self()); err != nil {
		t.Fatalf("release: %v", err)
	}
	live, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range live {
		if c.Live(time.Now()) {
			t.Fatalf("release left a live claim: holder=%+v intent=%q", c.Holder, c.Intent)
		}
	}
}

func TestDifferentHarnessSessionIsStillRefused(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BASHY_COORD_DIR", dir)
	t.Setenv("BASHY_CLAIM_FORCE", "")
	roots := []string{filepath.Join(dir, "project")}

	t.Setenv("CLAUDE_CODE_SESSION_ID", "f35f1737-f3d0-4ece-8f16-a6bd57404beb")
	newProcess(t)
	if err := Enforce(roots, "git commit"); err != nil {
		t.Fatalf("first session: %v", err)
	}

	t.Setenv("CLAUDE_CODE_SESSION_ID", "0b6b0b3c-0000-4000-8000-000000000001")
	newProcess(t)
	if err := Enforce(roots, "git push"); err == nil {
		t.Fatal("a different live session was allowed to write a claimed project")
	}
}
