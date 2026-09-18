// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

// Package webconsole is bashy's browser console: ONE local HTTP surface with a
// start page of tiles, and every other bashy web surface deep-linked beneath it.
//
// The shape is the one docs/agent-interaction-surfaces-design.md settled on: one
// console owning a port, not one cooperative app per verb, because "five
// unrelated tiles is not bashy on the web — one nav, one auth, one design
// system". A verb's --web-ui modifier is meant to deep-link INTO this, never to
// stand up a second server.
//
// # Panels
//
// A panel is either mounted in-process or reverse-proxied, and the rule is not
// about convenience:
//
//	in-process    the panel's state lives in THIS process, or in files this
//	              process already owns (the room, the terminal, the file tree)
//	proxy         another process owns the lifecycle (loom, dag --serve, ycode)
//
// Mounting the room in-process rather than proxying a separate `relay serve` is
// what keeps one lease holder and one transcript: the console adds a transport,
// never a second truth. Proxying anything supervised elsewhere is what keeps the
// console from quietly becoming a process supervisor.
//
// # Discovery
//
// The tile list is not hardcoded. A verb declares a browser UI by carrying an
// atlas.WebSurface, and the console renders whatever the atlas reports, so a new
// surface appears with no console edit. `bashy commands --view web` renders the
// same data in the terminal — one source, two renderers.
//
// # Trust
//
// The gate is meet's: ungated on direct loopback (the machine owner, on their
// own machine), cloud-vouched when arriving through outpost's tunnel, 403
// otherwise. Binding a non-loopback address additionally requires a system
// login, because the terminal panel hands out a shell running as this user and
// the file panel reads this user's files.
//
// The console is a DATA PLANE for the Files panel. That is fine on loopback and
// through outpost's direct tunnel; a download path that rides the cloudbox relay
// would violate the fail-closed data-plane block (dhnt docs/cloudbox-data-plane-block.md).
//
// # Default port and name
//
// Apps defaults to 22749, the standard telephone-keypad encoding BASHY =
// 2-2-7-4-9. Existing explicit 8639 configurations continue to work through
// --port; only the default moves. BASHY expands to "Bashy's Agentic Shell
// Harness Yoke". Bash compatibility describes behavior, not affiliation with
// GNU or the GNU Bash project.
//
// # Chrome
//
// The launcher (index.html) and every standalone managed-app page it owns
// (term.html, board.html, mb.html) share one header/footer look via app.css,
// but only the launcher itself is the "apps" page. Each managed app's own
// logo therefore links back to that app's own root, not to the launcher.
// The footer's copyright line — and, in same-tab mode, the managed app's
// rightmost header button back to the launcher — are injected SERVER-SIDE,
// once, in embed.go's injectChrome, not copied into every embedded HTML
// file, so the launcher's richer, live-rendered footer (build/session
// detail, owned by app.js) and a managed app's copyright-only footer both
// come from the one seam every page already passes through, servePageFile.
// A page mounted elsewhere (files, relay, or any proxied atlas.WebProxy
// tool) ships its own separate frontend and is out of scope for this chrome
// — the console does not own its markup.
//
// # Look settings
//
// The "Open apps" mode is the console's one global look setting: same tab
// (the default — the safe, predictable navigation) or new tab (target=_blank
// with rel=noopener). It is persisted by the SERVER (look.go, ui.json under
// the console's state dir) rather than localStorage because both halves of
// the contract read it at serve/render time — the launcher's tile links and
// the managed apps' return control — and one truth cannot live in N
// browsers. The settings dialog in the launcher is the human surface
// (Same tab / New tab, aria-labelled); GET/PUT api/look is the structured,
// composable one. A missing, corrupt or hand-mangled document serves the
// same-tab default and is logged, never a boot failure; writes are atomic,
// and unknown values are rejected loudly with the two valid ones named.
package webconsole
