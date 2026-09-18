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

- [ ] **`cherry <base> <branch>`** — patch-id equivalence / merge check. **HIGH** —
      gates branch cleanup ("is this branch's work already in master, even under a
      different SHA?"); no easy host-free substitute (patch-id over the range).
- [ ] **`revert <commit>` (`--no-edit`)** — reverse-apply a commit. **HIGH** — rollback.
- [ ] **`clean -fd` / `-n`** — remove untracked files/dirs. **MED** — workspace hygiene.
- [ ] `bisect`, `reflog`, `describe`, `submodule`, `gc`, `prune`, `fsck`,
      `verify-tag`, `mktag`, `pack-refs`, `index-pack`, `verify-pack` — **LOW**.

## B. Routed but stubbed (entry exists, always returns `ErrUnsupported`)

- [ ] **`stash` / `stash push <file>` / `stash pop`** (`exec_read.go:1091`) — **HIGH** —
      bisect/loom local-change isolation.
- [ ] **`worktree add [-f] <path> [<commit>]` / `worktree remove --force`**
      (`exec_read.go:1095`) — **HIGH** — weave sandbox + bisect isolation.
- [ ] **`apply <patch>`** (`exec_write.go:471`) — MED — go-git lacks worktree patch-apply.
- [ ] **`read-tree`** (`exec_plumbing.go:215`) — LOW (plumbing).
- [ ] **`for-each-ref`** (`exec_read.go:925`) — LOW (format parsing).

## C. Supported subcommand, missing flags/options we use

| subcommand | missing flags | file:line | prio |
|---|---|---|---|
| **commit** | `--amend` (exists in typed `Commit`, `git.go:259` — just wire the argv), `-q` | `exec_write.go:558` | **HIGH** |
| **push** | `--delete` / `:<branch>` (delete remote branch), `-q`, `--dry-run`, `--tags` | `exec_write.go:18` | **HIGH** |
| **reset** | `--hard`, `--soft` (only an unstage form, which itself errors) | `exec_read.go:930,992` | **HIGH** |
| **checkout** | `--theirs`, `--ours`, `-f` (only `-b`/`-B` supported) | `exec_read.go:812` | MED |
| **diff** | `--stat`, `--name-only`, `--diff-filter`, full `--cached` output | `exec_read.go:255,308-330` | MED |
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

- [ ] **`branch` silently ignores *unknown* flags** (`exec_read.go:642` appends them
      as positionals) — the one real correctness bug; should reject.
- [ ] `branch -v` — parsed then ignored (`exec_read.go:649` `_ = verbose`).
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
