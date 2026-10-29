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

function button(label, cls, fn) {
  const b = el("button", "btn" + (cls ? " " + cls : ""), label);
  b.type = "button";
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

async function refresh() {
  let st;
  try { st = await call("GET", ""); } catch (e) { $("shape-result").textContent = e.message; return; }
  $("shape-who").textContent = st.principal;
  $("shape-active").textContent = st.active ? "your version " + st.active.slice(0, 10) : "";
  const list = $("shape-list");
  list.replaceChildren(...(st.candidates || []).sort((a, b) => (a.created < b.created ? 1 : -1)).map(render));
  clearTimeout(polling);
  if ((st.candidates || []).some((c) => c.status === "running")) polling = setTimeout(refresh, 3000);
}

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
