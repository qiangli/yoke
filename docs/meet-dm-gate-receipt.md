# `meet dm` real-process gate receipt (Sprint 418 / Story 1868)

Run: `scripts/meet-dm-gate.sh` at yoke `e6cebc5` (source includes `f132e07`), darwin/arm64,
isolated HOME/BASHY_HOME/fleet/room/ask dirs, binary = the commscli audit tool built from
this tree (`go test -c ./test/commscli`, `COMMSCLI_TOOL=1`). Agent = deterministic stub CLI
`gatebot` planted in the isolated fleet. Pair codes and tokens are redacted (`<code>`).

| Category | Result |
|---|---|
| unknown sender gets one code, no agent launched | PASS |
| repeat message from same sender: pending, no second code | PASS |
| `app pair approve CODE --allow ask --allow approve`; code single use | PASS |
| paired sender: reply on stdout (argv and JSON-on-stdin) | PASS |
| real S4 approval raised (`approve request`), resolved by `/approve ID` with the same id; leaves the queue; decision on timeline | PASS |
| ask-only sender `/approve` refused, approval stays pending | PASS |
| `--revoke` then same message: back to pairing, no agent | PASS |
| `--ttl 2s` pairing delivers inside ttl, refused after expiry, re-pairs | PASS |
| S3: 6 `inbound` events for the 6 messages from one sender | PASS |

Not covered (needs a claimed host / messenger account, kept separate): the bashy host mount
(`bashy meet dm` on Bashy `244740fe`), a two-host run, and a real channel bot-account gateway
(no claimed Sprint 408 messenger account/token). Agent reply is a stub, not a model turn.
