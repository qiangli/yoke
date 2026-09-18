// The standalone terminal page.
//
// A full browser page, not a pane inside the launcher: the shell gets the whole
// viewport, browser history and zoom behave normally, and the tab can be kept
// open on its own.
//
// Its <base href> is the LAUNCHER's, so "term/ws" and "./" resolve correctly at
// / on loopback and under outpost's /matrix/h/<host>/app/<name>/ prefix alike.
const url = (p) => new URL(p, document.baseURI);
const statusEl = document.getElementById("status");

// Theme follows the launcher's saved preference so the two pages match.
try {
  const cfg = JSON.parse(localStorage.getItem("bashy.apps.config") || "{}");
  if (cfg.theme && cfg.theme !== "system") document.documentElement.setAttribute("data-theme", cfg.theme);
} catch (_) {}

const isDark = () => {
  const t = document.documentElement.getAttribute("data-theme");
  if (t) return t === "dark";
  return matchMedia("(prefers-color-scheme: dark)").matches;
};

function start() {
  const host = document.getElementById("term");
  host.replaceChildren();
  statusEl.textContent = "";

  const term = new Terminal({
    cursorBlink: true,
    fontFamily: 'ui-monospace, SFMono-Regular, Menlo, "Cascadia Mono", monospace',
    fontSize: 13,
    theme: isDark()
      ? { background: "#0b0b0d", foreground: "#fafafa", cursor: "#fafafa" }
      : { background: "#1b1b1f", foreground: "#f4f4f5", cursor: "#f4f4f5" },
  });
  const fit = new FitAddon.FitAddon();
  term.loadAddon(fit);
  term.open(host);
  fit.fit();

  const ws = url("term/ws");
  ws.protocol = ws.protocol === "https:" ? "wss:" : "ws:";
  ws.searchParams.set("cols", term.cols);
  ws.searchParams.set("rows", term.rows);

  const sock = new WebSocket(ws);
  sock.binaryType = "arraybuffer";
  const enc = new TextEncoder();

  sock.onopen = () => term.focus();
  // Binary frames both ways: raw PTY bytes, never re-encoded. Text frames are
  // reserved for control messages, which is why resize is JSON and input is not.
  sock.onmessage = (e) => {
    if (e.data instanceof ArrayBuffer) term.write(new Uint8Array(e.data));
    else term.write(e.data);
  };
  term.onData((d) => { if (sock.readyState === WebSocket.OPEN) sock.send(enc.encode(d)); });

  const sendSize = () => {
    if (sock.readyState !== WebSocket.OPEN) return;
    sock.send(JSON.stringify({ type: "size", cols: term.cols, rows: term.rows }));
  };
  const ro = new ResizeObserver(() => { try { fit.fit(); sendSize(); } catch (_) {} });
  ro.observe(host);

  const ended = (why) => {
    ro.disconnect();
    statusEl.replaceChildren();
    const msg = document.createElement("span");
    msg.textContent = why;
    const again = document.createElement("button");
    again.className = "btn";
    again.textContent = "New session";
    again.addEventListener("click", start);
    statusEl.append(msg, again);
  };
  // The server closes with a reason when it can (a shell that quit during its
  // own startup is the common one); 1006 means the connection simply dropped.
  sock.onclose = (e) => ended(e.reason || (e.code === 1006 ? "Connection lost." : "Session ended."));
  sock.onerror = () => ended("Connection failed.");
}

// A terminal is rarely wanted one at a time, and going back to the launcher to
// get a second one is a detour through a page that exists to be left. The +
// opens another in its own tab, which is what every app here does.
document.getElementById("new-term").addEventListener("click", () => {
  window.open(url("term/"), "_blank", "noopener");
});

start();
