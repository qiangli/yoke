# bashy live Chrome extension

Lets `bashy browser --mode live` drive **this** Chrome — your real,
logged-in profile — through a local WebSocket to `bashy browser hub`.
(The manifest name `ycode-live` and the folder `ycode-chrome-ext` are
kept so an extension loaded before the bashy migration keeps working.)

## License

Apache-2.0. The bytes here are embedded in the bashy binary
(`go:embed all:extension`). No third-party JavaScript, no minified
bundles.

## Install (one-time)

1. Run `bashy browser setup live`. It extracts the extension to
   `~/Downloads/ycode-chrome-ext/`, opens `chrome://extensions`, opens
   the folder, and copies its path to the clipboard (`--no-open` skips
   the desktop actions).
2. In `chrome://extensions`, turn on **Developer mode**, then drag the
   folder onto the page (or **Load unpacked** and paste the path).
3. On the tab to drive, click the extension icon → **Connect this tab**.

Developer mode + Load unpacked is the one human step left; it goes away
with a Chrome Web Store listing (the `chrome-ext-*` release targets
build the upload package).

## After the first Connect

- The extension remembers the Connect until you press Disconnect: it
  reconnects by itself after a Chrome restart, a worker restart, or a
  hub restart (a periodic alarm retries while the hub is down).
- Upgrades are zero-touch: when an older extension connects, the hub
  refreshes `~/Downloads/ycode-chrome-ext/` from the binary and asks the
  extension to reload; it reconnects running the new code. (Extensions
  older than 0.8.0 cannot self-reload — reload them once at
  `chrome://extensions`.)

## Wire protocol

Plain JSON over WebSocket; not MCP. See `protocol.go` in the parent Go
package. The extension talks only to `ws://127.0.0.1:<port>/ws`.

## Files

- `manifest.json` — Manifest V3 declaration, permissions, popup.
- `background.js` — service worker; owns the WebSocket, the dispatch
  table, reconnect, and frame routing.
- `page.js` — the page-side core (extract, click, type, select, …),
  injected into every frame; fixture-tested by `page_test.go`.
- `guard.js` — the sensitive-input field check (password / autocomplete /
  label / visible question text), injected before `page.js`; it never reads a
  field's value. A protected path: see docs/browser.md "Sensitive-input guard".
- `frames.js` — pure helpers that number elements tab-wide across
  frames and route an index back to its frame.
- `popup.html` / `popup.js` — Connect / Disconnect UI.

## Security model

- Loopback only. The extension never connects to anything other than
  `127.0.0.1`.
- All page automation runs in your real Chrome session. **Treat what
  the agent sees as confidential** — never use this mode for prompts
  that may exfiltrate page contents to a third party without your
  consent.
