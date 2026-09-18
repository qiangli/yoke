# bashy command groups

Generated from `../bashy/bin/bashy commands --json` on 2026-07-07.

> **Superseded as the live grouping source by the Command Atlas**
> (`bashy/docs/command-atlas.md`; live data: `bashy commands --atlas --json`,
> tables in `coreutils/pkg/atlas`). This file is kept as the 2026-07-07
> count snapshot; the userland has grown since (Phase B additions), so
> regenerate from `--atlas --json` when citing counts.

This is the live bashy command catalog reported by `bashy commands`: shell builtins, in-process coreutils tools, and bashy front-door verbs. The groups below are mutually exclusive for the combined command surface. Commands implemented both as shell builtins and in the in-process coreutils layer are listed in the Bash builtins group, because the shell resolver handles builtins before the coreutils `ExecHandler`.

Raw catalog counts:

| Source | Count |
|---|---:|
| Shell builtins | 61 |
| In-process coreutils tools | 117 |
| Front-door verbs | 37 |
| Unique command names across all sources | 210 |

Coreutils names shadowed by Bash builtins: `echo`, `false`, `pwd`, `true`.

## Bash builtins

Count: 61.

`.`, `:`, `[`, `alias`, `bg`, `bind`, `break`, `builtin`, `caller`, `cd`, `command`, `compgen`, `complete`, `compopt`, `continue`, `declare`, `dirs`, `disown`, `echo`, `enable`, `eval`, `exec`, `exit`, `export`, `false`, `fc`, `fg`, `getopts`, `hash`, `help`, `history`, `jobs`, `kill`, `let`, `local`, `logout`, `mapfile`, `popd`, `printf`, `pushd`, `pwd`, `read`, `readarray`, `readonly`, `return`, `set`, `shift`, `shopt`, `source`, `suspend`, `test`, `times`, `trap`, `true`, `type`, `typeset`, `ulimit`, `umask`, `unalias`, `unset`, `wait`

## File utils

Count: 27.

`basename`, `chgrp`, `chmod`, `chown`, `clip`, `cp`, `df`, `dirname`, `du`, `find`, `link`, `ln`, `ls`, `mkdir`, `mktemp`, `mv`, `readlink`, `realpath`, `rm`, `rmdir`, `stat`, `sync`, `tar`, `touch`, `tree`, `truncate`, `unlink`

## Text utils

Count: 37.

`awk`, `base32`, `base64`, `cat`, `cmp`, `comm`, `cut`, `diff`, `grep`, `gunzip`, `gzip`, `head`, `hexdump`, `join`, `jq`, `md5sum`, `paste`, `sed`, `sha1sum`, `sha224sum`, `sha256sum`, `sha384sum`, `sha512sum`, `shuf`, `sort`, `split`, `strings`, `tac`, `tail`, `tee`, `tokens`, `tr`, `tsort`, `uniq`, `wc`, `xargs`, `zcat`

## Shell utils

Count: 27.

`at`, `atq`, `atrm`, `batch`, `cal`, `crontab`, `date`, `duration`, `env`, `hostname`, `id`, `ncal`, `ntp`, `printenv`, `seq`, `sleep`, `sntp`, `time`, `timeout`, `tty`, `tz`, `uname`, `uptime`, `watch`, `which`, `whoami`, `yes`

## Misc / Extended support

Unique count: 58.

This group contains bashy-specific in-process tools and front-door verbs for AgentOS, managed external CLIs, language/runtime bootstrap, graph/code intelligence, containers, scheduling, secrets, and workflow orchestration.

In-process extended tools, count 22:

`browser`, `fetch`, `foreman`, `graph`

Front-door verbs, count 37:

`act`, `act-runner`, `agent`, `ast`, `chat`, `check`, `commands`, `context`, `dag`, `docker`, `doctl`, `doctor`, `foreman`, `gh`, `git`, `helm`, `kopia`, `kubectl`, `login`, `loom`, `mirror`, `ollama`, `podman`, `rclone`, `run`, `schedule`, `sdlc`, `seaweedfs`, `secrets`, `self`, `skills`, `sphere`, `sprint`, `tessaro`, `verify`, `weave`, `web`, `zot`

`foreman` appears in both the in-process extended tools and front-door verbs; it is counted once in the unique misc total.
