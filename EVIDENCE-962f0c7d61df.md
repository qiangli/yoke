# Story 962f0c7d61df (#2369) — lineage-aware story holder check

Red (before fix): `go test -run TestStoryClaimHolderCloneLineage ./pkg/weave`
  FAIL: holder's verified clone refused: "story ... is held by parent — ... --force to take it"
  (foreign agent and lookalike `parent-w9` already refused.)

Fix: `storyHolderCloneOf` in pkg/weave/weave_story_claim.go — claimant must be a fleet
catalog record with Ephemeral && ClonedFrom == holder. Name is never parsed.

Green (after fix): `go test -run TestStoryClaim ./pkg/weave` -> ok (42s)
Cases: verified clone accepted; foreign agent refused; unrelated `parent-w9` (name lookalike, not catalog-cloned) refused.
Candidate SHA: see git log (commit carrying Story-ID 962f0c7d61df).
