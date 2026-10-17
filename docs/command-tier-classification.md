# Command tier classification — review evidence (Sprint 449, S2)

Status: review record for Sprint 449 story `ca04fac71ca3` ("S2 Reclassify
command metadata by purpose"), 2026-10-17. Canonical taxonomy: dhnt
`docs/execution-tiers.md` (S1, accepted 2026-10-17). Source of truth for the
data: `pkg/atlas/atlas.go` (hand-set tables) and `external/registry` (the
declarative managed-CLI registry, derived through `atlas.RegistryEntry`).
Pinned by `pkg/atlas/tier_test.go`.

## The rule

A tier names a command's **primary purpose**. It is one field per command, it
says nothing about where an invocation runs, and it introduces no execution
restriction. The six canonical tiers, in stack order, are:

| # | Tier | Primary purpose |
|---|------|-----------------|
| 1 | `userland` | POSIX/coreutils, classic and popular utilities, general toolchains, the platform's own surface |
| 2 | `workplace` | agent work and collaboration |
| 3 | `sandbox` | OCI/Podman containers |
| 4 | `sphere` | peer-to-peer capabilities (pooled inference/compute) |
| 5 | `cluster` | DKS orchestration |
| 6 | `cloud` | external providers |

`account` (`tessaro`, `login`) is the Tessaro front door beside the stack. An
entry may carry it, but `atlas.Tiers()` returns exactly the six: it is never a
seventh tier. `workspace` is **not** a tier spelling and has no alias; it keeps
its filesystem meaning (a weave clone, its paths, `@ws`, persisted run fields).

Review rules applied to every entry, alias, managed external and recipe:

1. Purpose, not package: a verb implemented in yoke, coreutils or a managed
   binary is classified by what it is for.
2. General utilities and toolchains stay userland, whoever provisions them.
3. Dedicated OCI, peer, DKS and provider operations keep sandbox, sphere,
   cluster and cloud respectively.
4. Orchestration- and knowledge-group verbs are workplace unless rule 2 or 3
   applies (the exceptions are listed by name below).
5. An alias inherits its command's tier (`aliasVerb` copies it; the
   hand-written aliases are pinned by test).
6. A recipe (idiom) carries the tier of at least one member command; a
   builtin-only recipe is userland.

## Changes made by this review

| Entry | Before | After | Why |
|-------|--------|-------|-----|
| tier 2 constant / string / `Tiers()` order | `TierWorkspace` = `workspace` | `TierWorkplace` = `workplace` | S1 taxonomy; no `workspace` alias |
| `Tiers()` | six tiers + `account` | the six tiers | account is represented separately, never a seventh tier |
| weave, sprint, dag, sdlc, genie, loom, todo (+`issue`), pair, judge | workspace | workplace | rename only; membership unchanged |
| chat (+`invoke`), delegate, ycode, coach, supervise | userland (default) | workplace | run and supervise agent sessions: agent work |
| meet, inbox, mb (+`messages`), notify, ping, bus, handoff, resume, claim, steward, feed, herald | userland (default) | workplace | agent/human collaboration primitives |
| capability, leaderboard, stats | userland (default) | workplace | route, rank and score agent work |
| tool (+`tools`), model (+`models`), agent (+`agents`), person (+`people`), whois, schedule | userland (default) | workplace | the fleet registry and scheduler: who and what does agent work on this host |
| kb, skill (+`skills`), craft, lexicon, define | userland (default) | workplace | knowledge shared between agents |
| mod | workspace | userland | a go.work toolchain helper; its "workspace" is the filesystem word, not the tier |
| registry `rg` | 2 | 1 (userland) | a general text-search utility |
| registry `gitea` | 5 (cluster) | 2 (workplace) | the forge `loom` fronts; not a DKS operation |
| registry `ollama` | 3 (sandbox) | 4 (sphere) | agrees with the hand-listed `ollama` verb; not an OCI operation |

No recipe changed tier. No execution restriction was added anywhere.

## Ambiguous entries, one line each

- **mod** → userland. Manages go.mod pins in a go.work "workspace"; that
  homonym, not agent work, is why it was tier 2.
- **herald** → workplace. Reaches an agent on another host; collaboration is
  the purpose, remoteness is an execution location.
- **chat, invoke, delegate, ycode, coach, genie, supervise** → workplace. They
  start or supervise agent sessions; that is agent work, not a general utility,
  even though each runs on the native host.
- **pair, judge** → workplace. Pair-review and LLM judgement of agent work.
  **gate** stays userland: the general mechanical pass/fail runner that any
  project uses, like `check` and `conform`.
- **stats** → workplace. A pure filter, but over agent-run results JSONL; it
  evaluates agent work.
- **capability, leaderboard** → workplace. Routing input and ledger of what the
  fleet did.
- **tool, model, agent, person, whois** → workplace. The fleet registry is the
  cast of agent work on this host; `whois` resolves a collaborator.
- **schedule** → workplace. Schedules agent runs under llm-budget admission;
  the classic cron faces (`crontab`, `at`, `batch`) stay userland tools.
- **ping** → workplace. The verb exists for the board (read/message); its ICMP
  branch is the classic utility reachable through the same name.
- **mb, messages, notify, inbox, bus, handoff, resume, claim, steward, feed**
  → workplace. Collaboration primitives: the board, the bus, the handoff, the
  project claim, the host seat, the federated feed.
- **meet** → workplace (required). **who, write, mail, mailx, talk** stay
  userland: classic user-to-user utilities in the tool table.
- **kb, skill, craft, lexicon, define** → workplace. Knowledge shared between
  agents (kb is required; the others are the same surface).
- **search, sota** → userland. General search and research utilities (web,
  files, knowledge); agents use them, they do not coordinate agents.
- **mcp** → userland. A transport surface that exposes the whole userland to
  external harnesses, like `app` and the shell; not itself agent work.
- **act, act-runner** → sandbox (kept). They run CI workflows in OCI
  containers, a dedicated container operation, although they sit in the
  orchestration group and could be read as forge tooling.
- **loom** → workplace (required). The agent forge, regardless of being a
  managed Gitea binary (`managed-external` in the forge group).
- **git, gh, git-scm** → userland (kept). General forge clients and the real
  git provisioner.
- **llm** → userland (kept) while **ollama** → sphere (kept). `llm` serves this
  host's flat-billed seats only; `ollama` is the pooled p2p inference engine the
  taxonomy names for sphere. Same group, different purpose.
- **mirror, rclone, zot, seaweedfs, kopia** → userland (kept). General storage
  services, managed or not.
- **registry `gitea`** → 2. Same purpose as `loom`; the raw binary is not a
  cluster operation. **registry `ollama`** → 4, agreeing with the verb; the
  entry is download-disabled by supply-chain policy, so only `doctor` shows it.
- **why** (managed external, diagnostics) and the POSIX external providers
  (`m4`, `man`, `ctags`, `ar`, `nm`, `strip`, `ex`, `vi`, `lp`, `localedef`)
  → userland. Tools are userland by definition; provisioning does not change
  purpose.
- **tessaro, login** → account (kept): the front door, beside the stack.

## Managed externals

| Name | Tier | Note |
|------|------|------|
| git, gh | userland | forge clients |
| loom | workplace | the agent forge (managed Gitea) |
| act, act-runner | sandbox | CI workflows in OCI containers |
| mirror, rclone, zot, seaweedfs, kopia | userland | storage |
| kubectl, helm | cluster | DKS/k8s client CLIs |
| why | userland | diagnostics (tool table) |
| POSIX providers | userland | tool table, `managed-external` subclass |
| registry: doctl, gcloud, tofu | cloud (6) | provider CLIs, `RegistryEntry(6)` |
| registry: gitea | workplace (2) | forge binary behind loom |
| registry: ollama | sphere (4) | download-disabled; agrees with the verb |
| registry: rg | userland (1) | general utility |

## Recipes (idioms)

| Recipe | Tier | Members | Agreement |
|--------|------|---------|-----------|
| count-matches, top-n, find-exec, list-inspect, tempfile-cleanup, archive, fetch-extract, who-write | userland | userland tools | all members userland |
| scoped-cd | userland | `cd` (builtin) | builtin-only → userland |
| forge-loop | userland | git, gh (userland), act (sandbox) | git/gh carry the tier |
| fleet-suite | workplace | weave, sprint, dag | all workplace |
| whois-notify | workplace | whois, notify | both workplace |
| mb-meet | workplace | mb, meet | both workplace |
| notify-inbox | workplace | notify, inbox | both workplace |
| cluster-deploy | cluster | kubectl, helm | both cluster |
| pair-first | account | login (account), peer (sphere) | login carries the tier |

## Complete classification

Generated from the atlas tables after this review (tier, group, class, name,
stage, subclass, alias-of). Tools are userland by definition (pinned by
`atlas_coverage_test.go`); the table lists them for completeness.

| Tier | Group | Class | Name | Stage | Subclass | Alias of |
|------|-------|-------|------|-------|----------|----------|
| account | account | verb | `login` | cross |  |  |
| account | account | verb | `tessaro` | cross |  |  |
| cluster | cluster-cloud | verb | `dks` | deploy |  |  |
| cluster | cluster-cloud | verb | `helm` | deploy | managed-external |  |
| cluster | cluster-cloud | verb | `kubectl` | deploy | managed-external |  |
| sandbox | engines | verb | `docker` | cross |  | oci |
| sandbox | engines | verb | `oci` | cross |  |  |
| sandbox | engines | verb | `podman` | cross |  | oci |
| sandbox | engines | verb | `sandbox` | cross |  | oci |
| sandbox | orchestration | verb | `act` | test | managed-external |  |
| sandbox | orchestration | verb | `act-runner` | test | managed-external |  |
| sphere | engines | verb | `ollama` | cross |  |  |
| sphere | engines | verb | `peer` | deploy |  |  |
| sphere | engines | verb | `sphere` | deploy |  | peer |
| userland | code-intel | tool | `ast` | cross |  |  |
| userland | code-intel | tool | `ctags` | cross | managed-external |  |
| userland | code-intel | tool | `graph` | cross |  |  |
| userland | diagnostics | verb | `check` | test |  |  |
| userland | diagnostics | verb | `conform` | test |  |  |
| userland | diagnostics | verb | `doctor` | cross |  | inspect |
| userland | diagnostics | verb | `gate` | test |  |  |
| userland | diagnostics | verb | `inspect` | cross |  |  |
| userland | diagnostics | tool | `posix-gate` | cross |  |  |
| userland | diagnostics | tool | `resources` | cross |  |  |
| userland | diagnostics | verb | `verify` | test |  | conform |
| userland | diagnostics | tool | `why` | cross | managed-external |  |
| userland | engines | verb | `llm` | cross |  |  |
| userland | fileutils | tool | `basename` | cross |  |  |
| userland | fileutils | tool | `chcon` | cross |  |  |
| userland | fileutils | tool | `chgrp` | cross |  |  |
| userland | fileutils | tool | `chmod` | cross |  |  |
| userland | fileutils | tool | `chown` | cross |  |  |
| userland | fileutils | tool | `clip` | cross |  |  |
| userland | fileutils | tool | `cp` | cross |  |  |
| userland | fileutils | tool | `cygpath` | cross |  |  |
| userland | fileutils | tool | `dd` | cross |  |  |
| userland | fileutils | tool | `df` | cross |  |  |
| userland | fileutils | tool | `dir` | cross |  |  |
| userland | fileutils | tool | `dircolors` | cross |  |  |
| userland | fileutils | tool | `dirname` | cross |  |  |
| userland | fileutils | tool | `du` | cross |  |  |
| userland | fileutils | tool | `file` | cross |  |  |
| userland | fileutils | tool | `find` | cross |  |  |
| userland | fileutils | tool | `install` | cross |  |  |
| userland | fileutils | tool | `link` | cross |  |  |
| userland | fileutils | tool | `ln` | cross |  |  |
| userland | fileutils | tool | `ls` | cross |  |  |
| userland | fileutils | tool | `mkdir` | cross |  |  |
| userland | fileutils | tool | `mkfifo` | cross |  |  |
| userland | fileutils | tool | `mknod` | cross |  |  |
| userland | fileutils | tool | `mktemp` | cross |  |  |
| userland | fileutils | tool | `mv` | cross |  |  |
| userland | fileutils | tool | `pax` | cross |  |  |
| userland | fileutils | tool | `readlink` | cross |  |  |
| userland | fileutils | tool | `realpath` | cross |  |  |
| userland | fileutils | tool | `rm` | cross |  |  |
| userland | fileutils | tool | `rmdir` | cross |  |  |
| userland | fileutils | tool | `shred` | cross |  |  |
| userland | fileutils | tool | `stat` | cross |  |  |
| userland | fileutils | tool | `sync` | cross |  |  |
| userland | fileutils | tool | `tar` | cross |  |  |
| userland | fileutils | tool | `touch` | cross |  |  |
| userland | fileutils | tool | `tree` | cross |  |  |
| userland | fileutils | tool | `truncate` | cross |  |  |
| userland | fileutils | tool | `unlink` | cross |  |  |
| userland | fileutils | tool | `vdir` | cross |  |  |
| userland | fileutils | tool | `wslpath` | cross |  |  |
| userland | forge | verb | `gh` | cross | managed-external |  |
| userland | forge | verb | `git` | cross | managed-external |  |
| userland | forge | verb | `git-scm` | cross | provisioner |  |
| userland | knowledge | verb | `search` | cross |  |  |
| userland | knowledge | verb | `sota` | cross |  |  |
| userland | net | tool | `browser` | cross |  |  |
| userland | net | verb | `curl` | cross | provisioner |  |
| userland | net | tool | `fetch` | cross |  |  |
| userland | net | tool | `lp` | cross | managed-external |  |
| userland | net | tool | `mail` | cross |  | mailx |
| userland | net | tool | `mailx` | cross |  |  |
| userland | net | tool | `talk` | cross |  |  |
| userland | net | verb | `web` | cross |  |  |
| userland | orchestration | verb | `mcp` | cross |  |  |
| userland | platform | verb | `agentic` | cross |  |  |
| userland | platform | verb | `app` | cross |  |  |
| userland | platform | verb | `apps` | cross |  | app |
| userland | platform | verb | `ask` | cross |  |  |
| userland | platform | verb | `audit` | cross |  | inspect |
| userland | platform | verb | `bootstrap` | cross |  | self |
| userland | platform | verb | `commands` | cross |  |  |
| userland | platform | verb | `context` | cross |  | inspect |
| userland | platform | verb | `install-agent` | cross |  |  |
| userland | platform | verb | `otel` | cross |  |  |
| userland | platform | verb | `run` | cross |  |  |
| userland | platform | verb | `secret` | cross |  |  |
| userland | platform | verb | `secrets` | cross |  | secret |
| userland | platform | verb | `self` | cross |  |  |
| userland | platform | verb | `upgrade` | cross |  | self |
| userland | shellutils | tool | `[` | cross |  | test |
| userland | shellutils | tool | `arch` | cross |  |  |
| userland | shellutils | tool | `at` | cross |  |  |
| userland | shellutils | tool | `atq` | cross |  |  |
| userland | shellutils | tool | `atrm` | cross |  |  |
| userland | shellutils | tool | `batch` | cross |  |  |
| userland | shellutils | tool | `bc` | cross |  |  |
| userland | shellutils | tool | `cal` | cross |  |  |
| userland | shellutils | tool | `crontab` | cross |  |  |
| userland | shellutils | tool | `date` | cross |  |  |
| userland | shellutils | tool | `duration` | cross |  |  |
| userland | shellutils | tool | `echo` | cross |  |  |
| userland | shellutils | tool | `env` | cross |  |  |
| userland | shellutils | tool | `expr` | cross |  |  |
| userland | shellutils | tool | `factor` | cross |  |  |
| userland | shellutils | tool | `false` | cross |  |  |
| userland | shellutils | tool | `gencat` | cross |  |  |
| userland | shellutils | tool | `getconf` | cross |  |  |
| userland | shellutils | tool | `groups` | cross |  |  |
| userland | shellutils | tool | `hostid` | cross |  |  |
| userland | shellutils | tool | `hostname` | cross |  |  |
| userland | shellutils | tool | `id` | cross |  |  |
| userland | shellutils | tool | `kill` | cross |  |  |
| userland | shellutils | tool | `locale` | cross |  |  |
| userland | shellutils | tool | `localedef` | cross | managed-external |  |
| userland | shellutils | tool | `logger` | cross |  |  |
| userland | shellutils | tool | `logname` | cross |  |  |
| userland | shellutils | tool | `man` | cross | managed-external |  |
| userland | shellutils | tool | `mesg` | cross |  |  |
| userland | shellutils | tool | `ncal` | cross |  |  |
| userland | shellutils | tool | `newgrp` | cross |  |  |
| userland | shellutils | tool | `nice` | cross |  |  |
| userland | shellutils | tool | `nohup` | cross |  |  |
| userland | shellutils | tool | `nproc` | cross |  |  |
| userland | shellutils | tool | `ntp` | cross |  |  |
| userland | shellutils | tool | `pathchk` | cross |  |  |
| userland | shellutils | tool | `pinky` | cross |  |  |
| userland | shellutils | tool | `printenv` | cross |  |  |
| userland | shellutils | tool | `printf` | cross |  |  |
| userland | shellutils | tool | `ps` | cross |  |  |
| userland | shellutils | tool | `pwd` | cross |  |  |
| userland | shellutils | tool | `renice` | cross |  |  |
| userland | shellutils | tool | `seq` | cross |  |  |
| userland | shellutils | tool | `sleep` | cross |  |  |
| userland | shellutils | tool | `sntp` | cross |  |  |
| userland | shellutils | tool | `stdbuf` | cross |  |  |
| userland | shellutils | tool | `stty` | cross |  |  |
| userland | shellutils | tool | `tabs` | cross |  |  |
| userland | shellutils | tool | `test` | cross |  |  |
| userland | shellutils | tool | `time` | cross |  |  |
| userland | shellutils | tool | `timeout` | cross |  |  |
| userland | shellutils | tool | `tput` | cross |  |  |
| userland | shellutils | tool | `true` | cross |  |  |
| userland | shellutils | tool | `tty` | cross |  |  |
| userland | shellutils | tool | `tz` | cross |  |  |
| userland | shellutils | tool | `uname` | cross |  |  |
| userland | shellutils | tool | `uptime` | cross |  |  |
| userland | shellutils | tool | `users` | cross |  |  |
| userland | shellutils | tool | `watch` | cross |  |  |
| userland | shellutils | tool | `which` | cross |  |  |
| userland | shellutils | tool | `who` | cross |  |  |
| userland | shellutils | tool | `whoami` | cross |  |  |
| userland | shellutils | tool | `write` | cross |  |  |
| userland | shellutils | tool | `yes` | cross |  |  |
| userland | storage | verb | `kopia` | cross | managed-external |  |
| userland | storage | verb | `mirror` | cross | managed-external |  |
| userland | storage | verb | `rclone` | cross | managed-external |  |
| userland | storage | verb | `seaweedfs` | cross | managed-external |  |
| userland | storage | verb | `zot` | cross | managed-external |  |
| userland | textutils | tool | `awk` | cross |  |  |
| userland | textutils | tool | `b2sum` | cross |  |  |
| userland | textutils | tool | `base32` | cross |  |  |
| userland | textutils | tool | `base64` | cross |  |  |
| userland | textutils | tool | `basenc` | cross |  |  |
| userland | textutils | tool | `cat` | cross |  |  |
| userland | textutils | tool | `cksum` | cross |  |  |
| userland | textutils | tool | `cmp` | cross |  |  |
| userland | textutils | tool | `comm` | cross |  |  |
| userland | textutils | tool | `csplit` | cross |  |  |
| userland | textutils | tool | `cut` | cross |  |  |
| userland | textutils | tool | `diff` | cross |  |  |
| userland | textutils | tool | `ed` | cross |  |  |
| userland | textutils | tool | `ex` | cross | managed-external |  |
| userland | textutils | tool | `expand` | cross |  |  |
| userland | textutils | tool | `fmt` | cross |  |  |
| userland | textutils | tool | `fold` | cross |  |  |
| userland | textutils | tool | `grep` | cross |  |  |
| userland | textutils | tool | `gunzip` | cross |  |  |
| userland | textutils | tool | `gzip` | cross |  |  |
| userland | textutils | tool | `head` | cross |  |  |
| userland | textutils | tool | `hexdump` | cross |  |  |
| userland | textutils | tool | `iconv` | cross |  |  |
| userland | textutils | tool | `join` | cross |  |  |
| userland | textutils | tool | `jq` | cross |  |  |
| userland | textutils | tool | `m4` | cross | managed-external |  |
| userland | textutils | tool | `md5sum` | cross |  |  |
| userland | textutils | tool | `more` | cross |  |  |
| userland | textutils | tool | `nl` | cross |  |  |
| userland | textutils | tool | `numfmt` | cross |  |  |
| userland | textutils | tool | `od` | cross |  |  |
| userland | textutils | tool | `paste` | cross |  |  |
| userland | textutils | tool | `patch` | cross |  |  |
| userland | textutils | tool | `pr` | cross |  |  |
| userland | textutils | tool | `ptx` | cross |  |  |
| userland | textutils | tool | `sed` | cross |  |  |
| userland | textutils | tool | `sha1sum` | cross |  |  |
| userland | textutils | tool | `sha224sum` | cross |  |  |
| userland | textutils | tool | `sha256sum` | cross |  |  |
| userland | textutils | tool | `sha384sum` | cross |  |  |
| userland | textutils | tool | `sha512sum` | cross |  |  |
| userland | textutils | tool | `shuf` | cross |  |  |
| userland | textutils | tool | `sort` | cross |  |  |
| userland | textutils | tool | `split` | cross |  |  |
| userland | textutils | tool | `strings` | cross |  |  |
| userland | textutils | tool | `sum` | cross |  |  |
| userland | textutils | tool | `tac` | cross |  |  |
| userland | textutils | tool | `tail` | cross |  |  |
| userland | textutils | tool | `tee` | cross |  |  |
| userland | textutils | tool | `tokens` | cross |  |  |
| userland | textutils | tool | `tr` | cross |  |  |
| userland | textutils | tool | `tsort` | cross |  |  |
| userland | textutils | tool | `unexpand` | cross |  |  |
| userland | textutils | tool | `uniq` | cross |  |  |
| userland | textutils | tool | `uudecode` | cross |  |  |
| userland | textutils | tool | `uuencode` | cross |  |  |
| userland | textutils | tool | `vi` | cross | managed-external |  |
| userland | textutils | tool | `wc` | cross |  |  |
| userland | textutils | tool | `xargs` | cross |  |  |
| userland | textutils | tool | `zcat` | cross |  |  |
| userland | toolchains | tool | `ar` | cross | managed-external |  |
| userland | toolchains | verb | `cargo` | code | provisioner |  |
| userland | toolchains | verb | `clang` | code | provisioner |  |
| userland | toolchains | verb | `cmake` | code | provisioner |  |
| userland | toolchains | verb | `go` | code | provisioner |  |
| userland | toolchains | tool | `make` | cross |  |  |
| userland | toolchains | verb | `mise` | code | provisioner |  |
| userland | toolchains | verb | `mod` | cross |  |  |
| userland | toolchains | tool | `nm` | cross | managed-external |  |
| userland | toolchains | verb | `node` | code | provisioner |  |
| userland | toolchains | verb | `npm` | code | provisioner |  |
| userland | toolchains | verb | `npx` | code | provisioner |  |
| userland | toolchains | verb | `pip` | code | provisioner |  |
| userland | toolchains | verb | `pnpm` | code | provisioner |  |
| userland | toolchains | tool | `posix-providers` | cross | provisioner |  |
| userland | toolchains | verb | `pwsh` | code | provisioner |  |
| userland | toolchains | verb | `python` | code | provisioner |  |
| userland | toolchains | verb | `rust` | code | provisioner | rustc |
| userland | toolchains | verb | `rustc` | code | provisioner |  |
| userland | toolchains | verb | `rustup` | code | provisioner |  |
| userland | toolchains | tool | `strip` | cross | managed-external |  |
| userland | toolchains | verb | `uv` | code | provisioner |  |
| userland | toolchains | verb | `yarn` | code | provisioner |  |
| userland | toolchains | verb | `zig` | code | provisioner |  |
| workplace | forge | verb | `loom` | cross | managed-external |  |
| workplace | knowledge | verb | `craft` | cross |  |  |
| workplace | knowledge | verb | `define` | cross |  |  |
| workplace | knowledge | verb | `kb` | cross |  |  |
| workplace | knowledge | verb | `lexicon` | cross |  |  |
| workplace | knowledge | verb | `skill` | cross |  |  |
| workplace | knowledge | verb | `skills` | cross |  | skill |
| workplace | orchestration | verb | `agent` | cross |  |  |
| workplace | orchestration | verb | `agents` | cross |  | agent |
| workplace | orchestration | verb | `bus` | code |  |  |
| workplace | orchestration | verb | `capability` | cross |  |  |
| workplace | orchestration | verb | `chat` | code |  |  |
| workplace | orchestration | verb | `claim` | cross |  |  |
| workplace | orchestration | verb | `coach` | code |  |  |
| workplace | orchestration | verb | `dag` | cross |  |  |
| workplace | orchestration | verb | `delegate` | code |  |  |
| workplace | orchestration | verb | `feed` | cross |  |  |
| workplace | orchestration | verb | `genie` | code |  |  |
| workplace | orchestration | verb | `handoff` | cross |  |  |
| workplace | orchestration | verb | `herald` | cross |  |  |
| workplace | orchestration | verb | `inbox` | cross |  |  |
| workplace | orchestration | verb | `invoke` | code |  | chat |
| workplace | orchestration | verb | `issue` | plan |  | todo |
| workplace | orchestration | verb | `judge` | test |  |  |
| workplace | orchestration | verb | `leaderboard` | cross |  |  |
| workplace | orchestration | verb | `mb` | cross |  |  |
| workplace | orchestration | verb | `meet` | plan |  |  |
| workplace | orchestration | verb | `messages` | cross |  | mb |
| workplace | orchestration | verb | `model` | cross |  |  |
| workplace | orchestration | verb | `models` | cross |  | model |
| workplace | orchestration | verb | `notify` | cross |  |  |
| workplace | orchestration | verb | `pair` | test |  |  |
| workplace | orchestration | verb | `people` | cross |  | person |
| workplace | orchestration | verb | `person` | cross |  |  |
| workplace | orchestration | verb | `ping` | cross |  |  |
| workplace | orchestration | verb | `resume` | cross |  |  |
| workplace | orchestration | verb | `schedule` | cross |  |  |
| workplace | orchestration | verb | `sdlc` | deploy |  |  |
| workplace | orchestration | verb | `sprint` | plan |  |  |
| workplace | orchestration | verb | `stats` | test |  |  |
| workplace | orchestration | verb | `steward` | cross |  |  |
| workplace | orchestration | verb | `supervise` | code |  |  |
| workplace | orchestration | verb | `todo` | plan |  |  |
| workplace | orchestration | verb | `tool` | cross |  |  |
| workplace | orchestration | verb | `tools` | cross |  | tool |
| workplace | orchestration | verb | `weave` | code |  |  |
| workplace | orchestration | verb | `whois` | cross |  |  |
| workplace | orchestration | verb | `ycode` | code |  |  |

## Noted, out of this story's scope

- bashy's `pkg/harnessrunner` uses `atlas.TierWorkspace` as a filesystem
  *scope* for effects (the weave clone), i.e. the workspace meaning, not the
  tier. It needs its own scope constant when bashy bumps the yoke pin (S3).
- bashy's `internal/agentos` tier synopsis and section order still list the
  old spelling and `account` as a seventh row (S3).
- `pkg/skills` help text says "tier-2 workspace skills"; the skills'
  `metadata.tier` field is the skills mechanism's own and is reconciled by the
  docs/help story (S4).
