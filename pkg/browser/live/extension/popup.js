// ycode-live extension — popup
//
// Lets the user pick the port the Go-side hub is listening on
// (defaults to 58082, persists via chrome.storage.local), and
// connect/disconnect the extension to it. Connecting also captures
// the currently-active tab so requests target it.
//
// Also renders a live debug-log panel of recent tool calls fed from
// background.js over a long-lived chrome.runtime.connect port.

document.getElementById("version").textContent =
  "v" + chrome.runtime.getManifest().version;

const portEl = document.getElementById("port");
const connectBtn = document.getElementById("connect");
const disconnectBtn = document.getElementById("disconnect");
const statusEl = document.getElementById("status");
const debugListEl = document.getElementById("debug-list");
const mirrorEl = document.getElementById("mirror");
const clearEl = document.getElementById("clear");
const popoutEl = document.getElementById("popout");

// When loaded with ?windowed=1 we're in the detached resizable window —
// the body fills the viewport and the pop-out button hides itself.
const isWindowed = new URLSearchParams(location.search).has("windowed");
if (isWindowed) {
  document.body.classList.add("windowed");
  popoutEl.style.display = "none";
}

// Each browser has its own hub port, 58081 + (first letter - 'b'): brave
// 58081, chrome 58082, edge 58084. A port the
// user typed is kept; otherwise the field shows this browser's default.
const DEFAULT_HUB_PORT = /\bEdg\//.test(navigator.userAgent) ? 58084 : navigator.brave ? 58081 : 58082;
portEl.value = DEFAULT_HUB_PORT;
chrome.storage.local.get(["port", "portSetByUser"], ({ port, portSetByUser }) => {
  if (portSetByUser && typeof port === "number" && port > 0) portEl.value = port;
});
portEl.addEventListener("change", () => {
  chrome.storage.local.set({ portSetByUser: parseInt(portEl.value, 10) !== DEFAULT_HUB_PORT });
});

function setStatus(cls, text) {
  statusEl.className = cls;
  statusEl.textContent = text;
}

// --- route: is the selected tab the one a Bashy agent drives? ---
//
// The extension's worker holds the route (tab + agent + chat, for this
// socket). The popup only shows it, offers an explicit Focus, and lets the
// note follow it when the selected tab is the driven one. No route, or a
// stale one, leaves the manual recipient exactly as it was.
const routeEl = document.getElementById("route");
const routeTextEl = document.getElementById("route-text");
const routeListEl = document.getElementById("route-list");
let activeRoute = null; // the route the selected tab follows, else null

async function tabTitle(r) {
  try {
    const t = await chrome.tabs.get(r.tabId);
    return (t && (t.title || t.url)) || `tab ${r.tabId}`;
  } catch (_) {
    return `tab ${r.tabId}`;
  }
}

// One compact row per current route: the tab's real title, the agent's
// stable colour and name, and an explicit Focus for that tab.
async function renderRows(routes, selId) {
  const titles = await Promise.all(routes.map(tabTitle));
  routeListEl.replaceChildren();
  routes.forEach((r, i) => {
    const row = document.createElement("div");
    row.className = "route-row";
    row.setAttribute("data-tab-id", String(r.tabId));
    row.setAttribute("data-agent", r.agent);
    const sw = document.createElement("span");
    sw.className = "swatch";
    sw.setAttribute("data-color", r.color || "");
    sw.setAttribute("style", `background:${r.color || "#6d28d9"}`);
    const name = document.createElement("span");
    name.className = "agent";
    name.textContent = r.agent + (r.stale ? " (chat gone)" : "");
    const title = document.createElement("span");
    title.className = "title";
    title.textContent = titles[i];
    const btn = document.createElement("button");
    btn.className = "focus";
    btn.textContent = "Focus";
    btn.disabled = r.tabId === selId;
    btn.addEventListener("click", async () => {
      const resp = await chrome.runtime.sendMessage({ type: "ycode-live:focus", tabId: r.tabId });
      if (resp && !resp.ok) routeTextEl.textContent = resp.error || "could not focus the driven tab";
    });
    for (const c of [sw, name, title, btn]) row.appendChild(c);
    routeListEl.appendChild(row);
  });
}

async function renderRoute(resp) {
  const routes = (resp && resp.routes) || [];
  const sel = routes.length ? await currentTab() : null;
  const route = routes.find((r) => sel && r.tabId === sel.id) || null;
  activeRoute = route && !route.stale ? route : null;
  routeEl.className = !routes.length ? "" : route ? (route.stale ? "stale" : "driven") : "other";
  if (!routes.length) routeTextEl.textContent = "";
  else if (route && route.stale) routeTextEl.textContent = `Bashy route stale: ${route.agent}'s chat is gone. Type a recipient below.`;
  else if (route) routeTextEl.textContent = `This tab is driven by Bashy: ${route.agent}. Notes go to its chat.`;
  else routeTextEl.textContent = `Bashy agents drive ${routes.length} tab${routes.length === 1 ? "" : "s"}; the tab you selected is not driven by Bashy.`;
  await renderRows(routes, sel ? sel.id : 0);
  shapeToEl.disabled = !!activeRoute;
}

async function refreshStatus() {
  const resp = await chrome.runtime.sendMessage({ type: "ycode-live:status" });
  await renderRoute(resp);
  if (resp && resp.connected) {
    setStatus("connected", `connected on port ${resp.port}`);
  } else if (resp && resp.busy) {
    setStatus("disconnected", `waiting: ${resp.busy}`);
  } else {
    setStatus("disconnected", "disconnected");
  }
}

connectBtn.addEventListener("click", async () => {
  const port = parseInt(portEl.value, 10) || DEFAULT_HUB_PORT;
  await chrome.storage.local.set({ port });
  const tabs = await chrome.tabs.query({ active: true, currentWindow: true });
  const tabId = tabs.length > 0 ? tabs[0].id : null;
  setStatus("connecting", `connecting to 127.0.0.1:${port}...`);
  await chrome.runtime.sendMessage({ type: "ycode-live:connect", port, tabId });
  setTimeout(refreshStatus, 600);
});

disconnectBtn.addEventListener("click", async () => {
  await chrome.runtime.sendMessage({ type: "ycode-live:disconnect" });
  refreshStatus();
});

refreshStatus();

// --- Shape: send the current tab, with a note, to an agent ----------
//
// The hub decides who receives it: the agent named in To, or with To
// empty the latest live chat session, else the operator's own inbox. The
// status line says which. The note and the hub's answer are plain text
// end to end: read from .value, shown with textContent, never as markup.

const shapeToEl = document.getElementById("shape-to");
const shapeNoteEl = document.getElementById("shape-note");
const shapeBtn = document.getElementById("shape-send");
const shapeStatusEl = document.getElementById("shape-status");

chrome.storage.local.get(["shapeTo"], ({ shapeTo }) => {
  if (typeof shapeTo === "string") shapeToEl.value = shapeTo;
});
shapeToEl.addEventListener("change", () => {
  chrome.storage.local.set({ shapeTo: shapeToEl.value.trim() });
});

function setShapeStatus(cls, text) {
  shapeStatusEl.className = cls;
  shapeStatusEl.textContent = text;
}

// currentTab is the tab the human is looking at. The popped-out panel is
// a window of its own, so there it is the active tab of the last normal
// browser window.
async function currentTab() {
  if (isWindowed) {
    const win = await chrome.windows.getLastFocused({ populate: true, windowTypes: ["normal"] });
    return ((win && win.tabs) || []).find((t) => t.active) || null;
  }
  const tabs = await chrome.tabs.query({ active: true, currentWindow: true });
  return tabs.length > 0 ? tabs[0] : null;
}

shapeBtn.addEventListener("click", async () => {
  shapeBtn.disabled = true;
  setShapeStatus("", "sending...");
  try {
    const tab = await currentTab();
    if (!tab || typeof tab.id !== "number") {
      setShapeStatus("error", "no current tab to send");
      return;
    }
    await renderRoute(await chrome.runtime.sendMessage({ type: "ycode-live:status" }));
    const to = shapeToEl.value.trim();
    chrome.storage.local.set({ shapeTo: to });
    const resp = await chrome.runtime.sendMessage({
      type: "ycode-live:shape",
      tab: { id: tab.id, url: tab.url || "", title: tab.title || "" },
      note: shapeNoteEl.value,
      to,
      ...(activeRoute ? { useRoute: true } : {}),
    });
    if (resp && resp.ok) {
      setShapeStatus("sent", resp.status || "sent");
      // A note the hub had to cut stays in the box, so the rest can follow.
      if (!resp.truncated) shapeNoteEl.value = "";
    } else {
      setShapeStatus("error", (resp && resp.error) || "not sent");
    }
  } catch (e) {
    setShapeStatus("error", "not sent: " + String(e.message || e));
  } finally {
    await renderRoute(await chrome.runtime.sendMessage({ type: "ycode-live:status" }));
    shapeBtn.disabled = false;
  }
});

// --- debug log panel ------------------------------------------------

function fmtTime(ts) {
  const d = new Date(ts);
  const hh = String(d.getHours()).padStart(2, "0");
  const mm = String(d.getMinutes()).padStart(2, "0");
  const ss = String(d.getSeconds()).padStart(2, "0");
  return `${hh}:${mm}:${ss}`;
}

function renderRow(entry) {
  const row = document.createElement("div");
  row.className = "row " + (entry.ok ? "ok" : "err");
  const head = document.createElement("div");
  head.className = "head";

  const mark = document.createElement("span");
  mark.className = "mark";
  mark.textContent = entry.ok ? "✓" : "✗";

  const method = document.createElement("span");
  method.className = "method";
  method.textContent = `${fmtTime(entry.startedAt)}  ${entry.method}`;

  const dur = document.createElement("span");
  dur.className = "dur";
  dur.textContent = `${entry.durationMs}ms`;

  head.append(mark, method, dur);

  const body = document.createElement("div");
  body.className = "body";
  const tail = entry.error
    ? `<span class="label">error</span> ${escapeHtml(entry.error)}`
    : `<span class="label">result</span> ${escapeHtml(entry.resultPreview)}`;
  body.innerHTML = `<span class="label">params</span> ${escapeHtml(entry.paramsPreview)}\n${tail}`;

  row.append(head, body);
  row.addEventListener("click", () => row.classList.toggle("open"));
  return row;
}

function escapeHtml(s) {
  return String(s)
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;");
}

function renderAll(entries) {
  debugListEl.replaceChildren();
  for (const e of entries) prependRow(e);
}

function prependRow(entry) {
  const row = renderRow(entry);
  debugListEl.prepend(row);
  // Cap DOM to avoid unbounded growth if the popup stays open.
  while (debugListEl.children.length > 100) {
    debugListEl.removeChild(debugListEl.lastChild);
  }
}

const logPort = chrome.runtime.connect({ name: "ycode-live:log-stream" });
logPort.onMessage.addListener((msg) => {
  if (!msg) return;
  if (msg.kind === "snapshot") {
    mirrorEl.checked = !!msg.mirror;
    renderAll(msg.entries || []);
  } else if (msg.kind === "entry") {
    prependRow(msg.entry);
  } else if (msg.kind === "mirror") {
    mirrorEl.checked = !!msg.mirror;
  }
});

mirrorEl.addEventListener("change", () => {
  logPort.postMessage({ type: "mirror", value: mirrorEl.checked });
});

clearEl.addEventListener("click", () => {
  logPort.postMessage({ type: "clear" });
});

popoutEl.addEventListener("click", async () => {
  await chrome.windows.create({
    url: chrome.runtime.getURL("popup.html?windowed=1"),
    type: "popup",
    width: 560,
    height: 720,
  });
  window.close();
});
