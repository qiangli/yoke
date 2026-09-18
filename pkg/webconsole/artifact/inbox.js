// The system-wide inbox page.
//
// A full browser page like the terminal and the board, not a pane inside the
// launcher, and its <base href> is the LAUNCHER's — so "api/inbox" and "./"
// resolve correctly at / on loopback and under outpost's
// /matrix/h/<host>/app/<name>/ prefix alike.
//
// WHAT THIS PAGE IS FOR. `bashy inbox` can only ever answer "what is waiting for
// ME": it fixes the filter to one reader and, unless --peek, advances that
// reader's cursor. Nobody's inbox is "everybody's", so the question a human
// actually has — what is waiting across the whole fleet, and who is sitting on
// a backlog — has no CLI form. That is this page.
//
// WHAT IT DOES NOT DO: consume anybody ELSE's mail. Every other name is a PEEK
// — no cursor moves, no record is marked read, no subscription is opened, and
// there is no route through which another inbox can even be named. Opening a
// tab must not eat a message an agent has not been handed yet.
//
// Your OWN inbox is not something you observe, it is something you read, so it
// has the controls a person expects: mark one, or mark all. They go through the
// nameless POST api/inbox/read, which acts on the caller's own inbox and has no
// parameter for any other.
const url = (p) => new URL(p, document.baseURI);

// Theme follows the launcher's saved preference so the pages match.
try {
  const cfg = JSON.parse(localStorage.getItem("bashy.apps.config") || "{}");
  if (cfg.theme && cfg.theme !== "system") document.documentElement.setAttribute("data-theme", cfg.theme);
} catch (_) {}

const $ = (id) => document.getElementById(id);
const el = (tag, cls, text) => {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text !== undefined) n.textContent = text;
  return n;
};

const FILTERS = ["from", "topic", "room", "delivery", "state", "age", "q"];

const state = {
  viewer: "",
  groups: [],
  holders: {},
  name: "",
  facets: "",
  newestFirst: false,
  items: [],
  // Set from the LIST response's own `kind`, never inferred by comparing two
  // fetches. See mine().
  isMine: false,
  // Nothing has come back yet. Without this the empty list renders "Nothing in
  // this inbox matches" before the first response — a filter verdict, stated
  // over data that has not been asked for.
  loaded: false,
};

// ---- the selection and the filters live in the hash, so a view is a link ----

function readHash() {
  const p = new URLSearchParams(location.hash.replace(/^#/, ""));
  const f = { name: p.get("name") || "" };
  for (const k of FILTERS) f[k] = p.get(k) || "";
  f.order = p.get("order") === "new" ? "new" : "old";
  f.waiting = p.get("waiting") === "1";
  f.find = p.get("find") || "";
  return f;
}

function writeHash(f) {
  const p = new URLSearchParams();
  if (f.name) p.set("name", f.name);
  for (const k of FILTERS) if (f[k]) p.set(k, f[k]);
  if (f.order === "new") p.set("order", "new");
  if (f.waiting) p.set("waiting", "1");
  if (f.find) p.set("find", f.find);
  const s = p.toString();
  const next = s ? "#" + s : "#";
  if (location.hash !== next) history.replaceState(null, "", next);
}

function currentHash() {
  return {
    name: state.name,
    from: $("f-from").value,
    topic: $("f-topic").value,
    room: $("f-room").value,
    delivery: $("f-delivery").value,
    state: $("f-state").value,
    age: $("f-age").value,
    q: $("f-q").value.trim(),
    order: state.newestFirst ? "new" : "old",
    waiting: $("ib-waiting").checked,
    find: $("ib-find").value.trim(),
  };
}

// ---- small renderers -------------------------------------------------------

function when(ts) {
  const d = new Date(ts);
  if (isNaN(d)) return ts || "no timestamp";
  return d.toLocaleString(undefined, {
    month: "short", day: "numeric", hour: "2-digit", minute: "2-digit",
  });
}

const plural = (n, word) => n + " " + word + (n === 1 ? "" : "s");

// ---- the left nav ----------------------------------------------------------

function rosterRow(name) {
  const h = state.holders[name] || { unread: 0, total: 0 };
  const row = el("button", "ib-row");
  row.type = "button";
  row.dataset.name = name;
  if (name === state.name) row.classList.add("on");
  if (h.unread > 0) row.classList.add("waiting");

  row.append(el("span", "n", name));
  const count = el("span", "c");
  // The UNREAD count is the number on the row, and the total is only its
  // tooltip. An inbox of 400 read messages and one of 400 unread ones are
  // completely different situations, and the badge has to say which.
  if (h.unread > 0) {
    count.textContent = String(h.unread);
    count.classList.add("unread");
  } else if (h.total > 0) {
    count.textContent = String(h.total);
  }
  count.title = plural(h.unread, "unread") + " of " + plural(h.total, "message");
  row.append(count);

  row.title = (h.sources || []).join(", ");
  row.addEventListener("click", () => select(name));
  return row;
}

function renderRoster() {
  const host = $("ib-roster");
  const find = $("ib-find").value.trim().toLowerCase();
  const waitingOnly = $("ib-waiting").checked;

  const parts = [];
  let shown = 0;
  for (const g of state.groups) {
    const names = g.names.filter((n) => {
      if (find && !n.toLowerCase().includes(find)) return false;
      // The viewer's own row is never filtered out by "waiting only". It is the
      // page's fixed point — the row a reader goes to by muscle memory — and a
      // nav whose first entry disappears when a checkbox is ticked has moved
      // every other row under the reader's hand.
      if (waitingOnly && g.kind !== "person" && !(state.holders[n] || {}).unread) return false;
      return true;
    });
    if (!names.length) continue;
    shown += names.length;
    const sect = el("section", "ib-group");
    const head = el("header");
    head.append(el("span", "t", g.label));
    if (g.unread > 0) head.append(el("span", "u", g.unread + " unread"));
    sect.append(head);
    for (const n of names) sect.append(rosterRow(n));
    parts.push(sect);
  }
  if (!parts.length) parts.push(el("p", "empty", "No inbox matches."));
  host.replaceChildren(...parts);

  const total = state.groups.reduce((a, g) => a + g.names.length, 0);
  $("ib-nav-foot").textContent = shown === total
    ? plural(total, "inbox")
    : shown + " of " + plural(total, "inbox");
}

// mine reports whether the open inbox is the viewer's own — the only one this
// page may write to. The server enforces it with a nameless route; this only
// decides whether to OFFER a control that would be refused.
//
// It reads a flag the LIST response set, not `state.name === state.viewer`.
// The viewer's name arrives with the ROSTER and the open inbox with the LIST,
// and those are two fetches started together: whichever lost the race decided
// whether your own inbox showed its controls. The browser test caught it as an
// intermittent pill. One response carries both facts — the server already
// classified the inbox as `person` by comparing it to the viewer — so taking
// the answer from there removes the race instead of ordering it.
const mine = () => state.isMine;

// markRead posts to the nameless route, then repaints from the server's own
// count rather than decrementing a local one: two places keeping a tally is two
// places to disagree about it.
async function markRead(body, el) {
  if (el) el.disabled = true;
  try {
    const r = await fetch(url("api/inbox/read"), {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    if (!r.ok) {
      const d = await r.json().catch(() => ({}));
      if (el) {
        el.disabled = false;
        el.textContent = d.error || "refused";
      }
      return;
    }
  } catch (_) {
    if (el) {
      el.disabled = false;
      el.textContent = "no answer";
    }
    return;
  }
  state.loaded = false;
  await Promise.all([loadList(), loadRoster()]);
}

// ---- the message list ------------------------------------------------------

function itemEl(it) {
  const card = el("article", "ib-msg");
  if (!it.read) card.classList.add("unread");
  if (it.delivery === "interrupt") card.classList.add("urgent");
  if (it.source === "timeline") card.classList.add("pendingless");

  const head = el("header");
  head.append(el("span", "seq", "#" + it.seq));
  const from = el("button", "from", it.from || "unattributed");
  from.type = "button";
  from.title = "filter to this sender";
  from.addEventListener("click", () => { $("f-from").value = it.from || ""; apply(); });
  head.append(from);

  if (it.topic) {
    const t = el("button", "topic", it.topic);
    t.type = "button";
    t.title = "filter to this topic";
    t.addEventListener("click", () => { $("f-topic").value = it.topic; apply(); });
    head.append(t);
  }
  if (it.room) head.append(el("span", "room", it.room));
  if (it.delivery === "interrupt") head.append(el("span", "tag urgent", "interrupt"));
  head.append(el("span", "spacer"));
  head.append(el("time", "at", when(it.ts)));
  card.append(head);

  card.append(el("p", "body", it.body || "(no body)"));

  // The state line is the honest part, and it is why this panel exists rather
  // than a `--as <name>` flag on the CLI. Four facts that are routinely
  // conflated get four different sentences:
  //
  //   stamped read      the agent was actually handed this
  //   behind the cursor its own drain passed this, but nothing recorded a hand-off
  //   waiting           materialized for it, not yet read
  //   not materialized  published and durable, never yet folded into its buffer
  //
  // "Unread" alone would flatten all four, and an operator diagnosing a stalled
  // agent needs precisely the distinction between the second and the fourth.
  const foot = el("div", "state");
  const bits = [];
  if (it.read_at) bits.push("read " + when(it.read_at));
  else if (it.past_cursor) bits.push("behind the drain cursor, never stamped");
  else bits.push("waiting");
  if (it.source === "timeline") bits.push("on the timeline, not yet in this inbox's buffer");
  if (it.demoted) bits.push("demoted: " + it.demoted);
  if (it.match_reason) bits.push("matched by " + it.match_reason);
  foot.textContent = bits.join(" · ");
  // Marking one message is offered only on your own inbox, and only for
  // something actually unread. It acknowledges exactly this record — the server
  // uses CommitItem, not a cursor write, so the messages below it survive.
  if (mine() && !it.read) {
    const btn = el("button", "mark", "mark read");
    btn.type = "button";
    btn.title = "mark this message read in your inbox";
    btn.addEventListener("click", (e) => {
      e.stopPropagation();
      markRead({ seq: it.seq }, btn);
    });
    foot.append(document.createTextNode(" · "));
    foot.append(btn);
  }
  card.append(foot);
  return card;
}

function renderList() {
  const host = $("ib-list");
  if (!state.name) {
    host.replaceChildren(el("p", "empty", "Pick an inbox on the left."));
    return;
  }
  if (!state.loaded) {
    host.replaceChildren(el("p", "empty", "Reading " + state.name + "'s inbox…"));
    return;
  }
  if (!state.items.length) {
    host.replaceChildren(el("p", "empty", "Nothing in this inbox matches."));
    return;
  }
  // The server answers oldest-first — chronological, the order a conversation
  // happened in. Newest-first is a view of the same list, never a second query,
  // so the two orders can never disagree about what is in it.
  const items = state.newestFirst ? state.items.slice().reverse() : state.items;
  host.replaceChildren(...items.map(itemEl));
  if (!state.newestFirst) host.scrollTop = host.scrollHeight;
  else host.scrollTop = 0;
}

// `want` comes from the HASH, never from the element: the options do not exist
// until the first response lands, so a select whose value was set at init from a
// linked-to filter would silently revert to "any" — the data would be filtered
// and the control would say it was not.
function fillSelect(sel, facets, want) {
  const cur = want || "";
  const first = sel.firstElementChild.cloneNode(true);
  sel.replaceChildren(first);
  for (const f of facets || []) {
    const o = el("option", null, f.name + " (" + f.count + ")");
    o.value = f.name;
    sel.append(o);
  }
  sel.value = cur;
  // A filter naming something no longer in this inbox would silently match
  // nothing; keep it selectable rather than dropping it without saying so.
  if (sel.value !== cur && cur) {
    const o = el("option", null, cur + " (0)");
    o.value = cur;
    sel.append(o);
    sel.value = cur;
  }
}

function renderMeta(d) {
  state.viewer = d.viewer;
  state.isMine = d.kind === "person";
  $("ib-title").textContent = d.name;
  const kind = $("ib-kind");
  kind.textContent = d.kind === "person" ? "you" : d.kind;
  kind.className = "ib-chip k-" + d.kind;
  // An empty chip is not invisible — it still draws its border and padding, so
  // an unloaded page rendered "Inbox —" beside the heading.
  kind.hidden = !d.kind;

  const parts = [];
  parts.push(d.matched === d.total
    ? plural(d.total, "message")
    : d.matched + " of " + plural(d.total, "message"));
  if (d.unread) parts.push(d.unread + " unread");
  parts.push("cursor at #" + d.cursor);
  $("ib-stats").textContent = parts.join(" · ");

  const f = readHash();
  const stamp = JSON.stringify(d.facets) + "|" + JSON.stringify(f);
  if (stamp !== state.facets) {
    state.facets = stamp;
    fillSelect($("f-from"), d.facets.from, f.from);
    fillSelect($("f-topic"), d.facets.topic, f.topic);
    fillSelect($("f-room"), d.facets.room, f.room);
    fillSelect($("f-delivery"), d.facets.delivery, f.delivery);
    $("f-state").value = f.state;
    $("f-age").value = f.age;
  }

  // The header pill states what THIS inbox is, and it has to be per-inbox: a
  // standing "read-only" was true of the page as a whole until your own inbox
  // gained controls, and a banner that contradicts the buttons under it is
  // worse than no banner. Your own inbox says nothing (the YOU chip beside the
  // title already does); every other name says PEEK.
  const mode = $("ib-mode");
  mode.hidden = mine();
  mode.textContent = "peek";
  mode.title = "reading here advances no cursor and marks nothing — only " +
    d.name + " can consume " + d.name + "'s mail";

  // Mark-all acts on the WHOLE inbox, never on the filtered view: marking
  // "everything" while a filter is on would consume messages that were never
  // shown, which is the one thing a bulk control must not do. It is hidden
  // rather than disabled when there is nothing to mark — a control that can
  // never do anything is noise.
  const all = $("ib-markall");
  all.hidden = !(mine() && d.unread > 0);
  all.textContent = "Mark all read (" + d.unread + ")";
  all.title = "mark all " + d.unread + " unread in your inbox read" +
    (d.matched !== d.total ? " — all of them, not just the " + d.matched + " matching this filter" : "");

  const capped = d.matched > d.items.length
    ? " Showing the newest " + d.items.length + " of " + d.matched + " matching."
    : "";
  $("ib-foot").textContent = (mine()
    ? "Your inbox. Marking read here is the same act as `bashy inbox` — it advances your own cursor and nobody else's."
    : "A peek at " + d.name + "'s inbox: nothing here advances a cursor or marks a message read, " +
      "so only " + d.name + " can consume it.") + capped;
}

// ---- loading ---------------------------------------------------------------

let rosterInflight = null;
let listInflight = null;

async function loadRoster() {
  if (rosterInflight) return;
  rosterInflight = fetch(url("api/inbox")).then((r) => r.json());
  let d;
  try {
    d = await rosterInflight;
  } catch (_) {
    return;
  } finally {
    rosterInflight = null;
  }
  if (d.error) {
    $("ib-roster").replaceChildren(el("p", "empty", d.error));
    return;
  }
  state.viewer = d.viewer;
  state.groups = d.groups || [];
  state.holders = {};
  for (const h of d.holders || []) state.holders[h.name] = h;

  const waiting = (d.holders || []).reduce((a, h) => a + (h.unread || 0), 0);
  $("ib-who").textContent = "viewing as " + d.viewer;
  $("ib-who").title = plural(waiting, "message") + " waiting across every inbox on this host";

  // The default selection is the VIEWER — the human this page is for — and it
  // is chosen only when the hash names nobody, so a shared link always wins.
  if (!state.name) {
    const f = readHash();
    select(f.name || d.viewer, true);
    return;
  }
  renderRoster();
}

function query() {
  const p = new URLSearchParams();
  for (const k of FILTERS) {
    const v = k === "q" ? $("f-q").value.trim() : $("f-" + k).value;
    if (v) p.set(k, v);
  }
  return p.toString();
}

async function loadList() {
  if (!state.name || listInflight) return;
  const qs = query();
  const name = state.name;
  listInflight = fetch(url("api/inbox/" + encodeURIComponent(name) + (qs ? "?" + qs : "")))
    .then((r) => r.json());
  let d;
  try {
    d = await listInflight;
  } catch (_) {
    return;
  } finally {
    listInflight = null;
  }
  // A poll that landed after the reader moved on must not repaint the list with
  // somebody else's mail under the new heading.
  if (d.error || d.name !== state.name) return;
  state.loaded = true;
  state.items = d.items || [];
  renderMeta(d);
  renderList();
}

function select(name, silent) {
  if (!name) return;
  const changed = name !== state.name;
  state.name = name;
  if (changed) {
    // A different inbox is a different set of senders, topics and rooms, so
    // every filter derived from the old one is dropped rather than carried
    // into a view where it may match nothing without saying why.
    for (const k of FILTERS) {
      const id = k === "q" ? "f-q" : "f-" + k;
      $(id).value = "";
    }
    state.facets = "";
    state.items = [];
    state.loaded = false;
    // Until the new inbox's response says otherwise, assume it is not yours.
    // Carrying the previous answer forward would flash write controls over
    // somebody else's mail for one frame.
    state.isMine = false;
  }
  writeHash(currentHash());
  renderRoster();
  if (!silent || changed) renderList();
  loadList();
}

function apply() {
  writeHash(currentHash());
  state.facets = "";
  loadList();
}

// A filtered view is meant to be a LINK, so the hash has to be live: pasting one
// into an open tab, or pressing Back, must re-select and re-filter. apply() uses
// replaceState, which fires no hashchange, so this cannot loop.
function onHashChange() {
  const f = readHash();
  state.newestFirst = f.order === "new";
  syncOrderButton();
  $("ib-find").value = f.find;
  $("ib-waiting").checked = f.waiting;
  for (const k of FILTERS) $(k === "q" ? "f-q" : "f-" + k).value = f[k];
  state.facets = "";
  if (f.name && f.name !== state.name) {
    state.name = f.name;
    state.items = [];
    state.loaded = false;
    state.isMine = false;
  }
  renderRoster();
  loadList();
}

function syncOrderButton() {
  const b = $("f-order");
  b.textContent = state.newestFirst ? "Newest first" : "Oldest first";
  b.setAttribute("aria-pressed", state.newestFirst ? "true" : "false");
  b.title = state.newestFirst
    ? "newest first (reverse chronological)"
    : "oldest first (chronological)";
}

// ---- wiring ----------------------------------------------------------------

function init() {
  const f = readHash();
  state.newestFirst = f.order === "new";
  state.name = f.name || "";
  $("ib-find").value = f.find;
  $("ib-waiting").checked = f.waiting;
  for (const k of FILTERS) $(k === "q" ? "f-q" : "f-" + k).value = f[k];
  syncOrderButton();

  addEventListener("hashchange", onHashChange);

  for (const k of ["from", "topic", "room", "delivery", "state", "age"]) {
    $("f-" + k).addEventListener("change", apply);
  }
  let t = null;
  $("f-q").addEventListener("input", () => { clearTimeout(t); t = setTimeout(apply, 200); });
  $("f-clear").addEventListener("click", () => {
    for (const k of FILTERS) $(k === "q" ? "f-q" : "f-" + k).value = "";
    apply();
  });
  $("ib-markall").addEventListener("click", (e) => markRead({ all: true }, e.currentTarget));
  $("f-order").addEventListener("click", () => {
    state.newestFirst = !state.newestFirst;
    syncOrderButton();
    writeHash(currentHash());
    renderList();
  });

  // The nav's own controls filter what is ALREADY loaded — no request, so they
  // stay responsive on a host with a large fleet.
  $("ib-find").addEventListener("input", () => { renderRoster(); writeHash(currentHash()); });
  $("ib-waiting").addEventListener("change", () => { renderRoster(); writeHash(currentHash()); });

  renderList();
  loadRoster();
  // A deep link names its inbox, and loadRoster only selects one when the hash
  // named none — so without this the linked-to inbox sat empty until the first
  // poll five seconds later, under a heading that already said its name.
  loadList();
  // The same 5s cadence the launcher and the board poll at. Both reads are
  // pure: no cursor moves because a tab was left open.
  setInterval(() => { loadRoster(); loadList(); }, 5000);
}

init();
