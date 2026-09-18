// The message board page.
//
// A full browser page like the terminal, not a pane inside the launcher, and
// its <base href> is the LAUNCHER's — so "api/mb" and "./" resolve correctly at
// / on loopback and under outpost's /matrix/h/<host>/app/<name>/ prefix alike.
//
// The board is PUBLIC BY CONSTRUCTION and this page reads it as a whole. What it
// deliberately does NOT do is advance anybody's cursor: `seen_seq` only draws
// the "unread for me" line. A page that marked posts read would eat an agent's
// mail every time somebody opened a tab.
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

const MAX_BODY = 1024;
const state = { posts: [], high: 0, reader: "", seen: 0, matched: 0, facets: "" };

// ---- filters live in the hash, so a filtered board is a link -----------------

function readHash() {
  const p = new URLSearchParams(location.hash.replace(/^#/, ""));
  return {
    from: p.get("from") || "",
    to: p.get("to") || "",
    topic: p.get("topic") || "",
    q: p.get("q") || "",
    unread: p.get("unread") === "1",
  };
}

function writeHash(f) {
  const p = new URLSearchParams();
  for (const k of ["from", "to", "topic", "q"]) if (f[k]) p.set(k, f[k]);
  if (f.unread) p.set("unread", "1");
  const s = p.toString();
  const next = s ? "#" + s : "#";
  if (location.hash !== next) history.replaceState(null, "", next);
}

function query(f, extra) {
  const p = new URLSearchParams();
  for (const k of ["from", "to", "topic", "q"]) if (f[k]) p.set(k, f[k]);
  if (f.unread) p.set("unread", "1");
  for (const k in extra || {}) p.set(k, extra[k]);
  return p.toString();
}

// ---- rendering ---------------------------------------------------------------

function when(at) {
  const d = new Date(at);
  if (isNaN(d)) return at || "";
  return d.toLocaleString(undefined, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
}

function postEl(p) {
  const card = el("article", "mb-post");
  if (p.directed) card.classList.add("directed");
  if (p.seq > state.seen) card.classList.add("fresh");

  const head = el("header");
  head.append(el("span", "seq", "#" + p.seq));
  head.append(el("span", "from", p.from));
  head.append(el("span", "arrow", "→"));
  head.append(el("span", "to", p.to || "all"));
  if (p.topic && p.topic !== "mb") {
    const t = el("button", "topic", p.topic);
    t.type = "button";
    t.title = "filter to this topic";
    t.addEventListener("click", (e) => {
      e.stopPropagation();
      $("f-topic").value = p.topic;
      apply();
    });
    head.append(t);
  }
  if (p.mode === "any") head.append(el("span", "tag", "offer"));
  head.append(el("span", "spacer"));
  head.append(el("time", "at", when(p.at)));
  card.append(head);

  card.append(el("p", "body", p.body));

  const receipt = el("div", "receipt");
  receipt.hidden = true;
  card.append(receipt);

  // Receipts are one file read per post on the server, so they are fetched only
  // for the post somebody actually asked about.
  card.addEventListener("click", async () => {
    receipt.hidden = !receipt.hidden;
    if (receipt.hidden || receipt.dataset.loaded) return;
    receipt.textContent = "reading receipts…";
    try {
      const r = await fetch(url("api/mb/" + p.seq + "/viewers")).then((x) => x.json());
      const bits = [];
      if (r.holder) bits.push("claimed by " + r.holder);
      bits.push(r.viewers && r.viewers.length ? "read by " + r.viewers.join(", ") : "no reader has recorded a view");
      receipt.textContent = bits.join(" · ");
      receipt.dataset.loaded = "1";
    } catch (_) {
      receipt.textContent = "receipts unavailable";
    }
  });
  return card;
}

function renderList() {
  const host = $("mb-list");
  if (!state.posts.length) {
    host.replaceChildren(el("p", "empty", "Nothing on the board matches."));
    return;
  }
  host.replaceChildren(...state.posts.map(postEl));
}

// `want` comes from the HASH, never from the element. The options do not exist
// until the first response lands, so a select whose value was set at init from
// a linked-to filter silently reverts to "anyone" — the data would be filtered
// and the control would say it was not.
function fillSelect(sel, facets, want) {
  const cur = want || "";
  const first = sel.firstElementChild.cloneNode(true);
  sel.replaceChildren(first);
  for (const f of facets) {
    const o = el("option", null, f.name + " (" + f.count + ")");
    o.value = f.name;
    sel.append(o);
  }
  sel.value = cur;
  // A filter naming something no longer on the board would silently match
  // nothing; keep it selectable rather than dropping it without saying so.
  if (sel.value !== cur && cur) {
    const o = el("option", null, cur + " (0)");
    o.value = cur;
    sel.append(o);
    sel.value = cur;
  }
}

// A selector audience ("band 4 · tool ycode") and the broadcast pseudo-name
// ("all") describe a set, not an address: offering them as send targets would
// invite a post that resolves to nothing.
const addressable = (n) => n && n !== "all" && !n.includes("·") && !/^band /.test(n);

function renderMeta(d) {
  state.reader = d.reader;
  state.seen = d.seen_seq;
  $("mb-who").textContent = "signing as " + d.reader;

  // Facets are rebuilt only when they actually changed. A poll every five
  // seconds that replaced the options unconditionally would close an open
  // dropdown under the reader's cursor.
  const f = readHash();
  const stamp = JSON.stringify(d.facets) + "|" + JSON.stringify(f);
  if (stamp !== state.facets) {
    state.facets = stamp;
    fillSelect($("f-from"), d.facets.from, f.from);
    fillSelect($("f-to"), d.facets.to, f.to);
    fillSelect($("f-topic"), d.facets.topic, f.topic);

    const names = new Set();
    for (const f of d.facets.from) if (addressable(f.name)) names.add(f.name);
    for (const f of d.facets.to) if (addressable(f.name)) names.add(f.name);
    names.delete(d.reader);
    $("mb-agents").replaceChildren(...[...names].sort().map((n) => {
      const o = el("option"); o.value = n; return o;
    }));

    const topics = new Set(d.concerns);
    for (const f of d.facets.topic) topics.add(f.name);
    $("mb-topics").replaceChildren(...[...topics].map((n) => {
      const o = el("option"); o.value = n; return o;
    }));
  }

  // The count describes WHAT IS ON SCREEN, never the last response. An
  // incremental poll matches only the posts newer than the cursor, so reading
  // `matched` here printed "0 of 283 posts" over a full screen of them five
  // seconds after load.
  const shown = state.posts.length;
  const unread = Math.max(0, d.high_seq - d.seen_seq);
  const parts = [];
  parts.push(state.matched > shown
    ? shown + " of " + state.matched + " matching"
    : shown + " post" + (shown === 1 ? "" : "s"));
  if (state.matched !== d.total) parts.push(d.total + " on the board");
  if (unread) parts.push(unread + " since you last read");
  $("mb-count").textContent = parts.join(" · ");

  const declared = d.declared && d.declared.length ? d.declared.join(", ") : "none";
  $("mb-foot").textContent =
    "The live board only — posts are archived after " + d.retention_days +
    " days and `bashy mb --history` is the full record. Your declared concerns: " + declared + ".";
}

// ---- loading -----------------------------------------------------------------

let inflight = null;

async function load(full) {
  const f = readHash();
  if (inflight) return;
  const qs = query(f, full ? {} : { since: state.high });
  inflight = fetch(url("api/mb?" + qs)).then((r) => r.json());
  let d;
  try {
    d = await inflight;
  } catch (_) {
    return;
  } finally {
    inflight = null;
  }
  if (d.error) return;

  if (full) {
    state.posts = d.posts;
    // `matched` counts what the FILTER matched, which only a full load asks.
    state.matched = d.matched;
  } else if (d.posts.length) {
    state.matched += d.posts.length;
    // The board is append-only and seq is monotonic, so new posts are simply
    // the newer ones — no merge, no dedupe.
    state.posts = d.posts.concat(state.posts);
  }
  state.high = d.high_seq;
  renderMeta(d);
  if (full || d.posts.length) renderList();
}

function apply() {
  writeHash({
    from: $("f-from").value,
    to: $("f-to").value,
    topic: $("f-topic").value,
    q: $("f-q").value.trim(),
    unread: $("f-unread").checked,
  });
  reload();
}

// reload drops everything derived from the old filter. state.high must go too:
// leaving it would make the next poll ask for posts newer than a sequence the
// new filter never fetched, and the first screen would come back empty.
function reload() {
  state.posts = [];
  state.high = 0;
  state.matched = 0;
  load(true);
}

// A filtered board is meant to be a LINK, so the hash has to be live: pasting
// one into an open tab, or pressing Back, must re-filter. apply() uses
// replaceState, which fires no hashchange, so this cannot loop.
function onHashChange() {
  syncControls(readHash());
  reload();
}

function syncControls(f) {
  $("f-q").value = f.q;
  $("f-unread").checked = f.unread;
  // The selects are filled from the hash in renderMeta, once their options
  // exist; forcing a refill is what makes that happen on a hash change.
  state.facets = "";
}

// ---- the composer ------------------------------------------------------------

const bytes = (s) => new TextEncoder().encode(s).length;

function countBody() {
  const n = bytes($("c-body").value);
  const c = $("c-count");
  c.textContent = n + " / " + MAX_BODY + " bytes";
  c.classList.toggle("over", n > MAX_BODY);
  $("c-send").disabled = n === 0 || n > MAX_BODY;
}

async function send(e) {
  e.preventDefault();
  const body = $("c-body").value;
  if (!body.trim() || bytes(body) > MAX_BODY) return;
  const out = $("c-result");
  out.className = "";
  out.textContent = "posting…";
  $("c-send").disabled = true;
  try {
    const res = await fetch(url("api/mb"), {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ to: $("c-to").value.trim(), topic: $("c-topic").value.trim(), body }),
    });
    const d = await res.json();
    if (!res.ok) {
      out.className = "bad";
      // The board's own refusal, near misses and all — it names what would
      // have worked, which a generic "failed" cannot.
      out.textContent = d.error || "post refused";
      return;
    }
    $("c-body").value = "";
    // A SendResult is already the board's delivery truth. Keep every recipient
    // beside its canonical state and the transport's own reason: reducing this
    // to unique state names loses who is waiting (and why) on a group send.
    const r = d.result || {};
    out.className = "ok";
    out.replaceChildren(document.createTextNode("#" + r.seq + " to " + r.label));
    for (const delivery of r.deliveries || []) {
      const line = [delivery.to, delivery.state, delivery.reason].filter(Boolean).join(" · ");
      if (!line) continue;
      out.append(document.createElement("br"), el("span", "delivery", line));
    }
    await load(false);
  } catch (_) {
    out.className = "bad";
    out.textContent = "the console did not answer";
  } finally {
    countBody();
  }
}

// ---- wiring ------------------------------------------------------------------

function init() {
  syncControls(readHash());
  addEventListener("hashchange", onHashChange);

  for (const id of ["f-from", "f-to", "f-topic", "f-unread"]) $(id).addEventListener("change", apply);
  let t = null;
  $("f-q").addEventListener("input", () => { clearTimeout(t); t = setTimeout(apply, 200); });
  $("f-clear").addEventListener("click", () => {
    for (const id of ["f-from", "f-to", "f-topic", "f-q"]) $(id).value = "";
    $("f-unread").checked = false;
    apply();
  });

  $("c-body").addEventListener("input", countBody);
  $("c-body").addEventListener("keydown", (e) => {
    if ((e.metaKey || e.ctrlKey) && e.key === "Enter") $("mb-composer").requestSubmit();
  });
  $("mb-composer").addEventListener("submit", send);
  countBody();

  load(true);
  // The same 5s cadence the launcher polls at. The log is append-only with a
  // monotonic seq, so a quiet poll carries no posts at all.
  setInterval(() => load(false), 5000);
}

init();
