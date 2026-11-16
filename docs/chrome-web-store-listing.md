# Chrome Web Store listing kit — bashy live

Everything the Chrome Web Store developer dashboard asks for, ready to
paste. Sprint 529, story S529.1 (todo `b4e290d6`). Field names follow the
dashboard's tabs: **Package**, **Store listing**, **Privacy**,
**Distribution**.

## Package

Build the upload zip from a clean yoke checkout:

```bash
bashy dag chrome-ext-release      # or: go run ./cmd/chrome-ext-release -out dist/chrome-ext
```

Upload `dist/chrome-ext/bashy-live-<version>.zip`. The pack is
reproducible: the target packs twice and refuses if the archives differ,
and `.sha256` beside it is the published checksum. The zip holds only
runtime files (manifest, scripts, popup, icons); developer files are left
out.

## Store listing

| Field | Value |
|---|---|
| Name | bashy live *(from the manifest)* |
| Summary | Lets bashy on this computer drive this browser. The extension talks only to 127.0.0.1; it sends nothing to any server. *(manifest description, 118/132 chars)* |
| Category | Developer Tools |
| Language | English |
| Icon | `pkg/browser/live/extension/icons/icon-128.png` (bashy brand mark) |
| Homepage | https://github.com/qiangli/bashy |
| Support | https://github.com/qiangli/yoke/issues |

**Description:**

> bashy live connects this browser to bashy, the agentic shell running on
> your own computer, so bashy can do browser work for you in your real,
> signed-in profile: open pages, read their text, fill forms, click,
> take screenshots and read a site's cookies after you sign in.
>
> How it works: run `bashy browser hub`, then press Connect in this
> extension's popup on the tab you want driven. The extension talks only
> to bashy on your machine (127.0.0.1). It contacts no other server, has
> no analytics and runs no remote code.
>
> Safety: before typing into a password, payment or other secret field,
> bashy asks you first. Typed and clipboard values are logged by length
> only.
>
> Open source (Apache-2.0): https://github.com/qiangli/yoke

**Screenshots** (1280×800 or 640×400, at least one): still to capture —
the popup connected to a tab, and a terminal running a `bashy browser`
command beside the driven page.

**Small promo tile** (440×280, optional): skip for an unlisted launch.

## Privacy

**Single purpose:**

> Let the bashy program on the user's own computer automate this browser
> (navigate, read, fill, click, screenshot) over a local 127.0.0.1
> connection, at the user's request.

**Permission justifications:**

| Permission | Justification |
|---|---|
| `tabs` | List, open, switch and address the tabs bashy is asked to drive. |
| `activeTab` | Act on the tab the user connects from the popup. |
| `scripting` | Inject the extension's own bundled page script to read text and fill, click or scroll elements. |
| host access `<all_urls>` | The user decides which sites bashy drives; the extension cannot know them in advance. It acts only while the user has connected it, and only on the tabs bashy is asked to drive. |
| `debugger` | Send trusted keystrokes and text input that sites accept only from a real user; capture screenshots of a background tab and full-page screenshots without stealing focus; read the driven tab's network requests, console messages and performance trace when the user asks bashy to debug that page. Attached only to tabs bashy is driving, only while the user has connected the extension. |
| `cookies` | Read a site's cookies, including HttpOnly ones, when the user asks bashy to complete a login and capture its session. |
| `clipboardRead`, `clipboardWrite` | Copy and paste actions the user requests through bashy. |
| `storage` | Remember the user's Connect choice, port and tab. |
| `alarms` | Keep the local connection alive and reconnect after the browser suspends the service worker. |

**Remote code:** No. All JavaScript ships in the package.

**Data usage** (operator to confirm before submitting): the extension
handles *website content*, *web history* (the URLs of driven tabs) and
*authentication information* (cookies, when asked), and transmits them
only to bashy on the same computer. Certify that data is not sold, not
used for unrelated purposes and not used for credit decisions.

**Privacy policy URL:**
https://github.com/qiangli/yoke/blob/main/docs/browser-extension-privacy.md

## Distribution

Start **Unlisted**: users install from the link `bashy browser setup live`
opens, with the same review but no search placement. Switch to Public
later in the same tab. Regions: all.

## Operator checklist

1. Register a Chrome Web Store developer account (one-time fee) under the
   chosen publisher; verify the contact email.
2. Capture the screenshots above.
3. New item → upload the zip → fill the four tabs from this kit →
   Submit for review. Expect a longer, manual review because of
   `debugger` and `<all_urls>`. If review refuses them, the fallback is a
   store variant that requests them at Connect time
   (`optional_permissions`, `optional_host_permissions`).
4. When it is published, send the item URL to the conductor:
   `live.ChromeWebStoreURL` gets set, and `bashy browser setup live` then
   opens the store page (`--unpacked` keeps the developer path).

## How a store install behaves

- The store assigns its own extension ID. The hub accepts any ID, so no
  hub change is needed.
- The extension reports its install type in its hello. The hub
  hot-reloads only unpacked copies (`development`); a store install
  (`normal`) is updated by the store.
- Unpacked copies (`~/Downloads/ycode-chrome-ext`) keep working side by
  side. Only one profile holds the hub at a time.
