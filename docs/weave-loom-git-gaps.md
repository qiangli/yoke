# Weave/Loom git features in `coreutils/git`

Status: **implemented** (this document reflects what shipped, not a proposal).

## Why

ycode's weave/loom subsystem shelled out to the system `git` binary for
every git operation (19 `exec.Command("git", …)` sites in
`cmd/ycode/weave_impl.go`) — a hard external dependency in an otherwise
single-static-binary, pure-Go project, and one that fails with a
misleading "not in a git repo" when git is absent. `coreutils/git`
(go-git/v5 wrapper, two-tier typed + `Exec(argv)` API) already backed
`outpost git` and ycode's toolexec native-git tier, but lacked the
specific operations weave/loom needed. This work closed those gaps so the
weave/loom git path can run with no system git.

Hard rules honored (`coreutils/CLAUDE.md`): go-git/v5 based, never shell
out, never port GPL GNU source, unsupported flags fail loudly, pure-Go
file transport preserved (`InstalledFileTransportIsPureGo` stays green),
two-tier API with `ErrUnsupported` fallback.

## Gap closure — what weave/loom needs vs. what now exists

| Op weave/loom needs | Before | Now |
|---|---|---|
| `merge --no-ff` of **diverged** branches + conflict detect + abort | `Merge` ff-only; diverged → hard error | **real atomic 3-way merge** (`merge3.go`) |
| `clone --local --no-hardlinks --branch` | `--branch` ok; local flags → `ErrUnsupported` | flags accepted (effective no-ops); local-path clone |
| `fetch --no-tags <local-path> <src:dst>` | remote-name only | local-path/URL + refspecs + `--no-tags` |
| `merge-base --is-ancestor` (exit 0/1) | not supported | `IsAncestor` typed + `--is-ancestor` exit-code |
| `checkout -B` | `-b` only | `-B` (create-or-reset) + `CheckoutOptions.Force` |
| `diff --cached --quiet` (staged predicate) | `ErrUnsupported` | exit-code predicate (staged & unstaged) |
| `git remote remove <name>` | only `get-url` | `RemoteRemove` typed + `remote remove`/`rm` |
| `branch -d` vs `-D` | unconditional delete | `-d` refuses unmerged; `-D` forces (`BranchOptions.Force`) |
| `rev-parse --show-toplevel` | Exec only | `RepoRoot(path)` typed helper |
| `reset --hard`, `status --untracked-files=all`, `rev-list --count`, `add -A`, `commit -m`, `config user.*` | already supported | unchanged |

Deferred (no current consumer): loom's conflict-leaving `rebase` onto an
upstream and the paired `diff --name-only --diff-filter=U` (Phase 3, gated
behind real demand); `clone --reference` alternates (go-git has no
alternates support — fails loudly rather than faking it).

## The 3-way merge (`merge3.go`)

### Why it was mandatory

weave clones each worker sandbox from base HEAD and `weave pull` merges
the branches back **sequentially**. Merge #1 fast-forwards, but after it
main has advanced, so merge #2's branch (cut from the *old* base) has
genuinely diverged → a real 3-way merge. The old ff-only `Merge` errored
out here, so multi-issue pull was impossible on the pure-Go path.

### Design

`threeWayMerge(r, w, ours, theirs, msg, sig)` operates on go-git
primitives, reusing `treeChanges` / `writeBlobToWorktree` from `merge.go`:

1. Require a clean worktree (a merge touches many paths).
2. `base = merge-base(ours, theirs)`.
3. Compute net per-path changes `base→ours` and `base→theirs`
   (`collectChanges`, keyed by path; renames are delete+insert).
4. Classify each path touched by theirs:
   - theirs-only → take theirs;
   - both deleted, or both produced the identical blob → no-op (ours
     already has it);
   - both changed differently → attempt a line-level **diff3**
     (`diff3Merge`); clean → take the merged blob, else **conflict**.
5. **Atomicity:** all conflicts are collected and, if any, a
   `*ConflictError` is returned **before any mutation**. This collapses
   `git merge` + `git merge --abort` into one all-or-nothing op — no
   separate abort step, no half-merged state on conflict.
6. On a clean merge: write theirs-only / diff3-merged blobs to
   worktree+index, then record a merge commit with parents `[ours, theirs]`.

### diff3 conflict rules (`diff3Merge`, `diffHunks`, `rangesOverlap`)

Each side is diffed against base into non-overlapping line hunks (LCS via
`matchMap`); the two hunk lists are merged in base order. Non-overlapping
edits from both sides merge; an overlapping region conflicts unless the
edits are identical.

Two subtleties learned from adversarial review (regression-tested):

- **Insertions are zero-width**, so plain interval intersection misses an
  insertion landing adjacent to the other side's delete/modify — which
  would silently resurrect a deleted line. `rangesOverlap` therefore
  treats an insertion at point `p` as conflicting with any hunk whose
  range `[s,e]` satisfies `s ≤ p ≤ e` (inclusive). This is **deliberately
  conservative**: it may flag a conflict where git's recursive strategy
  would auto-merge an insertion immediately abutting a change, but it
  never produces a silent wrong merge.
- **Criss-cross histories** (multiple merge bases) are **refused** rather
  than guessing a base (which could silently mis-merge). git would
  synthesize a virtual base via recursive merge; the pure-Go path returns
  a clear error instead. weave's linear sequential merges don't create
  this shape.

### Known limitations (documented, not bugs)

- Conservative insertion-adjacency conflicts (above) — safe over-conflict.
- Criss-cross refused (above).
- Binary files (NUL byte) with edits on both sides always conflict.
- A both-sides content merge keeps ours' file mode; a theirs-*only* mode
  change (e.g. exec bit) is preserved (explicit `chmod`, since `O_TRUNC`
  reuses an existing file's perms).

## Conflict contract for callers

`Merge` returns `*git.ConflictError{ Files []string }` on an
unreconcilable merge, with the repo untouched. Callers distinguish it via
`errors.As`. weave maps it to issue state `conflict`; any other error
(unresolved ref, dirty tree, missing identity, criss-cross) is a generic
failure with the repo also untouched (identity is checked up front).

## Tests

All in `coreutils/git/`, hermetic (no network, no system git):

- `merge3_test.go` — `diff3Merge` table (disjoint hunks, identical edits,
  overlap/delete-modify conflicts, insertion-adjacency regressions,
  no-trailing-newline); full `Merge` integration: diverged-clean
  same-file hunks, atomic conflict (HEAD/worktree/index untouched),
  theirs-only mode preservation, criss-cross refusal, and
  **`TestMerge_NoSystemGit`** (a full diverged merge with `PATH` emptied —
  proves the dependency is gone).
- `gaps_test.go` — `IsAncestor` + `--is-ancestor` exit codes, local-path
  clone, local-path+refspec `--no-tags` fetch, `checkout -B`,
  `diff --cached --quiet`, `status --untracked-files=all`, `branch -d`/`-D`,
  `RemoteRemove`, `RepoRoot`.
- `verbs_test.go` — `TestMergeDiverged` updated to assert the clean 3-way
  merge (both files present, merge commit has two parents).

Run: `cd coreutils && go test ./git/` (or `./...`).

## Downstream migration (ycode, separate change)

Swapping `cmd/ycode/weave_impl.go`'s `exec.Command("git", …)` to
`coreutils/git` is a follow-up in the ycode repo. ycode already depends on
coreutils (`replace github.com/qiangli/coreutils => ../coreutils`).
Recommended phased order, lowest-risk first:

1. **Read-only ops** — `rev-parse`/`RepoRoot`, `RevListCount`,
   `IsAncestor`, `Status` (drop the porcelain string-parse in
   `weaveMeasureDirtiness` for the typed `[]StatusEntry`). Fix the
   misleading `weaveRepoRoot` error.
2. **Sandbox allocation** — `Clone` (local path), `Checkout` `-b`,
   `ConfigSet` user.*, `RemoteRemove` origin. (Reflog scrub at
   `weave_impl.go:1527` is a filesystem op, stays as-is.)
3. **Fetch + branch delete** — local-path+refspec `Fetch`; `Branch` delete
   preserving `-d` vs `-D`.
4. **Merge + reset (highest risk, last)** — rewrite the pull loop
   (`weave_impl.go:~2296`) to call `Merge` and branch on `*ConflictError`
   (no more `merge --abort`); `Reset` for the suite-gate rollback.

After migration the weave happy path needs **no system git**; a preflight
becomes unnecessary (the point is that ycode no longer assumes a host
git). Validate with `cmd/ycode/weave_e2e_test.go`, then do the umbrella
pin-bump (commit+push coreutils → bump ycode `go.mod` → bump umbrella
pins; never commit submodule edits from the umbrella root).
