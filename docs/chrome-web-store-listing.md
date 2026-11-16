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
| Summary | bashy's companion for web pages: lets bashy on your computer read pages and fill in forms in this browser. Talks only to 127.0.0.1. *(manifest description, 131/132 chars)* |
| Category | Developer Tools |
| Language | English |
| Icon | `pkg/browser/live/extension/icons/icon-128.png` (the bashy live mark, `docs/brand/bashy-live/` in the umbrella) |
| Homepage | https://github.com/qiangli/bashy |
| Support | https://github.com/qiangli/yoke/issues |

**Description:**

> bashy live is the companion that lets bashy, the agentic shell on your
> computer, work on web pages in your real, signed-in browser.
>
> Fill in forms: tell bashy what to enter and it fills the fields for
> you, on the sites where you are already signed in. Before it types
> into a password, payment or other secret field, it asks you first.
>
> It can also read a page's text, click, scroll, take screenshots and,
> when you ask, read a site's cookies after you sign in.
>
> How it works: run `bashy browser hub`, then press Connect in this
> extension's popup on the tab you want bashy to work on. The extension
> talks only to bashy on your machine (127.0.0.1). It contacts no other
> server, has no analytics and runs no remote code. Typed and clipboard
> values are logged by length only.
>
> Open source (Apache-2.0): https://github.com/qiangli/yoke

**Screenshots** (1280×800, upload in this order): `docs/chrome-web-store/`
`1-fill-in-forms.png`, `2-asks-before-secrets.png`, `3-connect-a-tab.png`.
Captured 2026-11-16 from a real run: bashy built from main drove extension
0.11.1 in Chrome for Testing through a real hub, filling a demo form. The
terminal lines are bashy's verbatim output, with two substitutions: the
local test URL is shown as `northwind-makerspace.example` (a reserved demo
domain) and the hub port as the default 58082. The popup is the extension's
own `popup.html` with its Chrome calls answered as connected. The
checkbox click is left out: its `clicked: no-effect` report is a known
defect (todo filed alongside).

**Small promo tile** (440×280, optional): skip for an unlisted launch.

## Privacy

**Single purpose:**

> A companion for bashy: lets the bashy program on the user's own
> computer work on web pages in the user's browser (fill in forms, read
> page content, click, take screenshots) over a local 127.0.0.1
> connection, at the user's request.

**Permission justifications:**

| Permission | Justification |
|---|---|
| `tabs` | List, open, switch and address the tabs bashy is asked to drive. |
| `activeTab` | Act on the tab the user connects from the popup. |
| `scripting` | Inject the extension's own bundled page script to read text and fill, click or scroll elements. |
| host access `<all_urls>` | bashy live lets the bashy program on the user's own computer complete multi-step tasks on websites the user chooses at run time, such as filling a form across several pages. activeTab is insufficient: its grant ends when the tab navigates, and every task navigates (form steps, sign-in redirects, confirmation pages), so access would be lost after the first step. A fixed site list is not possible: the user decides which sites bashy works on, and those are unknown when the extension is published. The extension acts only after the user presses Connect in its popup, only on the tabs bashy is asked to drive, and it talks only to bashy on 127.0.0.1. It sends nothing to any server and runs no remote code. *(as submitted 2026-11-16; the dashboard suggested activeTab or a site list, both rejected for this reason)* |
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
