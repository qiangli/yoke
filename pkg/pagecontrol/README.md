# Private live-page actions

This package supplies a small shared implementation, not an external page
standard. Apps define their own action names, input validation and handlers.
UI controls call those same handlers. Existing server APIs and CLIs continue
to own domain operations.

Construct `pagecontrol.New(pagecontrol.Options{AllowAction: allowed})` once per
authority domain. The allowlist defaults to deny. Mount `Hub.Serve(w,r,principal)`
behind existing authentication and cookie-CSRF middleware, stripping the mount
to these paths:

| Method/path | Purpose |
| --- | --- |
| GET `/events` | Same-origin browser websocket; announces mounted actions |
| GET `/pages` | Caller-visible connected pages, labels and actions |
| POST `/invoke` | `{page_id, action, input}`; returns 202 with command ID |
| GET `/result?id=...` | Caller-visible command outcome |

The principal is a canonical server-authenticated identity, never a request
parameter. Partition it further by app or authenticated session where those
must be isolated. Call `ClosePrincipal` on logout/revocation and `Close` when
the application shuts down. Every socket expires after five minutes and must
re-authenticate to reconnect. Also respect the application's shorter session
expiry: middleware or a session invalidation hook must close affected pages.

Only allow actions whose authority is shared by **every** credential allowed
to invoke this hub. A browser with an elevated cookie must not become a proxy
for a CLI bearer that cannot perform the same operation. Prefer presentation
and draft-input actions here; submit domain changes through existing server
APIs with their usual scopes, validation, approval and elevation. Never enable
a generic script evaluator, selector clicker, arbitrary fetch or unrestricted
form submission.

Serve `Script` or `ServeScript` before the app's own scripts. The plain-JS API:

```js
const unregister = BashyPage.register("settings.draft", {
  description: "Set the editable networking draft",
  run: setNetworkingDraft // validates input; same function used by UI
});
const stop = BashyPage.connect({url: "api/page/events", label: "Settings"});
// Both lifecycle operations are explicit:
unregister();
stop(); // or BashyPage.disconnect()
```

`connect` resolves URLs against `document.baseURI`, so reverse-proxy prefixes
work without a hard-coded root. It refuses foreign origins. Registration does
not imply authorization. `describe()` supplies discovery metadata and
`invoke(name,input)` awaits the actual handler result. Mount/unmount handlers
with their page; clear draft state separately on identity change.

A page receives an unpredictable ID on every connection. Commands are delivered
at most once. A lost connection marks an undelivered command failed and an
in-flight command expired with an unknown outcome; reconnect never replays it.
Results remain available for roughly five minutes after expiry. The default
command TTL is 30 seconds. There are at most 16 pages and 128 retained commands
per principal, 256 pages and 2048 retained commands per hub, and 16 queued
commands per page. Action input/result payloads are bounded to 32 KiB.

These are ephemeral live interactions. Persistent settings, revisions and
undo belong to the application's existing store; this hub does not create a
second persistence or domain-logic layer.

Focused checks:

```sh
GOWORK=off go test -race ./pkg/pagecontrol
node --test pkg/pagecontrol/page_test.cjs
```
