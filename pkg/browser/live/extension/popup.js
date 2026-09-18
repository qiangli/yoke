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

chrome.storage.local.get(["port"], ({ port }) => {
  if (typeof port === "number" && port > 0) portEl.value = port;
});

function setStatus(cls, text) {
  statusEl.className = cls;
  statusEl.textContent = text;
}

async function refreshStatus() {
  const resp = await chrome.runtime.sendMessage({ type: "ycode-live:status" });
  if (resp && resp.connected) {
    setStatus("connected", `connected on port ${resp.port}`);
  } else {
    setStatus("disconnected", "disconnected");
  }
}

connectBtn.addEventListener("click", async () => {
  const port = parseInt(portEl.value, 10) || 58082;
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
