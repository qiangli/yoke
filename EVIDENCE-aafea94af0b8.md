# S583.3 — Owner circle: secrets by handle, redaction at egress, human-only reveal

Story: #3134 / aafea94af0b8 · Sprint: #583 (2035b56b-d719-5d80-8c39-6195df0d1e6a)
KISS rescope 2026-11-09: verify first, close gaps only, no new secret engine.

## Verification matrix (what was checked, what held, what was a gap)

### (1) Agents and children get secrets by handle, never plaintext env — HOLDS

Existing machinery, verified by reading + existing tests (all green):

- `secrets.ScrubAgentEnv` (pkg/secrets/firewall.go) strips every
  vault-projected name AND credential-shaped name from a spawned agent's env,
  deny-by-default, fails closed — call sites: pkg/agentlaunch/launch.go,
  pkg/chat/chat.go, pkg/cligw/worker.go, pkg/dag/snippet_runner.go,
  pkg/weave/weave_agent.go (tests: firewall_test.go, firewall_shape_test.go,
  agentlaunch, chat, weave suites).
- Handle use exists as NAME-based references resolved at the exec boundary:
  `secret exec NAME[=ENV] -- CMD` (values into the child env only, never
  printed — exec_test.go), model `api_key_ref` grants
  (secrets.GrantAgentKey / PreserveEnvNames / PreserveEnvAliases), and
  dagrun's `CredentialRef` → out-of-band `DeliveryBundle` with its own
  Redactor (pkg/dagrun/credentials.go — refs never in argv or manifest).

NOTE for the record: the literal `secret://<account>/<name>` URI grammar from
the story text does NOT exist anywhere in yoke or bashy today. What exists is
the handle-by-name mechanism above. Introducing the URI grammar is a new
surface, not a gap close — recorded as follow-up (F3), not done here.

### (2) Redactor on member-facing egress points (S583.1 inventory, existing today)

The S583.1 deliverable `bashy docs/boundary.md` does not exist yet (S583.1
still open); the inventory used is the one named in the rescope: LLM door
responses/logs, cross-host mb/bus posts, handoff, feed.

| Surface | Before | After |
|---|---|---|
| LLM door capture (pkg/broker/capture.go turns.jsonl) | shape + host-identity masking only; an arbitrary vault value embedded in a body survived | + exact vault-value layer via `secrets.RedactKnownValues` |
| Cross-host mb/bus mail (pkg/bus sendRemote: relay envelope + world-readable outbox copy) | none | `bus.RedactCrossHostBody` applied at the sendRemote choke |
| Cross-host shared-room post (pkg/weave SendRemoteMessage: relay event Summary) | none | same `bus.RedactCrossHostBody` before AppendEvent |
| Handoff artifacts (pkg/handoff Save: Continuity/NextAction/Blockers/Diff/Untracked[].Content at 0644, meant to travel off-host) | none | `redactRecord` at the Save choke (in place, so --json echo + later briefs see masked text); `recordEvent` briefs masked before the host event log |
| Feed outbound channel text (pkg/feed SendEnvelopes → Discord/Slack/Telegram/Matrix/toolcmd argv) | control-char scrub only | exact vault-value masking at the one outbound choke |

All five use one shared construction: `secrets.EnvValueRedactor(environ)`
(new) = vault-projected names (`VaultEnvNames`) ∩ values in the calling
process's environment → `secrets.Redactor`. Values, not shapes: ordinary
text that merely looks credential-shaped is never rewritten. Same posture as
the two existing egress redactors (pkg/chat reduce path, pkg/weave capture).

Surfaces verified as ALREADY covered (untouched): chat invoke-output +
transcript read path (chat.go reduceChatBytes, transcript.go ReadTranscript),
weave agent-output capture (capture_redaction.go), kb note transcripts,
reduce spill, dagrun RunRecords.

Out of inventory scope, noted for S583.15 (closure suite): room timeline
producers other than handoff, meet PostMB/Notify, feed Publish git/rclone
sinks, broker live response bytes (loopback to the calling principal) and
audit Error text.

### (3) Plaintext reveal verbs refuse non-TTY and agent callers — GAP, CLOSED

`secret show` and `secret env` printed plaintext values to any caller with a
vault token — including a non-interactive agent piping stdout into its
transcript. Now both pass `secrets.CheckReveal` before touching the vault:

- agent caller (BASHY_PRINCIPAL=dhnt:agent/*, BASHY_AGENT_ID,
  BASHY_AGENT_BINDING) → refused;
- non-interactive stdin (pipe/buffer/detached) → refused;
- refusal precedes any template read or network call (a refused caller
  learns nothing, not even whether the name exists — pinned by test);
- the TTY probe is a package var seam for tests, deliberately NOT an
  environment override: an agent subprocess can write $BASHY_* but cannot
  flip a Go var in this process. Handle use (`secret exec`) is ungated by
  design — it is the mechanism agents are supposed to use.

Reveal verbs that print no plaintext were left alone: list (names only),
check (verdicts only), gen (mints, never resolves), set/import/rm (writes).

## Tests (all under temporary BASHY_HOME / XDG dirs)

- pkg/secrets/reveal_test.go — gate units + CLI refusals (no vault request)
- pkg/secrets/envredact_test.go — EnvValueRedactor/RedactKnownValues
- pkg/bus/redact_body_test.go — relay envelope + outbox copy masked
- pkg/handoff/redact_test.go — artifact fields + event-log brief masked
- pkg/feed/redact_test.go — outbound envelope text masked, ordinary text kept
- pkg/broker/capture_redact_test.go — vault-value layer + shape layer kept
- 8 pre-existing env/show render tests stamped with allowRevealForTest
  (simulated interactive owner); suites green: secrets, bus, feed, broker,
  handoff (full), agentlaunch/dagrun/weave (targeted)

## Follow-ups recorded (NOT done here — need bashy-side code or another story)

- F1 (bashy): the human-only APPROVE-gate reveal form — raising
  `approve request -- secret reveal ...`, human allow via ask, value
  delivered to the human's TTY — needs wiring in bashy's approve tree
  (internal/agentos); yoke's CheckReveal refusal is the floor, not the gate.
- F2 (bashy): bashy's own tests drive `bashy secret env/show`
  non-interactively; they will need the same simulated-owner treatment on
  the next bashy run against this yoke.
- F3: the `secret://<account>/<name>` URI grammar (story text) does not
  exist; either introduce it as sugar over the existing name handles or
  amend the story text — do not treat S583.3 as having built it.
- F4 (bashy): bashy-side member-facing surfaces named in the original story
  but out of the rescope inventory (meet posts, kb/memory writes, exechist,
  stdout/stderr capture beyond weave) — output_reduce.go covers the shell's
  own capture; the rest belongs to their circle stories (S583.4-S583.15).
