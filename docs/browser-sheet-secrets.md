# Copy-sheet secrets by reference: threat model

Sprint 529, story S529.7 (served copy sheet), with S529.8 (Remember store)
building on it. Written before the code, as the story requires.

## What it does

A copy-sheet field may name a secret instead of carrying a value:

```json
{"label": "API key", "secret": "openai-key"}
```

The sheet the agent writes holds only the name. The live hub serves the
sheet; the field shows a mask with **Reveal** (about 10 s) and **Copy**. On a
click the page asks the hub, which reads the value from the local-only sheet
store and returns it to that page. The value flows store → hub → page →
clipboard (or the screen while revealed). It is never in the HTML file, the
agent's output, stdout, or a log line.

The store (`$BASHY_HOME/browser/sheet-secrets/`, directory 0700, one 0600
file per name) is local-only: it is never synced and is not the cloudbox
vault (operator decision, S529.8).

**Entry (S529.8).** A named secret with no stored value shows an entry box.
The human types the value there and saves it, which POSTs it to the same
endpoint (`op: put`) under the same Origin, fetch-metadata and token rules.
With **Remember** the value goes into the store, for later sheets. Without
it, the value is held once (`.once/NAME`, 0600) for a fill by reference: the
first `TakeSheetSecret` consumes it (an atomic rename, so it is taken at most
once), and it lapses after 15 minutes. Either way the response, the
`copysheet --json` output (`pending` names) and the hub log carry only the
name, and the hub adds the value to its redaction set.

**Fill by reference (S529.9).** When the guard would need several values
from the human, the agent lists them as named secrets on one sheet; the
human enters them there; the agent fills each field with
`browser type --from-sheet NAME <target>`. The guard records and claims its
usual per-field admission (so the live hub's `guard_fill` check holds),
takes the value itself (a held-once entry is consumed) and fills and
verifies by reference, as for `--from-ask`. The hub records the driven tab's
site when it registers the sheet; an entry saved there carries that site,
the sheet tells the human which site it is for, and the guard refuses a
fill on any other site (`from-sheet:other-site`) or for an entry with no
site. This keeps an agent from carrying a value the human entered for one
site to another.

## Assets

The secret values in the sheet store.

## Who we defend against

| Adversary | Defence |
|---|---|
| **A web page in the user's browser** (another origin, DNS rebinding to 127.0.0.1) | The hub answers only Host 127.0.0.1/localhost/[::1] (`loopbackHost`). The secret endpoint requires `Origin` equal to the hub's own origin and refuses `Sec-Fetch-Site` other than `same-origin`. It takes a POST with a JSON body, so a cross-site form or image cannot call it. The sheet URL carries a random 128-bit token, and the sheet is served `X-Frame-Options: SAMEORIGIN`, `no-store`, `no-referrer`. |
| **Another local user** | The hub listens on loopback only. The store and its directory are owner-only (0600/0700). |
| **The agent seeing a value by accident** (the main risk: a value landing in the agent's context, transcript or logs) | The agent names secrets, never handles them: `copysheet` output lists names and labels only. The hub **redacts every value it has served** from all extension results passing through it (`text`, `extract`, `eval`, …), replacing it with `[guarded:sheet]`, so reading the sheet while a value is revealed, or reading the form after the human pastes, does not carry it back. Values are never logged. |
| **A stale or leaked sheet URL** | Sheet tokens expire (30 min after registration). A sheet may ask only for the names it was registered with. Nothing is served while the hub gate is paused (S529.10). |
| **An agent carrying an entry to another site** | Each entry records the site the sheet was served beside; `--from-sheet` fills only that site and the sheet shows it to the human. |
| **The clipboard outliving the paste** | After Copy, the page clears the clipboard about 30 s later if it still holds the value (where the browser lets the page read it; otherwise it clears unconditionally). |

## Out of scope (stated, not solved)

- **A deliberately hostile process running as the same user.** It can read
  0600 files and call loopback like any other client, which is the same
  boundary as `bashy ask` sinks and the guard's admissions. Encrypting the
  file would not help: the key would have to sit beside it (the reason the
  operator rejected encryption in the sheet file).
- **Pixels.** A screenshot taken while a value is revealed shows it. Reveal is
  human-initiated and short.
- **The destination page.** Once pasted, the value belongs to the form the
  human chose. Redaction covers the hub's own results, not other software.

## Acceptance (from the story)

Unit and e2e: the reveal/copy round trip; refusals for a bad token, an
expired token, a foreign Origin, a cross-site fetch, an unregistered name and
the paused gate; the value absent from the sheet file, the command output and
the hub log; redaction of a served value in a dispatched result.
