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
(`image.edit`, `email.send`). The command name is always the head; the
dotted role is the provider's, never a second command name.

| Service | Example roles | Capability tag | Notes |
|---|---|---|---|
| `plan` | `plan` | `planning` | `claude:plan`, `agy:plan`, `muse:plan` (print) are seeded |
| `search` | `search` | `web-search` | `bashy search` backends become providers |
| `image` | `image.generate`, `image.edit` | `image` | |
| `draw` | `draw` | `draw` | diagrams and vector drawings |
| `pdf` | `pdf.read` | `pdf` | |
| `video` | `video.understand` | `video` | |
| `transcribe` | `transcribe.*` | `transcribe` | `bashy transcribe` selector is the base of the shared one; `muse:transcribe` (print) is seeded |
| `narrate` | `narrate` | `narrate` | spoken audio; no vendor CLI produces it headlessly yet |
| `email` | `email.read`, `email.send` | `email` | `mail` stays the Unix mail verb |
| `drive` | `drive.read`, `drive.write` | `drive` | |
| `song` | `song` | `song` | Sprint #627 |
| `animate` | `animate` | `animate` | Sprint #627 |
| `film` | `film` | `film` | Sprint #627 |

A name in this catalog says what to call the feature, not that any tool
offers it. Seeded today: `claude:plan`, `agy:plan` and `muse:plan` (Sprint
#626 S1, print), `muse:transcribe` (S4, print) and `codex:review`; a seed for any other service needs its
own measurement. A tool has one command per name: the claude `/plan` TUI
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
