// The Shape panel: one person asks for a change to a shaped app, previews the
// candidate the shaping agent produced, and approves or rejects it. Everything
// here is that person's alone — the server scopes every candidate to the
// signed-in principal, and the diff arrives already rendered and escaped.
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
  if (c.agent) meta.push("by " + c.agent);
  if (c.verdict) meta.push(c.verdict.pass ? "verified" : "verification failed" + (c.verdict.detail ? ": " + c.verdict.detail : ""));
  if (c.error) meta.push(c.error);
  if (meta.length) card.append(el("div", "shape-meta", meta.join(" · ")));
  if (c.rev) {
    const diff = el("div", "shape-diff-host");
    fetch(api("/candidates/" + c.id + "/diff")).then((r) => (r.ok ? r.text() : "")).then((h) => { diff.innerHTML = h; });
    card.append(diff);
    const acts = el("div", "row foot");
    const prev = el("a", "btn", "Preview");
    prev.href = url("shape/" + encodeURIComponent(app) + "/preview/" + c.id + "/");
    prev.target = "_blank";
    prev.rel = "noopener";
    acts.append(prev, el("span", "spacer"),
      button("Verify", "", () => call("POST", "/candidates/" + c.id + "/verify")),
      button("Reject", "", () => call("POST", "/candidates/" + c.id + "/reject")),
      button("Approve", "primary", () => call("POST", "/candidates/" + c.id + "/approve")));
    card.append(acts);
  } else if (c.status !== "running") {
    const acts = el("div", "row foot");
    acts.append(el("span", "spacer"), button("Discard", "", () => call("POST", "/candidates/" + c.id + "/reject")));
    card.append(acts);
  }
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
  const title = el("b", "", (v.request || v.kind || "version") + (v.active ? " (active)" : ""));
  head.append(title, el("span", "spacer"), el("span", "shape-status", v.kind || ""));
  li.append(head);
  const meta = [];
  if (v.author) meta.push("by " + v.author);
  if (v.when) meta.push(v.when);
  if (v.names && v.names.length) meta.push("named " + v.names.join(", "));
  if (v.verdict) meta.push("verifier: " + v.verdict);
  if (v.evidence) meta.push("evidence " + v.evidence);
  if (v.active) meta.push("your active version");
  if (meta.length) li.append(el("div", "shape-meta", meta.join(" · ")));

  const nameId = "history-name-" + short(v.revision);
  const nameLabel = el("label", "", "Name");
  nameLabel.setAttribute("for", nameId);
  nameLabel.setAttribute("aria-label", "Name for version " + short(v.revision));
  const nameInput = document.createElement("input");
  nameInput.type = "text";
  nameInput.id = nameId;
  nameInput.name = nameId;
  nameInput.placeholder = "e.g. good";
  nameInput.setAttribute("aria-label", "Name for version " + short(v.revision));
  if (v.names && v.names.length) nameInput.value = v.names[0];

  const diffId = "history-diff-" + short(v.revision);
  const diffCheck = document.createElement("input");
  diffCheck.type = "checkbox";
  diffCheck.id = diffId;
  diffCheck.value = v.revision;
  diffCheck.className = "history-diff-select";
  diffCheck.setAttribute("aria-label", "Select version " + short(v.revision) + " for comparison");
  const diffLabel = el("label", "", "Compare");
  diffLabel.setAttribute("for", diffId);

  const prev = el("a", "btn", "Preview");
  prev.href = url("shape/" + encodeURIComponent(app) + "/history/" + encodeURIComponent(v.revision) + "/");
  prev.target = "_blank";
  prev.rel = "noopener";
  prev.setAttribute("aria-label", "Preview version " + short(v.revision));

  const acts = el("div", "row foot");
  const restore = async () => {
    historyStatus("");
    try {
      const res = await historyCall("POST", "/restore", { rev: v.revision });
      historyStatus("Active version is now " + short(res.active) + ".");
    } catch (e) { historyStatus(e.message); }
    refresh();
  };
  const saveName = async () => {
    historyStatus("");
    try {
      const label = nameInput.value.trim();
      if (!label) { historyStatus("Type a name first."); return; }
      await historyCall("POST", "/name", { rev: v.revision, name: label });
      historyStatus("Version " + short(v.revision) + " is now named " + label + ".");
    } catch (e) { historyStatus(e.message); }
    refresh();
  };
  const restoreBtn = button("Restore", "primary", restore, "Restore version " + short(v.revision));
  const nameBtn = button("Save name", "", saveName, "Save name for version " + short(v.revision));
  acts.append(prev, el("span", "spacer"), diffCheck, diffLabel, nameLabel, nameInput, nameBtn, restoreBtn);
  li.append(acts);
  return li;
}

async function refreshHistory() {
  let h;
  try { h = await historyCall("GET", ""); }
  catch (e) { historyStatus(e.message); return; }
  const list = $("history-list");
  const rows = (h.versions || []).map(renderVersion);
  // The base release is the oldest restorable version: its Restore button
  // is the last one, and restoring it reverts the app to how it started.
  // It is only offered once a version is recorded; with no versions yet the
  // app is already the base release.
  if (h.base && (h.versions || []).length) {
    rows.push(renderVersion({ revision: h.base, kind: "base", request: "base release", active: h.active === h.base }));
  }
  list.replaceChildren(...rows);
  if (!(h.versions || []).length) historyStatus("No versions yet. Shaped changes you keep will appear here.");
}

async function compareSelected() {
  historyStatus("");
  const picked = [...document.querySelectorAll(".history-diff-select:checked")].map((n) => n.value);
  if (picked.length !== 2) {
    historyStatus("Select exactly two versions to compare.");
    return;
  }
  const res = await fetch(api("/history/diff?from=" + encodeURIComponent(picked[0]) + "&to=" + encodeURIComponent(picked[1])));
  const host = $("history-diff-host");
  if (!res.ok) {
    host.replaceChildren();
    historyStatus((await res.text()).trim() || res.statusText);
    return;
  }
  host.innerHTML = await res.text();
  historyStatus("Showing " + short(picked[0]) + " compared with " + short(picked[1]) + ".");
}

async function refresh() {
  let st;
  try { st = await call("GET", ""); } catch (e) { $("shape-result").textContent = e.message; return; }
  $("shape-who").textContent = st.principal;
  $("shape-active").textContent = st.active ? "your version " + st.active.slice(0, 10) : "";
  const list = $("shape-list");
  list.replaceChildren(...(st.candidates || []).sort((a, b) => (a.created < b.created ? 1 : -1)).map(render));
  clearTimeout(polling);
  if ((st.candidates || []).some((c) => c.status === "running")) polling = setTimeout(refresh, 3000);
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
$("history-compare").addEventListener("click", compareSelected);
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
