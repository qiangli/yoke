# ycode-live Chrome extension

This extension lets ycode drive **this** Chrome instance — using your
real, logged-in profile — through a local WebSocket to a ycode session.

## License

Apache-2.0. Source bytes shipped in the extension are the same bytes
checked into `internal/runtime/mcpservers/live/extension/` in the ycode
repository; the binary embeds them via `go:embed all:extension`. No
third-party JavaScript, no minified bundles.

## Install (one-time)

1. Run `ycode browser setup live`. ycode prints a path like
   `~/Downloads/ycode-chrome-ext/`.
2. Open `chrome://extensions`.
3. Toggle **Developer mode** (top-right).
4. Click **Load unpacked** → point at the path from step 1.
5. Pin the extension to the toolbar (puzzle-piece icon → pin).

## Use

1. In the Chrome tab you want ycode to control, click the ycode-live
   toolbar icon.
2. Confirm the port matches your ycode setting (default `58082`).
3. Click **Connect this tab**. The status badge turns green.
4. From ycode, run any `browser_*` action — they all target the
   connected tab.

## Wire protocol

Plain JSON over WebSocket; not MCP. See `protocol.go` in the parent Go
package for the request/response shapes. The extension talks only to
`ws://127.0.0.1:<port>/ws` — it never reaches the public internet.

## Files

- `manifest.json` — Manifest V3 declaration, permissions, popup.
- `background.js` — service worker; owns the WebSocket and the
  command dispatch table. `chrome.alarms` keeps it alive.
- `popup.html` / `popup.js` — Connect / Disconnect UI; persists the
  port via `chrome.storage.local`.

## Security model

- Loopback only. The extension never connects to anything other than
  `127.0.0.1`.
- Manifest declares the minimum permissions necessary
  (`tabs`, `scripting`, `activeTab`, `storage`, `alarms`).
- All page automation runs in your real Chrome session. **Treat what
  ycode sees as confidential** — never use this mode for prompts that
  may exfiltrate page contents to a third party without your consent.
