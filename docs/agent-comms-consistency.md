# Agent Comms CLI Consistency

Status: shipped.

The agent communication commands use one flag vocabulary across the message
board, bus, ping front door, and meet read surfaces. This matters because agents
transfer patterns between commands: a flag that is valid in two places but means
two different things silently punishes the audience these tools are for.

## Canonical Vocabulary

| Flag | Meaning |
| --- | --- |
| `--as <identity>` | Who I am: sender and reader identity. |
| `--to <target>` | The addressee. Positional targets remain where they are the primary operand; `--to` is always accepted as the explicit form. |
| `--wait <dur>` | Bounded block. A timeout is an empty successful read. |
| `--follow` | Unbounded stream. |
| `--interval <dur>` | How often a follow re-reads. |
| `--peek` | Read without advancing my cursor. |
| `-n`, `--limit` | Cap output. |
| `--tail <n>` | Show the last N. There is no visible `-n` shorthand for tail. |
| `--history` | Ignore my cursor and show everything. |
| `--all` | No filter, only on `bus watch`. |
| `--json` | Structured output on every read surface. |

## Deprecated Compatibility Aliases

Old spellings remain hidden so a running agent does not break mid-turn. Each
prints a one-line notice to stderr naming the replacement.

| Old spelling | Replacement |
| --- | --- |
| `mb --all` | `mb --history` |
| `bus publish --principal` | `bus publish --as` |
| `bus watch --poll` | `bus watch --interval` |
| `meet observe -n` | `meet observe --tail` |

No visible flag name in this group has two meanings.
