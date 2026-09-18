// bashy apps — the launcher.
//
// A pill search, then Overview, Favorites, Recent and the app grid, each section
// toggleable, with a settings sheet for background, theme, section visibility
// and recent depth.
//
// Plain JS on purpose: the launcher ships inside a Go binary with no build step,
// and it is bashy's own surface — free to evolve without reference to anything
// else that happens to render tiles.
//
// EVERY url is built from document.baseURI. The server rewrites <base href> per
// request, so the same bytes work at / on loopback and under outpost's
// /matrix/h/<host>/app/<name>/ prefix without knowing which it is.
const url = (p) => new URL(p, document.baseURI);

// A script error used to leave a blank card and no clue — the page simply
// stopped painting. Surface it instead: a launcher that cannot render should say
// so on the launcher.
addEventListener("error", (e) => showFatal(e.message || String(e.error)));
addEventListener("unhandledrejection", (e) => showFatal(String(e.reason)));
function showFatal(msg) {
  const host = document.getElementById("grid-host") || document.body;
  if (document.getElementById("fatal")) return;
  const box = document.createElement("div");
  box.id = "fatal";
  box.className = "fatal";
  const h = document.createElement("strong");
  h.textContent = "The launcher failed to render.";
  const p = document.createElement("p");
  p.textContent = msg;
  box.append(h, p);
  host.prepend(box);
}

const view = document.getElementById("grid-host");
const whoEl = document.getElementById("who");
const bgEl = document.getElementById("bg");
const searchEl = document.getElementById("search");
const footLeft = document.getElementById("foot-left");
const verEl = document.getElementById("ver");

let apps = [];
// The Inbox tile's numbers, or null when this session may not read them.
let inboxCounts = null;
let search = "";

// ------------------------------------------------------------------- look --
// The GLOBAL "Open apps" mode, persisted by the SERVER (api/look), not
// localStorage: the server also conditions the managed apps' return control
// on it, so there must be one truth or the two halves of the setting drift.
// "same-tab" (the default, and the fallback whenever the look API cannot be
// reached) navigates this window; "new-tab" opens each app in its own tab
// with rel=noopener.
let openApps = "same-tab";

// ------------------------------------------------------------------ config --
// localStorage only: this is per-viewer chrome, not state anything else reads.
// Every access is guarded — a private window or blocked site data throws on
// access rather than returning null.
const CFG_KEY = "bashy.apps.config";
// Bump when a default changes in a way a previously-saved config would mask.
const CFG_VERSION = 2;
const DEFAULTS = {
  v: CFG_VERSION,
  // A real background by default. The glass card only engages over one (a
  // translucent panel on a flat colour is just a muddier flat colour), so
  // defaulting to "none" meant the launcher shipped looking like an unstyled
  // page unless you went hunting in settings for the thing that styles it.
  background: "sky",
  theme: "system",
  showSummary: true,
  showFavorites: true,
  showRecents: true,
  recentLimit: 8,
  favorites: [],
  recents: [],
};
function loadCfg() {
  let saved = {};
  try {
    saved = JSON.parse(localStorage.getItem(CFG_KEY) || "{}");
  } catch (_) {
    return { ...DEFAULTS };
  }
  // A saved config shadows every default, and the whole config is persisted on
  // any tile click (see recordVisit) — so a single click under an older build
  // pinned that build's defaults forever, and changing one later had no visible
  // effect for anyone who had already used the page. Migrate the fields whose
  // default actually moved rather than silently losing to a stale value.
  if ((saved.v || 1) < 2) {
    delete saved.background; // was "none"; the launcher now ships a real one
    saved.v = CFG_VERSION;
  }
  return { ...DEFAULTS, ...saved };
}
let cfg = loadCfg();
function saveCfg(patch) {
  cfg = { ...cfg, ...patch };
  try { localStorage.setItem(CFG_KEY, JSON.stringify(cfg)); } catch (_) {}
  applyChrome();
}
const THEME_ICON = { system: "auto", light: "sun", dark: "moon" };

function applyChrome() {
  const bg = cfg.background || "none";
  bgEl.setAttribute("data-bashy-bg", bg);
  // The glass card and glassy header only make sense over a real background.
  document.body.classList.toggle("has-bg", bg !== "none");
  const tb = document.getElementById("theme-btn");
  if (tb) {
    tb.replaceChildren(svgIcon(SVG[THEME_ICON[cfg.theme] || "auto"]));
    tb.title = "Theme: " + cfg.theme;
  }
  const sb = document.getElementById("settings-btn");
  if (sb && !sb.firstElementChild) sb.replaceChildren(svgIcon(SVG.settings));
  const root = document.documentElement;
  if (cfg.theme === "system") root.removeAttribute("data-theme");
  else root.setAttribute("data-theme", cfg.theme);
}

function isFav(name) { return cfg.favorites.includes(name); }
function toggleFav(name) {
  const f = isFav(name) ? cfg.favorites.filter((n) => n !== name) : [...cfg.favorites, name];
  saveCfg({ favorites: f });
  render();
}
function recordVisit(name) {
  saveCfg({ recents: [name, ...cfg.recents.filter((n) => n !== name)].slice(0, 24) });
}

// ------------------------------------------------------------------- icons --
// Crisp SVG marks rather than Unicode glyphs.
//
// A character like the folder or gear codepoint renders as whatever the host
// font happens to have — a different weight, a different optical size, sometimes
// a colour emoji — so a grid of them never looks like one set. These are drawn
// on a 24 grid at a single stroke weight, which is what makes the row read as a
// family. Unknown apps still fall back to their initial: a letter is honest
// about being a placeholder, and inventing a picture for something we know
// nothing about would not be.
const SVG = {
  terminal: "M5 8l4 4-4 4M13 16h6",
  files:    "M4 8a2 2 0 0 1 2-2h3.5l2 2H18a2 2 0 0 1 2 2v6a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2z",
  meet:     "M4 7.5a2 2 0 0 1 2-2h8a2 2 0 0 1 2 2v4a2 2 0 0 1-2 2H8.5L4.5 16.5zM18.5 10.5H19a2 2 0 0 1 2 2v5l-2.8-2H13",
  // A DAG is nodes and edges; a single stroked path could only ever suggest it.
  dag:      { paths: ["M7.6 8.4l3.2 6", "M16.4 8.4l-3.2 6"],
              circles: [[6, 6.5, 2.1], [18, 6.5, 2.1], [12, 17.3, 2.1]] },
  loom:     "M6 4.5v15M18 4.5v15M6 9.5h12M6 14.5h12",
  // An envelope: the message board is mail, not a conversation.
  mb:       "M4 8a2 2 0 0 1 2-2h12a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2zM4.4 8.6L12 14l7.6-5.4",
  // Columns of differing height: a kanban, read at a glance.
  sprint:   { paths: ["M6 19V9", "M12 19V5", "M18 19v-6", "M4 21h16"] },
  // A tray with mail arriving into it. Deliberately NOT the envelope: the
  // message board next door is already an envelope, and two apps sharing a
  // mark is the one thing a grid of icons must not do.
  inbox:    { paths: ["M4 14h4l1.6 2.4h4.8L16 14h4M4 14l2.4-7.2h11.2L20 14v5.5H4z"] },
  settings: "M12 15.5a3.5 3.5 0 1 0 0-7 3.5 3.5 0 0 0 0 7zM19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 1 1-4 0v-.09a1.65 1.65 0 0 0-1.08-1.51 1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 1 1 0-4h.09A1.65 1.65 0 0 0 4.6 8.9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 1 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 1 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z",
  moon:     "M20 14.5A8.5 8.5 0 1 1 9.5 4a6.5 6.5 0 0 0 10.5 10.5z",
  sun:      "M12 17a5 5 0 1 0 0-10 5 5 0 0 0 0 10zM12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4",
  auto:     "M12 3a9 9 0 1 0 0 18zM12 3a9 9 0 0 1 0 18",
  star:     "M12 4.5l2.3 4.7 5.2.8-3.8 3.6.9 5.1-4.6-2.4-4.6 2.4.9-5.1L4.5 10l5.2-.8z",
  logout:   "M15 17l5-5-5-5M20 12H9M12 20H6a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2h6",
};

// A pinned colour for the apps we know, and a deterministic hash into a fixed
// palette for everything else, so an app keeps its identity across restarts
// without anyone assigning one.
const COLORS = {
  ycode: "#1f2328", shell: "#0a0e1a", terminal: "#0a0e1a", desktop: "#af52de",
  ssh: "#0f766e", files: "#3478f6", meet: "#f59e0b", dag: "#ef4444", loom: "#8b5cf6",
  mb: "#0ea5e9", sprint: "#16a34a", inbox: "#6366f1",
};
const ICON_PALETTE = ["#4f46e5","#3478f6","#ec4899","#f59e0b","#10b981","#8b5cf6",
  "#5ac8fa","#ef4444","#06b6d4","#22c55e","#0ea5e9","#a855f7","#84cc16"];
// A panel may describe its own mark (dhnt-app-meta-v1 `icon`): SVG path data on
// the same 24 grid, or a single emoji. Server-supplied wins, then our own mark
// for a name we know, then the initial. The fallback chain is the point — a
// third-party app is never REQUIRED to ship art, and the letter stays honest
// about being a placeholder.
function appIcon(name, icon) {
  const color = COLORS[name] || ICON_PALETTE[Math.abs(hash(name)) % ICON_PALETTE.length];
  if (icon) {
    // Path data is drawn; anything else is treated as a text glyph (emoji).
    if (/^[MmLlHhVvCcSsQqTtAaZz0-9eE.,+\-\s]+$/.test(icon)) {
      return { color, path: icon, glyph: (name[0] || "?").toUpperCase() };
    }
    return { color, path: null, glyph: icon };
  }
  return { color, path: SVG[name] || null, glyph: (name[0] || "?").toUpperCase() };
}
function hash(name) {
  let h = 0;
  for (let i = 0; i < name.length; i++) h = ((h << 5) - h + name.charCodeAt(i)) | 0;
  return h;
}

// svgIcon builds one mark. strokeWidth is deliberately uniform across the set.
function svgIcon(spec, cls, width) {
  const ns = "http://www.w3.org/2000/svg";
  const svg = document.createElementNS(ns, "svg");
  svg.setAttribute("viewBox", "0 0 24 24");
  svg.setAttribute("fill", "none");
  svg.setAttribute("stroke", "currentColor");
  svg.setAttribute("stroke-width", String(width || 1.9));
  svg.setAttribute("stroke-linecap", "round");
  svg.setAttribute("stroke-linejoin", "round");
  if (cls) svg.setAttribute("class", cls);

  const shape = typeof spec === "string" ? { paths: [spec] } : spec;
  for (const d of shape.paths || []) {
    const p = document.createElementNS(ns, "path");
    p.setAttribute("d", d);
    svg.append(p);
  }
  for (const [cx, cy, r] of shape.circles || []) {
    const c = document.createElementNS(ns, "circle");
    c.setAttribute("cx", cx); c.setAttribute("cy", cy); c.setAttribute("r", r);
    svg.append(c);
  }
  return svg;
}

// ------------------------------------------------------------------- tiles --
// The Inbox tile's unread badge.
//
// WHOSE COUNT: the VIEWER's own, never the fleet's. The panel behind this tile
// is a PEEK at every other name's mail — looking at an agent's inbox there
// advances nothing, by construction, so an agent's backlog falls only when that
// agent itself reads. A badge showing the fleet total would therefore be a
// number the person looking at it can never clear, and a badge that never
// clears is one people learn to ignore. The viewer's own count is the only one
// their own reading moves, which is what makes it a badge rather than a gauge.
//
// The fleet backlog is still worth knowing, so it goes in the TOOLTIP — stated,
// not alarmed — along with how many inboxes it is spread across, because 300
// messages in one stalled inbox and 300 across the fleet are different
// situations.
//
// WHY THIS IS A NAMED SPECIAL CASE rather than a generic "any panel may report
// a count" channel: the obvious generic home is /api/apps, and that path is
// consoleWidePath — reachable by ANY admitted session regardless of scope. Per-
// panel counts there would hand a session scoped to the Terminal alone the
// fleet's mail volume, quietly widening exactly the boundary the Inbox panel's
// scope note draws. One consumer, one route, gated where its data is gated.
function applyInboxCount(a, icon, btn) {
  if (a.name !== "inbox" || !inboxCounts) return;
  const fleet = inboxCounts.unread || 0;
  // `waiting`, not `inboxes`: the number of inboxes that actually HOLD
  // something. Spreading the total over every inbox on the host — most of them
  // empty — would read as "2 unread across 3 inboxes" when only two hold
  // anything, and the shape of a backlog is the point of stating it at all.
  const boxes = inboxCounts.waiting || 0;
  const mine = inboxCounts.viewer_unread || 0;

  const scan = fleet
    ? fleet + " unread across " + boxes + " inbox" + (boxes === 1 ? "" : "es") +
      " — a peek; reading here consumes nothing"
    : "nothing unread anywhere on this host";
  btn.title = mine
    ? mine + " waiting for you · " + scan
    : (a.tip ? a.tip + " · " : "") + scan;

  // A count on a panel that is not running would be a number from the last time
  // it was, so the status dot has that corner to itself.
  if (!mine || a.status !== "ready") return;
  const count = document.createElement("span");
  count.className = "count";
  // Capped so a four-digit backlog cannot widen the tile and break the grid.
  count.textContent = mine > 99 ? "99+" : String(mine);
  count.setAttribute("aria-label", mine + " unread messages waiting for you");
  icon.append(count);
}

function tile(a, opts = {}) {
  const ic = appIcon(a.name, a.icon);
  const wrap = document.createElement("div");
  wrap.className = "tile-wrap";

  // A real link, never framed inside the launcher: the launcher is a start
  // page, not a container. WHERE it opens follows the console's global
  // "Open apps" mode — same tab (the default) navigates this window, and
  // new tab gives the app a window of its own with rel=noopener, so it
  // cannot reach back into this page.
  const open = a.status === "ready";
  const btn = document.createElement(open ? "a" : "button");
  btn.className = "tile";
  if (open) {
    btn.href = url(a.path.replace(/^\//, ""));
    if (openApps === "new-tab") {
      btn.target = "_blank";
      btn.rel = "noopener";
    }
  } else {
    btn.type = "button";
  }
  if (a.status === "unavailable") {
    btn.disabled = true;
    btn.title = a.note || "unavailable on this host";
  } else if (a.status === "stopped") {
    btn.title = a.start_hint ? "Not running. Start it with: " + a.start_hint : "not running";
  } else if (a.tip) {
    btn.title = a.tip;
  }

  const icon = document.createElement("span");
  icon.className = "icon" + (ic.path ? " icon-svg" : "");
  icon.style.background = ic.color;
  icon.append(ic.path ? svgIcon(ic.path, null, 1.8) : document.createTextNode(ic.glyph));
  const badge = document.createElement("span");
  badge.className = "badge " + a.status;
  icon.append(badge);
  applyInboxCount(a, icon, btn);
  btn.append(icon);

  const label = document.createElement("span");
  label.className = "label";
  label.textContent = a.label;
  btn.append(label);


  btn.addEventListener("click", () => { if (open) recordVisit(a.name); });
  wrap.append(btn);

  if (!opts.noStar) {
    const star = document.createElement("button");
    star.type = "button";
    star.className = "star" + (isFav(a.name) ? " on" : "");
    const mark = svgIcon(SVG.star, null, 1.7);
    if (isFav(a.name)) mark.setAttribute("fill", "currentColor");
    star.replaceChildren(mark);
    star.title = isFav(a.name) ? "Remove from favorites" : "Add to favorites";
    star.setAttribute("aria-label", star.title);
    star.addEventListener("click", (e) => { e.stopPropagation(); toggleFav(a.name); });
    wrap.append(star);
    attachLongPressFavorite(wrap);
  }
  return wrap;
}

// WHAT THE PHONE MAY REACH.
//
// A pass defaults to meet/mb/sprint, with `bashy apps pair --allow ...`
// the only way to widen it — so a phone that hit "terminal is not in the
// scope" got a refusal and no way to act on it. The choice belongs here: the
// console already required an OS login to reach this page, the chooser is the
// signed-in operator at their own keyboard, and the server validates the names
// and can only ever NARROW its own session.
//
// The shell and the filesystem stay OFF by default. Default-deny is the point:
// granting them is a decision, and a decision made by ticking a box you can
// see is a different thing from one made by a default you never read.
const PAIR_SCOPE_DEFAULT = ["meet", "mb", "sprint"];
const PAIR_SCOPE_SENSITIVE = { terminal: "a full shell as your OS user", files: "read your files" };

function pairScopeNames() {
  // Offer what this console actually serves, in launcher order, so the list
  // cannot drift from the panels or promise one that is not mounted.
  const names = apps.map((a) => String(a.name || "").toLowerCase()).filter(Boolean);
  return names.length ? names : PAIR_SCOPE_DEFAULT.slice();
}

function buildPairScope() {
  const host = document.getElementById("pair-scope");
  if (!host) return;
  const rows = pairScopeNames().map((name) => {
    const row = document.createElement("label");
    row.className = "row pair-scope-row";
    const span = document.createElement("span");
    span.textContent = name;
    const warn = PAIR_SCOPE_SENSITIVE[name];
    if (warn) {
      const note = document.createElement("small");
      note.className = "pair-scope-warn";
      note.textContent = " — " + warn;
      span.append(note);
    }
    const cb = document.createElement("input");
    cb.type = "checkbox";
    cb.className = "pair-scope-box";
    cb.value = name;
    cb.checked = PAIR_SCOPE_DEFAULT.includes(name);
    row.append(span, cb);
    return row;
  });
  const head = document.createElement("p");
  head.className = "hint";
  head.textContent = "What this phone may reach:";
  host.replaceChildren(head, ...rows);
}

// PAIR_TTL_KEY remembers the operator's last choice, per browser. It is a
// convenience, not state the console depends on: a missing or unreadable value
// falls back to the marked-up default, which is the same 24 hours the server
// applies when no choice is sent at all.
const PAIR_TTL_KEY = "bashy.apps.pairTTL";

function selectedPairTTL() {
  const sel = document.getElementById("pair-ttl");
  if (!sel) return undefined; // no control on this page: let the server decide
  const hours = Number(sel.value);
  if (!Number.isFinite(hours) || hours < 0) return undefined;
  try {
    localStorage.setItem(PAIR_TTL_KEY, String(hours));
  } catch (_) {}
  // 0 is a real answer here — it is how the operator spells "never expires" —
  // so it must survive as a number rather than being coalesced away.
  return hours;
}

function restorePairTTL() {
  const sel = document.getElementById("pair-ttl");
  if (!sel) return;
  try {
    const saved = localStorage.getItem(PAIR_TTL_KEY);
    if (saved !== null && [...sel.options].some((o) => o.value === saved)) {
      sel.value = saved;
    }
  } catch (_) {}
}

function selectedPairScope() {
  const boxes = [...document.querySelectorAll(".pair-scope-box")];
  const on = boxes.filter((b) => b.checked).map((b) => b.value);
  // An empty selection is not "everything" — the server would read it as
  // absent and fall back to the default, so say what will happen instead of
  // minting something the operator did not choose.
  return on.length ? on : PAIR_SCOPE_DEFAULT.slice();
}

// SUPPRESS THE BROWSER'S LONG-PRESS MENU ON A TILE.
//
// Long press already works for favouriting on a phone: the press puts the tile
// in its hover state, the star appears, and it can be tapped. What ruins it is
// that the SAME gesture also opens the browser's own long list of context-menu
// items on top of the star.
//
// So the fix is only to take that menu away, and only here: page-wide
// suppression would cost copy-link, open-in-new-tab and share everywhere else,
// which is a bad trade for one shortcut. A mouse right-click keeps its menu —
// a desktop user has no long press and loses nothing.
function attachLongPressFavorite(wrap) {
  wrap.addEventListener("contextmenu", (e) => {
    if (e.pointerType === "mouse") return;
    if (matchMedia("(hover: none)").matches) e.preventDefault();
  });
}

function sectionEl(title, nodes) {
  const s = document.createElement("section");
  s.className = "sect";
  const head = document.createElement("div");
  head.className = "sect-head";
  const h = document.createElement("h2");
  h.textContent = title;
  head.append(h);
  s.append(head);
  const grid = document.createElement("div");
  grid.className = "grid";
  grid.append(...nodes);
  s.append(grid);
  return s;
}

// -------------------------------------------------------------------- home --
// render is the single repaint entry point. Everything that changes what the
// page shows — a search keystroke, a settings toggle, a liveness poll — calls
// this rather than reaching into the DOM itself.
function render() { renderHome(); }

function matches(a) {
  const t = search.trim().toLowerCase();
  if (!t) return true;
  return a.name.toLowerCase().includes(t) || (a.label || "").toLowerCase().includes(t);
}

function renderHome() {
  const pad = document.createElement("div");
  pad.className = "pad";
  const shown = apps.filter(matches);
  const byName = new Map(apps.map((a) => [a.name, a]));

  if (cfg.showSummary) {
    const ready = apps.filter((a) => a.status === "ready").length;
    const stopped = apps.filter((a) => a.status === "stopped").length;
    const na = apps.filter((a) => a.status === "unavailable").length;
    const s = document.createElement("section");
    s.className = "sect";
    const head = document.createElement("div");
    head.className = "sect-head";
    const h = document.createElement("h2"); h.textContent = "Overview"; head.append(h);
    s.append(head);
    const card = document.createElement("div");
    card.className = "summary";
    for (const [n, l] of [[apps.length, "Apps"], [ready, "Ready"], [stopped, "Stopped"], [na, "Unavailable"]]) {
      const d = document.createElement("div");
      const b = document.createElement("b"); b.textContent = String(n);
      const sp = document.createElement("span"); sp.textContent = l;
      d.append(b, sp); card.append(d);
    }
    s.append(card);
    pad.append(s);
  }

  if (cfg.showFavorites) {
    const favs = cfg.favorites.map((n) => byName.get(n)).filter(Boolean).filter(matches);
    if (favs.length) pad.append(sectionEl("Favorites", favs.map((a) => tile(a))));
  }

  if (cfg.showRecents) {
    const rec = cfg.recents.map((n) => byName.get(n)).filter(Boolean).filter(matches)
      .slice(0, Math.max(1, cfg.recentLimit | 0));
    if (rec.length) pad.append(sectionEl("Recent", rec.map((a) => tile(a, { noStar: true }))));
  }

  if (shown.length) {
    pad.append(sectionEl("Apps", shown.map((a) => tile(a))));
  } else {
    const s = document.createElement("section");
    s.className = "sect";
    const p = document.createElement("p");
    p.className = "empty";
    p.textContent = apps.length ? "No app matches that search." : "No surfaces declared.";
    s.append(p);
    pad.append(s);
  }

  view.replaceChildren(pad);
}

// ---------------------------------------------------------------- settings --
const BG_PRESETS = [
  ["none", "None"], ["sky", "Sky"], ["ocean", "Ocean"], ["mountains", "Mountains"],
  ["plateau", "Plateau"], ["lakes", "Lakes"], ["bamboo", "Bamboo"],
];
const SECTIONS = [["showSummary", "Overview"], ["showFavorites", "Favorites"], ["showRecents", "Recent"]];
const dlg = document.getElementById("settings");

function buildSettings() {
  const sw = document.getElementById("bg-swatches");
  sw.replaceChildren(...BG_PRESETS.map(([id, label]) => {
    const b = document.createElement("button");
    b.type = "button"; b.className = "swatch-btn";
    b.setAttribute("aria-pressed", String((cfg.background || "none") === id));
    const s = document.createElement("div");
    s.className = "bg-swatch"; s.setAttribute("data-bg", id);
    const t = document.createElement("span"); t.textContent = label;
    b.append(s, t);
    b.addEventListener("click", () => { saveCfg({ background: id }); buildSettings(); });
    return b;
  }));

  for (const b of document.querySelectorAll("#theme-seg button")) {
    b.setAttribute("aria-pressed", String(b.dataset.themeVal === cfg.theme));
    b.onclick = () => { saveCfg({ theme: b.dataset.themeVal }); buildSettings(); };
  }

  for (const b of document.querySelectorAll("#open-apps-seg button")) {
    b.setAttribute("aria-pressed", String(b.dataset.openVal === openApps));
    b.onclick = () => saveLook(b.dataset.openVal);
  }

  const st = document.getElementById("section-toggles");
  st.replaceChildren(...SECTIONS.map(([key, label]) => {
    const row = document.createElement("label");
    row.className = "row";
    const s = document.createElement("span"); s.textContent = label;
    const sw2 = document.createElement("span"); sw2.className = "switch";
    const inp = document.createElement("input"); inp.type = "checkbox"; inp.checked = !!cfg[key];
    const knob = document.createElement("i");
    inp.addEventListener("change", () => { saveCfg({ [key]: inp.checked }); render(); });
    sw2.append(inp, knob);
    row.append(s, sw2);
    return row;
  }));

  const rl = document.getElementById("recent-limit");
  rl.value = cfg.recentLimit;
  rl.onchange = () => { saveCfg({ recentLimit: Math.max(1, Math.min(24, +rl.value || 8)) }); render(); };

  document.getElementById("fav-summary").textContent =
    `${cfg.favorites.length} favorite${cfg.favorites.length === 1 ? "" : "s"}, ${cfg.recents.length} recent.`;

  buildPairing();
}
document.getElementById("settings-btn").addEventListener("click", () => { buildSettings(); dlg.showModal(); });
document.getElementById("clear-favs").addEventListener("click", () => { saveCfg({ favorites: [] }); buildSettings(); render(); });
document.getElementById("clear-recents").addEventListener("click", () => { saveCfg({ recents: [] }); buildSettings(); render(); });
dlg.addEventListener("close", () => { resetPairing(); render(); });

// ------------------------------------------------------------- phone access --
// Phone pairing is OFF until the operator flips the toggle, and flipping it is
// the ONLY thing that ever asks the server to mint a ticket. A fresh load or a
// closed sheet mints nothing and shows no code — the toggle is the whole
// consent gesture. Redemption stays the password-sparing path `bashy apps pair`
// already built; this only moves the mint behind a click.
//
// serverPairing mirrors /api/session's `pairing`: whether this console was
// started with LAN pairing armed. It lets the section say, before minting
// anything, whether enabling will produce a code or the command to restart with
// --pair. Default false — fail closed if the session probe has not answered.
let serverPairing = false;
const pairButton = () => document.getElementById("pair-btn");
const pairRefresh = () => document.getElementById("pair-refresh");
const pairPanel = () => document.getElementById("pair-panel");

function resetPairing() {
  const p = pairPanel(); if (p) { p.hidden = true; p.replaceChildren(); }
  const r = pairRefresh(); if (r) r.hidden = true;
}

// Phone pairing reads like Appearance or Background: the section is always
// there, stating what it does. What it does NOT have is an on/off switch — a
// switch implies a persistent setting, and pairing is not one. Each pass is a
// single-use, time-boxed credential, so asking for one is an ACTION (the
// Favorites idiom), and it stays an explicit click rather than something that
// happens merely because Settings was opened.
function buildPairing() {
  const b = pairButton();
  if (!b) return;
  resetPairing();
  buildPairScope();
  restorePairTTL();
  const instructions = document.getElementById("pair-instructions-host");
  if (instructions) instructions.replaceChildren(pairInstructions());
  const hint = document.getElementById("pair-armed-hint");
  if (hint) hint.remove();
  if (!serverPairing) {
    // Not armed: say so up front rather than after a wasted mint. Asking will
    // still explain how to restart with LAN pairing on.
    const note = pairNote("This console is not armed for LAN pairing yet — asking for a code shows the command to restart with it on.");
    note.id = "pair-armed-hint";
    // Insert before the ACTIONS ROW, not the button: the button lives inside
    // .pair-actions, so it is not a child of <section> and insertBefore would
    // throw NotFoundError — taking the whole Settings dialog down with it,
    // since buildSettings calls this before opening.
    const anchor = b.closest(".pair-actions") || b;
    anchor.parentNode.insertBefore(note, anchor);
  }
  b.onclick = () => mintPairing();
  // Refresh is the RE-pairing path. A pass is single-use by design, so the code
  // on screen is spent the moment a phone redeems it — and the second device,
  // or a retry after "that code has already been used", needs a fresh one. It
  // stays hidden until there is a code to replace, so it never offers to
  // refresh nothing.
  const r = pairRefresh();
  if (r) r.onclick = () => mintPairing();
}

async function mintPairing() {
  const p = pairPanel();
  p.hidden = false;
  p.replaceChildren(pairNote("Minting a one-time code…"));
  let data;
  try {
    const res = await fetch(url("api/pair"), {
      method: "POST",
      headers: { Accept: "application/json", "Content-Type": "application/json" },
      body: JSON.stringify({ scope: selectedPairScope(), ttl_hours: selectedPairTTL() }),
    });
    data = await res.json();
  } catch (e) {
    p.replaceChildren(pairNote("Could not reach the console: " + e));
    return;
  }
  // enabled:false means LAN access was NOT opened — show the exact restart
  // command rather than an empty panel.
  if (data && data.enabled === false) {
    p.replaceChildren(pairFailClosed(data));
    return;
  }
  if (!data || data.error) {
    p.replaceChildren(pairNote((data && data.error) || "Pairing failed."));
    return;
  }
  p.replaceChildren(pairCodes(data));
  const r = pairRefresh();
  if (r) r.hidden = false;
}

function pairFailClosed(data) {
  const box = document.createElement("div");
  box.className = "pair-closed";
  const h = document.createElement("p");
  h.className = "pair-closed-title";
  h.textContent = data.reason || "Phone access is not available on this console.";
  box.append(h);
  if (data.detail) {
    const d = document.createElement("p");
    d.className = "hint";
    d.textContent = data.detail;
    box.append(d);
  }
  if (data.restart) {
    const lbl = document.createElement("p");
    lbl.className = "hint";
    lbl.textContent = "Restart the console with LAN pairing on:";
    const code = document.createElement("code");
    code.className = "pair-restart";
    code.textContent = data.restart;
    box.append(lbl, code);
  }
  return box;
}

function pairCodes(data) {
  const addrs = data.addresses || [];
  if (!addrs.length) {
    return pairNote("The console could not work out an address a phone could reach. " +
      "Check this host is on a network, or run `bashy apps pair --host <ip>`.");
  }
  const box = document.createElement("div");
  box.className = "pair-codes-wrap";

  // Two labelled codes: whichever the phone's network can resolve. The mDNS
  // name survives a DHCP lease change; the raw LAN IP is the always-works
  // fallback. Both carry the SAME single-use ticket.
  const codes = document.createElement("div");
  codes.className = "pair-codes";
  for (const a of addrs) {
    const c = document.createElement("div");
    c.className = "pair-code";
    if (a.qr) {
      const img = document.createElement("img");
      img.src = a.qr;
      img.alt = "Pairing QR code for " + a.host;
      img.className = "pair-qr";
      c.append(img);
    }
    const lab = document.createElement("div");
    lab.className = "pair-code-label"; lab.textContent = a.label;
    const host = document.createElement("div");
    host.className = "pair-code-host";
    host.textContent = a.access_url || a.host;
    c.append(lab, host);
    codes.append(c);
  }
  box.append(codes);

  const meta = document.createElement("p");
  meta.className = "hint";
  // Two clocks, and they are not the same thing: the CODE is single-use and
  // stale in about two minutes, while the DEVICE keeps access for as long as
  // the operator chose. Saying "never expires" in words matters — the server
  // stores that as a date a century out, and a reader should not have to
  // decode a year to learn there is nothing to renew.
  meta.textContent = `Scope: ${(data.scope || []).join(", ")} · code is single-use and expires in ~2 min` +
    (data.never_expires
      ? " · the phone stays paired until you revoke it"
      : data.device_ttl
        ? ` · device access lasts ${data.device_ttl}`
        : "") + ".";
  box.append(meta);

  if (data.note) {
    const n = document.createElement("p");
    n.className = "pair-warn";
    n.textContent = data.note;
    box.append(n);
  }

  return box;
}

// pairInstructions is always present in Settings, but collapsed until someone
// asks for it. Only the likely phone family is shown initially; the adjacent
// switch keeps the other instructions one click away when detection is wrong.
function pairInstructions() {
  const disclosure = document.createElement("details");
  disclosure.className = "pair-instructions";
  const summary = document.createElement("summary");
  summary.textContent = "Add to home screen";
  const body = document.createElement("div");
  body.className = "pair-instructions-body";
  disclosure.append(summary, body);

  const ios = instructionBlock("iPhone / iPad (Safari)", [
    "Tap the Share button (a square with an up arrow)",
    "Scroll down and tap “Add to Home Screen”",
    "Tap “Add”",
  ]);
  const android = instructionBlock("Android (Chrome)", [
    "Tap the ⋮ menu (three dots, top right)",
    "Tap “Add to Home screen” / “Install app”",
    "Tap “Add”",
  ]);
  ios.classList.add("pair-os");
  android.classList.add("pair-os");

  const platform = `${navigator.userAgentData?.platform || ""} ${navigator.platform || ""} ${navigator.userAgent || ""}`;
  let selected = /Android|Linux|Win/i.test(platform) ? "android" : "ios";
  const switcher = document.createElement("button");
  switcher.type = "button";
  switcher.className = "pair-os-switch";
  const renderOS = () => {
    const current = selected === "ios" ? ios : android;
    const otherName = selected === "ios" ? "Android" : "iPhone / iPad";
    switcher.textContent = `Show ${otherName} instructions`;
    switcher.setAttribute("aria-label", switcher.textContent);
    body.replaceChildren(current, switcher);
  };
  switcher.addEventListener("click", () => {
    selected = selected === "ios" ? "android" : "ios";
    renderOS();
  });
  renderOS();
  return disclosure;
}

function instructionBlock(title, steps) {
  const d = document.createElement("div");
  const t = document.createElement("p");
  t.className = "pair-os-title"; t.textContent = title;
  const ol = document.createElement("ol");
  for (const s of steps) { const li = document.createElement("li"); li.textContent = s; ol.append(li); }
  d.append(t, ol);
  return d;
}

function pairNote(text) {
  const p = document.createElement("p");
  p.className = "hint"; p.textContent = text;
  return p;
}

// saveLook PUTs one mode to the console's look settings and repaints with
// whatever the server confirms. The dialog stays usable when the request
// cannot land (offline, auth): the current mode simply stays put, which is
// the honest outcome of "not saved" rather than a silently wrong control.
async function saveLook(v) {
  try {
    const r = await fetch(url("api/look"), {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ open_apps: v }),
    });
    if (!r.ok) throw new Error("look: " + r.status);
    const l = await r.json();
    openApps = l.open_apps === "new-tab" ? "new-tab" : "same-tab";
  } catch (_) { /* keep the current mode; the server did not take the write */ }
  buildSettings();
  render();
}

// ----------------------------------------------------------------- session --
// Sign-out appears only when there is a session to end.
//
// On loopback the gate admits the machine owner without one, so a sign-out
// button there would be a control that does nothing — and the next thing a
// reader concludes from a button that does nothing is that the page is broken.
function renderSession(s) {
  const form = document.getElementById("logout");
  if (!form) return;
  const signedIn = s.via === "session";
  form.hidden = !signedIn;
  if (signedIn && !form.querySelector("svg")) {
    form.querySelector("button").append(svgIcon(SVG.logout));
  }
}

// ------------------------------------------------------------------- build --
// Which binary am I looking at, and is this page from it or from a cache?
//
// `assets` is the content hash of this very script, so a stale cached copy shows
// a different value from the one the server reports — the question that took a
// whole round trip to answer becomes a glance.
function renderBuild(b) {
  if (!b) return;
  const short = (b.release || b.version || "devel") + (b.commit ? " · " + b.commit : "") + (b.dirty ? " · dirty tree" : "");
  // The badge carries the RELEASE — the thing a person quotes in a bug report.
  // The commit is one hover away and in the footer; showing a bare hash here
  // answered a question almost nobody asks first.
  verEl.textContent = (b.release || b.version || "devel") + (b.dirty ? "*" : "");
  verEl.title = [short, b.time, b.go, "assets " + (b.assets || "?")].filter(Boolean).join("\n");
  const foot = document.getElementById("foot-build");
  if (foot) {
    foot.textContent = short + (b.time ? " · " + b.time.slice(0, 10) : "") + (b.go ? " · " + b.go : "");
  }
}

// ------------------------------------------------------------------ wiring --
searchEl.addEventListener("input", () => { search = searchEl.value; render(); });
document.getElementById("theme-btn").addEventListener("click", () => {
  const order = ["system", "light", "dark"];
  saveCfg({ theme: order[(order.indexOf(cfg.theme) + 1) % order.length] });
});

async function refresh() {
  try {
    const [a, s, l, i] = await Promise.all([
      fetch(url("api/apps")).then((r) => r.json()),
      fetch(url("api/session")).then((r) => r.json()).catch(() => null),
      fetch(url("api/look")).then((r) => r.json()).catch(() => null),
      // ?summary=1 is six numbers, not 175 holder rows. It rides along and
      // fails soft: a session whose scope excludes the Inbox panel is REFUSED
      // this, and the right answer to that is a tile with no badge — not a
      // broken start page.
      fetch(url("api/inbox?summary=1")).then((r) => (r.ok ? r.json() : null)).catch(() => null),
    ]);
    apps = a.apps || [];
    inboxCounts = i && !i.error ? i : null;
    // The look ride-along fails soft: no answer leaves the mode at the
    // same-tab default, which is exactly the server's own fallback.
    if (l && l.open_apps) openApps = l.open_apps === "new-tab" ? "new-tab" : "same-tab";
    if (s) {
      serverPairing = !!s.pairing;
      whoEl.textContent = s.user + " · " + s.via;
      renderBuild(s.build);
      renderSession(s);
      const ready = apps.filter((x) => x.status === "ready").length;
      footLeft.textContent = `${ready}/${apps.length} ready · signed in as ${s.user} (${s.via})`;
    }
  } catch (e) {
    apps = [];
    inboxCounts = null;
    whoEl.textContent = "offline";
  }
  // Only the home grid reflects liveness; repainting under a live terminal
  // would tear down the session every few seconds.
  render();
}

applyChrome();
refresh();
setInterval(refresh, 5000);
