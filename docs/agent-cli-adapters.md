# Agent-CLI adapters: shaped Bash# adapters over a thin core

Status: D0 design (Sprint 584, story `852bea9fa54b`). Operator review gates A1-A4.
Plan and research: `docs/sprint-agent-cli-adapters-plan.md`.

## Problem

A third-party agent CLI (claude, codex, opencode, agy, muse, zcode, aider, ...) is
described twice: as data in `pkg/fleet/baseline/tools/<name>.yaml`, and as Go branches
on its name in about thirty non-test files (unsafe flags, stream decoders, stdin
message shape, auth checks, config-dir env, role defaults, env sniffing, install
recipes). The Go copy contradicts the rule stated in `pkg/fleet/embed.go`
("no vendor knowledge in code") and drifts: muse and zcode are full catalog
entries yet absent from half the Go tables. Adding or fixing a tool needs a bashy
release, which is fish, not a rod.

## Rules

1. **Native default first.** A tool's own default model and subscription/login
   (claude subscription, codex ChatGPT login, agy Google sign-in, `muse login`,
   opencode's own provider config, zcode Z.ai key) is the supported default. A launch
   with no bound model, or with the tool's native seed model, renders exactly what it
   renders today. Other LLM providers (AWS Bedrock, the model door, any
   OpenAI-compatible endpoint) are opt-in per-agent bindings and never rewrite the
   native path.
2. **Mechanism in Go, knowledge in assets.** Go may name a tool only in the frozen
   recognition fallback (below) and in tests. Everything else is read from the
   catalog.
3. **Safety is enforced on the result, not on the adapter's word.** Adapters declare;
   the core checks the final argv/env.
4. **Seeds, not built-ins.** Third-party tool contracts are seed data the operator can
   override or drop. Only genie/ycode is built in (`builtin: true`).

## Layers

```
core (Go, no vendor names)       adapter (asset, shapeable)
-------------------------------  -----------------------------------------
catalog rings + overlay merge    tools/<name>.yaml   declarative contract
managed binary pinning (binmgr)  tools/<name>.bsh    optional Bash# hooks
credential firewall + grants                         (+ embedded skill face)
unsafe guard / ReadOnly / attended
containment decision
generic event decoders
hook runner + trust check
defaults record lookup
```

The ring order is unchanged: embedded < shared (`BASHY_TOOLS_PATH`) < org overlay <
local (`BASHY_TOOLS_DIR`/`BASHY_FLEET_DIR`). An adapter's YAML and `.bsh` resolve as
a pair from the same ring; an overlay that replaces one keeps the other unless the
overlay names it.

## Declarative contract: additions to `cli.launch`

Existing fields stay (`exec`, `steer_exec`, `fork_exec`, `acp_exec`, `warm`,
`events_*`, `session_env`, `credential_env`, `key_env`, `env`, `setup`,
`version_probe_exec`, `trust_preseed`, `trust_clear`, `auth_hint`, `usage_signals`,
`integration`). New fields, each replacing a named Go table:

| Field | Shape | Replaces |
|---|---|---|
| `builtin` | bool (tool level) | `chat.firstPartyTool`, ycode special cases |
| `unsafe` | `{args: [..], values: {flag: [vals]}, position: before\|after}` | `agentlaunch.SeededProfiles.UnsafeArgs`, `canonicalUnsafeFlag` |
| `headless_args` | argv fragment | `SeededProfiles.Args` (fallback profile) |
| `effort` | `{args: "--effort {effort}"}` or `{env: "X={effort}"}` | `cligw` effort args, `GENIE_EFFORT` |
| `stdin_format` | `stream-json-user` \| `raw` \| `none` | `cligw/session.go` message shape |
| `gateway_args` | `{system_prompt, tools_off, telemetry_off, new_project}` fragments | `cligw/worker.go` per-tool argv |
| `decoder` | `{kind: jsonl-type\|jsonl-path\|text, text_path, final_path, done}` | `chat/streamdecode.go` muse/agy decoders |
| `auth` | `{check_exec, ok_match, lane_args}` (keeps `auth_hint`) | `chat/delegate_guard.go` |
| `config_dir_env` | env name | `weave/booth.go` `*_CONFIG_DIR` cases |
| `env_markers` | (exists, now read) | `fleet/detect.go`, `sdlc.normalizeAgentName` |
| `os` | `{darwin: {...}, windows: {...}}` overrides | codex-on-darwin branches |

Unknown fields fail validation (`bashy tool verify`), so a typo is loud.

## Hooks: `tools/<name>.bsh`

Optional. Used only for behavior data cannot express. One Bash# file, two faces
(the `brainstorm.bsh` precedent): an executable body with subcommands and an embedded
` ~~~skill as guide ` block that tells an agent how the tool works and how to shape
it. `bashy --bashsharp tools/<name>.bsh --skill` prints the guide.

| Hook | Called with | Must output | Used by |
|---|---|---|---|
| `setup PROVIDER MODEL` | env: `STATE_DIR`, `BASE_URL`, granted credential | exit 0; files only under `STATE_DIR` | launch, only when a non-native provider is bound |
| `auth-check` | none | JSON `{ok, detail}` | `delegate_guard`, `weave fleet --auth` |
| `probe` | none | JSON `{version, ok}` | `weave fleet --probe`, `tool verify` |
| `install` | `VERSION` | exit 0 | `bashy tool install` when no `managed` recipe |
| `decode-event` | one event line on stdin | JSON `{kind, text, done, fail}` | only when `decoder.kind: hook` |
| `detect` | env snapshot on stdin | `yes`/`no` | harness recognition beyond `env_markers` |

Contract rules: hooks are non-interactive, bounded by the tool's watchdog, receive the
already-scrubbed environment plus their explicit grant, and their outputs are checked
by the core (for `setup`, the subsequent argv/env go through the unsafe guard and the
firewall like any other launch). A missing hook means "use the declarative field or
the core default". The hook contract carries a version (`adapter: 1`); the core
refuses a newer major.

`decode-event` runs per line and is the slow path; the named Go decoders cover
claude, codex, opencode, agy and muse today and stay the default.

## Trust

| Ring | Rule |
|---|---|
| embedded | ships with the binary; trusted as the binary is |
| shared / org | the `.bsh` is pinned by sha256 in the YAML (`adapter_sha256`); mismatch refuses the hook and `tool verify` reports it |
| local | runs only after `bashy shape` approval (`wish -> verify -> preview -> approve -> undo`, `pkg/shape`); approval records the hash |

A hook can never widen safety: it cannot set `BASHY_ALLOW_UNSAFE_AGENT_LAUNCH`, cannot
receive a credential the model binding did not grant, and its argv/env output is
guarded as a whole.

## Defaults as data

A seeded `defaults` record (fleet kind, overlayable) replaces the Go defaults:

```yaml
weave_fleet: [genie, claude, codex, opencode, agy, muse, zcode]
roles: {conductor: claude, release: claude, reviewer: codex, qa: codex}
meet_secretary: claude
sdlc_conductor_fallback: codex
```

Each favored tool resolves to its native default agent. Readers:
`weaveDefaultFleet`, `chat.roleDefaults`, `meet.go` secretary, `sota.go`,
`sdlc.DefaultConductorAgent`. Help text renders from the record.

## Recognition

`fleet/detect.go` keeps its frozen fallback table (a retired harness must still be
recognized) and adds catalog `env_markers` in front. `sdlc` normalization uses catalog
`aliases`. Markers are added only from observed env, never guessed.

## Mechanism -> seed (closes the C5 open point)

`pkg/fleet/kinds.go` registers tools as `Mechanism: true`, so `BASHY_FLEET_SEEDS=off`
cannot drop third-party contracts. A4 splits the kind: tools with `builtin: true`
stay mechanism; all others become seeds. Regression: with seeds off only genie/ycode
resolve, and the native agents still render byte-identical argv/env with seeds on.

## Optional providers: AWS Bedrock (B1)

Model seeds `provider: aws-bedrock` with `bedrock-runtime` (cross-region profiles,
Guardrails) or `bedrock-mantle` (`/v1/models`, server tools) endpoints, per-tool `ids:`.
Credential: bearer (`api_key_ref: aws-bedrock` -> `credential_env`
`AWS_BEARER_TOKEN_BEDROCK`), or SigV4/profile via a multi-name grant built on
`fleet.AccountSelection`. Per-tool `setup aws-bedrock` hooks: claude env
(`CLAUDE_CODE_USE_BEDROCK=1`, `AWS_REGION`), codex `config.toml` `model_provider`,
opencode `amazon-bedrock` block, zcode `provider_config.json` on the runtime
`/openai/v1` URL, agy via a gateway, muse declares unsupported. B1 also moves
`AWS_PROFILE`/`AWS_REGION`/`CLAUDE_CODE_USE_*` from implicit passthrough to the grant.

## Delivery order and gates

A1 schema + defaults record (additive, no behavior change) -> A2 hook runner + trust
-> A3 per-area migration (one commit per area; a Go table is removed only when adapter
fixtures reproduce its behavior) -> A4 mechanism split -> B1. Gates per story:
`GOWORK=off go test` for touched packages, adapter fixtures (rendered argv/env/setup
per tool x provider; recorded event streams), `script/layer-gate.sh`, and a shrink-only
ratchet counting third-party tool-name literals in non-test Go. Pin bumps run the full
yoke suite and bashy `make test` + `make test-bash` 86/86 on the remote test host.
A3 is sequenced after S575 S1 (fleet leaves the cert build).
