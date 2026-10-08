# coreutils/git — gap tracker

The pure-Go native-git tier. The argv dispatch (`Exec` in `exec.go`) routes
**44 subcommands** (`exec.go:34-81`); an unrouted subcommand returns the
sentinel `ErrUnsupported` — callers (outpost `outpost git`, ycode toolexec,
`bashy weave`/loom) then fall back to host git or report it. The package
contract is **"unsupported flags fail loudly, naming the flag — never
approximate"**.

`gaps_test.go` covers gaps already *closed* (the consumer-needed combos:
`clone --local --no-hardlinks --branch`, `fetch --no-tags <path> <refspec>`,
`merge-base --is-ancestor`, `checkout -B`, `diff --cached --quiet`). **This doc
tracks the OPEN gaps**, prioritized by consumer-workflow impact.

> Reality check (2026-06-27): the umbrella branch-cleanup + a regression bisect
> used `git cherry`, `git worktree`, `git revert`, `git push --delete`,
> `git reset --hard`, `git stash`, `git checkout --theirs`, and
> `git diff --name-only --diff-filter=U` — **none of which this tier supports**,
> so all of it would have fallen back to host git. Those are the priorities.

## A. Unrouted subcommands (no dispatch entry → `ErrUnsupported`)

- [x] **`cherry <base> <branch>`** — patch-id equivalence / merge check. **HIGH** —
      gates branch cleanup ("is this branch's work already in master, even under a
      different SHA?"); no easy host-free substitute (patch-id over the range).
      CLOSED 2026-10-08 (sprint 252, S252.1): `nativeCherry` in `cherry.go`,
      routed in `exec.go`, proven by `TestNativeCherry_MissingThenEquivalent`
      (+ then - on replicated change) and `TestNativeCherry_SkipsMerges`
      (merges never listed, post-merge-base equivalence set — both probed
      against host git the same day). `go test -short ./...` green.
- [x] **`revert <commit>` (`--no-edit`)** — reverse-apply a commit. **HIGH** — rollback.
      CLOSED 2026-10-08 (sprint 252, S252.4): `nativeRevert` in `exec_write.go`
      (single-parent reverse patch via the shared all-or-nothing applier,
      `-n`/`--no-commit` stages without committing, conventional Revert
      message; merges, sequencer flags, multi-commit stay loud).
      Proven by `TestNativeRevert_Rollback`.
- [x] **`clean -fd` / `-n`** — remove untracked files/dirs. **MED** — workspace hygiene.
      CLOSED 2026-10-08 (sprint 252, S252.5): `clean.go` — host parity
      (no -d never recurses into untracked dirs, tracked/ignored content
      never touched, embedded repos skipped with a note, bare clean
      fatals 128, single-dash bundles). Proven by
      `TestNativeClean_DryRunAndForce`.
- [ ] `bisect`, `reflog`, `describe`, `submodule`, `gc`, `prune`, `fsck`,
      `verify-tag`, `mktag`, `pack-refs`, `index-pack`, `verify-pack` — **LOW**.

## B. Routed but stubbed (entry exists, always returns `ErrUnsupported`)

- [x] **`stash` / `stash push <file>` / `stash pop`** — **HIGH** —
      bisect/loom local-change isolation.
      CLOSED 2026-10-08 (sprint 252, S252.2): `stash.go` — snapshot
      semantics (push records tracked changes + HEAD base, restores HEAD;
      pop re-applies file-by-file with overlap check, drops on success),
      `push [-m] [path...]`, `pop [stash@{n}]`, `list`. Untracked files
      survive push (sheltered across the hard reset). Storage is a JSON
      stack under the main `.git/bashy-stash/` — host git neither sees our
      entries nor vice versa (documented limitation). `--index`/`-u` and
      drop/apply/show/branch stay loud ErrUnsupported. Proven by
      `TestNativeStash_PushPopList` + `TestNativeStash_PathspecConflictAndRef`.
- [x] **`worktree add [-f] <path> [<commit>]` / `worktree remove --force`**
      — **HIGH** — weave sandbox + bisect isolation.
      CLOSED 2026-10-08 (sprint 252, S252.2): `worktree.go` over real
      gitfile linked worktrees (host-interoperable layout; `list` and
      `--porcelain` included). Branch double-checkout refused without -f,
      dirty removal refused without --force. `openRepo` now opens `.git`
      files with go-git commondir support (plain repos unaffected). Proven
      by `TestNativeWorktree_AddRemoveList` (fresh checkout reads clean
      through go-git).
- [x] **`apply <patch>`** — MED — go-git lacks worktree patch-apply.
      CLOSED 2026-10-08 (sprint 252, S252.5): `apply.go` — real anchored
      hunks with exact context (no fuzz), new/delete/rename files,
      exec-bit flips, --check, multi-file all-or-nothing; mismatches fail
      exit 1 with the tree untouched, binaries/mode-only-beyond-exec/
      -R/--cached/--index/stdin stay loud. Proven by
      `TestNativeApply_Files`.
- [ ] **`read-tree`** (`exec_plumbing.go:215`) — LOW (plumbing).
- [ ] **`for-each-ref`** (`exec_read.go:925`) — LOW (format parsing).

## C. Supported subcommand, missing flags/options we use

| subcommand | missing flags | file:line | prio |
|---|---|---|---|
| **commit** | ~`--amend` (exists in typed `Commit`, `git.go:259` — just wire the argv), `-q`~ CLOSED 2026-10-08 (S252.3, `TestNativeCommit_Amend`) | `exec_write.go:558` | **HIGH** |
| **push** | ~`--delete` / `:<branch>` (delete remote branch)~ CLOSED 2026-10-08 (S252.1, `TestNativePush_Delete`); `-q`, `--dry-run`, `--tags` still open | `exec_write.go:18` | **HIGH** |
| **reset** | ~`--hard`, `--soft`~ CLOSED 2026-10-08 (S252.3, `TestNativeReset_HardSoft`; soft/hard with paths + bad rev fail 128 like host) | `exec_read.go:930,992` | **HIGH** |
| **checkout** | ~`--theirs`, `--ours`, `-f`~ CLOSED 2026-10-08 (S252.5, `TestNativeCheckout_Sides`: unmerged stage 2/3 resolution with index collapse, force switches) | `exec_read.go:812` | MED |
| **diff** | `--stat`, ~`--name-only`, `--diff-filter`~ CLOSED 2026-10-08 (S252.4, `TestNativeDiff_NameOnlyFilter`: worktree + `--cached` columns, filter select/exclude, pathspecs, `--quiet` honors filter), full `--cached` output still open | `exec_read.go:255,308-330` | MED |
| **log** | `-S` (pickaxe), `--grep`; `--author` rejected; ~9 `--format` placeholders only; `--date=` ignored | `exec_read.go:129-252` | MED |
| **branch** | `-f` (force create) | `exec_read.go:612` | LOW |
| **tag** | `-F <file>` (annotated message from file; typed `tag.go` also lacks it), `--format` on list | `exec_read.go:1161` | LOW |
| **remote** | `remote -v` (typed `Remotes()` exists, `git.go:914`; exec layer only `get-url`/`remove`) | `exec_read.go:885` | LOW |
| **ls-tree** | single-path filter, `-d`/`-t`/`-l`/`--name-only` | `exec_plumbing.go:549` | LOW |
| **config** | write ops, scopes (`--global`/`--local`/`--system`), keys beyond `user.name`/`user.email` | `exec_read.go:459` | LOW |
| **fetch** | `-f`, `--depth`, `-q` (`--tags`/`--prune` accepted-but-ignored) | `exec_read.go:1286` | LOW |
| **cherry-pick** | `--continue`/`--abort`/`--skip`, `-n`, conflict handling, multi-commit | `exec_write.go:105` | LOW |
| **rebase** | all flags (`-i`/`--onto`/`--continue`/…); linear replay only | `exec_write.go:260` | LOW |
| **show** | `<commit>:<path>` (blob), `--stat`, `--format` | `exec_read.go:1018` | LOW |
| **clone** | `--bare`, `--mirror`, `--recurse-submodules` (`--local`/`--no-hardlinks` are no-ops) | `exec.go:138` | LOW |

## D. Contract violations — "fail loudly" rule silently broken (bugs)

These accept-and-ignore instead of rejecting; per the package rule they should
`ErrUnsupported` (or be implemented). Fixing them is independent of new features.

- [x] **`branch` silently ignores *unknown* flags** — the one real correctness bug; should reject.
      CLOSED 2026-10-08 (sprint 252, S252.5): probed live — every
      unknown-flag shape already returned ErrUnsupported — then made
      structural (rejected up front in the flag loop, never let into
      a positional) and pinned by test (`branch --bogus`).
      Note: the old code *appended* flags to a positional list that no
      downstream path consumed silently; the guarantee is now explicit.
- [x] `branch -v` — parsed then ignored (`_ = verbose`).
      CLOSED 2026-10-08 (S252.5): `-v`/`--verbose` now renders tip hash +
      subject on all three listing paths (degrades to plain on resolve
      failure, never breaks listing).
- [ ] `log --date=<fmt>` — parsed, value ignored (always RFC3339).
- [ ] `status --short` vs `--porcelain` — treated identically; no version distinction.
- [ ] `fetch --tags` / `--prune` — accepted, no effect.

## Suggested order to close

1. **`cherry` + `push --delete`** — unblock branch-cleanup workflows (high frequency).
2. **Un-stub `worktree` + `stash`** — routed already; core to weave/loom/bisect isolation.
3. **`commit --amend` in the exec layer** (typed already has it) **+ `reset --hard`/`--soft`**.
4. **`revert`** + **`diff --name-only --diff-filter`** (rollback + conflict triage).
5. **Fix the `branch` silent-ignore** (restores the loud-failure contract).

Each closed gap should get a `gaps_test.go` case mirroring the consumer's exact
argv (the pattern already used there), so the surface stays test-pinned.
