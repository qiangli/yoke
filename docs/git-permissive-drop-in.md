# Retiring the GPL git: growing `coreutils/git` (go-git) into a drop-in

*Design exploration. After removing the Temurin/Maven `java` external, **git is the
only non-permissive tool bashy provisions** (git-for-windows, GPL-2.0). This maps the
path to eliminating it — grow the pure-Go `coreutils/git` (go-git v5) until the GPL
`git-scm` fallback is never reached, using **gh's git usage as the acceptance target**.*

## The structure already exists

- **`coreutils/git/`** — a go-git-v5 implementation with ~40 native subcommands:
  `add apply blame branch cat-file checkout cherry-pick clone commit commit-tree config
  diff diff-tree fetch for-each-ref format-patch grep hash-object log ls-files ls-tree
  merge merge-base pull push read-tree rebase remote reset rev-list rev-parse rm show
  show-ref stash status symbolic-ref tag update-ref worktree write-tree`.
- **`ErrUnsupported`** — every handler returns it for an unimplemented subcommand *or
  flag combination*, and callers "try the next tier."
- **`git-scm`** (GPL git-for-windows, provisioned by binmgr) — the next tier: `bashy
  git-scm` execs a real git for what the pure-Go layer can't do.

So this is **not a greenfield build**. The network primitives (clone/fetch/pull/push via
`transport.go`) and gh's core plumbing (rev-parse, config, remote, symbolic-ref,
for-each-ref, show-ref) are already native. The job is **closing the long tail** — the
specific subcommands and flags gh invokes that still fall through.

## gh's git surface, and the exact gaps

gh (GitHub CLI, MIT) shells out to `git` for two things: **local repo detection** (the
bulk) and **authenticated network ops** (`gh repo clone/sync`, `gh pr checkout`). Measured
against `coreutils/git`:

**Missing subcommands (fall through to GPL git today):**
| subcommand | gh uses it for | difficulty |
|---|---|---|
| `git version` | startup capability check | trivial (print a version string) |
| `git init` | `gh repo create` | easy (go-git `PlainInit` already imported) |
| `git ls-remote` | check remote refs before clone/fork | easy (go-git `Remote.List`) |
| `git credential` (`fill`/`approve`/`reject`) | **the auth path** — see below | hard |

**Missing flags inside existing handlers:**
| flag | subcommand | gh uses it for |
|---|---|---|
| `--get-regexp` | `config` | **enumerate remotes** (`config --get-regexp ^remote\.`) — gh's primary repo-detection call |
| `--left-right` | `rev-list` | ahead/behind counts vs upstream |
| `--unshallow` | `fetch` | `gh repo sync` on a shallow clone |

**Already present (verified):** `--force-with-lease`, `--set-upstream`, `--porcelain`.

## The two tiers — and the crux

The gaps split cleanly, and the split is the plan:

1. **Local plumbing (easy, pure-Go, no network).** `version`, `init`, `ls-remote`,
   `config --get-regexp`, `rev-list --left-right`. These are the *majority* of gh's git
   calls (it detects the repo/remotes locally, then talks to the GitHub **API**, not git,
   for most operations). Closing these alone lets gh operate against a bashy repo for
   everything except authenticated transfer. **High value, low risk, days not weeks.**

2. **Authenticated transfer (the crux).** `gh repo clone` / `pr checkout` / `repo sync`
   run `git clone`/`fetch`/`push` **over HTTPS with gh as the credential helper**. Today
   `coreutils/git` (git.go:87) "uses direct auth instead of wiring a credential helper" —
   it takes an `AuthMethod` in-process. gh's model is the opposite: git invokes the
   configured `credential.helper` (which is `gh auth git-credential`) via the
   **git-credential wire protocol** (write `protocol/host/path` on stdin → read back
   `username`/`password`). **go-git does not speak this protocol**, so this is the real
   work: implement a `credential.Helper` client that shells the configured helper per the
   git-credential spec and feeds the result into go-git's `transport.AuthMethod`. Once
   that exists, authenticated clone/fetch/push "just work" with gh, and — bonus — with any
   other credential helper (osxkeychain, libsecret, cache).

## What needs go-git *library* work vs. wrapper work

Most is wrapper-level (parse the flag, call an existing go-git API). Three items may need
go-git-level effort or careful shims:

- **`config --get-regexp`** — go-git's config API is structured key/value, not a flat
  key space; regex-matching over the raw config requires reading the sections/subsections
  and formatting `section.subsection.key value` lines to match git's output.
- **`fetch --unshallow`** — go-git shallow support is partial; `--unshallow` (deepen to
  full history) may need go-git changes or a fallback.
- **`for-each-ref --format=… --sort=…`** — the format-template + sort engine is git-
  specific; gh passes custom `--format`. Needs a small formatter (git's `%(refname)`
  atoms) if gh's exact patterns aren't yet covered.

The credential-helper protocol is **new code in `coreutils/git`**, not a go-git change —
go-git already accepts an `AuthMethod`; we just need to *produce* one from a helper.

## Phased plan

- **P1 — local plumbing (retire the fallback for `gh` repo-detection):** `version`,
  `init`, `ls-remote`, `config --get-regexp`, `rev-list --left-right`. Pure-Go, testable
  without network.
- **P2 — the credential-helper client (unlocks authenticated transfer):** implement the
  git-credential protocol → `AuthMethod`, wired into clone/fetch/pull/push. This is what
  makes `gh repo clone` work on bashy-git.
- **P3 — the long tail:** `--unshallow`, `for-each-ref` format/sort atoms, and any flag a
  differential run still surfaces.

## The gate — differential against real git, measured not asserted

Same discipline as the bash-5.3 conformance work: **capture gh's actual git invocations
and run each against both `coreutils/git` and the reference git, asserting byte-identical
stdout/exit** (with a `GIT_TRACE`-style capture of what gh calls, or gh's own integration
tests pointed at bashy-git via `PATH`). A subcommand/flag is "supported" only when it
matches the reference — the same integrity rule as the shell waist. When the differential
is green for gh's whole surface, `git-scm` (GPL) can drop from bashy's default provisioning
and become an explicit opt-in (`bashy git-scm`) for the residual long tail — the same
"compat is the floor, the pure-Go superset is the default" stance as the rest of bashy.

## The one-line framing

**git is the last GPL tool, and the exit is already 80% built:** `coreutils/git` covers
the surface; the remaining work is the credential-helper protocol (the crux) plus a
short list of flags, gated by a differential against real git using gh as the yardstick.
