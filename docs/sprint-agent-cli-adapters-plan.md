# Sprint 584 master execution plan — external agent CLIs as shaped Bash# adapters

Sprint-ID: d1c8cf7b-5ef0-566c-9a2e-8a9ea94bc833 (local label #584). Planning only; no
implementation is authorized beyond the C stories until D0 is reviewed.


## Context
Third-party agent CLIs (claude, codex, opencode, agy, muse, zcode, …) are partly data and partly
Go code:
- **Data:** the embedded YAML tool contracts in `yoke/pkg/fleet/baseline/tools/`.
- **Go:** about 30 non-test Go files in yoke and bashy branch on tool names. The examples are
  unsafe flags, decoders, auth checks, stdin format, config dirs, defaults and env sniffing.

This breaks the project's own rule: `yoke/pkg/fleet/embed.go:15` says "no vendor knowledge in
code". It is also why muse and zcode are only partly supported. They are in the catalog, but
missing from `weaveDefaultFleet`, `SeededProfiles`, `canonicalUnsafeFlag`, the cooldown name
sniffer, the sdlc normalizer and `recognitionMarkers`.

Operator direction:
- Keep a named default list of favored tools: genie/ycode built in, plus claude, codex, opencode,
  agy, muse, zcode.
- Every external tool is managed by "rod not fish" plus software shaping, not hard-coded into the
  invariant core.
- Bash# scripts are the shaping medium. Bash# reaches every language, and a `.bsh` file is both a
  command and a skill.

Also in scope: research and plan AWS LLM (Bedrock) support for these tools. Nothing is coded
until the design below is reviewed and filed as a `bashy sprint`.

## Findings that shape the design
- **Precedents to copy:**
  - The service-unit design: "built-ins are seeds, not name branches" (`bashy/docs/bashy-service-design.md:28-60`).
  - S512 browser site skills: a thin core plus per-site Bash# skills.
  - `brainstorm.bsh`: one file registered as both a command and a skill (`bashy/docs/plan-classroom-brainstorm.md:38`).
- **Blocker:** tools are `Mechanism: true` (`yoke/pkg/fleet/kinds.go:109`), so
  `BASHY_FLEET_SEEDS=off` cannot drop third-party tool contracts. Story C5 (`c3e7759fdd72`)
  already moved models and agents to seeds and explicitly left this decision open.
- **Schema gaps:**
  - `cli.launch.env_markers` is declared (`yoke/pkg/fleet/types.go:533`) but never read.
  - There are no fields for unsafe flags or their position, effort templates, the stdin message
    format, decoder kind, auth check, config-dir env, the first-party flag, or role defaults.
- **The core is already nearly clean.** The only CORE-path vendor branch is
  `yoke/pkg/src/core_repair.go:162`. The protected spots are `pkg/policy/coord/identity.go:115`
  and `pkg/binmgr/openclaw.go`. S575 S1 removes the `yoke/pkg/fleet` link from the cert build.
- **AWS:** there is no LLM-provider support for AWS in the repo. There are reusable parts:
  `yoke/pkg/provider/aws` (stdlib SigV4 and credentials) and `fleet.AccountSelection{Profile,AWSKeys}`.
  Per tool:
  - **Native:** claude (`CLAUDE_CODE_USE_BEDROCK`), codex (`model_provider=amazon-bedrock[-runtime]`,
    OpenAI models only), opencode (`amazon-bedrock`), aider, goose.
  - **Through the OpenAI-compatible `bedrock-runtime …/openai/v1` endpoint with a bearer key:**
    zcode and kimi-cli. Not confirmed for zcode.
  - **Gateway only:** agy.
  - **Not supported:** muse (it accepts only Meta's endpoint).
- **Security gaps found, fixed independently as small C commits:**
  1. `AWS_BEARER_TOKEN_BEDROCK` gets past `secrets.ScrubAgentEnv`
     (`yoke/pkg/secrets/firewall.go:145`). `AWS_PROFILE`, `AWS_REGION` and
     `CLAUDE_CODE_USE_BEDROCK` also pass through unscrubbed, so claude can silently run on Bedrock.
  2. zcode's `--mode yolo` (two words) gets past `UnsafeLaunchFlags`
     (`yoke/pkg/agentlaunch/launch.go:621`).
  3. Possible quoting bug: `SetupScript` single-quotes `{base_url}` inside zcode's heredoc JSON.
     Not verified; needs a rendering test.

## Design: thin core plus per-tool Bash# adapters

**0. Native default first, other LLMs opt-in (operator rule):** each third-party tool's own
default model and subscription/login stays the supported default path:
- claude: Anthropic subscription login
- codex: ChatGPT/OpenAI login
- agy: Google sign-in
- muse: `muse login`
- opencode: its own provider setup
- zcode: Z.ai key

A tool with no bound model, or bound to its native seed model, launches exactly as today with no
provider setup hook. Bedrock, the model door and other OpenAI-compatible endpoints are optional
model bindings an operator chooses per agent. They never replace or rewrite the native default,
and an adapter's `setup PROVIDER` hook runs only when a non-native provider is bound. Regression
gate: the existing native agents (e.g. `claude-opus5.5`, `codex-gpt6-sol`, `muse-spark1.3`) render
byte-identical argv/env before and after every A/B story.

**1. What stays in the invariant core (Go, no vendor names):**
- The adapter **hook contract** and its versioned schema.
- The catalog rings (embedded → shared → org → local).
- Managed-binary pinning (`binmgr`).
- The credential firewall plus grant projection.
- The unsafe-launch guard and `ReadOnly` stripping. These are checked on the **final argv/env**
  an adapter produces, never on what the adapter claims. Adapters *declare* their unsafe tokens
  (including multi-word ones such as `--mode yolo`). Core *also* checks a fixed list of known
  dangerous spellings, which can only grow.
- Containment and sandbox decisions.
- Generic decoders that adapters select by name (`jsonl-type`, `jsonl-path`, `text`), so the
  per-line hot path stays in Go.
- Genie/ycode stays first-party mechanism through a `builtin: true` field. That field replaces
  the Go `firstPartyTool()` check.

**2. Per-tool adapter = `tools/<name>.yaml` plus an optional `tools/<name>.bsh`:**
- The YAML keeps the declarative launch contract. New fields:
  - `unsafe: {args, position}`
  - `effort: {args|env}`
  - `stdin_format`
  - `decoder: {kind, text_path, final_path, done}`
  - `auth: {check_exec, ok_match, hint}`
  - `config_dir_env`
  - `builtin`

  Wire up the existing `env_markers`. Unify the four placeholder sets.
- The `.bsh` file implements optional hooks for behaviour that data can't express:
  - `setup PROVIDER MODEL` (write provider config files, e.g. Bedrock for opencode/codex/zcode)
  - `auth-check`
  - `probe`
  - `install`
  - `decode-event` (only for unusual stream formats)
  - `detect`

  It carries skill text in the same file, so an agent can read it and shape a new tool's adapter
  (rod). The adapter is registered through the existing `bashy commands`/`bashy skill` verbs, the
  same way as `brainstorm.bsh`.
- Trust:
  - Embedded seed adapters are pinned with the binary.
  - Adapters from shared or org rings are pinned by content hash and shown in `bashy tool verify`.
  - Local adapters are allowed only after `bashy shape` approval. This reuses the
    `wish→verify→preview→approve→undo` lifecycle in `yoke/pkg/shape`.
- Every adapter ships fixture tests: rendered argv/env/setup for a given model and provider, and
  a recorded event stream in with expected events out.

**3. Seeds instead of name branches:**
- Third-party tool contracts move from Mechanism to seed. `BASHY_FLEET_SEEDS=off` then drops
  them. ycode/genie stays mechanism.
- The default list moves to data, in a seeded `fleet/defaults.yaml` (or the `roles` record):
  - weave default fleet: genie, claude, codex, opencode, agy, muse, zcode
  - role defaults: conductor, reviewer, qa, release
  - the meet secretary
  - the sdlc fallback conductor
- This replaces the following Go branches with catalog lookups:
  - `weaveDefaultFleet`
  - `chat.roleDefaults`
  - `meet.go:128`
  - `sota.go:85`
  - `sdlc.DefaultConductorAgent`
- Name normalization and env detection read `aliases` and `env_markers` from the catalog. Core
  keeps a frozen fallback marker table only for retired harnesses, per the intent stated in
  `detect.go`.

**4. AWS Bedrock as the first optional provider (shows the design end to end; opt-in only):**
- Model seeds `provider: aws-bedrock`, `kind: api`:
  - **runtime** `base_url` for cross-region profiles and Guardrails
  - **mantle** for `/v1/models` and server-side tools
  - `ids:` holds the per-tool spelling (e.g. claude `us.anthropic.claude-…`,
    codex `openai.gpt-6-…`, zcode `zai.glm-5`)
- Credential grants:
  - **Bearer:** `api_key_ref: aws-bedrock` → per-tool `credential_env`
    (`AWS_BEARER_TOKEN_BEDROCK` for claude/codex/opencode; `ZCODE_API_KEY` for zcode).
  - **SigV4/profile:** extend the grant contract so one ref can project several names
    (`AWS_PROFILE` plus `AWS_REGION`, or a key triple). Reuse `AccountSelection`.
- Each adapter's `setup aws-bedrock` hook produces the per-tool wiring:
  - claude: env `CLAUDE_CODE_USE_BEDROCK=1`, `AWS_REGION`
  - codex: `config.toml` `model_provider`
  - opencode: an `opencode.json` provider block
  - zcode: `provider_config.json` with the runtime `/openai/v1` URL
  - agy: gateway settings, which require an llmgw backend
  - muse: the adapter declares the provider unsupported
- Later and separate: a native Bedrock (Converse) client for genie/ycode and an llmgw backend,
  signed with `yoke/pkg/provider/aws`.

## Delivery: a sprint to file, nothing implemented now
File a `bashy sprint` (uuid, provenance trailers) with these stories:
- **D0 design doc.** `yoke/docs/agent-cli-adapters.md`, linked from `docs/INDEX.md` and
  `README.md`. Contents: the hook contract, the schema additions, the trust model, and the
  Mechanism→seed decision for tool contracts (closing C5's open point). Operator review gate.
- **C1–C3 security fixes**, independent and small:
  - C1: scrub `AWS_BEARER_TOKEN_BEDROCK` and gate the `AWS_*`/`CLAUDE_CODE_USE_*` passthrough
    behind grants.
  - C2: multi-word unsafe-flag matching for zcode `--mode yolo`; add muse and zcode to
    `canonicalUnsafeFlag`.
  - C3: zcode setup rendering test, plus the fix if the bug is confirmed.
- **A1 schema plus loader:** the new fields, wiring for `env_markers`, `builtin`, and a seeded
  defaults record.
- **A2 Bash# hook runner:** runs `tools/<name>.bsh` hooks, shared with the `commands`/`skill`
  projection, with hash pinning and shape-approval gating.
- **A3 migrate Go branches tool by tool**, one commit per area: launch/unsafe, cligw/stream
  decode, delegate_guard auth, booth config dir, sdlc/detect, defaults, install-agent. Each
  removes a Go table only once the fixture tests show the same behaviour from the adapter.
- **A4 Mechanism→seed** for third-party tools, plus a regression check that
  `BASHY_FLEET_SEEDS=off` leaves only genie/ycode.
- **B1 Bedrock models plus adapters:** claude, codex and opencode are native; zcode goes through
  OpenAI-compat; agy is gateway only; muse is marked unsupported. Verify on a test host with a
  real short-term Bedrock key.
- **B2 (follow-up):** native Bedrock client for genie and an llmgw backend.

**Coordination:**
- A3 conflicts with S575 S1, which removes the fleet link from the cert build. Run A3 after S1,
  or in a weave on top of it.
- S583.8 (circles on catalog entries) affects how org-ring adapters are scoped.

## Verification (no regressions from baseline)
- Per story:
  - `cd yoke && GOWORK=off go test ./pkg/fleet/... ./pkg/agentlaunch/... ./pkg/cligw/... ./pkg/chat/... ./pkg/weave/... ./pkg/sdlc/... ./pkg/secrets/...`
  - Adapter fixture tests.
  - `script/layer-gate.sh` (shrink-only ratchets).
- A grep ratchet that can only shrink: count of third-party tool-name literals in non-test Go
  under yoke/bashy. A3 drives it toward zero outside the frozen fallback table.
- The full yoke suite plus `cd bashy && make test && make test-bash` (86/86) runs on the remote
  test host after each pin bump.
- Manual checks on the test host:
  - `bashy weave fleet` lists the seeded defaults.
  - A user-added tool (YAML plus `.bsh`, never in Go) launches, decodes and cools down like a
    seeded one.
  - Bedrock is reached by claude, codex, opencode and zcode with a short-term key.
  - The unsafe guard refuses `--mode yolo` on an uncontained host.
