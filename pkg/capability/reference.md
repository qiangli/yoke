# bashy capability — reference

The **living agent × capability matrix**: which agent is best for each capability,
seeded from research priors and refined by observed outcomes on this host. It is the
routing table behind **capability-routed delegation** (interleaving `tool:model`
capabilities within one task instead of one agent doing everything).

## Terminology

- **tool** — the CLI binary (`codex`, `claude`, `aider`, `opencode`, `agy`); governs
  the *harness* capabilities (operability, shell, tool-use, isolation).
- **model** — the LLM + access tier (`opus`, `fable`, `kimi-k2.7-code`, …); governs the
  *quality* capabilities (coding, review, research, …).
- **agent** — a tool bound to a model, written **`tool:model`** (`claude:opus`,
  `opencode:kimi-k2.7-code`). This is the unit: one matrix row per agent. A bare tool
  or bare model is not an agent.

Capability factorizes as **`harness(tool) ⊕ quality(model)`** — same model → similar
quality columns; same tool → shared harness columns.

## Capabilities

- harness: `operability` · `shell` · `tool-use` · `isolation`
- quality: `coding` · `bug-fixing` · `code-review` · `test-generation` ·
  `deep-research` · `web-search` · `browser-use` · `data-analysis` · `planning` ·
  `decision-support` · `orchestration`

## Verbs

```
bashy capability matrix                 # the full agent × capability grid
bashy capability best <cap> [--all] [--by quality|value|cost]   # rank routable agents (--all: include non-routable)
bashy capability show <agent>           # one agent's row + operability status
bashy capability record --agent tool:model --capability C --outcome pass|fail [--latency ms --cost n]
bashy capability seed [--force]         # (re)write the research-prior matrix
bashy capability reference              # this document
```

`chat --capability <cap>` routes to the best **routable** agent from the matrix and
launches its tool.

## Living matrix

Stored per-host at `~/.bashy/capability/matrix.json` (override with
`BASHY_CAPABILITY_DIR`), reconcile-on-write. Cells carry `quality` (0–1), latency, and
cost, with a `source` of `prior` (research seed) or `host` (measured). `record` folds
an observed outcome via an exponential moving average and flips the cell to
host-measured — so routing improves as the fleet runs real assignments on this host.
`meet` auto-records each participant's **operability** at close (operability is
tool-governed, so it updates every row of that tool).

**Ranking keys** (`best --by`): `quality` (raw fit, default); `cost` (cheapest first);
`value` = **quality × reliability ÷ cost** — the routing objective, where reliability
is the agent's operability (a gate-pass-rate proxy). So a flaky or premium agent
loses to a cheaper, reliable one on commodity work (the "dishwasher rule"), and a
low-operability agent is penalised (the reliability/rework term). `chat --capability`
routes by **value**.

## Operability gate

`Operable(tool)` reports whether an agent can be driven headless here. It is the same
gate `meet` uses for attendees and `capability best` uses to filter routable agents.
