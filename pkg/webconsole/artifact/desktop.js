// The standalone desktop page: a peer host's screen over the mesh.
//
// A port of Periscope's desktop client (cloudbox ui/src/features/desktop) to
// plain JS. Same wire: outpost's /desktop relay expects ONE JSON frame
// {user,password} before the RFB stream, so it can terminate the VNC server's
// own authentication and the browser half runs as auth None. noVNC opens its
// own socket when handed a URL, so to inject that frame the socket is opened
// here and the already-open WebSocket is passed to RFB instead.
//
// The console proxies desktop/<peer>/ws to a mesh forward of that peer's
// outpost — never through cloudbox. Its <base href> is the launcher's, so the
// relative URLs resolve at / and under a prefix alike.
import RFB from "./vendor/novnc/core/rfb.js";

const url = (p) => new URL(p, document.baseURI);
const $ = (id) => document.getElementById(id);
const statusEl = $("status"), form = $("creds"), screen = $("screen"), errEl = $("error"), discBtn = $("disconnect");

// The peer id is the path segment after "desktop": /desktop/<peer> at the root
// and .../app/<name>/desktop/<peer> under outpost's prefix alike.
const segs = location.pathname.split("/").filter(Boolean);
const peer = segs[segs.indexOf("desktop") + 1] || "";
$("desktop-peer").textContent = peer ? peer.slice(0, 12) + "…" + peer.slice(-6) : "";
document.title = "Desktop " + peer.slice(-6) + " — bashy";

try {
  const cfg = JSON.parse(localStorage.getItem("bashy.apps.config") || "{}");
  if (cfg.theme && cfg.theme !== "system") document.documentElement.setAttribute("data-theme", cfg.theme);
} catch (_) {}

const MAX_RECONNECT = 2;
const RECONNECT_BACKOFF_MS = [800, 1600];

// In memory only, never storage: cleared on Disconnect and on a hard failure.
let creds = null;
let attempt = 0;
let userClosed = false;
let rfb = null;
let sock = null;

const setStatus = (t) => { statusEl.textContent = t; };
const showError = (t) => { errEl.textContent = t; errEl.hidden = !t; };

function showForm(msg) {
  if (rfb) { try { rfb.disconnect(); } catch (_) {} rfb = null; }
  if (sock) { try { sock.close(); } catch (_) {} sock = null; }
  screen.hidden = true;
  screen.replaceChildren();
  form.hidden = false;
  discBtn.hidden = true;
  setStatus("");
  showError(msg || "");
  $("password").value = "";
  $("password").focus();
}

function connect() {
  if (!peer) { showForm("No peer in the address."); return; }
  if (!creds) { showForm(""); return; }
  form.hidden = true;
  screen.hidden = false;
  discBtn.hidden = false;
  setStatus("connecting…");

  const ws = url("desktop/" + encodeURIComponent(peer) + "/ws");
  ws.protocol = ws.protocol === "https:" ? "wss:" : "ws:";
  sock = new WebSocket(ws, "binary");
  sock.binaryType = "arraybuffer";
  let attached = false;
  const mine = sock;

  sock.addEventListener("open", () => {
    if (mine !== sock) { mine.close(); return; }
    try {
      mine.send(JSON.stringify(creds));
    } catch (e) {
      showForm("Failed to send credentials: " + e);
      return;
    }
    rfb = new RFB(screen, mine);
    attached = true;
    rfb.scaleViewport = true;
    rfb.resizeSession = false;
    // noVNC measures the container at construction; the flex layout settles a
    // tick later, so bounce scaleViewport once the box has its size.
    requestAnimationFrame(() => { if (rfb) { rfb.scaleViewport = false; rfb.scaleViewport = true; } });
    // noVNC announces the desktop's name before it declares the session
    // connected; keep the name in the header when it has one.
    rfb.addEventListener("connect", () => { attempt = 0; setStatus(document.body.dataset.desktopName || "connected"); document.body.dataset.desktop = "connected"; });
    rfb.addEventListener("desktopname", (e) => { setStatus(e.detail.name); document.body.dataset.desktopName = e.detail.name; });
    rfb.addEventListener("securityfailure", (e) => { showError("VNC security handshake failed" + (e.detail && e.detail.reason ? ": " + e.detail.reason : "") + "."); });
    rfb.addEventListener("disconnect", (e) => {
      rfb = null;
      document.body.dataset.desktop = "disconnected";
      if (userClosed) { creds = null; showForm(""); return; }
      if (e.detail && e.detail.clean && attempt < MAX_RECONNECT && creds) {
        const delay = RECONNECT_BACKOFF_MS[attempt] || 2000;
        attempt += 1;
        setStatus("reconnecting…");
        setTimeout(connect, delay);
        return;
      }
      creds = null;
      attempt = 0;
      showForm(errEl.hidden ? "Disconnected. Please reconnect." : errEl.textContent);
    });
  });
  sock.addEventListener("close", (e) => {
    if (mine !== sock) return;
    if (attached) return; // RFB's own disconnect handler runs
    creds = null;
    attempt = 0;
    // The relay closes with its reason before any RFB byte when the VNC
    // server refuses the credentials — say that, not a generic failure.
    showForm(e.reason ? "The peer refused: " + e.reason : "Connection failed. Is the peer's desktop on and reachable over the mesh?");
  });
}

form.addEventListener("submit", (e) => {
  e.preventDefault();
  creds = { user: $("user").value, password: $("password").value };
  userClosed = false;
  attempt = 0;
  showError("");
  connect();
});
discBtn.addEventListener("click", () => {
  userClosed = true;
  creds = null;
  if (rfb) { try { rfb.disconnect(); } catch (_) {} } else showForm("");
});
if (!peer) showForm("No peer in the address — open a desktop from the Neighborhood.");
else $("password").focus();
// The module has run and the form is wired: a submit before this point would
// be the browser's own GET, credentials in the address bar.
document.body.dataset.desktopReady = "1";
