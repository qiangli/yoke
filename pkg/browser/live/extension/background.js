// ycode-live extension — service worker
//
// Maintains a WebSocket to the ycode Go-side hub (default
// 127.0.0.1:58082/ws). Each incoming request is dispatched to a
// chrome.* API or a content-script injection; the result is shipped
// back over the same socket. A chrome.alarms ping every 25 seconds
// keeps the MV3 service worker from being killed mid-session.

const DEFAULT_PORT = 58082;
const RECONNECT_BASE_MS = 1000;
const RECONNECT_MAX_MS = 15000;
const KEEPALIVE_PERIOD_MIN = 25 / 60; // 25 seconds, expressed in minutes

let ws = null;
let reconnectMs = RECONNECT_BASE_MS;
let connectedPort = 0;
let activeTabId = null;
// User intent. Only the popup's connect/disconnect commands flip this.
// openWS/scheduleReconnect bail when it's false so a disconnect (or a
// socket that closes after the user asked to stop) fully tears down the
// reconnect loop instead of spinning on ERR_CONNECTION_REFUSED forever.
let wantConnected = false;
let reconnectTimer = null;

// --- entry: open/close commands from popup ---

chrome.runtime.onMessage.addListener((msg, _sender, sendResponse) => {
  if (msg && msg.type === "ycode-live:connect") {
    connectedPort = msg.port || DEFAULT_PORT;
    if (msg.tabId) {
      activeTabId = msg.tabId;
    }
    wantConnected = true;
    reconnectMs = RECONNECT_BASE_MS;
    openWS();
    sendResponse({ status: "connecting", port: connectedPort });
  } else if (msg && msg.type === "ycode-live:disconnect") {
    // Flip intent off *before* closing so the socket's onclose handler
    // doesn't schedule a fresh reconnect.
    wantConnected = false;
    if (reconnectTimer !== null) {
      clearTimeout(reconnectTimer);
      reconnectTimer = null;
    }
    if (ws) {
      try { ws.close(); } catch (e) { /* ignore */ }
      ws = null;
    }
    chrome.alarms.clear("ycode-live-keepalive");
    sendResponse({ status: "disconnected" });
  } else if (msg && msg.type === "ycode-live:status") {
    sendResponse({
      connected: ws !== null && ws.readyState === WebSocket.OPEN,
      port: connectedPort,
      tabId: activeTabId,
    });
  }
  return true;
});

// --- websocket lifecycle ---

function openWS() {
  if (!wantConnected) {
    return;
  }
  if (ws && ws.readyState !== WebSocket.CLOSED) {
    return;
  }
  const port = connectedPort || DEFAULT_PORT;
  const url = `ws://127.0.0.1:${port}/ws`;
  try {
    ws = new WebSocket(url);
  } catch (err) {
    console.warn("ycode-live: WebSocket constructor failed", err);
    scheduleReconnect();
    return;
  }
  ws.onopen = () => {
    reconnectMs = RECONNECT_BASE_MS;
    chrome.alarms.create("ycode-live-keepalive", { periodInMinutes: KEEPALIVE_PERIOD_MIN });
    // _hello envelope — id == 0, method == "_hello", result carries
    // {version, methods, permissions}. The hub uses this to detect
    // version drift without a separate round-trip.
    try {
      const manifest = chrome.runtime.getManifest();
      ws.send(JSON.stringify({
        id: 0,
        method: "_hello",
        result: {
          version: manifest.version,
          methods: SUPPORTED_METHODS,
          permissions: manifest.permissions || [],
        },
      }));
    } catch (e) {
      console.warn("ycode-live: _hello send failed", e);
    }
  };
  ws.onmessage = onMessage;
  ws.onerror = (e) => console.warn("ycode-live: socket error", e);
  ws.onclose = () => {
    ws = null;
    chrome.alarms.clear("ycode-live-keepalive");
    scheduleReconnect();
  };
}

function scheduleReconnect() {
  // Respect user intent — a disconnect (or a close that races the
  // disconnect command) must not revive the loop.
  if (!wantConnected) {
    return;
  }
  if (reconnectTimer !== null) {
    return; // a reconnect is already pending
  }
  reconnectTimer = setTimeout(() => {
    reconnectTimer = null;
    openWS();
  }, reconnectMs);
  reconnectMs = Math.min(reconnectMs * 2, RECONNECT_MAX_MS);
}

chrome.alarms.onAlarm.addListener((alarm) => {
  if (alarm.name === "ycode-live-keepalive" && ws && ws.readyState === WebSocket.OPEN) {
    // No-op send keeps the MV3 worker alive without server traffic.
    try { ws.send(JSON.stringify({ id: 0, method: "_ping" })); } catch (e) { /* ignore */ }
  }
});

// --- request dispatch ---

const SUPPORTED_METHODS = [
  "navigate", "back", "screenshot", "extract",
  "click", "type", "scroll", "tabs", "evaluate",
  "wait_for_selector", "keyboard_press",
  "clipboard_read", "clipboard_write",
  "cookies_get", "storage_get", "capabilities",
  "dispatch_event",
  // DevTools-flavored — gated on a long-lived chrome.debugger attach
  // managed by debuggerAttach below.
  "network_list", "console_get", "perf_start", "perf_stop", "lighthouse",
];

// --- chrome.debugger attach manager + devtools event buffers --------
//
// chrome.debugger.attach is exclusive per-target, so multiple consumers
// (trusted keystrokes; long-lived Network/Runtime/Tracing listeners)
// share one attach via reference counting. Each acquire(tabId, domain)
// must pair with release; the attach is dropped when refCount hits 0.
// onDetach (page close, user-dismissed banner, etc.) wipes the entry
// so a subsequent acquire reattaches cleanly.
//
// Ring buffers (network responses, console messages) live module-level
// and start capturing on first acquire — matches probe semantics
// ("capture begins when the agent asks").

const NET_RING_MAX = 200;
const CONSOLE_RING_MAX = 200;

let netRing = [];
let consoleRing = [];
let traceState = { active: false, startedAt: 0, eventCount: 0 };

function pushBounded(ring, entry, max) {
  ring.push(entry);
  if (ring.length > max) ring.splice(0, ring.length - max);
}

const debuggerAttach = {
  // tabId -> { refCount, domains: Set<string> }
  byTab: new Map(),

  async acquire(tabId, domain) {
    let state = this.byTab.get(tabId);
    if (!state) {
      state = { refCount: 0, domains: new Set() };
      this.byTab.set(tabId, state);
      await chrome.debugger.attach({ tabId }, "1.3");
    }
    state.refCount++;
    if (domain && !state.domains.has(domain)) {
      await chrome.debugger.sendCommand({ tabId }, `${domain}.enable`);
      state.domains.add(domain);
    }
  },

  async release(tabId) {
    const state = this.byTab.get(tabId);
    if (!state) return;
    state.refCount--;
    if (state.refCount <= 0) {
      this.byTab.delete(tabId);
      try { await chrome.debugger.detach({ tabId }); } catch (_) { /* already gone */ }
    }
  },
};

chrome.debugger.onDetach.addListener((source, reason) => {
  if (source && source.tabId) {
    debuggerAttach.byTab.delete(source.tabId);
  }
  // Trace state belongs to whichever tab was being recorded; just
  // mark it inactive on any detach so a fresh perf_start works.
  if (traceState.active) traceState.active = false;
});

chrome.debugger.onEvent.addListener((_source, method, params) => {
  try {
    if (method === "Network.responseReceived" && params && params.response) {
      pushBounded(netRing, {
        url: params.response.url,
        status: params.response.status,
        mime_type: params.response.mimeType,
        resource_type: params.type,
        when: new Date().toISOString(),
      }, NET_RING_MAX);
    } else if (method === "Runtime.consoleAPICalled" && params) {
      const text = (params.args || []).map((a) => {
        if (a.value !== undefined) return String(a.value);
        if (a.description) return a.description;
        return "";
      }).join(" ");
      pushBounded(consoleRing, {
        level: params.type,
        text: text,
        when: new Date().toISOString(),
      }, CONSOLE_RING_MAX);
    } else if (method === "Runtime.exceptionThrown" && params && params.exceptionDetails) {
      const det = params.exceptionDetails;
      const text = (det.exception && det.exception.description) || det.text || "";
      pushBounded(consoleRing, {
        level: "exception",
        text: text,
        when: new Date().toISOString(),
      }, CONSOLE_RING_MAX);
    } else if (method === "Tracing.dataCollected" && traceState.active && params && params.value) {
      traceState.eventCount += params.value.length;
    }
  } catch (e) {
    console.warn("ycode-live: debugger event handler failed", e);
  }
});

// --- tool-call debug log ---------------------------------------------
//
// Bounded ring buffer of recent tool calls, surfaced in the popup UI
// below the connection status and optionally mirrored to console.debug
// for power users. In-memory only; service-worker eviction wipes it.

const TOOL_LOG_MAX = 50;
const PREVIEW_MAX = 240;
let toolLog = [];
let debugMirror = false;
const logPorts = new Set();

chrome.storage.local.get(["ycodeDebugMirror"], ({ ycodeDebugMirror }) => {
  debugMirror = ycodeDebugMirror === true;
});

function previewValue(v) {
  if (v === null || v === undefined) return "";
  let s;
  try {
    s = typeof v === "string" ? v : JSON.stringify(v);
  } catch (_) {
    s = String(v);
  }
  if (s.length > PREVIEW_MAX) return s.slice(0, PREVIEW_MAX) + "…";
  return s;
}

function broadcastLog(msg) {
  for (const p of logPorts) {
    try { p.postMessage(msg); } catch (_) { /* port closed */ }
  }
}

function recordToolCall(call) {
  const entry = {
    id: call.id,
    method: call.method,
    paramsPreview: previewValue(call.params),
    resultPreview: call.error ? "" : previewValue(call.result),
    error: call.error || "",
    ok: !call.error,
    durationMs: call.durationMs,
    startedAt: call.startedAt,
  };
  toolLog.push(entry);
  if (toolLog.length > TOOL_LOG_MAX) {
    toolLog.splice(0, toolLog.length - TOOL_LOG_MAX);
  }
  if (debugMirror) {
    const tag = entry.ok ? "ok" : "err";
    console.debug(
      `ycode-live[${tag}] ${entry.method} ${entry.durationMs}ms`,
      { params: call.params, result: call.result, error: entry.error },
    );
  }
  broadcastLog({ kind: "entry", entry });
}

chrome.runtime.onConnect.addListener((port) => {
  if (port.name !== "ycode-live:log-stream") return;
  logPorts.add(port);
  try {
    port.postMessage({ kind: "snapshot", entries: toolLog, mirror: debugMirror });
  } catch (_) { /* popup already closed */ }
  port.onMessage.addListener((msg) => {
    if (!msg) return;
    if (msg.type === "clear") {
      toolLog = [];
      broadcastLog({ kind: "snapshot", entries: toolLog, mirror: debugMirror });
    } else if (msg.type === "mirror") {
      debugMirror = !!msg.value;
      chrome.storage.local.set({ ycodeDebugMirror: debugMirror });
      broadcastLog({ kind: "mirror", mirror: debugMirror });
    }
  });
  port.onDisconnect.addListener(() => logPorts.delete(port));
});

async function onMessage(ev) {
  let req;
  try {
    req = JSON.parse(ev.data);
  } catch (e) {
    return; // not our protocol
  }
  if (!req || typeof req.id !== "number") return;
  if (req.method === "_pong" || req.method === "_ping") return;

  const startedAt = Date.now();
  let result;
  let error = "";
  try {
    result = await dispatch(req.method, req.params || {});
    ws.send(JSON.stringify({ id: req.id, result }));
  } catch (err) {
    error = String(err.message || err);
    ws.send(JSON.stringify({ id: req.id, error }));
  }
  // Record EVERY dispatched method — including capabilities. Do not
  // add per-method skips here. A pathological client that hammers
  // capabilities (or any other low-cost probe) is exactly what this
  // log is meant to surface; suppressing "noisy" methods hides the
  // misbehavior that the panel exists to catch.
  recordToolCall({
    id: req.id,
    method: req.method,
    params: req.params || {},
    result,
    error,
    durationMs: Date.now() - startedAt,
    startedAt,
  });
}

// targetTabId is the ONE source of truth for "which tab is being
// driven". Every method that touches a page resolves through it —
// including screenshot, which previously captured whatever happened to
// be in the foreground and so could hand back a real PNG of a page the
// caller never asked for.
async function targetTabId() {
  return (await targetTab()).id;
}

async function targetTab() {
  if (activeTabId !== null) {
    try {
      return await chrome.tabs.get(activeTabId);
    } catch (e) {
      activeTabId = null;
    }
  }
  const tabs = await chrome.tabs.query({ active: true, currentWindow: true });
  if (tabs.length === 0) throw new Error("no active tab");
  activeTabId = tabs[0].id;
  return tabs[0];
}

async function dispatch(method, params) {
  // Capabilities does not need a tab (it's a pure metadata read).
  if (method === "capabilities") return capabilities();
  if (method === "cookies_get") return cookiesGet(params);
  if (method === "tabs") return handleTabs(params);

  const tabId = await targetTabId();
  const out = await dispatchOnTab(method, params, tabId);
  // Stamp the resolved tab on every page-touching result so a caller
  // can prove which page answered instead of inferring it.
  if (out && typeof out === "object" && out.tab_id === undefined) {
    out.tab_id = tabId;
  }
  return out;
}

async function dispatchOnTab(method, params, tabId) {
  switch (method) {
    case "navigate":
      return navigate(tabId, params.url);
    case "back":
      return chrome.tabs.goBack(tabId).then(() => extractInTab(tabId, {}));
    case "screenshot":
      return takeScreenshot(tabId, params);
    case "extract":
      return extractInTab(tabId, params);
    case "click":
      return runInTab(tabId, "click", params);
    case "type":
      return runInTab(tabId, "type", params);
    case "scroll":
      return runInTab(tabId, "scroll", params);
    case "evaluate":
      return evaluateInTab(tabId, params.script);
    case "dispatch_event":
      return dispatchEventInTab(tabId, params);
    case "wait_for_selector":
      return waitForSelector(tabId, params);
    case "keyboard_press":
      return keyboardPress(tabId, params);
    case "clipboard_read":
      return clipboardRead(tabId);
    case "clipboard_write":
      return clipboardWrite(tabId, params);
    case "storage_get":
      return storageGet(tabId, params);
    case "network_list":
      return networkList(tabId);
    case "console_get":
      return consoleGet(tabId);
    case "perf_start":
      return perfStart(tabId);
    case "perf_stop":
      return perfStop(tabId);
    case "lighthouse":
      return lighthouse(tabId);
  }
  throw new Error(
    `unknown method: ${method}; expected one of: ${SUPPORTED_METHODS.join(", ")}`
  );
}

function capabilities() {
  const m = chrome.runtime.getManifest();
  return {
    data: JSON.stringify({
      mode: "live",
      version: m.version,
      methods: SUPPORTED_METHODS,
      permissions: m.permissions || [],
    }),
  };
}

async function cookiesGet(params) {
  // Use the chrome.cookies API rather than document.cookie so HttpOnly
  // cookies are visible. Filters: name + domain (both optional).
  const tabId = await targetTabId();
  const tab = await chrome.tabs.get(tabId);
  let domain = params.domain || "";
  try {
    if (!domain && tab.url) domain = new URL(tab.url).hostname;
  } catch (_) { /* about:blank etc */ }
  const all = await chrome.cookies.getAll({});
  const want = (params.name || "").trim();
  const out = [];
  for (const c of all) {
    if (want && c.name !== want) continue;
    if (domain) {
      // chrome cookies store .example.com — match by suffix.
      const cd = (c.domain || "").replace(/^\./, "");
      if (cd !== domain && !domain.endsWith("." + cd) && !cd.endsWith("." + domain)) continue;
    }
    out.push({
      name: c.name, value: c.value, domain: c.domain, path: c.path,
      secure: c.secure, httpOnly: c.httpOnly,
      session: c.session, sameSite: c.sameSite,
      expirationDate: c.expirationDate,
    });
  }
  return { data: JSON.stringify(out) };
}

async function navigate(tabId, url) {
  if (!url) throw new Error("navigate: url required");
  await chrome.tabs.update(tabId, { url });
  await waitForLoad(tabId);
  return extractInTab(tabId, {});
}

function waitForLoad(tabId) {
  return new Promise((resolve) => {
    const listener = (updatedId, info) => {
      if (updatedId === tabId && info.status === "complete") {
        chrome.tabs.onUpdated.removeListener(listener);
        resolve();
      }
    };
    chrome.tabs.onUpdated.addListener(listener);
    setTimeout(() => {
      chrome.tabs.onUpdated.removeListener(listener);
      resolve();
    }, 20000);
  });
}

const SETTLE_MS_DEFAULT = 150;

// CDP_CAPTURE_TIMEOUT_MS bounds the debugger capture path.
//
// A background tab has no compositing surface, and Page.captureScreenshot on
// one does not fail — it NEVER RETURNS. That is worse than an error here,
// because the focus-and-restore fallback below is reached only by a thrown
// exception, so a hang bypassed the fallback entirely and blocked until the
// caller's 30s ceiling killed the whole request. Found on the first live-mode
// run against a real background tab; no unit test over the extension source
// could have seen it.
const CDP_CAPTURE_TIMEOUT_MS = 4000;

// SETTLE_TIMEOUT_MS bounds the paint barrier. Belt and braces on top of the
// visible-tab check above: a barrier that cannot complete must cost a slightly
// early frame, never the whole request.
const SETTLE_TIMEOUT_MS = 2000;

// withTimeout rejects if p has not settled within ms. The underlying promise
// is left to finish on its own — it cannot be cancelled — so callers that hold
// a resource must release it on the timeout path too.
function withTimeout(p, ms, label) {
  let timer;
  const bell = new Promise((_, reject) => {
    timer = setTimeout(() => reject(new Error(label + ": no answer within " + ms + "ms")), ms);
  });
  return Promise.race([p, bell]).finally(() => clearTimeout(timer));
}

// viewportOf reads the target page's own dimensions. chrome.scripting works on
// a hidden tab even though compositing does not, which is what makes the
// surface override below possible.
async function viewportOf(tabId) {
  try {
    const [{ result } = {}] = await chrome.scripting.executeScript({
      target: { tabId },
      func: () => ({
        width: document.documentElement.clientWidth || window.innerWidth || 0,
        height: document.documentElement.clientHeight || window.innerHeight || 0,
        dpr: window.devicePixelRatio || 1,
      }),
    });
    return result || null;
  } catch (_) {
    return null;
  }
}

// settleTab waits for the renderer to actually paint the current DOM.
// Two composited frames plus a short tail: a capture issued straight
// after a click otherwise returns the PRE-click frame while `extract`
// already reports the post-click DOM — an image and a DOM that
// disagree, both reported as success.
//
// ONLY MEANINGFUL FOR A VISIBLE TAB, and getting that wrong hung the whole
// call. requestAnimationFrame NEVER FIRES in a hidden tab — Chrome stops
// servicing frames for one — so awaiting a frame there waits forever. It is
// not merely slow, it never completes, and because the await is on the path
// BEFORE the capture it took the request down with it. Measured directly:
// script injection into a hidden tab returns in 0s, while a rAF callback in
// the same tab had not fired after 2s.
//
// A hidden tab also has nothing to settle: it is not painting, and its
// capture path renders on demand. So the barrier is skipped rather than
// bounded-and-waited.
async function settleTab(tabId, settleMs, visible) {
  const ms = settleMs === undefined || settleMs === null || settleMs < 0
    ? SETTLE_MS_DEFAULT
    : settleMs;
  if (!visible) {
    // Still yield briefly so a DOM mutation issued moments ago has landed,
    // but through a timer, which a hidden tab does service.
    if (ms > 0) await new Promise((r) => setTimeout(r, Math.min(ms, 250)));
    return;
  }
  try {
    await withTimeout(chrome.scripting.executeScript({
      target: { tabId },
      args: [ms],
      func: async (ms) => {
        const frame = () => new Promise((r) => requestAnimationFrame(() => r()));
        await frame();
        await frame();
        if (ms > 0) await new Promise((r) => setTimeout(r, ms));
        return true;
      },
    }), SETTLE_TIMEOUT_MS, "settle");
  } catch (_) {
    // A page we cannot inject into (chrome://, the web store), or a frame
    // that never arrived: capture anyway, just without the barrier. Never
    // hang on the way to a screenshot.
    if (ms > 0) await new Promise((r) => setTimeout(r, ms));
  }
}

// takeScreenshot captures THE TARGET TAB — the tab every other method
// drives — not whatever is in the foreground.
//
// chrome.tabs.captureVisibleTab captures a *window's visible tab*, so
// on any window whose foreground tab is not the driven one it returns
// a real, well-formed PNG of the wrong page. The fix is to route a
// non-foreground (or full-page) capture through CDP
// Page.captureScreenshot, which addresses the tab directly, and to
// re-check the foreground tab immediately before the cheap path so the
// two can never disagree.
async function takeScreenshot(tabId, params) {
  params = params || {};
  const tab = await chrome.tabs.get(tabId);

  // Resolve visibility FIRST: it selects both the capture path and the kind
  // of settle that is even possible. Settling before knowing was the bug.
  const fullPage = !!params.full_page;
  let visible = false;
  if (!fullPage) {
    try {
      const [front] = await chrome.tabs.query({ active: true, windowId: tab.windowId });
      visible = !!front && front.id === tabId;
    } catch (_) { visible = false; }
  }

  await settleTab(tabId, params.settle_ms, visible);

  if (visible) {
    const dataUrl = await chrome.tabs.captureVisibleTab(tab.windowId, { format: "png" });
    return screenshotResult(dataUrl, tab, "capture_visible_tab", fullPage);
  }

  // Background tab, or a full-page request: CDP addresses the tab
  // itself. Costs a debugger attach (and its banner) — which is why
  // the foreground case above stays on the cheap path.
  //
  // TIME-BOUNDED: see CDP_CAPTURE_TIMEOUT_MS. A hidden tab can leave
  // Page.captureScreenshot pending forever, and an unbounded await here would
  // never reach the fallback below.
  try {
    const dataUrl = await withTimeout(
      captureViaDebugger(tabId, fullPage), CDP_CAPTURE_TIMEOUT_MS, "cdp capture");
    return screenshotResult(dataUrl, tab, "cdp_capture_screenshot", fullPage);
  } catch (cdpErr) {
    // The raced promise may still be in flight holding the attach; drop our
    // reference so a timed-out capture cannot leak a debugger session.
    try { await debuggerAttach.release(tabId); } catch (_) { /* already gone */ }
    if (fullPage) {
      throw new Error(
        `screenshot: full_page needs CDP and the debugger attach failed: ${cdpErr.message || cdpErr}`
      );
    }
    // Last resort: bring the target tab forward, capture, put the
    // previous foreground tab back. Visible to the user, so it is the
    // fallback rather than the default — but a correct image of the
    // right page beats a silent image of the wrong one.
    const [prev] = await chrome.tabs.query({ active: true, windowId: tab.windowId });
    await chrome.tabs.update(tabId, { active: true });
    try {
      await settleTab(tabId, params.settle_ms, true);
      const dataUrl = await chrome.tabs.captureVisibleTab(tab.windowId, { format: "png" });
      return screenshotResult(dataUrl, tab, "focus_and_capture", fullPage);
    } finally {
      if (prev && prev.id !== tabId) {
        try { await chrome.tabs.update(prev.id, { active: true }); } catch (_) { /* closed */ }
      }
    }
  }
}

async function captureViaDebugger(tabId, fullPage) {
  await debuggerAttach.acquire(tabId, "Page");
  let overrode = false;
  try {
    // Force a compositing surface. A hidden tab has none, which is precisely
    // why Page.captureScreenshot hangs on one; explicit device metrics give
    // the renderer something to draw into so the capture can complete WITHOUT
    // stealing focus from whatever the operator is looking at.
    const vp = await viewportOf(tabId);
    if (vp && vp.width > 0 && vp.height > 0) {
      await chrome.debugger.sendCommand({ tabId }, "Emulation.setDeviceMetricsOverride", {
        width: vp.width, height: vp.height,
        deviceScaleFactor: vp.dpr || 1, mobile: false,
      });
      overrode = true;
    }
    const opts = { format: "png" };
    if (fullPage) opts.captureBeyondViewport = true;
    const res = await chrome.debugger.sendCommand({ tabId }, "Page.captureScreenshot", opts);
    if (!res || !res.data) throw new Error("Page.captureScreenshot returned no data");
    return res.data; // already bare base64, no data: prefix
  } finally {
    if (overrode) {
      // Leaving an override behind would resize the operator's page.
      try {
        await chrome.debugger.sendCommand({ tabId }, "Emulation.clearDeviceMetricsOverride");
      } catch (_) { /* tab closed, or already detached */ }
    }
    await debuggerAttach.release(tabId);
  }
}

// screenshotResult normalises the two capture paths onto one envelope.
// Caller (Go side) is responsible for MaxBytes / SavePath
// post-processing; the extension always emits a raw base64 PNG so the
// re-encode/write path lives in one place.
function screenshotResult(data, tab, method, fullPage) {
  const idx = data.indexOf(",");
  const image = data.startsWith("data:") && idx >= 0 ? data.slice(idx + 1) : data;
  return {
    image,
    tab_id: tab.id,
    url: tab.url || "",
    title: tab.title || "",
    data: JSON.stringify({ capture: method, full_page: !!fullPage, tab_id: tab.id }),
  };
}

async function extractInTab(tabId, params) {
  const result = await injectOne({
    target: { tabId },
    args: [params || {}],
    func: (params) => {
      const SCOPE_SEL = (params.scope || "").trim();
      const MATCH = (params.match_text || params.goal || "").trim();
      const LIMIT = params.limit && params.limit > 0 ? params.limit : 50;
      const OFFSET = params.offset && params.offset > 0 ? params.offset : 0;
      const root = SCOPE_SEL ? document.querySelector(SCOPE_SEL) : document;
      if (!root) {
        return { title: document.title, url: location.href, content: "", elements: "", error: "extract: scope " + SCOPE_SEL + " not found" };
      }
      const navFilter = !SCOPE_SEL;
      const INCLUDE_HIDDEN = !!params.include_hidden;
      function boxOf(el) {
        const r = el.getBoundingClientRect();
        return { x: Math.round(r.x), y: Math.round(r.y), w: Math.round(r.width), h: Math.round(r.height) };
      }
      function shown(el) {
        const cs = getComputedStyle(el);
        if (cs.visibility === "hidden" || cs.display === "none" || cs.opacity === "0") return false;
        const r = el.getBoundingClientRect();
        return r.width > 0 && r.height > 0;
      }
      const all = root.querySelectorAll(
        "a, button, input, select, textarea, [role='button'], [role='link']"
      );
      const matches = [];
      const want = MATCH.toLowerCase();
      for (let i = 0; i < all.length; i++) {
        const el = all[i];
        if (navFilter && el.closest && el.closest("nav, aside, [role='navigation'], [role='complementary']")) continue;
        const visible = ((el.innerText) || el.value || el.getAttribute("aria-label") || "").trim();
        if (want) {
          const ph = el.getAttribute("placeholder") || "";
          const ar = el.getAttribute("aria-label") || "";
          if (visible.toLowerCase().indexOf(want) < 0 &&
              ph.toLowerCase().indexOf(want) < 0 &&
              ar.toLowerCase().indexOf(want) < 0) continue;
        }
        if (!INCLUDE_HIDDEN && !shown(el)) continue;
        matches.push(el);
      }
      const total = matches.length;
      const slice = matches.slice(OFFSET, OFFSET + LIMIT);
      const lines = [];
      const records = [];
      for (let j = 0; j < slice.length; j++) {
        const el = slice[j];
        const tag = el.tagName.toLowerCase();
        const text = ((el.innerText) || el.value || el.getAttribute("aria-label") || "").trim().slice(0, 80);
        const attrs = {};
        const attrPairs = [];
        for (const a of ["type", "placeholder", "href", "name", "value", "role", "aria-label"]) {
          const v = el.getAttribute(a);
          if (v) {
            attrs[a] = String(v).slice(0, 60);
            attrPairs.push(`${a}="${String(v).slice(0, 60)}"`);
          }
        }
        const vis = shown(el);
        // The index is the CLICK ADDRESS: `click --index N` resolves
        // through the identical enumeration, so what is listed here is
        // what gets clicked.
        lines.push(`[${OFFSET + j + 1}]${vis ? "" : " (hidden)"} <${tag} ${attrPairs.join(" ")}>${text}</${tag}>`);
        records.push({
          index: OFFSET + j + 1,
          tag: tag,
          text: text,
          attrs: attrs,
          visible: vis,
          box: boxOf(el),
        });
      }
      const body = (root === document ? (document.body && document.body.innerText) : root.innerText) || "";
      const viewport = {
        width: Math.round(document.documentElement.clientWidth || window.innerWidth || 0),
        height: Math.round(document.documentElement.clientHeight || window.innerHeight || 0),
        dpr: window.devicePixelRatio || 1,
      };
      return {
        title: document.title,
        url: location.href,
        content: body.length > 16000 ? body.slice(0, 16000) + "\n... (truncated)" : body,
        elements: lines.join("\n"),
        total: total,
        truncated: total > (OFFSET + LIMIT),
        viewport: viewport,
        // Structured mirror of `elements`, so a --json caller never
        // has to re-parse a pretty-printer's output to recover data
        // the tool already had.
        data: JSON.stringify({ viewport: viewport, elements: records }),
      };
    },
  }, "extract");
  return result;
}

// injectOne runs one scripting.executeScript and returns the single
// frame's result, converting "no frame" and "the injected function
// threw" into errors.
//
// The old code destructured `[{ result } = {}]` from await and then
// returned `result` with an empty-object fallback. When injection
// failed — the commonest cause
// being an invalid CSS selector, which makes querySelector throw —
// `result` was undefined and the caller got a bare `{}`, which the
// hub reported as `{"success":true}`. A click that resolved nothing
// looked exactly like a click that worked.
async function injectOne(opts, what) {
  let frames;
  try {
    frames = await chrome.scripting.executeScript(opts);
  } catch (e) {
    throw new Error(`${what}: injection failed: ${String((e && e.message) || e)}`);
  }
  if (!frames || frames.length === 0) {
    throw new Error(`${what}: no frame answered (page may have navigated away)`);
  }
  const frame = frames[0];
  if (frame && frame.error) {
    const msg = frame.error.message || String(frame.error);
    throw new Error(`${what}: ${msg}`);
  }
  if (frame.result === undefined || frame.result === null) {
    throw new Error(`${what}: script returned no result (it threw, or the frame was replaced)`);
  }
  return frame.result;
}

async function runInTab(tabId, kind, params) {
  const result = await injectOne({
    target: { tabId },
    args: [kind, params],
    func: (kind, params) => {
      const matchText = params.match_text || "";
      function query(root, sel) {
        try {
          return root.querySelector(sel);
        } catch (e) {
          return { __badSelector: sel, __reason: String((e && e.message) || e) };
        }
      }
      function elemsInScope() {
        const scoped = params.scope ? query(document, params.scope) : document;
        const scope = scoped && !scoped.__badSelector ? scoped : document;
        return scope.querySelectorAll(
          "a, button, input, select, textarea, [role='button'], [role='link']"
        );
      }
      function shown(el) {
        const cs = getComputedStyle(el);
        if (cs.visibility === "hidden" || cs.display === "none" || cs.opacity === "0") return false;
        const r = el.getBoundingClientRect();
        return r.width > 0 && r.height > 0;
      }
      function flatCandidates() {
        // Mirror the extract enumeration EXACTLY — same landmark skip,
        // same visibility filter. `extract` numbers this list, so [N]
        // there is the element clicked by element_id N here. If the
        // two predicates ever diverge, click-by-index silently hits a
        // neighbour, which is the failure this whole path exists to
        // remove.
        const navFilter = !params.scope;
        const includeHidden = !!params.include_hidden;
        const flat = [];
        for (const el of elemsInScope()) {
          if (navFilter && el.closest && el.closest("nav, aside, [role='navigation'], [role='complementary']")) continue;
          if (!includeHidden && !shown(el)) continue;
          flat.push(el);
        }
        return flat;
      }
      function resolveTarget() {
        if (params.element_id && params.element_id > 0) {
          const flat = flatCandidates();
          const el = flat[params.element_id - 1];
          if (!el) {
            return { __miss: `element index ${params.element_id} out of range (extract listed ${flat.length})` };
          }
          return el;
        }
        if (params.selector) {
          const scoped = params.scope ? query(document, params.scope) : document;
          if (scoped && scoped.__badSelector) {
            return { __miss: `scope ${JSON.stringify(params.scope)} is not a valid CSS selector: ${scoped.__reason}` };
          }
          const el = query(scoped || document, params.selector);
          if (el && el.__badSelector) {
            const numeric = /^[0-9]+$/.test(params.selector);
            return {
              __miss: numeric
                ? `${JSON.stringify(params.selector)} is not a CSS selector; pass an element index as element_id (bashy browser click --index ${params.selector})`
                : `${JSON.stringify(params.selector)} is not a valid CSS selector: ${el.__reason}`,
            };
          }
          if (!el) return { __miss: `no element matches selector ${JSON.stringify(params.selector)}` };
          return el;
        }
        if (matchText) {
          const want = matchText.toLowerCase();
          for (const el of elemsInScope()) {
            const v = ((el.innerText) || el.value || el.getAttribute("aria-label") || "").trim().toLowerCase();
            if (v.indexOf(want) >= 0) return el;
          }
          return { __miss: `no element whose text contains ${JSON.stringify(matchText)}` };
        }
        return { __miss: "no selector, element index, or match text given" };
      }
      function describe(el) {
        const tag = el.tagName ? el.tagName.toLowerCase() : "?";
        const label = ((el.innerText) || el.value || el.getAttribute("aria-label") || "").trim().slice(0, 60);
        return label ? `<${tag}> ${label}` : `<${tag}>`;
      }
      switch (kind) {
        case "click": {
          const el = resolveTarget();
          if (el && el.__miss) return { error: `click: ${el.__miss}` };
          el.click();
          return { content: "clicked", data: JSON.stringify({ matched: describe(el) }) };
        }
        case "type": {
          const el = resolveTarget();
          if (el && el.__miss) return { error: `type: ${el.__miss}` };
          el.focus();
          if ("value" in el) {
            el.value = params.text || "";
          } else {
            el.textContent = params.text || "";
          }
          el.dispatchEvent(new Event("input", { bubbles: true }));
          el.dispatchEvent(new Event("change", { bubbles: true }));
          return { content: "typed", data: JSON.stringify({ matched: describe(el) }) };
        }
        case "scroll": {
          let amount = params.amount || 500;
          if (params.direction === "up") amount = -amount;
          window.scrollBy(0, amount);
          return { content: `scrolled to ${window.scrollY}`, data: String(window.scrollY) };
        }
      }
      return { error: `runInTab: unknown kind ${kind}` };
    },
  }, kind);
  if (result && result.error) {
    throw new Error(result.error);
  }
  return result;
}

// dispatchEventInTab fires a named DOM event from the extension's
// ISOLATED world. A page whose CSP omits 'unsafe-eval' disables
// `evaluate` entirely, and the usual workaround — "click whatever
// element happens to share the handler" — is not always available.
// Nothing here is compiled from a string, so no CSP directive applies.
async function dispatchEventInTab(tabId, params) {
  const name = (params && params.event || "").trim();
  if (!name) throw new Error("dispatch_event: event name required");
  const result = await injectOne({
    target: { tabId },
    args: [name, params.detail || "", params.selector || ""],
    world: "MAIN",
    func: (name, detailRaw, selector) => {
      let detail = null;
      if (detailRaw) {
        try { detail = JSON.parse(detailRaw); } catch (_) { detail = detailRaw; }
      }
      let target = window;
      let where = "window";
      if (selector === "document") {
        target = document; where = "document";
      } else if (selector) {
        try {
          target = document.querySelector(selector);
        } catch (e) {
          return { error: `dispatch_event: ${JSON.stringify(selector)} is not a valid CSS selector` };
        }
        if (!target) return { error: `dispatch_event: no element matches ${JSON.stringify(selector)}` };
        where = selector;
      }
      const ev = detail === null
        ? new Event(name, { bubbles: true, cancelable: true })
        : new CustomEvent(name, { bubbles: true, cancelable: true, detail });
      const notCancelled = target.dispatchEvent(ev);
      return {
        content: `dispatched ${name} on ${where}`,
        data: JSON.stringify({ event: name, target: where, default_prevented: !notCancelled }),
      };
    },
  }, "dispatch_event");
  if (result && result.error) throw new Error(result.error);
  return result;
}

async function evaluateInTab(tabId, script) {
  if (!script) throw new Error("evaluate: script required");
  // Run in the MAIN world so the script sees the page's globals
  // (matches chromedp.Evaluate semantics on probe/solo).
  const [{ result } = {}] = await chrome.scripting.executeScript({
    target: { tabId },
    args: [script],
    world: "MAIN",
    func: (src) => {
      const stringify = (v) => {
        if (v === undefined) return "";
        if (typeof v === "string") return v;
        try { return JSON.stringify(v); } catch (_) { return String(v); }
      };
      let value;
      try {
        value = (new Function("return (" + src + ")"))();
      } catch (e1) {
        try {
          value = (new Function(src))();
        } catch (e2) {
          return { error: String(e1 && e1.message || e1) };
        }
      }
      if (value && typeof value.then === "function") {
        return value.then(
          (r) => ({ value: stringify(r) }),
          (e) => ({ error: String(e && e.message || e) })
        );
      }
      return { value: stringify(value) };
    },
  });
  if (result && result.error) throw new Error(result.error);
  return { data: result ? result.value : "" };
}

async function waitForSelector(tabId, params) {
  const sel = params.selector;
  if (!sel) throw new Error("wait_for_selector: selector required");
  const timeoutMs = params.timeout_ms && params.timeout_ms > 0 ? params.timeout_ms : 5000;
  const state = params.state || "visible";
  // Poll inside the page in 100 ms ticks. Cheaper than IPC because
  // chrome.scripting.executeScript per tick would burn quota.
  const [{ result } = {}] = await chrome.scripting.executeScript({
    target: { tabId },
    args: [sel, timeoutMs, state],
    func: async (sel, timeoutMs, state) => {
      const deadline = Date.now() + timeoutMs;
      function visible(el) {
        if (!el) return false;
        const cs = getComputedStyle(el);
        if (cs.visibility === "hidden" || cs.display === "none") return false;
        const r = el.getBoundingClientRect();
        return r.width > 0 && r.height > 0;
      }
      while (Date.now() < deadline) {
        const el = document.querySelector(sel);
        if (state === "detached") {
          if (!el) return { ok: true };
        } else if (state === "attached") {
          if (el) return { ok: true };
        } else {
          if (visible(el)) return { ok: true };
        }
        await new Promise((r) => setTimeout(r, 100));
      }
      return { ok: false };
    },
  });
  if (!result || !result.ok) {
    throw new Error(`wait_for_selector: timeout after ${timeoutMs}ms (state=${state})`);
  }
  return { data: `state=${state}` };
}

async function keyboardPress(tabId, params) {
  if (!params.key) throw new Error("keyboard_press: key required");
  // Prefer chrome.debugger + Input.dispatchKeyEvent for trusted
  // keystrokes (manifest 0.4.0). Falls back to the synthetic
  // KeyboardEvent path if the debugger attach fails — for example
  // if DevTools is open on the same tab.
  if (params.selector) {
    try {
      await chrome.scripting.executeScript({
        target: { tabId },
        args: [params.selector],
        func: (sel) => {
          const el = document.querySelector(sel);
          if (el && el.focus) el.focus();
        },
      });
    } catch (_) { /* ignore focus failures; keystrokes still go to body */ }
  }
  try {
    await dispatchTrustedKey(tabId, params.key, params.modifiers || []);
    return { data: "pressed=" + params.key + " (trusted)" };
  } catch (e) {
    // Fall back to synthetic events.
    console.warn("ycode-live: trusted key dispatch failed, falling back", e);
  }
  const [{ result } = {}] = await chrome.scripting.executeScript({
    target: { tabId },
    args: [params.key, params.modifiers || []],
    func: (key, modifiers) => {
      const mods = new Set(modifiers.map((m) => String(m).toLowerCase()));
      const opts = {
        key: key,
        code: key.length === 1 ? "Key" + key.toUpperCase() : key,
        bubbles: true,
        cancelable: true,
        shiftKey: mods.has("shift"),
        ctrlKey: mods.has("control") || mods.has("ctrl"),
        altKey: mods.has("alt"),
        metaKey: mods.has("meta") || mods.has("cmd") || mods.has("command"),
      };
      const target = document.activeElement || document.body;
      target.dispatchEvent(new KeyboardEvent("keydown", opts));
      target.dispatchEvent(new KeyboardEvent("keypress", opts));
      if (key === "Enter" && target.form) {
        try { target.form.requestSubmit ? target.form.requestSubmit() : target.form.submit(); } catch (_) { /* ignore */ }
      }
      target.dispatchEvent(new KeyboardEvent("keyup", opts));
      return { ok: true };
    },
  });
  if (result && result.error) throw new Error(result.error);
  return { data: "pressed=" + params.key + " (synthetic)" };
}

// dispatchTrustedKey attaches chrome.debugger to the target tab,
// dispatches a real Input.dispatchKeyEvent pair (keyDown + keyUp),
// then detaches. Modifiers map to the CDP bitfield. Throws on any
// failure — the caller falls back to a synthetic KeyboardEvent.
const KEY_TO_CODE = {
  Enter: { code: "Enter", windowsVirtualKeyCode: 13 },
  Tab: { code: "Tab", windowsVirtualKeyCode: 9 },
  Escape: { code: "Escape", windowsVirtualKeyCode: 27 },
  Backspace: { code: "Backspace", windowsVirtualKeyCode: 8 },
  Delete: { code: "Delete", windowsVirtualKeyCode: 46 },
  ArrowUp: { code: "ArrowUp", windowsVirtualKeyCode: 38 },
  ArrowDown: { code: "ArrowDown", windowsVirtualKeyCode: 40 },
  ArrowLeft: { code: "ArrowLeft", windowsVirtualKeyCode: 37 },
  ArrowRight: { code: "ArrowRight", windowsVirtualKeyCode: 39 },
  Home: { code: "Home", windowsVirtualKeyCode: 36 },
  End: { code: "End", windowsVirtualKeyCode: 35 },
  PageUp: { code: "PageUp", windowsVirtualKeyCode: 33 },
  PageDown: { code: "PageDown", windowsVirtualKeyCode: 34 },
};

async function dispatchTrustedKey(tabId, key, modifiers) {
  const target = { tabId: tabId };
  const mods = new Set(modifiers.map((m) => String(m).toLowerCase()));
  // CDP modifier bitfield: 1=Alt, 2=Ctrl, 4=Meta, 8=Shift.
  let modBits = 0;
  if (mods.has("alt")) modBits |= 1;
  if (mods.has("control") || mods.has("ctrl")) modBits |= 2;
  if (mods.has("meta") || mods.has("cmd") || mods.has("command")) modBits |= 4;
  if (mods.has("shift")) modBits |= 8;

  // Input.* commands don't need a domain enable; pass null so the
  // manager only handles attach refcount.
  await debuggerAttach.acquire(tabId, null);
  try {
    const named = KEY_TO_CODE[key];
    const isPrintable = key.length === 1;
    const base = named
      ? { key: key, code: named.code, windowsVirtualKeyCode: named.windowsVirtualKeyCode, modifiers: modBits }
      : { key: key, code: isPrintable ? "Key" + key.toUpperCase() : key, modifiers: modBits };
    const keyDown = Object.assign({ type: isPrintable ? "keyDown" : "rawKeyDown" }, base);
    if (isPrintable) keyDown.text = key;
    await chrome.debugger.sendCommand(target, "Input.dispatchKeyEvent", keyDown);
    await chrome.debugger.sendCommand(target, "Input.dispatchKeyEvent", Object.assign({ type: "keyUp" }, base));
  } finally {
    await debuggerAttach.release(tabId);
  }
}

async function clipboardRead(tabId) {
  // navigator.clipboard requires a focused, secure-context page. We
  // run in the page's MAIN world; the extension's clipboardRead
  // permission grants the underlying access. Many sites strip the
  // permission with a page-level CSP, so callers should expect this
  // to fail on locked-down pages.
  const [{ result } = {}] = await chrome.scripting.executeScript({
    target: { tabId },
    world: "MAIN",
    func: async () => {
      try {
        const v = await navigator.clipboard.readText();
        return { value: v };
      } catch (e) {
        return { error: String(e && e.message || e) };
      }
    },
  });
  if (result && result.error) throw new Error("clipboard_read: " + result.error);
  return { data: (result && result.value) || "" };
}

async function clipboardWrite(tabId, params) {
  const text = params.text || "";
  const [{ result } = {}] = await chrome.scripting.executeScript({
    target: { tabId },
    args: [text],
    world: "MAIN",
    func: async (text) => {
      try {
        await navigator.clipboard.writeText(text);
        return { ok: true };
      } catch (e) {
        return { error: String(e && e.message || e) };
      }
    },
  });
  if (result && result.error) throw new Error("clipboard_write: " + result.error);
  return { content: "wrote " + text.length + " chars" };
}

async function storageGet(tabId, params) {
  const kind = (params.storage || "local").toLowerCase();
  if (kind !== "local" && kind !== "session") {
    throw new Error(`storage_get: unknown storage "${params.storage}" (local|session)`);
  }
  const [{ result } = {}] = await chrome.scripting.executeScript({
    target: { tabId },
    args: [kind, params.key || ""],
    world: "MAIN",
    func: (kind, key) => {
      const s = kind === "session" ? sessionStorage : localStorage;
      if (key) return { value: JSON.stringify({ key: key, value: s.getItem(key) }) };
      const out = {};
      for (let i = 0; i < s.length; i++) {
        const k = s.key(i);
        out[k] = s.getItem(k);
      }
      return { value: JSON.stringify(out) };
    },
  });
  return { data: (result && result.value) || "{}" };
}

const TAB_ACTIONS = ["list", "switch", "new", "close"];

function tabRecord(t, i, driven) {
  return {
    index: i + 1,
    id: t.id,
    title: t.title || "",
    url: t.url || "",
    active: !!t.active,
    driven: t.id === driven,
    window_id: t.windowId,
  };
}

// selectTab resolves a tab from whichever address the caller gave.
// Index is kept for compatibility, but --id / --url / --title are the
// stable forms: an index captured at the start of a script is stale
// the moment any tab opens or closes, which is a race no caller can
// win. An ambiguous substring is an error listing the candidates,
// never a silent pick of the first.
function selectTab(tabs, params, driven) {
  const wantURL = (params.match_url || "").toLowerCase();
  const wantTitle = (params.match_title || "").toLowerCase();
  if (wantURL || wantTitle) {
    const hits = tabs.filter((t) => {
      if (wantURL && (t.url || "").toLowerCase().indexOf(wantURL) < 0) return false;
      if (wantTitle && (t.title || "").toLowerCase().indexOf(wantTitle) < 0) return false;
      return true;
    });
    const what = wantURL
      ? `url containing ${JSON.stringify(params.match_url)}`
      : `title containing ${JSON.stringify(params.match_title)}`;
    if (hits.length === 0) throw new Error(`tabs: no tab with ${what}`);
    if (hits.length > 1) {
      const lines = hits.map((t, i) => `  [${i + 1}] ${t.title || ""} — ${t.url || ""}`);
      throw new Error(`tabs: ${hits.length} tabs match ${what}; be more specific:\n${lines.join("\n")}`);
    }
    return hits[0];
  }
  if (params.by_id && params.tab_id) {
    const t = tabs.find((x) => x.id === params.tab_id);
    if (!t) throw new Error(`tabs: no tab with id ${params.tab_id}`);
    return t;
  }
  const idx = (params.tab_id || 1) - 1;
  if (idx < 0 || idx >= tabs.length) {
    throw new Error(`tabs: index ${idx + 1} out of range (${tabs.length} tabs open)`);
  }
  return tabs[idx];
}

async function handleTabs(params) {
  const action = params.action || "list";
  if (!TAB_ACTIONS.includes(action)) {
    throw new Error(
      `unknown tab action ${JSON.stringify(action)}; expected one of: ${TAB_ACTIONS.join(", ")}`
    );
  }
  const driven = activeTabId;
  if (action === "list") {
    let tabs = await chrome.tabs.query({});
    const wantURL = (params.match_url || "").toLowerCase();
    const wantTitle = (params.match_title || "").toLowerCase();
    if (wantURL) tabs = tabs.filter((t) => (t.url || "").toLowerCase().indexOf(wantURL) >= 0);
    if (wantTitle) tabs = tabs.filter((t) => (t.title || "").toLowerCase().indexOf(wantTitle) >= 0);
    const records = tabs.map((t, i) => tabRecord(t, i, driven));
    const lines = records.map(
      (r) => `[${r.index}]${r.driven ? " *" : ""} ${r.title}\n    ${r.url}`
    );
    return {
      content: lines.join("\n"),
      data: JSON.stringify(records),
      total: records.length,
      tab_id: driven || 0,
    };
  }
  if (action === "switch") {
    const tabs = await chrome.tabs.query({});
    const t = selectTab(tabs, params, driven);
    activeTabId = t.id;
    await chrome.tabs.update(t.id, { active: true });
    return {
      content: `switched to ${t.title || t.url || t.id}`,
      url: t.url || "",
      title: t.title || "",
      tab_id: t.id,
      data: JSON.stringify(tabRecord(t, tabs.indexOf(t), t.id)),
    };
  }
  if (action === "new") {
    const t = await chrome.tabs.create({ url: params.url || "about:blank" });
    activeTabId = t.id;
    return {
      content: `opened tab ${t.id}`,
      url: t.url || "",
      tab_id: t.id,
      data: JSON.stringify(tabRecord(t, 0, t.id)),
    };
  }
  // close
  const tabs = await chrome.tabs.query({});
  const target = (params.tab_id || params.match_url || params.match_title)
    ? selectTab(tabs, params, driven)
    : { id: await targetTabId() };
  await chrome.tabs.remove(target.id);
  if (activeTabId === target.id) activeTabId = null;
  return { content: `closed tab ${target.id}`, tab_id: target.id };
}

// --- DevTools-flavored actions (network_list / console_get / perf_* /
//     lighthouse). Match probe response shapes so the agent sees
//     identical JSON across modes. ----------------------------------

async function networkList(tabId) {
  // Sticky acquire — Network keeps recording after the read so a
  // follow-up call sees more entries. Caller pays no extra detach cost
  // on each read.
  await debuggerAttach.acquire(tabId, "Network");
  return { data: JSON.stringify({ count: netRing.length, entries: netRing }) };
}

async function consoleGet(tabId) {
  await debuggerAttach.acquire(tabId, "Runtime");
  return { data: JSON.stringify({ count: consoleRing.length, entries: consoleRing }) };
}

async function perfStart(tabId) {
  if (traceState.active) {
    throw new Error("perf_start: trace already active — call perf_stop first");
  }
  // Tracing.start enables the domain itself; no <Domain>.enable needed.
  await debuggerAttach.acquire(tabId, null);
  try {
    await chrome.debugger.sendCommand({ tabId }, "Tracing.start", {
      transferMode: "ReportEvents",
    });
  } catch (e) {
    await debuggerAttach.release(tabId);
    throw e;
  }
  traceState.active = true;
  traceState.startedAt = Date.now();
  traceState.eventCount = 0;
  return { data: "tracing started" };
}

async function perfStop(tabId) {
  if (!traceState.active) {
    throw new Error("perf_stop: no active trace");
  }
  const startedAt = traceState.startedAt;
  try {
    await chrome.debugger.sendCommand({ tabId }, "Tracing.end");
  } catch (e) {
    // Best-effort cleanup; surface the error.
    traceState.active = false;
    await debuggerAttach.release(tabId);
    throw e;
  }
  // Drain the final dataCollected events; CDP buffers them after End().
  // 250ms matches probe.doPerfStop.
  await new Promise((r) => setTimeout(r, 250));
  const count = traceState.eventCount;
  traceState.active = false;
  await debuggerAttach.release(tabId);
  return {
    data: JSON.stringify({
      duration_ms: Date.now() - startedAt,
      event_count: count,
      note: "raw trace events are dropped after counting — perf_stop returns aggregate only",
    }),
  };
}

async function lighthouse(tabId) {
  // Mirrors probe's lighthouseScript verbatim so agents get identical
  // JSON across modes. Not full Lighthouse — Core Web Vitals + Navigation
  // Timing only. No chrome.debugger needed; runs as a MAIN-world script.
  const [{ result } = {}] = await chrome.scripting.executeScript({
    target: { tabId },
    world: "MAIN",
    func: () => {
      const out = {
        mode: "core-web-vitals",
        paint: {},
        navigation: {},
        largest_contentful_paint_ms: null,
        cumulative_layout_shift: null,
        first_input_delay_ms: null,
        resource_count: 0,
        notes: [],
      };
      try {
        for (const p of performance.getEntriesByType("paint")) {
          out.paint[p.name.replace(/-/g, "_")] = Math.round(p.startTime);
        }
        const nav = performance.getEntriesByType("navigation")[0];
        if (nav) {
          out.navigation = {
            ttfb_ms: Math.round(nav.responseStart - nav.requestStart),
            dom_content_loaded_ms: Math.round(nav.domContentLoadedEventEnd),
            load_event_ms: Math.round(nav.loadEventEnd),
            transfer_size: nav.transferSize,
            encoded_body_size: nav.encodedBodySize,
            type: nav.type,
          };
        }
        const lcps = performance.getEntriesByType("largest-contentful-paint");
        if (lcps && lcps.length) {
          out.largest_contentful_paint_ms = Math.round(lcps[lcps.length - 1].startTime);
        }
        let cls = 0;
        for (const ls of performance.getEntriesByType("layout-shift")) {
          if (!ls.hadRecentInput) cls += ls.value;
        }
        out.cumulative_layout_shift = Number(cls.toFixed(4));
        const fids = performance.getEntriesByType("first-input");
        if (fids && fids.length) {
          out.first_input_delay_ms = Math.round(fids[0].processingStart - fids[0].startTime);
        }
        out.resource_count = performance.getEntriesByType("resource").length;
        if (out.largest_contentful_paint_ms === null) {
          out.notes.push("LCP not yet observed — observer needs to have run since page load. Navigate and wait a beat.");
        }
        if (out.first_input_delay_ms === null) {
          out.notes.push("FID not observed — fires only on the first user interaction.");
        }
      } catch (e) {
        out.error = String(e);
      }
      return JSON.stringify(out);
    },
  });
  return { data: result || "{}" };
}
