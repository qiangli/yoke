# yoke

bashy's **agentic userland**: everything the [bashy](https://github.com/qiangli/bashy)
shell adds on top of the certified POSIX package
[coreutils](https://github.com/qiangli/coreutils) that is not itself a
POSIX-required or GNU coreutils program. Pure Go, no cgo in core, one
identical toolset on Linux, macOS and Windows.

yoke **imports** coreutils (its `tool/` framework, `multicall/`, `shell/`
adapter and the handful of shared packages the applets need) and coreutils
never imports yoke. The split (Sprint 208, 2026-09-18) exists so the
certified package can stay stable — bug fixes only — while this one changes
as fast as agentic tooling does.

## What is here

- **The AgentOS hub** (`pkg/`): `fleet` (tools/models/agents/skills
  registry), `kb` (the host-scope knowledge base), `bus`/`room`/`meet`
  (agent comms), `weave`/`dag`/`foreman`/`sprint` (orchestration), `chat`,
  `skills`/`craft`, `secrets`, `binmgr` (managed external binaries),
  `atlas` (the Command Atlas), `webconsole` (the bashy apps console), and
  the rest — one implementation, imported by bashy, outpost and ycode.
- **Non-POSIX applets and front-door verbs** (`cmds/`): `ast`, `browser`,
  `fetch`, `jq`, `tar`, `gzip`, `tree`, `which`, `watch`, `tokens`, `tz`,
  `ntp`, `cal`, `hexdump`, `clip`, `duration`, `why`, `atq`/`atrm`, `graph`,
  `foreman`, `resources`. `cmds/all` blank-imports coreutils' certified
  `cmds/all` and adds these — import it to get the whole userland.
- **`git/`** — the self-contained pure-Go git client (go-git; the "file"
  transport is in-process, `git-upload-pack` is never spawned).
- **`mcp/`** — an MCP server over the tool registry (`yoke mcp`).
- **`external/`** — binmgr-managed externals (gh, helm, kubectl, loom, zot,
  seaweedfs, kopia, rclone, act, mise, …), toolchain provisioners (go, node,
  python, rust, clang, cmake), the dhnt front doors, and the embedded
  **ollama** and **podman** forks (submodules) plus the otel stack module.
- **`cmd/yoke`** — the busybox-style multicall binary over the whole
  userland (`yoke <tool>`, argv[0] dispatch, `yoke mcp`).

Membership is one rule, recorded row by row in the dhnt umbrella's
`docs/coreutils-required-set.tsv`: a package is here iff it is not
POSIX-required ∪ GNU coreutils and not in that set's import closure.

## The agent contract

The same contract as coreutils, unchanged: deterministic `LC_ALL=C` output,
GNU exit-code conventions, unsupported flags fail loudly naming the flag,
never a silent approximation under an upstream name. See coreutils'
README for the full statement.

## Build & test

```bash
go build -o bin/yoke ./cmd/yoke
go vet  $(go list ./... | grep -v /external/)
go test $(go list ./... | grep -v /external/)
scripts/crossvet.sh          # cross-OS compile gate; run alongside go test
```

Flat siblings: `../coreutils`, `../sh`, `../filebrowser` (replace
directives in `go.mod`). Inside the dhnt umbrella they are submodules; a
standalone clone puts them next door.

## Consumers

- [bashy](https://github.com/qiangli/bashy) — mounts the hub as its
  in-process userland and front-door verbs.
- [outpost](https://github.com/qiangli/outpost) — `outpost git …` over
  `git/`'s typed API; the mesh supervisor uses `pkg/binmgr`.
- [ycode](https://github.com/qiangli/ycode) — native git tier through
  `git.Exec`; code intelligence through `pkg/{treesitter,repomap,codegraph}`.

## License

MIT
