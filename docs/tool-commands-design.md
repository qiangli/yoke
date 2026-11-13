# Tool commands

A tool's `commands:` block (`fleet.ToolCommand`, Sprint #324) exposes one of
its own vendor features — a slash command, a skill, a subcommand or a local
HTTP service — as a bashy command, `bashy tool cmd run TOOL:NAME`. It is the
inverse of `pkg/cligw`, which strips vendor slash commands from a model
request; the llm API filter `slash=<name>` (`pkg/cligw/slash.go`) reaches
the same commands through the model door.

- Declarations are data in `pkg/fleet/baseline/tools/<tool>.yaml` and are
  validated at load (`Tool.ValidateCommands`). An invalid command is
  reported; the tool is kept.
- Modes: `print` (one shot through the exec template or the command's own
  `exec`), `tui` (typed into a steered session through `steer_exec`) and
  `http` (a typed JSON request; see `toolcmd-http-commands.md`).
- The runner is `pkg/toolcmd`.
- The baseline seeds only commands measured on the wire, each with a
  `# MEASURED <date>` comment naming the CLI version and the result.
  `TestBaselineToolCommandsValidate` pins the seeded set.

## Canonical names

`fleet.CanonicalToolCommands` is the cross-tool vocabulary. A tool that
exposes one of these features registers it under this name whatever its own
spelling (`slash:`), so `claude:review` and `codex:review` are one service
on two tools. It is a vocabulary, not a whitelist: any name is allowed, and
a name outside it only warns. Keep this list and the Go list in step.

Session and agent features (Sprint #324):

| Name | Feature |
|---|---|
| `compact` | compact the session context |
| `deep-research` | multi-step researched report |
| `exit` | end the session |
| `init` | write the tool's project instruction file |
| `plan` | produce a plan without acting |
| `resume` | resume a prior session |
| `review` | review a change |
| `security-review` | security review of a change |
| `status` | session and account status |

## Service catalog

Sprint #626 (`docs/sprint-626-master-execution-plan.md` in the umbrella) puts
vendor features behind service verbs — `bashy plan`, `bashy search`,
`bashy image`, … — that share provider selection (`--provider`, `--route`),
the `bashy-svc-v1` envelope and the shell, MCP and llm API doors. A vendor
tool command registered under a service name is projected as a `vendor`
provider for that service; registered commands that declare `provides:`,
local open-source models, browser automation and paid APIs are the other
providers.

A provider role is an open dotted value whose head is the service
(`image.edit`, `email.send`). A command named for the service head serves
every role of that service, so the selector matches a role to a command by
its full name or its head (`video.understand` → `agy:video`). A tool that
offers only part of a service names the command by that role instead
(`claude:email.read`), so it is never offered for a sibling role. A
command named for the bare head serves a mutating role (`*.send`,
`*.write`) only if it declares that effect; a read-only `email` is
rejected for `email.send` (`TestSendRoleNeverSelectsReadOnlyCommand`).

| Service | Example roles | Capability tag | Notes |
|---|---|---|---|
| `plan` | `plan` | `planning` | `claude:plan`, `agy:plan`, `muse:plan` (print) are seeded |
| `search` | `search` | `web-search` | `bashy search` backends become providers |
| `image` | `image.generate`, `image.edit` | `image` | |
| `draw` | `draw` | `draw` | diagrams and vector drawings |
| `pdf` | `pdf.read` | `pdf` | |
| `video` | `video.understand` | `video` | `agy:video` (print) is seeded |
| `transcribe` | `transcribe.*` | `transcribe` | `bashy transcribe` selector is the base of the shared one; `muse:transcribe` (print) is seeded |
| `narrate` | `narrate` | `narrate` | spoken audio; no vendor CLI produces it headlessly yet |
| `email` | `email.read`, `email.send` | `email` | `mail` stays the Unix mail verb; `claude:email.read` (print, read-only) is seeded |
| `drive` | `drive.read`, `drive.write` | `drive` | `claude:drive.read` (print, read-only) is seeded |
| `song` | `song` | `song` | Sprint #627 |
| `animate` | `animate` | `animate` | Sprint #627 |
| `film` | `film` | `film` | Sprint #627 |

A name in this catalog says what to call the feature, not that any tool
offers it. Seeded today: `claude:plan`, `agy:plan` and `muse:plan` (Sprint
#626 S1, print), `muse:transcribe` (S4, print), `agy:video` (S6, print), `claude:email.read`
and `claude:drive.read` (S5, print) and `codex:review`; a seed for any other
service needs its own measurement. A tool has one command per name: the claude `/plan` TUI
spelling is in the umbrella's `docs/tool-commands-overlay-example.yaml`.

An `exec:` override gets no `events_stdout` flags, and the runner reads only
a JSON terminal `result` string (claude) or an agent-message item (codex).
A seed whose JSON keeps the text elsewhere (agy `result.response`, muse
`payload.text`) runs in text output. The runner guards the rendered `exec:`
argv; the tool's default launch, which the override discards, is not
guarded.

## Capability tags

A command's optional `capability:` tag routes `--capability` requests
(`capability.CommandFor`, `toolcmd.SelectMeasuredCommand`, the cligw
capability header). `capability.ParseCapability` resolves the tag with
aliases and resolves a dotted role to its head, so `capability:
image.generate` and `capability: image` match the same requests.

- `plan` and `search` reuse the matrix quality columns `planning` and
  `web-search`.
- The other services are service tags (`capability.ServiceCaps`). They are
  not matrix columns and get no seeded prior: a tool has the feature or not.
  A cell exists only after a host measurement is recorded
  (`bashy capability record`), which is all the measured selector reads.
- Aliases: `search` → `web-search`; `diagram` → `draw`; `gdrive` → `drive`;
  `movie` → `film`; `image-generation` → `image`; `tts`, `speech` →
  `narrate`; `music` → `song`; `animation` → `animate`; `stt`,
  `speech-to-text` → `transcribe`. `mail` is not an alias.

## Connector services (Sprint #626 S5)

`claude:email.read` and `claude:drive.read` reach the account's claude.ai
Gmail and Drive connectors in print mode. They are read-only by
construction, not by prompt:

- `--permission-mode dontAsk` denies every tool that `--allowedTools` does
  not name, and `--allowedTools` names only the connector's read tools.
- `--disallowedTools` removes the send/draft (Gmail) or share/create/update
  (Drive) tools, the other connectors and Bash/Edit/Write from the session.
- `--tools` is never used: any `--tools` list also hides the connector
  tools.

Sending or sharing is a separate command that declares the `send` effect,
so the door selects it only with `--yes`. No vendor send command is seeded;
the gws rods in bashy's `examples/google` include `google-email-send`. A
claude parent session exports `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`,
which keeps the connectors from loading in the child; the child then
reports that it has no Gmail tool, and the outcome is still success.

### Native Google access exploration (Sprint #626 S5b)

Measured 2026-11-12 with agy 1.5.7: agy has no native Gmail or Drive connectors
or built-in Workspace tools in headless print mode, and no `--allowedTools` or
`--disallowedTools` flags. In `--mode plan` it emits an implementation plan
without reading Gmail (exit 0, 39.5 s); asked directly without `run_command`,
it confirms no native Gmail/Drive tools exist (exit 0, 2.5 s); with general
prompt execution, it delegates to `bashy google` via `run_command` (Gmail:
exit 0, 73.6 s, 5 IDs; Drive: exit 0, 187.9 s, 403 insufficientPermissions).
Because agy cannot reach Gmail/Drive headlessly without general shell
execution, `agy:email.read` and `agy:drive.read` are not seeded in the baseline
per operator instruction.
