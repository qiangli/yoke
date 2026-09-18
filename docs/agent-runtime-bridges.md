# Agent runtime bridges

Bashy has two different boundaries. They should not share a name or leak into
workflow commands.

## Local agent tools

`pkg/agentlaunch` is the adapter for local agent CLIs. Fleet YAML declares the
binary, model token, headless/interactive/fork/ACP launches, event channels,
workspace binding, credentials, and safety posture. `agentlaunch` resolves that
declaration into one tool-independent launch and applies launch decoration
without requiring a caller to know Claude, Codex, AGY, ycode, or OpenCode flag
syntax.

`pkg/chat` is the runtime above it: one-shot invocation, ACP/event/PTY transport,
streaming, sessions, steering, process-tree shutdown, and conversation-store
isolation. Workflow packages call this runtime:

| Command/package | Shared boundary |
|---|---|
| `chat`, `delegate`, `invoke` | `chat.Invoke` / `chat.Start` |
| `meet` | `chat.Invoke` / `chat.Start` |
| `foreman` | `chat.Invoke` / `chat.Start` |
| `pair` | `chat.Invoke` |
| `supervise` / judge turns | `chat.Invoke` |
| `weave` | `agentlaunch` plus its workspace/process supervisor |

`weave` does not use `chat.Invoke` because it owns a durable git workspace,
wrapper PID, watchdogs, resume recipe, and merge lifecycle. It must still use
`agentlaunch` for every tool-specific launch decision. In particular, event
arguments and prompt-safe argument placement live in `agentlaunch`; weave must
not spell provider flags itself.

## Remote A2A peers

`pkg/herald` is the bridge for remote Agent2Agent peers. It maps a remote peer
onto the fleet's `tool:model` algebra so existing selection and attribution can
address it. Herald is not a local agent harness and is not the abstraction used
to launch local CLIs.

The separation is intentional:

```text
meet / foreman / pair / chat / supervise
                    |
                  chat runtime
                    |
                 agentlaunch ---- fleet declarations ---- local CLI

remote workflow ---- herald ---- A2A peer

weave ---- agentlaunch ---- local CLI
  |
git workspace + watchdog + merge lifecycle
```

When adding another workflow command, use `chat.Invoke` or `chat.Start` unless
the command owns a durable external lifecycle like weave. Such a lifecycle may
execute the process itself, but it still resolves and decorates the process only
through `agentlaunch`. Adding a switch on a provider/tool name in a workflow
package is an architecture regression; put the capability in fleet metadata and
the normalization in `agentlaunch` or `chat` instead.
