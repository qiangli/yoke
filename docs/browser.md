# `browser` — CDP-backed browser automation

`bashy browser` (also `coreutils browser`) drives Chrome/Chromium over the
DevTools protocol. One command, two dials: a **mode** (how it gets a browser)
and a **subcommand** (the action). Add `--json` for structured envelopes — the
agent-facing form, and the default under `$BASHY_AGENTIC`.

Engine: `yoke/pkg/browser` (`probe` / `solo` / `live` clients + the
`wire` action/result protocol); command: `yoke/cmds/browser`.

## Modes (`--mode`)

| mode | how it gets a browser | setup | best for |
|---|---|---|---|
| **`solo`** *(default)* | launches a **private headless Chrome** (`--headed` to show it), runs one action, exits | none | one-shot scrapes / automation — the zero-setup default |
| **`probe`** | attaches to a **Chrome you already started** with `--remote-debugging-port=9222` (override with `--probe-url`) | start Chrome yourself | a persistent session you control across many calls |
| **`live`** | drives your **real, logged-in Chrome** via an MV3 extension + a local WebSocket hub | `browser hub` + `browser setup` | pages exactly as *you're* logged in (cookies/SSO intact) |

**Session model:** `probe` and `live` attach to a *persistent* browser, so
multi-step flows (`navigate` → `click` → `extract`) keep state across separate
`bashy browser …` calls. **`solo` is one-shot** — each invocation is a fresh
Chrome, so a successful `navigate <url>` returns the loaded page itself
(`title`/`url`/`content`); a standalone `extract`/`eval` in solo mode would only
see `about:blank`. Scrape in solo mode with a single `navigate` (or an `eval`
whose script is self-contained).

> The default is **`solo`** — `bashy browser navigate URL` just works with no
> setup. Switch to `--mode probe` (attach to your Chrome) or `--mode live` (your
> logged-in Chrome) when you want a persistent or authenticated session.

## Subcommands

**Every subcommand has its own help page** — `browser <subcommand> --help`
states its operands, which modes implement it, and its output contract.

### Capability matrix

| action | solo | probe | live | notes |
|---|:--:|:--:|:--:|---|
| `status` | ✅ | ✅ | ✅ | per-mode fields; a field that does not apply to the mode is omitted, not defaulted |
| `navigate <url>` | ✅ | ✅ | ✅ | solo: this *is* the scrape — returns title/url/content |
| `extract [scope]` | ✅ | ✅ | ✅ | carries `viewport`; `--include-hidden` |
| `eval '<js>'` | ✅ | ✅ | ⚠️ | live compiles a string in the page's MAIN world → **refused by a strict page CSP**; use `dispatch-event`, or `text` to read |
| `dispatch-event <name>` | ✅ | ✅ | ✅ | the CSP remedy — nothing is compiled from a string |
| `text [selector]` | ✅ | ✅ | ✅ | visible text without eval — answers under a strict CSP; a password input reads as its name only |
| `click <sel>\|<index>` | ✅ | ✅ | ✅ | index resolves through the same enumeration `extract` prints; reports `navigated` / `dialog` / `dom-changed` / `no-effect` |
| `type <sel> <text>` | ✅ | ✅ | ✅ | |
| `wait-for-selector <sel>` | ✅ | ✅ | ✅ | `--timeout-ms`, `--state visible\|attached\|detached` |
| `screenshot` | ✅ | ✅ | ✅ | writes a file by default; `--base64`, `--full-page` (stitches inner scroll areas; `data` names `stitched_full_page` vs `full_page`), `--settle-ms` |
| `cookies-get [name [domain]]` | ✅ | ✅ | ✅ | live sees HttpOnly cookies (chrome.cookies) |
| `scroll [up\|down] [amount] [selector]` | ✅ | ✅ | ✅ | without a target the window scrolls; with one its nearest scrollable ancestor moves |
| `keyboard-press` · `back` | ✅ | ✅ | ✅ | |
| `tabs list` | ✅ | ✅ | ✅ | typed records under `--json` |
| `tabs switch\|new\|close` | ❌ | ❌ | ✅ | live only; address with `--url`/`--title` |
| `clipboard-*` · `storage-get` | ❌ | ❌ | ✅ | needs the extension's chrome.* permissions |
| `network-list` · `console-get` · `perf-*` | ✅ | ✅ | ✅ | CDP/debugger-backed |
| `fetch <url>` | n/a | n/a | n/a | plain HTTP — see below |
| `hub` · `setup` | n/a | n/a | ✅ | live-mode plumbing |
| `login` | ✅ | ✅ | ✅ | `--success-url`, `--token-selector`, `--cookie`, `--timeout`, `--dry-run` |

### Tab and field addresses

In live mode, every action honors `--id`, `--url` and `--title`.
Combined constraints must identify the same tab; ambiguous matches fail with
candidate details. An unaddressed `navigate` opens a new tab. Address it
explicitly when navigation should replace an existing page.

Field actions share CSS, `xpath=` (or `//`), `text=` and `label=` selectors.
Open shadow roots use `>>>`; same-origin frame selectors retain their frame
qualification. Reuse an extracted `selector` verbatim. Extraction rows add
`stable_id`, `label`, `selector` and `name`; treat `stable_id` as opaque.
An index is valid only for the extracted field identity: a shifted index
write is refused instead of filling a different field. With CLI `--json`,
`data` is a decoded object containing `viewport` and `elements`, not a
second JSON string to parse.

### Output contracts

| action | stream / encoding | envelope fields |
|---|---|---|
| `screenshot` | **path on stdout** (a PNG is written); `--base64` for inline | `path`, or `image` with `--base64` |
| `extract` | element listing on stdout | `content`, `elements`, `total`, `truncated`, `viewport`, `data` = `{viewport, elements[]}` |
| `tabs list` | `[N] title / url` on stdout | `content`, `data` = `[{index,id,title,url,active,driven}]` |
| `click` | `clicked: navigated` / `dialog` / `dom-changed` / `no-effect` on stdout | `content`, `data.effect`; **`success:false` with a reason when nothing resolved** |
| `type` | `typed` on stdout | `content`; **`success:false` with a reason when nothing resolved** |
| `text` | the rendered text on stdout | `content` (a password input reads as its name only) |
| `scroll` | the new position on stdout | `content` (names the panel for an addressed scroll) |
| `dispatch-event` | one line on stdout | `content`, `data` = `{event,target,default_prevented}` |
| `eval` | the value on stdout | `data` |
| `cookies-get` | JSON on stdout | `data` = array of cookie objects |
| everything else | one line on stdout | `success` plus whichever of `title`/`url`/`content`/`data` applies |

**Nothing writes outside `rc.Out`/`rc.Err`.** `browser hub` is the one
subcommand that logs — it is a long-running foreground process. Every other
invocation is silent, so `browser --json … >out 2>err` puts exactly one JSON
document in `out` and nothing anywhere else. (Redirecting both streams and
still seeing output on the terminal was a real defect: the live hub logged
through the process-level `slog` default, which is not the stream a bashy
builtin is handed.)

### `browser fetch` ≠ `bashy fetch`

`bashy fetch <url>` is a **plain HTTP/REST client** (no browser) — use it for
APIs and static pages. `browser fetch <url>` renders through Chrome (runs JS) —
use it (or `navigate`+`extract`) for JS-heavy pages.

## Result envelopes (`--json`)

Every action returns a `wire.Result`: `success` (bool), and on success one of
`title`/`url`/`content`/`elements`/`data`/`path`/`image` depending on the
action; on failure, `error`. Pipe to `jq`. Example — a solo scrape:

```sh
$ bashy browser --mode solo --json navigate \
    'data:text/html,<title>Demo</title><h1>Hello from headless Chrome</h1>'
{"success":true,"title":"Demo","url":"data:…","content":"Hello from headless Chrome"}
```

## Examples

**Solo (no setup — one-shot):**
```sh
bashy browser --mode solo --json navigate https://news.ycombinator.com
bashy browser --mode solo --json eval 'document.querySelector("h1").innerText'
bashy browser --mode solo --headed navigate https://example.com   # watch it run
```

**Probe (attach to a Chrome you started — persistent):**
```sh
"$CHROME" --remote-debugging-port=9222 &        # your Chrome/Chromium path
bashy browser status                             # reachable: true
bashy browser navigate https://example.com
bashy browser eval 'document.title'
bashy browser screenshot shot.png
```

**Live (your real logged-in Chrome):**
```sh
bashy browser hub          # start the local WebSocket hub (keep running)
bashy browser setup        # install/connect the MV3 extension (one-time)
bashy browser --mode live navigate https://your-app.example
bashy browser --mode live --json extract
```

### A worked live-mode session

```sh
bashy browser hub &                                  # 1. hub (keep running)
bashy browser setup live                             #    one-time: load unpacked in Chrome
bashy browser --mode live --json status              # 2. is it actually reachable?
# {"mode":"live","hub_port":58082,"hub_up":true,"reachable":true,
#  "extension_connected":true,"extension_version":"0.7.0", ...}

bashy browser --mode live --json tabs list --url localhost:5478   # 3. find the tab
bashy browser --mode live tabs switch --url localhost:5478        #    select it BY URL, not index
bashy browser --mode live --json extract | jq '.data.elements[]'  # 4. what can I click?
bashy browser --mode live --json click --index 36                 # 5. click it
bashy browser --mode live screenshot --output /tmp/after.png      # 6. capture THAT tab
```

Three things that session depends on, each of which was once broken:

- **Step 5 fails loudly if nothing resolved — and reports what happened when
  something did.** A click that matches no element is `success:false` with a
  reason, and both the index and selector forms return the same envelope.
  A click that resolved reports its OBSERVED effect within a bounded watch:
  `clicked: navigated`, `clicked: dialog`, `clicked: dom-changed`, or
  `clicked: no-effect` (the click landed; the page did not react).
  (`click 36` used to be sent as the CSS selector `"36"`, which is invalid
  CSS; the injected `querySelector` threw, the extension returned an empty
  frame, and the caller was told `{"success":true}`.)
- **Step 6 captures the tab step 3 selected**, not whatever is in the
  foreground, and waits for a paint first. `chrome.tabs.captureVisibleTab`
  captures a *window's* visible tab, so driving a background tab produced a
  real, well-formed PNG of a different page — a failure that reads as a
  success. Non-foreground and `--full-page` captures now go through CDP
  `Page.captureScreenshot`, which addresses the tab itself. `--full-page`
  additionally stitches inner scroll areas tile by tile, so every panel
  appears at its full content height; page and panel positions are restored
  even when the stitch fails. A failed stitch is an explicit error, never a
  silent single capture: once panels exist that fallback would be missing
  rows below each panel's fold.
- **Step 2 tells the truth.** `status` used to answer `mode "live" is not
  supported` while every sibling subcommand drove live mode happily.

### When `eval` is refused

A page whose Content-Security-Policy omits `'unsafe-eval'` — increasingly the
default — disables `eval` in live mode outright:

```
Evaluating a string as JavaScript violates the following Content Security
Policy directive because 'unsafe-eval' is not an allowed source of script…
```

That is the page's policy, not a bashy limit. The remedy is `dispatch-event`,
which fires a named event without compiling anything from a string — and
`text`, which reads visible text the same way:

```sh
bashy browser --mode live dispatch-event toggle-activity-panel
bashy browser --mode live dispatch-event my:event --detail '{"open":true}' --on '#root'
bashy browser --mode live text                  # the whole page, every frame
bashy browser --mode live text '#price'         # one element; password inputs read as their name only
```

### Extension version floor

The hub refuses to dispatch to an extension older than **0.7.0** and names the
remedy (`bashy browser setup live`, then reload at `chrome://extensions`). The
floor is deliberate rather than cosmetic: before 0.7.0 a screenshot could
capture the wrong tab, and no Go-side change can detect that from the outside —
the image is real, just of the wrong page.

The page-side **sensitive-input field check** (below) arrived in extension
**0.9.0**. Rather than raise the floor for every method, the hub gates on the
advertised `guard_field` method: `type`, `select` and `keyboard-press` on an
older extension are refused loudly ("no sensitive-input field check … reload
it"), while navigate/extract/screenshot keep working. A guarded field is never
filled silently by a stale extension. The by-reference fill of an operator's
answer (`guard_fill`) arrived in **0.9.1**; on 0.9.0 a "provide" answer stops
(`fill-failed`) rather than typing anything.

## Sensitive-input guard (enforced, every mode, every site)

bashy does not ask agents to keep SSNs, card numbers or passwords out of web
forms — it enforces it. One gate (`pkg/browser/guard`) sits in front of every
backend's `Execute` (live, probe, solo) and in front of the live hub's raw
`/dispatch`:

1. **Value check** — the text of `type`, `select`, `eval`, `dispatch-event`,
   clipboard writes, `navigate` URLs and printable `keyboard-press` keys is
   checked against strict validators (`pkg/sensitive`, shared with the
   operator's clipboard check) — SSN/ITIN (`ddd-dd-dddd`, SSA/IRS shape
   rules), EIN (`dd-ddddddd`, IRS prefixes), ABA routing (9 digits, Fed prefix,
   3-7-1 checksum), payment cards (Luhn + issuer prefix/length), IBAN
   (registered length + mod-97) — and against the bashy vault's values (exact
   or embedded; only keyed digests are kept, in a 0600 cache refreshed every
   10 minutes through the secrets client). Order numbers, phone numbers,
   dates, ZIP codes and amounts pass.
2. **Field check** — before a `type` (or printable keypress) the page-side
   `guard.js` inspects the target WITHOUT the value: `type=password`,
   `autocomplete` `username`/`current-password`/`new-password`/
   `one-time-code`/`cc-*`, and the field’s own name/id/associated `<label>`/aria/placeholder
   identifying ssn, tax id, ein, routing, bank account/account number, payment
   card/card number/security code, password or username. Generic “account” or
   “card”, surrounding prose and nearby question headings do not classify it.
   `page.js` refuses such a field on its own too, so a client that skipped the
   gate still cannot fill it.
3. **Human path** — a refusal asks the operator ONE question in a native
   dialog (`pkg/ask` forms, through the `guard.HumanAsker` the host installs —
   `pkg/browser/guardask`), naming the site, the question and the reason:
   - **Enter it here** / **Use clipboard** — the answer goes to a 0600 sink
     under an ask id (the clipboard is checked against the field kind with the
     same checksums, and cleared only if unchanged); bashy fills the field
     **by reference** from that id. The value never reaches the agent, its
     stdout, receipts or logs.
   - **I will type it myself** — bashy focuses the field in your browser; type
     it there, then answer **Done**. (Not offered for a headless Chrome.)
   - **Skip this question**, or **Stop** filling this form. No answer within
     10 minutes, a dismissed dialog or any error is **stop**, never a default
     fill.
   A provided answer is *admitted* once, for that ask id and that field only;
   `browser type --from-ask <id> <target>` performs that fill (the guard runs
   it itself; the verb exists so a fill that failed before the value was
   taken can be retried). It never takes text. The live hub accepts the
   extension's `guard_fill` only for an admission the guard claimed for the
   same field.
4. **Read access and write privacy** — read-only results pass through without
   value-based blocking or blanket masking. Password input values remain
   suppressed in structured extraction and visible-text results. Sensitive
   write results and operator-answer records retain masking. Screenshots
   remain available as reads.
5. **Protected** — no flag, environment variable or site-skill metadata turns
   it off. `pkg/browser/guard`, `pkg/sensitive`, `pkg/browser/guardask`,
   `live/guard.go`, `extension/guard.js` and `cdpactions/guard.go` are rows of
   `pkg/src`'s protected manifest, so a change merges only with a
   `bashy approve`d src approval.

What the caller sees (the contract site skills build on):

| outcome | stdout | exit | `--json` `outcome_class` |
|---|---|---|---|
| the operator provided it; bashy filled it | `guard:filled-by-human` | 0 | `guard_filled_by_human` |
| the operator typed it in the browser | `guard:human-typed` | 0 | `guard_human_typed` |
| the operator skipped the question | `guard:skipped` | 4 | `guard_skipped` |
| stop / timeout / no human channel / fill failed | `guard:stopped` | 5 | `guard_stopped` |

`--json` also carries `reason` (`field:label-ssn`, `field:type-password`,
`value:luhn`, `value:vault`, …) and `data` `{choice, via, reason, site,
question}` plus `ask_id` when filled by reference. With no human channel
installed (a host that never called `guardask.Install`) a refusal is
`guard:stopped` immediately.

Test seam: `BASHY_BROWSER_GUARD_ASK=fake:provide|fake:clipboard|fake:self|fake:type-myself|fake:skip|fake:stop|fake:timeout`
answers as a scripted operator (`fake:provide` fills
`BASHY_BROWSER_GUARD_ASK_VALUE`, default `human-provided`, by reference). It is
honoured only inside a `go test` binary or a build made with
`-tags bashy_guard_fake` — never a release build — and none of its answers can
put the AGENT's text into a guarded field.

Known gaps: screenshots; an `eval` that BUILDS a value at run time (the
literal text is checked, not what the script computes); bare 9-digit SSNs
(taken as order numbers — the field check still catches an SSN question).

**Automated login (capture a token/cookie):**
```sh
bashy browser login --success-url /dashboard --cookie session --timeout 3m
bashy browser login --token-selector '#api-token' --dry-run   # preview what it polls
```

## Notes

- Unknown flags/subcommands fail with a clear exit-2 error, never a silent
  guess — and the error **names the valid set** (`unknown tab action "4";
  expected one of: list, switch, new, close`). So does an unrecognised
  positional: `screenshot /tmp/a.png extra` is exit 2, not a discarded argument.
- **Address tabs by `--url` / `--title`, not by index.** An index captured at
  the start of a script is stale the moment any tab opens or closes. An
  ambiguous substring is an error listing the candidates, never a silent pick.
- `--json` returns **data, not rendered display strings**: `tabs list` gives an
  array of `{index,id,title,url,active,driven}` and `extract` gives
  `{viewport, elements[]}` with per-element `visible` and `box`.
- `solo` and `probe` prefer an installed Chrome, Chromium, or Edge. When none
  is installed, bashy provisions its SHA-256-pinned headless Chromium build in
  the binary cache; on minimal Linux it also provisions the pinned NSS/NSPR and
  alsa-lib runtime files required by that child. `--chrome-path` selects an
  explicit browser and `--user-data-dir` selects a persistent profile.
- Headless Chrome has its own network stack — if `navigate <live-url>` times out
  with `context deadline exceeded` while `bashy fetch <url>` succeeds, the
  browser's network path is being blocked (e.g. a restricted sandbox), not bashy.
