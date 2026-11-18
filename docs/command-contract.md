# Bashy command contract

Status: **v1, accepted by the operator 2026-11-18** (Sprint 655, story S0
`4b633be22c28`). Normative.

This is a living contract: it is improved as bashy itself is shaped. A change
records its date and reason in the change log below, keeps existing
requirement IDs stable (a retired ID is never reused), and prefers additive
changes; a change that breaks callers bumps the envelope `schema_version`.

Bashy is one app to the people and agents who use it: every command, every
registered tool and every app behaves the same way at the command line. This
contract defines that shared behavior. It is about what a caller can observe
(arguments, output, exit status, declared effects), never about how a command
is implemented: any language, any process, built in or registered by a third
party.

The contract codifies conventions bashy already ships. Nothing here needs a
new runtime: the envelope and exit codes are `coreutils/pkg/weavecli`, the
common options are `yoke/pkg/adapt`, the effects are `yoke/pkg/atlas`.

Requirement IDs (`C1`…) are stable, so a checker reports results by name.
MUST, SHOULD and MAY are used as in RFC 2119.

## 1. Scope and conformance

- The contract applies to anything reachable as a bashy command: bashy's own
  verbs, commands registered with `bashy commands add`, foreign commands used
  through `bashy adapt` and adapter records, and the actions of apps
  registered with `bashy app add` (including shaped apps).
- **Conformance is optional.** Commands and apps register exactly as today. A
  command conforms when it meets every requirement below for the options it
  declares; it can be made to conform later, for example by shaping it.
- **Tiers are the declarer's choice.** The stability tier
  (`experimental → preview → supported`, the atlas vocabulary) is declared by
  whoever owns the command. Bashy provides this contract and a checker; it does
  not assign tiers and adds no new refusals.

## 2. Invocation

- **C1 Syntax.** Arguments follow the POSIX utility syntax guidelines (XBD
  12.2), with GNU-style long options `--name` / `--name=value`. `--` ends
  option processing. Subcommands are words: `bashy <command> <subcommand>`.
- **C2 Help.** `--help` prints usage to stdout and exits 0 without side
  effects. Bashy's `--help --format classic|mcp|skill` renderings satisfy this.
- **C3 Version.** A standalone executable (a registered `exec` or `download`
  command, an app's own binary) answers `--version` on stdout with exit 0.
  Bashy subcommands are covered by `bashy --version`.

## 3. Common options

A command declares which of these it supports (in its help, its atlas row or
its registration record). The meaning is fixed; a command never reuses one of
these names for something else.

| ID | Option | Meaning |
|----|--------|---------|
| C4 | `--json` | Emit the result envelope (§4) instead of human text. |
| C5 | `--fields a,b` | Project the envelope's `result` to the named fields. Requires `--json`. |
| C6 | `--null` | NUL-separated records for safe piping. Cannot be combined with `--json`. |
| C7 | `--dry-run` | Report what would happen; change nothing. A command that cannot preview refuses; it never runs the real operation instead. |
| C8 | `--no-input` | Never prompt. When input is required, fail with exit 6 (`input_required`). |
| C9 | `--explain` | Describe what the command would do and its declared effects, without running it. |

Also recognized, with the same fixed meaning when supported: `--plain` (no
ANSI or spinners), `--quiet` (final result line only), `--depth N`,
`--since T`. `BASHY_AGENTIC=1` makes `--json` the default; an explicit
`--json=false` overrides it.

- **C10 Unsupported options fail loudly.** A common option the command does not
  support, or an invalid combination (`--fields` without `--json`, `--json`
  with `--null`), exits 2 with a message naming the option. It is never
  silently ignored.

## 4. Result envelope

- **C11 One envelope.** With `--json`, a command emits exactly one JSON object:
  on stdout when it succeeds, on stderr when it fails. Nothing else is written
  to stdout. The shape is `weavecli.Envelope`:

```json
{
  "schema_version": "loom-v2",
  "command": "sprint show",
  "status": "ok",
  "result": {},
  "error": {"code": "precondition_failed", "message": "…", "help": "…", "cause": {}},
  "hints": [{"why": "…", "suggest": "…"}]
}
```

  - `schema_version`, `command` and `status` are always present. `status` is
    `ok`, `error` or `partial`.
  - `result` carries the answer; `error` is present exactly when `status` is
    `error`, and `error.code` is the string for the exit status (§5) unless a
    foreign worker named its own code.
  - `loom-v2` is the historical identifier of this envelope, kept so existing
    readers do not break. Changes are additive; a breaking change requires a
    new `schema_version`.
  - Readers ignore fields they do not know.
- **C12 Side-channel records are not results.** Advisory records
  (`bashy-hint-v1`, `bashy-recommend-v1`) are one JSON object per line on
  stderr, never on stdout, and `BASHY_HINTS=off` suppresses them. A caller
  never needs them to read a result.

## 5. Exit status

- **C13** Exit statuses have these meanings; `error.code` carries the matching
  string.

| Status | `error.code` | Meaning |
|--------|--------------|---------|
| 0 | `ok` | Success. |
| 1 | `generic_failure` | Failure with no more specific class. |
| 2 | `invalid_arg` | Usage error, invalid argument, unsupported option (C10). |
| 3 | `precondition_failed` | A required precondition does not hold. |
| 4 | `state_conflict` | The target's state conflicts with the request. |
| 5 | `dependency_unhealthy` | A required service or tool is unavailable. |
| 6 | `input_required` | Input is needed and prompting is not allowed; the Bash# *yield*. |
| 125 | — | A wrapper (`bashy adapt`) failed before running the target. |
| 126 | — | Found but cannot be executed, or denied. |
| 127 | — | Command not found. |
| 128+n | — | Terminated by signal n. |

  Statuses 0–6 come from `weavecli`; 125–128+n follow POSIX shell and `env`
  conventions.

## 6. Effects

- **C14 Declared effects.** A command declares its effects from the atlas
  vocabulary: `pure`, `read`, `write`, `destroy`, `net`, `exec`, `cred`,
  `priv`, `remote`, `persist`, `send`, `spend`. It MAY declare a
  reversibility class: `readonly`, `idempotent`, `reversible`, `compensable`,
  `irreversible`. Core verbs declare them in their atlas row, registered
  commands in their record's `effects`, adapters in their adapter record.
  The declaration is the worst case over the command's options. `--explain`
  (C9) reports it. Callers and guards (`@guard(effects:)`) decide what to do
  with it.

## 7. Apps

- **C15 CLI parity.** Every action an app offers in its UI is also available
  as a command on the same entities, with the same identity, permissions and
  approval rules. The UI is a view over those commands and holds no capability
  they lack. `bashy shape` (CLI, REST and web over one lifecycle) is the
  reference example.

## 8. Agent tools

A conforming command can be presented to an agent as a tool without extra
work: its input schema comes from its declared flags, its output is the
envelope, and its effects are the tool's annotations. Bashy's
`--help --format mcp` rendering is the existing form; projecting every
conforming command as an MCP tool is deferred work.

## 9. Making a command conform

- Bashy's own Go commands use `weavecli` (`ResolveOutputModeEx`, `EmitOK`,
  `EmitError`, the `Exit*` constants) and declare effects in their atlas row.
- Registered commands and Bash# skill-scripts print the envelope with `--json`
  and use the exit statuses above; their record declares effects.
- Foreign tools gain the common options through an adapter record and
  `bashy adapt`.
- A shaped app or command can be asked to conform with an ordinary wish
  ("conform to the bashy command contract"); the checker reports what remains.

## Change log

- 2026-11-18 v1 accepted (Sprint 655 S0). Known gap at acceptance: bashy's
  own `bashy-recommend-v1` record is not silenced by `BASHY_HINTS=off` (C12).
