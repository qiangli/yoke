# Operator page actions (Sprint 641)

A shaping button puts the page within an agent's reach.

The console serves a small page-owned action registry over an authenticated
WebSocket. CLI/API callers address an open tab by its opaque page ID. This
changes the live tab; it does not generate or approve source revisions.
The existing `bashy shape wish/status/verify/approve/reject` commands still
manage durable source changes and their review gates.

```sh
bashy shape page list console
bashy shape page invoke console PAGE shape.open
bashy shape page invoke console PAGE shape.draft '{"request":"Make the header compact"}'
bashy shape page invoke console PAGE shape.submit
bashy shape page result console COMMAND
bashy shape page invoke console PAGE presentation.shape-only '{"enabled":true}'
bashy shape page invoke console PAGE presentation.shape-only '{"enabled":false}'
```

Replace PAGE with a listed page ID. Each invoke returns a command ID and queued
status. Poll `result` for completed/failed/expired. A completed `shape.submit`
returns the existing candidate API's acceptance response, not a completed build
or approval. Follow that candidate with the existing shape commands. Never
repeat an invoke merely because transport timed out: its outcome may be unknown.

Use `--console http://127.0.0.1:22749` locally; authenticated remote access uses
`--remote https://HOST/PREFIX --cookie-file SESSION_FILE`. Identity always comes
from the server's existing operator session. Device credentials and cloud
sharees are refused. App and principal namespaces isolate tabs and results.

| Named action | Input | Human equivalent / existing operation |
| --- | --- | --- |
| `shape.open` | `{}` | Open the shared Shape button form |
| `shape.draft` | `{"request": "text"}`; only this field | Edit the request textarea |
| `shape.submit` | `{}` | Make the wish, using the same candidate API |
| `presentation.shape-only` | `{"enabled": true/false}`; omit to toggle | Toggle Shape only in the form |

Shape-only hides ordinary links, navigation and buttons and makes fields inert
and readonly. It preserves the Shape control and restores original hidden,
disabled, readonly, inert and display states when turned off. Protected,
preview and draft-review pages expose no actions. Active review bars prohibit
hiding controls. Existing voice input remains capability-detected in the same
Shape form. This slice does not register every application-specific operation:
apps add explicit handlers using the shared registry, with corresponding server
allowlists and CLI parity; arbitrary DOM selectors and executable code are not
part of the transport.

Agent implementation recipe: share a handler with the human control, register a
named action with a typed input description, allowlist it on the server only for
credentials with the same authority, and verify HTTP authorization plus browser
completion and reversible state. Keep sign-in, pairing, approval and review
outside this action surface. Shutdown and logout invalidate connected pages.
