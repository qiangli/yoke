# Use the bashy LLM door from other agentic tools

`bashy llm` is the one model door. It serves local models and fleet agents on
the OpenAI- and Anthropic-compatible endpoint at `127.0.0.1:24556`; clients do
not need another gateway or adapter.

## Ready client blocks

Print the format the client accepts:

```sh
bashy llm env --client generic   # eval-able OpenAI, Anthropic and Ollama env
bashy llm env --client openclaw  # complete openclaw.json provider + default
bashy llm env --client codync    # env block for the codync-host service unit
```

The OpenClaw block registers `bashy/L4` with the `openai-completions` adapter
and selects it as the default. Paste or merge the printed JSON into
`openclaw.json`; no OpenClaw source change is needed. The Codync block belongs
under its `bashy service` unit and is inherited by the ACP agents Codync starts.
Every block contains the owner bearer token, so transfer it like a secret and
do not commit it.

## Multi-turn on one worker (sticky bind=worker reset=none)

A sticky binding with `--bind worker --reset none` holds one warm CLI for the
binding and forwards only the new turn of the conversation; the client keeps
sending the whole OpenAI-style message list and the door verifies the prefix.
Only tools that read further turns from a streaming stdin protocol (fleet
launch `warm: stdin-stream-json`) can hold a session. Every other tool is a
one-shot CLI and the turn is refused with 501; the body names the tool, the
reason and the supported tools.

| tool     | multi-turn | note                                                                 |
|----------|------------|----------------------------------------------------------------------|
| claude   | yes        | `warm: stdin-stream-json`                                            |
| agy      | yes        | `warm: stdin-stream-json`                                            |
| codex    | no         | 501: one-shot CLI (`warm: stdin`); needs a streaming stdin protocol  |
| muse     | no         | 501: one-shot CLI (`warm: cold`); needs a streaming stdin protocol   |
| opencode | no         | 501: one-shot CLI (`warm: cold`); needs a streaming stdin protocol   |
| genie    | no         | 501: one-shot CLI (`warm: cold`); needs a streaming stdin protocol   |

```sh
bashy llm sticky create conv --model claude-sonnet5 --bind worker --reset none
eval "$(bashy llm env --sticky conv)"
```

`--bind worker --reset each` stays one-shot on every tool.

## Reach a door through the outpost mesh

Expose the door on the host that owns the seats:

```sh
outpost mesh service add llm 127.0.0.1:24556
bashy llm env --client openclaw --via mesh >openclaw-bashy.json
```

On the paired client host, bind that service to the same loopback address, then
use the block produced on the owner host:

```sh
outpost mesh dial --local 127.0.0.1:24556 llm
```

If port 24556 is occupied on the client, choose another fixed port for the
dial and set `BASHY_LLM_PORT` to that port while printing the block. Mesh reach
is only configuration: the door and client code do not change.

## Register an external OpenAI-compatible gateway

The model registry already accepts API backends. Store the credential in the
named secret first, then add the gateway as data:

```sh
bashy model add company-gateway \
  --provider openai --kind api --upstream company-model \
  --base-url https://gateway.example/v1 --api-key-ref company-gateway \
  --band 3 --band-source operator
```

No provider-specific Go code is required.
