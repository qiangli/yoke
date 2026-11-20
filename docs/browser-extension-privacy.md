# bashy live — browser extension privacy policy

Effective 2026-11-20. Applies to the **bashy live** extension for Chrome
and other Chromium browsers (source: `pkg/browser/live/extension/` in this
repository, Apache-2.0).

## What the extension does

bashy live is bashy's companion for web pages: it lets the bashy program
on **your own computer** work on pages in the browser you install it in.
Mostly that means filling in forms; it can also open pages, read a page's
text, click, take screenshots and read cookies for a site. It acts only after you
press **Connect** in its popup and only while bashy is running.

The popup also has a **Shape** button. When you press it, the extension
sends bashy the tab you are looking at (its address, its title and its tab
number) and the note you typed, so that bashy can pass them to the agent
you named. It sends this only when you press the button.

## Where data goes

- The extension talks to exactly one place: a WebSocket on your own
  computer, `127.0.0.1` (the port you choose, default 58082), served by
  `bashy browser hub`.
- The extension sends **nothing** to the extension's authors, to any
  server, or to any third party. It has no analytics, no telemetry, no
  advertising, and no remote code: every script it runs ships inside the
  extension package.
- What bashy does with page data it receives is up to you and the bashy
  configuration on your computer. For example, you may use bashy with an
  AI model you choose, and bashy may then send that page data to that
  model. That is bashy's processing on your instruction, not the
  extension's.

## What it stores

The extension keeps these settings in the browser's local extension
storage: whether you pressed Connect, the port and tab it connects to, the
popup's "console" checkbox, and the name you last typed in the Shape
button's To field. It also keeps a random identifier for this installed
copy, which it sends only to bashy on your computer so that bashy can tell
two browser profiles apart. Nothing else is stored, and nothing is synced.
A note you type for Shape is not stored by the extension: it is sent to
bashy when you press the button.

## Sensitive fields

Before typing into a field that looks like a password, payment card or
other secret, bashy's guard asks you first. Values it types or reads from
the clipboard are logged by length only, never by content.

## Permissions

Each permission serves the single purpose above: `tabs`, `activeTab`,
`scripting` and host access to read and act on the pages you ask bashy to
drive; `debugger` for trusted keystrokes, screenshots of a background tab,
and reading a page's network, console and performance data when you ask
bashy to debug it; `cookies` to read a site's cookies when you ask (for example, after a
login); `clipboardRead`/`clipboardWrite` for copy and paste actions you
request; `storage` for the settings above; `alarms` to keep the local
connection alive.

## Removing it

Remove the extension from your browser's extensions page. Its stored
settings are deleted with it.

## Contact

Open an issue at https://github.com/qiangli/yoke/issues.
