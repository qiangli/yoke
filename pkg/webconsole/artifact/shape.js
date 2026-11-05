// The Shape panel: one person asks for a change to a shaped app, previews the
// candidate the shaping agent produced, and approves or rejects it. Everything
// here is that person's alone — the server scopes every candidate to the
// signed-in principal.
const url = (p) => new URL(p, document.baseURI);

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

const app = (location.pathname.match(/\/shape\/([^/]+)\/?$/) || [])[1] || "";
const api = (p) => url("api/shape/" + encodeURIComponent(app) + (p || ""));
let polling = 0;

async function call(method, path, body) {
  const res = await fetch(api(path), {
    method,
    headers: body ? { "Content-Type": "application/json" } : {},
    body: body ? JSON.stringify(body) : undefined,
  });
  const text = await res.text();
  if (!res.ok) {
    let msg = text.trim();
    try { const j = JSON.parse(text); msg = (j.verdict && j.verdict.detail) || j.error || msg; } catch (_) {}
    throw new Error(msg || res.statusText);
  }
  return text ? JSON.parse(text) : {};
}

function button(label, cls, fn, ariaLabel) {
  const b = el("button", "btn" + (cls ? " " + cls : ""), label);
  b.type = "button";
  // Every control carries an accessible name: the visible label by default,
  // a more specific description where the label repeats across versions.
  b.setAttribute("aria-label", ariaLabel || label);
  b.addEventListener("click", async () => {
    b.disabled = true;
    $("shape-result").textContent = "";
    try { await fn(); } catch (e) { $("shape-result").textContent = e.message; }
    b.disabled = false;
    refresh();
  });
  return b;
}

function render(c) {
  const card = el("article", "shape-card");
  const head = el("div", "row");
  head.append(el("b", "", c.request), el("span", "spacer"), el("span", "shape-status " + c.status, c.status));
  card.append(head);
  const meta = [];
  if (c.new_app) meta.push("Genie made a new app for this wish: " + (c.new_app.label || c.new_app.app) + " (its first wish is on the new app's draft)");
  else if (c.kind === "request") meta.push("filed as a request from a protected page: nothing is built; a maintainer takes it from here" + (c.deciding ? " (Genie is reading whether it asks for a new app)" : ""));
  if (c.page) meta.push("from page " + c.page);
  if (c.element) meta.push("pointing at " + c.element);
  if (c.agent) meta.push("by " + c.agent);
  if (c.verdict) meta.push(c.verdict.pass ? "verified" : "verification failed" + (c.verdict.detail ? ": " + c.verdict.detail : ""));
  if (c.error) meta.push(c.error);
  if (meta.length) card.append(el("div", "shape-meta", meta.join(" · ")));
  if (c.rev) {
    const acts = el("div", "row foot");
    const prev = el("a", "", "Review");
    prev.href = url("shape/" + encodeURIComponent(app) + "/preview/" + c.id + "/");
    prev.setAttribute("aria-label", "Review candidate " + c.id + " in place, inside the app");
    acts.append(prev, el("span", "spacer"),
      button("Reject", "", () => call("POST", "/candidates/" + c.id + "/reject")),
      button("Approve", "primary", () => call("POST", "/candidates/" + c.id + "/approve")));
    card.append(acts);
  } else if (c.status !== "running") {
    const acts = el("div", "row foot");
    acts.append(el("span", "spacer"));
    if (c.new_app) {
      const open = el("a", "btn primary", "Open the draft of " + (c.new_app.label || c.new_app.app));
      open.href = url("shape/" + encodeURIComponent(c.new_app.app) + "/draft/");
      open.setAttribute("aria-label", "Continue in the edit session of the new app " + c.new_app.app);
      acts.append(open);
    }
    acts.append(button("Discard", "", () => call("POST", "/candidates/" + c.id + "/reject")));
    card.append(acts);
  }
  return card;
}

// renderDraft is the open edit session (S526.B1h): its wishes so far and
// the way back in. Save and Cancel happen in the app, under the editing bar.
function renderDraft(d) {
  const card = el("article", "shape-card shape-draft");
  card.setAttribute("data-shape-draft", "");
  const head = el("div", "row");
  const n = (d.wishes || 0) + " " + (d.wishes === 1 ? "wish" : "wishes");
  head.append(el("b", "", "Editing draft: " + n), el("span", "spacer"), el("span", "shape-status draft", d.busy ? "working" : "open"));
  card.append(head);
  const applied = (d.steps || []).slice(0, d.cursor || 0);
  const undone = (d.steps || []).length - (d.cursor || 0);
  const meta = [];
  if (applied.length) meta.push(applied.map((s, i) => (i + 1) + ". " + s.request).join(" · "));
  if (undone) meta.push(undone + " undone");
  if (d.busy && d.running) meta.push("Genie is working on: " + d.running.request);
  meta.push("private to you; your active version does not change until you save");
  card.append(el("div", "shape-meta", meta.join(" · ")));
  const acts = el("div", "row foot");
  const back = el("a", "btn primary", "Continue editing");
  back.href = url("shape/" + encodeURIComponent(app) + "/draft/");
  back.setAttribute("aria-label", "Continue the edit session of " + app + " in place, inside the app");
  acts.append(el("span", "spacer"), back);
  card.append(acts);
  return card;
}

const short = (s) => (s || "").slice(0, 12);

function historyStatus(msg) {
  $("history-status").textContent = msg || "";
}

async function historyCall(method, path, body) {
  const res = await fetch(api("/history" + (path || "")), {
    method,
    headers: body ? { "Content-Type": "application/json" } : {},
    body: body ? JSON.stringify(body) : undefined,
  });
  const text = await res.text();
  if (!res.ok) throw new Error((text || res.statusText).trim() || res.statusText);
  return text ? JSON.parse(text) : {};
}

function renderVersion(v) {
  const li = document.createElement("li");
  li.className = "history-version";
  const head = el("div", "row");
  head.append(el("b", "history-summary", (v.trigger ? "repair of " + v.trigger + " · " : "") + (v.summary || v.request || "Base release")), el("span", "spacer"));
  if (v.active) head.append(el("span", "shape-status", "Current"));
  li.append(head);
  const meta = [];
  if (v.author) meta.push("by " + v.author);
  if (v.when) meta.push(v.when);
  if (meta.length) li.append(el("div", "shape-meta", meta.join(" · ")));
  const details = el("a", "history-details", "Details");
  details.href = url("loom/qiangli/" + encodeURIComponent(app) + "/commit/" + encodeURIComponent(v.activation || v.revision));
  details.setAttribute("aria-label", "Details for version " + short(v.revision) + " in Loom");

  const acts = el("div", "row foot");
  const restore = async () => {
    historyStatus("");
    try {
      const res = await historyCall("POST", "/restore", { rev: v.revision });
      historyStatus("Active version is now " + short(res.active) + ".");
    } catch (e) { historyStatus(e.message); }
    refresh();
  };
  const restoreBtn = button("Restore", "primary", restore, "Restore version " + short(v.revision));
  acts.append(details, el("span", "spacer"));
  if (v.submittable) {
    acts.append(button("Submit for review", "", async () => {
      historyStatus("Submitting for review…");
      try {
        const submitted = await call("POST", "/submit", { rev: v.revision });
        historyStatus("Submitted for review: proposal " + submitted.pr_id + ", run " + submitted.run.id + ".");
      } catch (e) { historyStatus(e.message); }
    }, "Submit version " + short(v.revision) + " for review"));
  }
  acts.append(restoreBtn);
  li.append(acts);
  return li;
}

async function refreshHistory() {
  let h;
  try { h = await historyCall("GET", ""); }
  catch (e) { historyStatus(e.message); return; }
  const list = $("history-list");
  const rows = (h.versions || []).slice().sort((a, b) => Date.parse(b.when || 0) - Date.parse(a.when || 0)).map(renderVersion);
  // The base release is the oldest restorable version: its Restore button
  // is the last one, and restoring it reverts the app to how it started.
  // It is only offered once a version is recorded; with no versions yet the
  // app is already the base release.
  if (h.base && (h.versions || []).length) {
    rows.push(renderVersion(h.base_version || { revision: h.base, kind: "base", summary: "Base release", active: h.active === h.base }));
  }
  list.replaceChildren(...rows);
  if (!(h.versions || []).length) historyStatus("No versions yet. Shaped changes you keep will appear here.");
}

async function refresh() {
  let st;
  try { st = await call("GET", ""); } catch (e) { $("shape-result").textContent = e.message; return; }
  $("shape-who").textContent = st.principal;
  $("shape-active").textContent = st.active ? "your version " + st.active.slice(0, 10) : "";
  const list = $("shape-list");
  list.replaceChildren(...(st.candidates || []).sort((a, b) => (a.created < b.created ? 1 : -1)).map(render));
  if (st.draft) list.prepend(renderDraft(st.draft));
  clearTimeout(polling);
  if ((st.candidates || []).some((c) => c.status === "running" || c.deciding)) polling = setTimeout(refresh, 3000);
  refreshHistory();
}

$("history-undo").addEventListener("click", async () => {
  historyStatus("");
  try {
    const res = await historyCall("POST", "/undo");
    historyStatus("Undid the restore. Active version is now " + short(res.active) + ".");
  } catch (e) { historyStatus(e.message); }
  refresh();
});
$("shape-app").textContent = app;
$("shape-ask").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const request = $("shape-request").value.trim();
  if (!request) return;
  $("shape-send").disabled = true;
  $("shape-result").textContent = "";
  try {
    await call("POST", "/candidates", { request });
    $("shape-request").value = "";
  } catch (e) { $("shape-result").textContent = e.message; }
  $("shape-send").disabled = false;
  refresh();
});
refresh();
