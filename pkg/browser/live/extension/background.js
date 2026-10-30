// ycode-live extension — service worker
//
// Maintains a WebSocket to the ycode Go-side hub (default
// 127.0.0.1:58082/ws). Each incoming request is dispatched to a
// chrome.* API or a content-script injection; the result is shipped
// back over the same socket. A chrome.alarms ping every 25 seconds
// keeps the MV3 service worker from being killed mid-session.

importScripts("frames.js"); // frameOrder, locateIndex, totalCount, mergeExtract

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
    rememberIntent();
    openWS();
    sendResponse({ status: "connecting", port: connectedPort });
  } else if (msg && msg.type === "ycode-live:disconnect") {
    // Flip intent off *before* closing so the socket's onclose handler
    // doesn't schedule a fresh reconnect.
    wantConnected = false;
    rememberIntent();
    chrome.alarms.clear("ycode-live-reconnect");
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

// --- remembered intent: zero-touch reconnect ---
//
// The first Connect is remembered (chrome.storage.local) until the user
// presses Disconnect. A Chrome restart, a service-worker restart, a hub
// restart, or a hub-requested reload then reconnects by itself: the
// worker re-reads the intent at startup, and a periodic alarm (which,
// unlike setTimeout, wakes a suspended MV3 worker) retries while the
// hub is down. Loopback only; nothing new is reachable.

function rememberIntent() {
  try {
    chrome.storage.local.set({ liveIntent: { want: wantConnected, port: connectedPort, tabId: activeTabId } });
  } catch (_) { /* storage unavailable: intent lasts for this worker only */ }
}

async function restoreIntent() {
  try {
    const { liveIntent } = await chrome.storage.local.get("liveIntent");
    if (!liveIntent || !liveIntent.want || wantConnected) return;
    wantConnected = true;
    connectedPort = liveIntent.port || DEFAULT_PORT;
    if (liveIntent.tabId) activeTabId = liveIntent.tabId; // stale ids fall back in targetTab()
    reconnectMs = RECONNECT_BASE_MS;
    openWS();
  } catch (_) { /* nothing remembered */ }
}

chrome.runtime.onStartup.addListener(restoreIntent);
chrome.runtime.onInstalled.addListener(restoreIntent);
restoreIntent();

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
    chrome.alarms.clear("ycode-live-reconnect");
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
  // Backstop that survives worker suspension (min period ~30s).
  chrome.alarms.create("ycode-live-reconnect", { periodInMinutes: 0.5 });
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
  if (alarm.name === "ycode-live-reconnect") {
    if (!wantConnected) restoreIntent();
    else openWS();
    return;
  }
  if (alarm.name === "ycode-live-keepalive" && ws && ws.readyState === WebSocket.OPEN) {
    // No-op send keeps the MV3 worker alive without server traffic.
    try { ws.send(JSON.stringify({ id: 0, method: "_ping" })); } catch (e) { /* ignore */ }
  }
});

// --- request dispatch ---

const SUPPORTED_METHODS = [
  "navigate", "back", "screenshot", "extract",
  "click", "type", "select", "scroll", "tabs", "evaluate",
  "wait_for_selector", "keyboard_press",
  "clipboard_read", "clipboard_write",
  "cookies_get", "storage_get", "capabilities",
  "dispatch_event",
  // The sensitive-input guard (guard.js): the hub's gate preflights every
  // type with guard_field and refuses type/select/keypress when this
  // extension does not advertise it; guard_focus hands a refused field to
  // the human operator.
  "guard_field", "guard_focus",
  // guard_fill types an operator's answer the guard admitted for one field
  // (the hub refuses it without that admission); it never echoes the value.
  "guard_fill",
  // The hub asks a stale extension to reload itself after refreshing
  // its unpacked folder (zero-touch upgrade).
  "reload",
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

// logParams keeps typed text out of the tool log (popup panel and debug
// mirror): a type or clipboard write is logged by length only, so a value
// the guard refused page-side — or any value — is never retained here.
function logParams(method, params) {
  if ((method === "type" || method === "clipboard_write" || method === "guard_fill") && params && typeof params.text === "string") {
    return Object.assign({}, params, { text: `<${params.text.length} chars>` });
  }
  return params;
}

function recordToolCall(call) {
  call.params = logParams(call.method, call.params);
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
  rememberIntent(); // a restarted worker drives the same tab
  return tabs[0];
}

// guardTabId resolves the tab a guard follow-up (guard_fill, guard_focus)
// names: the tab whose field the guard checked before it asked the
// operator. The worker may have been stopped or reloaded while the
// operator answered, losing the driven tab; the answer must still land in
// THAT tab, and never in whatever tab is active now — when it is gone, the
// call fails.
async function guardTabId(params) {
  const id = params && params.tab_id;
  if (!Number.isInteger(id) || id <= 0) return targetTabId();
  try {
    await chrome.tabs.get(id);
  } catch (_) {
    throw new Error("guard: the tab the field was checked in is gone (cause: tab-gone)");
  }
  activeTabId = id;
  rememberIntent();
  return id;
}

async function dispatch(method, params) {
  // Capabilities does not need a tab (it's a pure metadata read).
  if (method === "capabilities") return capabilities();
  if (method === "reload") {
    // Answer first, then reload; restoreIntent reconnects the new code.
    setTimeout(() => chrome.runtime.reload(), 200);
    return { content: "reloading", data: JSON.stringify({ from: chrome.runtime.getManifest().version }) };
  }
  if (method === "cookies_get") return cookiesGet(params);
  if (method === "tabs") return handleTabs(params);

  const addressed = !!(params && (params.match_url || params.match_title || params.by_id));
  let tabId;
  if (addressed) {
    // An addressed call acts on the one tab it names or fails; it never
    // falls back to the active tab.
    const tabs = await chrome.tabs.query({});
    const t = selectTab(tabs, params, null);
    const pinned = params.tab_id;
    if (!params.by_id && Number.isInteger(pinned) && pinned > 0 && pinned !== t.id) {
      throw new Error(`tabs: target changed — the address now resolves to tab ${t.id}, not tab ${pinned} (cause: tab-mismatch)`);
    }
    tabId = t.id;
    activeTabId = tabId;
    rememberIntent();
  } else if (method === "navigate") {
    // Unaddressed navigate opens a new tab instead of hijacking the active one.
    const t = await chrome.tabs.create({ url: "about:blank" });
    activeTabId = t.id;
    rememberIntent();
    tabId = t.id;
  } else {
    tabId = (method === "guard_fill" || method === "guard_focus") ? await guardTabId(params) : await targetTabId();
  }
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
    case "select":
      return runInTab(tabId, "select", params);
    case "scroll":
      return runInTab(tabId, "scroll", params);
    case "evaluate":
      return evaluateInTab(tabId, params.script);
    case "guard_field":
      return runInTab(tabId, "guardField", params);
    case "guard_focus":
      return guardFocus(tabId, params);
    case "guard_fill":
      return guardFill(tabId, params);
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

const extractedFrames = new Map();

async function extractInTab(tabId, params) {
  params = params || {};
  const frames = await callFrames(tabId, "extract", Object.assign({}, params, { limit: 1e6, offset: 0 }));
  const ordered = frameOrder(frames);
  const byIndex = new Map();
  let base = 0;
  for (const f of ordered) {
    let data = {};
    try { data = JSON.parse(f.result.data || "{}"); } catch (_) { /* empty frame */ }
    for (const e of data.elements || []) byIndex.set(base + e.index, {
      frameId: f.frameId, local: e.index, frameURL: f.result.url,
      identity: { tag: e.tag, name: e.name, label: e.label, stable_id: e.stable_id },
    });
    base += f.result.count || 0;
  }
  extractedFrames.set(tabId, { byIndex, scope: params.scope || "", includeHidden: !!params.include_hidden });
  return mergeExtract(frames, params);
}

// callPage injects page.js (the page-side core; idempotent per frame)
// and calls one of its functions in the same ISOLATED world of one
// frame (default: the top frame). All DOM logic lives in page.js so
// the Go fixture tests exercise the same bytes the extension runs.
async function callPage(tabId, fn, params, what, frameId) {
  const target = frameId ? { tabId, frameIds: [frameId] } : { tabId };
  try {
    await chrome.scripting.executeScript({ target, files: ["guard.js", "page.js"] });
  } catch (e) {
    throw new Error(`${what}: injection failed: ${String((e && e.message) || e)}`);
  }
  return injectOne({
    target,
    args: [fn, params],
    func: (fn, params) => globalThis.__bashyPage[fn](params),
  }, what);
}

// callFrames runs one page.js function in EVERY frame of the tab (top +
// iframes, cross-origin included — <all_urls> is granted) and returns
// [{frameId, result}]. A frame the extension cannot script (a sandboxed
// frame without allow-scripts, a chrome:// or Web Store frame) answers
// null and is skipped. If the all-frames call is refused outright, the
// top frame alone answers.
async function callFrames(tabId, fn, params) {
  try {
    await chrome.scripting.executeScript({ target: { tabId, allFrames: true }, files: ["guard.js", "page.js"] });
    const out = await chrome.scripting.executeScript({
      target: { tabId, allFrames: true },
      args: [fn, params],
      func: (fn, params) => (globalThis.__bashyPage ? globalThis.__bashyPage[fn](params) : null),
    });
    return frameOrder((out || []).map((f) => ({ frameId: f.frameId, result: f.result })));
  } catch (_) {
    const r = await callPage(tabId, fn, params, fn);
    return [{ frameId: 0, result: r }];
  }
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

// runInTab runs click/type/select in the frame that holds the target:
// an index is tab-wide (as extract numbers it) and maps to one frame; a
// selector or text is tried in frame order until a frame finds it.
async function runInTab(tabId, kind, params) {
  if (kind === "scroll") return callPage(tabId, kind, params, kind);
  let frameId = 0, local = params, result = null;
  const qualified = parseFrameSelector(params.selector);
  if (qualified) params = Object.assign({}, params, { selector: qualified.selector });
  const counted = await callFrames(tabId, "count", params);
  if (params.element_id > 0) {
    const snapshot = extractedFrames.get(tabId);
    const at = snapshot && snapshot.byIndex.get(params.element_id);
    if (!at || snapshot.scope !== (params.scope || "") || snapshot.includeHidden !== !!params.include_hidden)
      throw new Error(`${kind}: guard:fill-mismatch: index ${params.element_id} has no matching extract identity`);
    const current = counted.find((f) => f.frameId === at.frameId);
    if (!current || (current.result.url && at.frameURL && current.result.url !== at.frameURL))
      throw new Error(`${kind}: guard:fill-mismatch: frame for index ${params.element_id} changed`);
    frameId = at.frameId;
    local = Object.assign({}, params, { element_id: at.local, expected_identity: at.identity });
    result = await callPage(tabId, kind, local, kind, frameId);
  } else if (qualified) {
    if (!counted.some((f) => f.frameId === qualified.frameId))
      throw new Error(`${kind}: frame ${qualified.frameId} is gone`);
    frameId = qualified.frameId;
    result = await callPage(tabId, kind, params, kind, frameId);
  } else
  if (counted.length > 1) {
      for (const f of counted) {
        const r = await callPage(tabId, kind, params, kind, f.frameId);
        if (r && r.notfound && !result) { result = r; continue; }
        if (r && r.notfound) continue;
        result = r; frameId = f.frameId;
        break;
      }
  } else {
    result = await callPage(tabId, kind, params, kind);
  }
  if ((kind === "type" || kind === "guardFill") && result && result.retry_trusted) {
    result = await typeTrusted(tabId, local, result, frameId);
  }
  if (result && result.error) {
    throw new Error(result.error);
  }
  if (result && frameId) result.frame_id = frameId;
  return result;
}

// typeTrusted is the fallback for fields that reject synthetic input
// (masks, isTrusted checks, frameworks that reset on render): focus +
// select the field, insert the text through chrome.debugger
// Input.insertText (a real IME-style insert), then verify the value.
// Reports the page's own mismatch if the trusted path is unavailable.
async function typeTrusted(tabId, params, synthetic, frameId) {
  const focus = await callPage(tabId, "focusTarget", params, "type", frameId);
  if (focus && focus.error) return focus;
  const text = params.text || "";
  try {
    await debuggerAttach.acquire(tabId, null);
    try {
      if (text === "") {
        await dispatchTrustedKey(tabId, "Delete", []);
      } else {
        await chrome.debugger.sendCommand({ tabId }, "Input.insertText", { text });
      }
    } finally {
      await debuggerAttach.release(tabId);
    }
  } catch (e) {
    return { error: `${synthetic.error} (trusted retry unavailable: ${String((e && e.message) || e)})` };
  }
  await new Promise((res) => setTimeout(res, 80));
  const read = await callPage(tabId, "readValue", {}, "type", frameId);
  if (read.error) return read;
  if (read.value !== text) {
    return { error: `type: value did not stick on ${read.matched} (synthetic and trusted input): wanted ${JSON.stringify(text)}, field shows ${JSON.stringify(read.value)}` };
  }
  return { content: "typed", data: JSON.stringify({ matched: read.matched, value: read.value, how: "trusted" }) };
}

// guardFill types an operator's admitted answer (page.js guardFill), with the
// trusted-input fallback type uses, then answers the page's own VERIFICATION
// of the field: verified "match" | "mismatch" plus field_len and value_len.
// A fill that did not stick is a mismatch (reported, not hidden); only a
// field that cannot be found or read is an error. Whatever goes wrong, the
// error is generic, and no read-back of the field — the trusted retry's
// result carries one — leaves here: the answer is rebuilt from the verdict.
// The error names only a fixed cause code (fillCause), so a live failure is
// diagnosable without the page's own message, which can quote the field.
async function guardFill(tabId, params) {
  let r = null;
  try {
    r = await runInTab(tabId, "guardFill", params);
  } catch (_) { r = null; }
  if (!r || typeof r.verified !== "string") {
    try {
      r = await runInTab(tabId, "guardVerify", params);
    } catch (e) {
      throw new Error(`guard_fill: the operator's answer could not be filled (cause: ${fillCause(e)})`);
    }
  }
  const v = {
    verified: r.verified === "match" ? "match" : "mismatch",
    field_len: Number(r.field_len) | 0,
    value_len: Number(r.value_len) | 0,
  };
  return Object.assign({ content: "filled", data: JSON.stringify(v) }, v);
}

// fillCause maps a failed fill to a fixed code: never the message itself.
function fillCause(e) {
  const m = String((e && e.message) || e);
  if (/injection failed/.test(m)) return "inject-failed";
  if (/no frame answered|returned no result/.test(m)) return "no-frame";
  if (/no element|out of range|only hidden|no selector|equally|not a valid CSS/.test(m)) return "not-found";
  return "page-error";
}

// guardFocus brings the driven tab to the operator's attention and puts the
// caret in the refused field, so the human types the value into the page
// themselves — it never travels through bashy.
async function guardFocus(tabId, params) {
  try {
    const tab = await chrome.tabs.update(tabId, { active: true });
    if (tab && tab.windowId !== undefined) await chrome.windows.update(tab.windowId, { focused: true });
  } catch (_) { /* best effort: the field is still focused below */ }
  return runInTab(tabId, "guardFocus", params);
}

// dispatchEventInTab fires a named DOM event from the extension's
// ISOLATED world. A page whose CSP omits 'unsafe-eval' disables
// `evaluate` entirely, and the usual workaround — "click whatever
// element happens to share the handler" — is not always available.
// Nothing here is compiled from a string, so no CSP directive applies.
async function dispatchEventInTab(tabId, params) {
  const name = (params && params.event || "").trim();
  if (!name) throw new Error("dispatch_event: event name required");
  const selector = params.selector || "";
  const call = { event: name, detail: params.detail || "", selector };
  if (!selector || selector === "document") {
    const r = await callPage(tabId, "dispatch", call, "dispatch_event");
    if (r && r.error) throw new Error(r.error);
    return r;
  }
  const q = parseFrameSelector(selector);
  if (q) {
    call.selector = q.selector;
    const r = await callPage(tabId, "dispatch", call, "dispatch_event", q.frameId);
    if (r && r.error) throw new Error(r.error);
    return r;
  }
  let miss = null;
  for (const f of await callFrames(tabId, "count", {})) {
    const r = await callPage(tabId, "dispatch", call, "dispatch_event", f.frameId);
    if (r && r.notfound) { miss = miss || r; continue; }
    if (r && r.error) throw new Error(r.error);
    return r;
  }
  throw new Error(miss ? miss.error : `dispatch_event: no element matches ${JSON.stringify(selector)}`);
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
      // Result sanitization: raw password-input values never leave the tab.
      const secrets = () => {
        const out = [];
        const walk = (root) => {
          for (const el of root.querySelectorAll("*")) {
            if (el.tagName === "INPUT" && (el.getAttribute("type") || "").toLowerCase() === "password" && el.value) out.push(String(el.value));
            if (el.shadowRoot) walk(el.shadowRoot);
            if (el.tagName === "IFRAME" || el.tagName === "FRAME") {
              let d = null;
              try { d = el.contentDocument; } catch (_) { /* inaccessible */ }
              if (d) { try { walk(d); } catch (_) { /* skip */ } }
            }
          }
        };
        try { walk(document); } catch (_) { /* detached */ }
        return out;
      };
      const MARK = "[guarded:password]";
      const redact = (v, pw, seen) => {
        if (!pw.length) return v;
        if (typeof v === "string") {
          for (const p of pw) { if (v === p) return MARK; if (p.length >= 4 && v.includes(p)) v = v.split(p).join(MARK); }
          return v;
        }
        if (typeof v === "number" || typeof v === "bigint") return pw.includes(String(v)) ? MARK : v;
        if (!v || typeof v !== "object" || seen.has(v)) return v;
        seen.add(v);
        if (typeof v.toJSON === "function") { try { return redact(v.toJSON(), pw, seen); } catch (_) { return v; } }
        if (Array.isArray(v)) return v.map((e) => redact(e, pw, seen));
        const o = {};
        for (const k of Object.keys(v)) o[k] = redact(v[k], pw, seen);
        return o;
      };
      const before = secrets();
      const stringify = (v) => {
        v = redact(v, before.concat(secrets()), new WeakSet());
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
  const q = parseFrameSelector(sel);
  const call = { selector: q ? q.selector : sel };
  const deadline = Date.now() + timeoutMs;
  do {
    let matches = 0, visible = 0;
    if (q) {
      const r = await callPage(tabId, "probe", call, "wait_for_selector", q.frameId);
      if (r.error) throw new Error(`wait_for_selector: ${r.error}`);
      matches = r.matches; visible = r.visible;
    } else {
      for (const f of await callFrames(tabId, "probe", call)) {
        if (f.result.error) throw new Error(`wait_for_selector: ${f.result.error}`);
        matches += f.result.matches; visible += f.result.visible;
      }
    }
    if (state === "detached" ? matches === 0 : state === "attached" ? matches > 0 : visible > 0) {
      return { data: `state=${state}` };
    }
    await new Promise((r) => setTimeout(r, 100));
  } while (Date.now() < deadline);
  throw new Error(`wait_for_selector: timeout after ${timeoutMs}ms (state=${state})`);
}

// resolveKeyTarget focuses the one visible element selector names, in the
// frame that holds it. Missing, hidden-only, ambiguous or unfocusable
// targets throw BEFORE any key event is sent.
async function resolveKeyTarget(tabId, selector) {
  const q = parseFrameSelector(selector);
  const call = { selector: q ? q.selector : selector };
  let frameId = 0;
  if (q) {
    frameId = q.frameId;
  } else {
    const holders = [];
    let matches = 0;
    for (const f of await callFrames(tabId, "probe", call)) {
      if (f.result.error) throw new Error(`keyboard_press: ${f.result.error}`);
      matches += f.result.matches;
      if (f.result.visible) holders.push({ frameId: f.frameId, n: f.result.visible });
    }
    if (!matches) throw new Error(`keyboard_press: no element matches ${JSON.stringify(selector)}`);
    if (!holders.length) throw new Error(`keyboard_press: ${JSON.stringify(selector)} matches only hidden elements`);
    if (holders.length > 1 || holders[0].n > 1) throw new Error(`keyboard_press: ${JSON.stringify(selector)} is ambiguous (matches ${holders.reduce((a, h) => a + h.n, 0)} visible elements)`);
    frameId = holders[0].frameId;
  }
  const r = await callPage(tabId, "keyFocus", call, "keyboard_press", frameId);
  if (r.error) throw new Error(r.error.startsWith("keyboard_press") ? r.error : `keyboard_press: ${r.error}`);
  return frameId;
}

async function keyboardPress(tabId, params) {
  if (!params.key) throw new Error("keyboard_press: key required");
  // Prefer chrome.debugger + Input.dispatchKeyEvent for trusted
  // keystrokes (manifest 0.4.0). Falls back to the synthetic
  // KeyboardEvent path if the debugger attach fails — for example
  // if DevTools is open on the same tab.
  let frameId = 0;
  if (params.selector) frameId = await resolveKeyTarget(tabId, params.selector);
  try {
    await dispatchTrustedKey(tabId, params.key, params.modifiers || []);
    return { data: "pressed=" + params.key + " (trusted)" };
  } catch (e) {
    // Fall back to synthetic events.
    console.warn("ycode-live: trusted key dispatch failed, falling back", e);
  }
  const [{ result } = {}] = await chrome.scripting.executeScript({
    target: frameId ? { tabId, frameIds: [frameId] } : { tabId },
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
      if (params.by_id && params.tab_id && t.id !== params.tab_id) return false;
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
    rememberIntent();
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
    rememberIntent();
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
