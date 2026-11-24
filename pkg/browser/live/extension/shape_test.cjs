// Shape button behavioral fixtures — no Chrome.
//
// Loads the real background.js and popup.js into vm sandboxes with a
// stubbed chrome API, a scripted WebSocket and a DOM built from the ids
// popup.html really declares, then presses Shape and plays the hub's
// answers. Every fixture asserts OBSERVED behavior: the frame put on the
// socket, the response the popup gets, what the status line shows and
// whether the note stays in the box — never implementation source strings.
//
// Gate:
//
//	node --test pkg/browser/live/extension/shape_test.cjs
//
// (Short unit: no browser, no network. The hub's half — origin check, size
// cap, routing — is Go: pkg/browser/live/shape_test.go and
// cmds/browser/shape_test.go.)
"use strict";

const { test } = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

const DIR = __dirname;
const tick = () => new Promise((r) => setImmediate(r));
// plain copies a value made inside a sandbox into this realm, so deepEqual
// compares what it holds and not which realm's Object built it.
const plain = (v) => (v === undefined ? v : JSON.parse(JSON.stringify(v)));

// ---------------------------------------------------------------------------
// background.js: the press becomes one frame, and one answer settles it.
// ---------------------------------------------------------------------------

const TABS = [
  { id: 4711, url: "https://example.com/", title: "Example", active: false },
  { id: 4712, url: "https://example.com/two", title: "Two", active: false },
  { id: 9, url: "https://human.example/", title: "Human", active: true }, // the one the human selected
];

function loadBackground() {
  const state = { failScript: false };
  const sockets = [];
  const timers = []; // {fn, ms, cleared}
  const scripted = []; // every chrome.scripting.executeScript call
  const focused = []; // tabs.update / windows.update calls: only the Focus press may add any
  const tabListeners = { removed: [], updated: [] };
  let listener = null;
  class FakeSocket {
    constructor(url) {
      this.url = url;
      this.readyState = FakeSocket.CONNECTING;
      this.sent = [];
      this.failSend = "";
      sockets.push(this);
    }
    send(data) {
      if (this.failSend) throw new Error(this.failSend);
      this.sent.push(JSON.parse(data));
    }
    close() { this.readyState = FakeSocket.CLOSED; }
  }
  Object.assign(FakeSocket, { CONNECTING: 0, OPEN: 1, CLOSING: 2, CLOSED: 3 });

  const event = { addListener() {}, removeListener() {} };
  const chrome = {
    runtime: {
      onMessage: { addListener(fn) { listener = fn; } },
      onStartup: event, onInstalled: event, onConnect: event,
      getManifest: () => ({ version: "0.12.0", permissions: [] }),
      reload() {},
    },
    storage: {
      local: {
        get(keys, cb) {
          const r = Promise.resolve({});
          if (typeof cb === "function") r.then((v) => cb(v));
          return r;
        },
        set() { return Promise.resolve(); },
      },
    },
    alarms: { create() {}, clear() {}, onAlarm: event },
    tabs: {
      get: async (id) => ({ id }),
      query: async () => TABS,
      update: async (id, p) => { focused.push(["tab", id, p]); return { id, windowId: 3 }; },
      onUpdated: { addListener(fn) { tabListeners.updated.push(fn); }, removeListener() {} },
      onRemoved: { addListener(fn) { tabListeners.removed.push(fn); } },
      onActivated: event,
    },
    windows: { update: async (id, p) => { focused.push(["window", id, p]); return {}; } },
    scripting: { executeScript: async (c) => {
      scripted.push(c);
      if (state.failScript && c.func.name !== "paintBanner") throw new Error("page action failed");
      return [{}];
    } },
    debugger: {
      attach: async () => {}, sendCommand: async () => ({}), detach: async () => {},
      onDetach: event, onEvent: event,
    },
    cookies: { getAll: async () => [] },
  };
  const sandbox = {
    chrome,
    console: { log() {}, warn() {}, error() {}, info() {}, debug() {} },
    // Timers are recorded, never run: the fixture fires the one it means.
    setTimeout: (fn, ms) => { const t = { fn, ms, cleared: false }; timers.push(t); return t; },
    clearTimeout: (t) => { if (t) t.cleared = true; },
    setInterval: () => 0, clearInterval() {},
    WebSocket: FakeSocket,
    crypto: { randomUUID: () => "00000000-0000-4000-8000-000000000000" },
    navigator: { userAgent: "Mozilla/5.0 Chrome/140.0.0.0 Safari/537.36 Edg/140.0.0.0" },
  };
  sandbox.globalThis = sandbox;
  sandbox.window = sandbox;
  sandbox.self = sandbox;
  vm.createContext(sandbox);
  sandbox.importScripts = (name) => {
    vm.runInContext(fs.readFileSync(path.join(DIR, name), "utf8"), sandbox, { filename: name });
  };
  vm.runInContext(fs.readFileSync(path.join(DIR, "background.js"), "utf8"), sandbox, { filename: "background.js" });
  assert.equal(typeof listener, "function", "background.js registered no runtime.onMessage listener");

  // send plays the popup: one runtime message, the promise is its response.
  const send = (msg) => new Promise((resolve) => listener(msg, {}, resolve));
  // hub plays the hub's request frame to the worker and settles it.
  const hub = async (ws, params, method) => {
    await ws.onmessage({ data: JSON.stringify({ id: 9, method: method || "text", params }) });
    await tick();
  };
  return {
    state, sockets, timers, send, scripted, focused, tabListeners, hub, sandbox,
    // connect presses Connect and opens the socket, as the hub accepting it.
    async connect() {
      await send({ type: "ycode-live:connect", port: 58084, tabId: 7 });
      const ws = sockets[sockets.length - 1];
      ws.readyState = FakeSocket.OPEN;
      ws.onopen();
      await tick();
      ws.sent.length = 0; // the _hello
      return ws;
    },
  };
}

const TAB = { id: 4711, url: "https://example.com/orders?id=7", title: "Orders | Example" };
const answer = (ws, params) => ws.onmessage({ data: JSON.stringify({ id: 0, method: "_shape_result", params }) });

test("a press is one shape frame, and the hub's answer settles it", async () => {
  const bg = loadBackground();
  const ws = await bg.connect();
  const note = 'first line\n  <b>second</b> & "quoted"\n';
  let resp = null;
  bg.send({ type: "ycode-live:shape", tab: TAB, note, to: "codex" }).then((r) => { resp = r; });
  await tick();

  assert.equal(ws.sent.length, 1);
  const frame = ws.sent[0];
  assert.equal(frame.id, 0);
  assert.equal(frame.method, "shape");
  assert.ok(frame.result.nonce, "the frame carries a nonce to answer");
  assert.deepEqual({ ...frame.result, nonce: "" }, {
    nonce: "", tab_id: 4711, url: TAB.url, title: TAB.title, note, to: "codex", browser: "edge",
  });
  assert.equal(resp, null, "answered before the hub said anything");

  // An answer to some other press settles nothing.
  await answer(ws, { nonce: "not-this-one", ok: true, status: "sent" });
  await tick();
  assert.equal(resp, null);

  await answer(ws, { nonce: frame.result.nonce, ok: true, status: "sent to codex's live chat session", via: "steer" });
  await tick();
  assert.deepEqual(plain(resp), { ok: true, status: "sent to codex's live chat session", error: "", truncated: false });
  // The answer is not a call from the hub: nothing is dispatched or replied.
  assert.equal(ws.sent.length, 1);
});

test("each press is settled by its own answer, in any order", async () => {
  const bg = loadBackground();
  const ws = await bg.connect();
  const got = {};
  bg.send({ type: "ycode-live:shape", tab: TAB, note: "one", to: "" }).then((r) => { got.one = r; });
  bg.send({ type: "ycode-live:shape", tab: TAB, note: "two", to: "" }).then((r) => { got.two = r; });
  await tick();
  const [one, two] = ws.sent.map((f) => f.result);
  assert.notEqual(one.nonce, two.nonce);

  await answer(ws, { nonce: two.nonce, ok: false, error: '"zed" is not a live chat session or a registered agent; nothing was sent' });
  await answer(ws, { nonce: one.nonce, ok: true, status: "kept", truncated: true });
  await tick();
  assert.deepEqual(plain(got.two), { ok: false, status: "", error: '"zed" is not a live chat session or a registered agent; nothing was sent', truncated: false });
  assert.deepEqual(plain(got.one), { ok: true, status: "kept", error: "", truncated: true });
});

test("a press with no hub or no tab says so and sends nothing", async () => {
  const bg = loadBackground();
  let resp = await bg.send({ type: "ycode-live:shape", tab: TAB, note: "hello", to: "" });
  assert.equal(resp.ok, false);
  assert.match(resp.error, /not connected to the bashy hub/);
  assert.equal(bg.sockets.length, 0, "a press must not open a connection by itself");

  const ws = await bg.connect();
  resp = await bg.send({ type: "ycode-live:shape", tab: { url: "https://example.com/" }, note: "hello", to: "" });
  assert.deepEqual(plain(resp), { ok: false, error: "no current tab to send" });
  assert.equal(ws.sent.length, 0);
});

test("a hub that never answers is reported, and a late answer changes nothing", async () => {
  const bg = loadBackground();
  const ws = await bg.connect();
  const seen = [];
  bg.send({ type: "ycode-live:shape", tab: TAB, note: "hello", to: "" }).then((r) => seen.push(r));
  await tick();
  const pending = bg.timers.filter((t) => t.ms === 30000 && !t.cleared);
  assert.equal(pending.length, 1, "the press waits 30s for the hub");
  pending[0].fn();
  await tick();
  assert.equal(seen.length, 1);
  assert.equal(seen[0].ok, false);
  assert.match(seen[0].error, /did not answer.*older than the Shape button/);

  await answer(ws, { nonce: ws.sent[0].result.nonce, ok: true, status: "sent" });
  await tick();
  assert.equal(seen.length, 1);
});

test("an answered press stops waiting", async () => {
  const bg = loadBackground();
  const ws = await bg.connect();
  bg.send({ type: "ycode-live:shape", tab: TAB, note: "", to: "" });
  await tick();
  await answer(ws, { nonce: ws.sent[0].result.nonce, ok: true, status: "sent" });
  await tick();
  assert.equal(bg.timers.filter((t) => t.ms === 30000 && !t.cleared).length, 0);
});

test("a hub that goes away before answering fails the press", async () => {
  const bg = loadBackground();
  const ws = await bg.connect();
  let resp = null;
  bg.send({ type: "ycode-live:shape", tab: TAB, note: "hello", to: "" }).then((r) => { resp = r; });
  await tick();
  ws.readyState = 3;
  ws.onclose({ code: 1006, reason: "" });
  await tick();
  assert.deepEqual(plain(resp), { ok: false, error: "the bashy hub disconnected before it answered" });

  const ws2 = await bg.connect();
  ws2.failSend = "socket is closing";
  resp = await bg.send({ type: "ycode-live:shape", tab: TAB, note: "hello", to: "" });
  assert.deepEqual(plain(resp), { ok: false, error: "could not reach the bashy hub: socket is closing" });
});

// ---------------------------------------------------------------------------
// popup.js: the button, the note box, the To field and the status line.
// ---------------------------------------------------------------------------

// A DOM of exactly the ids popup.html declares: an id popup.js needs and the
// page lacks is null here, as it is in the browser.
function makeDOM() {
  const html = fs.readFileSync(path.join(DIR, "popup.html"), "utf8");
  const els = {};
  const markup = []; // every innerHTML write, as [id, html]
  for (const m of html.matchAll(/\bid="([^"]+)"/g)) {
    const id = m[1];
    const el = {
      id, value: "", textContent: "", className: "", disabled: false, checked: false,
      style: {}, children: [], listeners: {},
      classList: { add() {}, remove() {}, toggle() {} },
      addEventListener(type, fn) { (el.listeners[type] ||= []).push(fn); },
      replaceChildren() { el.children = []; },
      prepend(c) { el.children.unshift(c); },
      appendChild(c) { el.children.push(c); },
      removeChild() {},
      set innerHTML(v) { markup.push([id, v]); },
      fire(type) { return Promise.all((el.listeners[type] || []).map((fn) => fn({ type }))); },
    };
    els[id] = el;
  }
  return { els, markup };
}

function loadPopup(opts) {
  opts = opts || {};
  const { els, markup } = makeDOM();
  const stored = { ...(opts.stored || {}) };
  const messages = [];
  const chrome = {
    runtime: {
      getManifest: () => ({ version: "0.12.0" }),
      getURL: (p) => "chrome-extension://test/" + p,
      sendMessage: async (msg) => {
        messages.push(msg);
        if (msg.type === "ycode-live:focus") return opts.focus || { ok: true };
        if (msg.type === "ycode-live:shape") {
          if (opts.shapeThrows) throw new Error(opts.shapeThrows);
          return opts.shape;
        }
        return { connected: true, port: 58082, routes: opts.route ? [opts.route] : (opts.routes || []) };
      },
      connect: () => ({ onMessage: { addListener() {} }, postMessage() {} }),
    },
    storage: {
      local: {
        get(keys, cb) {
          const out = {};
          for (const k of [].concat(keys)) if (k in stored) out[k] = stored[k];
          if (typeof cb === "function") cb(out);
          return Promise.resolve(out);
        },
        set(kv) { Object.assign(stored, kv); return Promise.resolve(); },
      },
    },
    tabs: {
      query: async () => opts.tabs ?? [TAB],
      get: async (id) => ({ id, title: "title-" + id }),
    },
    windows: {
      getLastFocused: async (q) => { opts.windowQuery = q; return opts.lastFocused; },
      create: async () => ({}),
    },
  };
  const sandbox = {
    chrome,
    document: {
      getElementById: (id) => els[id] || null,
      createElement: () => {
        const el = { attrs: {}, children: [], listeners: {}, className: "", textContent: "", disabled: false,
          setAttribute(k, v) { el.attrs[k] = v; }, appendChild(c) { el.children.push(c); },
          addEventListener(type, fn) { (el.listeners[type] ||= []).push(fn); },
          fire(type) { return Promise.all((el.listeners[type] || []).map((fn) => fn({ type }))); } };
        return el;
      },
      body: { classList: { add() {} } },
    },
    location: { search: opts.windowed ? "?windowed=1" : "" },
    navigator: { userAgent: "Mozilla/5.0 Chrome/140.0.0.0 Safari/537.36" },
    URLSearchParams,
    setTimeout: () => 0,
    console,
  };
  sandbox.window = sandbox;
  vm.createContext(sandbox);
  vm.runInContext(fs.readFileSync(path.join(DIR, "popup.js"), "utf8"), sandbox, { filename: "popup.js" });
  const shapeMessages = () => messages.filter((m) => m.type === "ycode-live:shape");
  return { els, markup, stored, shapeMessages, messages, opts };
}

test("Shape sends the current tab, the note as typed and the recipient", async () => {
  const p = loadPopup({ shape: { ok: true, status: "sent to codex's live chat session", truncated: false } });
  const note = "  look at row 3\n<script>alert(1)</script>\n";
  p.els["shape-to"].value = "  codex ";
  p.els["shape-note"].value = note;
  await p.els["shape-send"].fire("click");

  assert.deepEqual(plain(p.shapeMessages()), [{
    type: "ycode-live:shape", tab: { id: 4711, url: TAB.url, title: TAB.title }, note, to: "codex",
  }]);
  assert.equal(p.els["shape-status"].textContent, "sent to codex's live chat session");
  assert.equal(p.els["shape-status"].className, "sent");
  assert.equal(p.els["shape-note"].value, "", "a delivered note leaves the box");
  assert.equal(p.els["shape-send"].disabled, false);
  assert.equal(p.stored.shapeTo, "codex", "the recipient is remembered");
});

test("the remembered recipient is back in the To field when the popup opens", async () => {
  const p = loadPopup({ stored: { shapeTo: "codex-gpt5.6-sol" } });
  assert.equal(p.els["shape-to"].value, "codex-gpt5.6-sol");
  // Editing the field is remembered without a press.
  p.els["shape-to"].value = " claude ";
  await p.els["shape-to"].fire("change");
  assert.equal(p.stored.shapeTo, "claude");
  assert.equal(loadPopup().els["shape-to"].value, "", "no recipient is the default");
});

test("a refused or failed press keeps the note and shows why", async () => {
  const refused = '"zed" is not a live chat session or a registered agent; nothing was sent';
  for (const [opts, want] of [
    [{ shape: { ok: false, error: refused } }, refused],
    [{ shape: undefined }, "not sent"],
    [{ shapeThrows: "Extension context invalidated." }, "not sent: Extension context invalidated."],
  ]) {
    const p = loadPopup(opts);
    p.els["shape-note"].value = "do not lose me";
    await p.els["shape-send"].fire("click");
    assert.equal(p.els["shape-status"].textContent, want);
    assert.equal(p.els["shape-status"].className, "error");
    assert.equal(p.els["shape-note"].value, "do not lose me");
    assert.equal(p.els["shape-send"].disabled, false);
  }
});

test("a note the hub cut stays in the box and the human is told", async () => {
  const status = "kept in your own inbox (note cut to 8 KB: only the first 8192 bytes were sent)";
  const p = loadPopup({ shape: { ok: true, status, truncated: true } });
  p.els["shape-note"].value = "x".repeat(9000);
  await p.els["shape-send"].fire("click");
  assert.equal(p.els["shape-status"].textContent, status);
  assert.equal(p.els["shape-status"].className, "sent");
  assert.equal(p.els["shape-note"].value.length, 9000);
});

test("the note and the hub's words are shown as text, never as markup", async () => {
  const evil = '<img src=x onerror="alert(1)">';
  for (const shape of [{ ok: true, status: evil }, { ok: false, error: evil }]) {
    const p = loadPopup({ shape });
    p.els["shape-note"].value = evil;
    p.els["shape-to"].value = evil;
    await p.els["shape-send"].fire("click");
    assert.equal(p.els["shape-status"].textContent, evil);
    assert.equal(p.shapeMessages()[0].note, evil);
    assert.deepEqual(p.markup, [], "nothing in a press may be written as HTML");
  }
});

test("with no tab to send, nothing is sent", async () => {
  const p = loadPopup({ tabs: [], shape: { ok: true, status: "sent" } });
  p.els["shape-note"].value = "hello";
  await p.els["shape-send"].fire("click");
  assert.equal(p.shapeMessages().length, 0);
  assert.equal(p.els["shape-status"].textContent, "no current tab to send");
  assert.equal(p.els["shape-status"].className, "error");
  assert.equal(p.els["shape-note"].value, "hello");
});

test("the popped-out panel sends the browser window's tab, not itself", async () => {
  const p = loadPopup({
    windowed: true,
    tabs: [{ id: 1, url: "chrome-extension://test/popup.html?windowed=1", title: "bashy live" }],
    lastFocused: { tabs: [{ id: 8, active: false, url: "https://a.example/" }, { id: 9, active: true, url: "https://b.example/", title: "B" }] },
    shape: { ok: true, status: "sent" },
  });
  await p.els["shape-send"].fire("click");
  assert.deepEqual(plain(p.shapeMessages()[0].tab), { id: 9, url: "https://b.example/", title: "B" });
  assert.deepEqual(plain(p.opts.windowQuery), { populate: true, windowTypes: ["normal"] });
});

// ---------------------------------------------------------------------------
// Route: the banner on the driven tab, selected vs driven, exact chat.
// ---------------------------------------------------------------------------

const DRIVE = { by_id: true, tab_id: 4711, agent: "claude-x", chat: "chat-123" };
const banners = (bg) => bg.scripted.filter((c) => typeof c.func === "function" && c.func.name === "paintBanner").map((c) => [c.target.tabId, c.args[0]]);

test("a call naming agent and chat paints one banner on that tab only; a plain call paints none", async () => {
  const bg = loadBackground();
  const ws = await bg.connect();
  await bg.hub(ws, { by_id: true, tab_id: 4711 });
  assert.deepEqual(banners(bg).filter(([, l]) => l), [], "legacy calls get no banner");

  await bg.hub(ws, DRIVE);
  assert.deepEqual(banners(bg).filter(([, l]) => l), [[4711, "claude-x"]]);
  assert.deepEqual(bg.focused, [], "driving a tab never moves focus");
  const st = plain(await bg.send({ type: "ycode-live:status" }));
  assert.equal(st.routes.length, 1);
  assert.match(st.routes[0].color, /^#[0-9a-f]{6}$/);
  assert.deepEqual({ ...st.routes[0], color: undefined }, { tabId: 4711, agent: "claude-x", chat: "chat-123", stale: false, color: undefined });
});

test("the banner is a click-through label built with textContent, no focus", () => {
  const bg = loadBackground();
  const painted = [];
  const doc = {
    getElementById: () => null,
    createElement: () => {
      const el = { attrs: {}, focus() { throw new Error("banner must not take focus"); },
        setAttribute(k, v) { el.attrs[k] = v; }, remove() {} };
      return el;
    },
    body: { appendChild: (el) => painted.push(el) },
  };
  const fn = bg.sandbox.paintBanner;
  vm.runInNewContext("(" + fn.toString() + ")('<img src=x onerror=1>')", { document: doc });
  assert.equal(painted.length, 1);
  assert.equal(painted[0].textContent, "Bashy \u00b7 <img src=x onerror=1>");
  assert.match(painted[0].attrs.style, /pointer-events:none/);
});

test("the popup tells a driven selected tab from a browser-selected one, and Focus is explicit", async () => {
  const route = { tabId: 4711, agent: "claude-x", chat: "chat-123", stale: false };
  const same = loadPopup({ route, shape: { ok: true, status: "sent to claude-x's live chat session" } });
  await tick(); await tick();
  assert.equal(same.els.route.className, "driven");
  assert.match(same.els["route-text"].textContent, /driven by Bashy: claude-x/);
  assert.equal(same.els["route-list"].children.length, 1);
  assert.equal(same.els["route-list"].children[0].children[3].disabled, true, "the selected tab's own row has nothing to focus");
  assert.equal(same.els["shape-to"].disabled, true);
  same.els["shape-note"].value = "look";
  await same.els["shape-send"].fire("click");
  assert.deepEqual(plain(same.shapeMessages()), [{
    type: "ycode-live:shape", tab: { id: 4711, url: TAB.url, title: TAB.title }, note: "look", to: "", useRoute: true,
  }]);

  const other = loadPopup({ route: { ...route, tabId: 99 }, shape: { ok: true, status: "kept" } });
  await tick(); await tick();
  assert.equal(other.els.route.className, "other");
  assert.match(other.els["route-text"].textContent, /not driven by Bashy/);
  const orow = other.els["route-list"].children[0];
  assert.equal(orow.children[1].textContent, "claude-x");
  assert.equal(orow.children[2].textContent, "title-99");
  await orow.children[3].fire("click");
  assert.deepEqual(plain(other.messages.filter((m) => m.type === "ycode-live:focus")), [{ type: "ycode-live:focus", tabId: 99 }]);
  assert.equal(other.els["shape-to"].disabled, false);
  other.els["shape-to"].value = "codex";
  await other.els["shape-send"].fire("click");
  assert.equal(other.shapeMessages()[0].useRoute, undefined, "an unbound tab keeps the manual recipient");
  assert.equal(other.shapeMessages()[0].to, "codex");
  assert.equal(other.focused, undefined);
});

test("Focus asks the worker, and only then does it focus the driven tab", async () => {
  const bg = loadBackground();
  const ws = await bg.connect();
  assert.deepEqual(plain(await bg.send({ type: "ycode-live:focus", tabId: 4711 })), { ok: false, error: "no tab is driven by an agent" });
  await bg.hub(ws, DRIVE);
  assert.deepEqual(bg.focused, []);
  assert.deepEqual(plain(await bg.send({ type: "ycode-live:focus", tabId: 4711 })), { ok: true });
  assert.deepEqual(plain(bg.focused), [["tab", 4711, { active: true }], ["window", 3, { focused: true }]]);
});

test("a note on the bound tab carries the exact chat; any other tab is refused", async () => {
  const bg = loadBackground();
  const ws = await bg.connect();
  await bg.hub(ws, DRIVE);
  ws.sent.length = 0;
  let resp = null;
  bg.send({ type: "ycode-live:shape", tab: TAB, note: "n", to: "", useRoute: true }).then((r) => { resp = r; });
  await tick();
  assert.equal(ws.sent.length, 1);
  assert.equal(ws.sent[0].result.tab_id, 4711);
  assert.equal(ws.sent[0].result.chat, "chat-123");
  await answer(ws, { nonce: ws.sent[0].result.nonce, ok: true, status: "sent to claude-x's live chat session" });
  await tick();
  assert.equal(resp.ok, true);

  ws.sent.length = 0;
  resp = await bg.send({ type: "ycode-live:shape", tab: { ...TAB, id: 5 }, note: "n", to: "", useRoute: true });
  assert.equal(resp.ok, false);
  assert.equal(ws.sent.length, 0, "a different tab never rides the bound chat");
  // A press without useRoute is the legacy frame: no chat field at all.
  bg.send({ type: "ycode-live:shape", tab: TAB, note: "n", to: "codex" });
  await tick();
  assert.equal("chat" in ws.sent[0].result, false);
});

test("a stale route is reported, the banner says so, and manual recipient still works", async () => {
  const bg = loadBackground();
  const ws = await bg.connect();
  await bg.hub(ws, DRIVE);
  let resp = null;
  bg.send({ type: "ycode-live:shape", tab: TAB, note: "n", to: "", useRoute: true }).then((r) => { resp = r; });
  await tick();
  await answer(ws, { nonce: ws.sent[ws.sent.length - 1].result.nonce, ok: false, stale: true, error: "the agent chat bound to this tab is gone; nothing was sent" });
  await tick();
  assert.equal(resp.ok, false);
  assert.equal(resp.stale, true);
  assert.deepEqual(banners(bg).pop(), [4711, "claude-x (chat gone)"]);
  assert.equal(plain(await bg.send({ type: "ycode-live:status" })).routes[0].stale, true);

  const refused = await bg.send({ type: "ycode-live:shape", tab: TAB, note: "n", to: "", useRoute: true });
  assert.equal(refused.ok, false);
  const p = loadPopup({ route: { tabId: 4711, agent: "claude-x", chat: "chat-123", stale: true } });
  await tick(); await tick();
  assert.equal(p.els.route.className, "stale");
  assert.match(p.els["route-text"].textContent, /stale/);
  assert.equal(p.els["shape-to"].disabled, false);
});

test("tab close, socket close and Disconnect each clear the route and banner", async () => {
  for (const how of ["tab", "socket", "disconnect"]) {
    const bg = loadBackground();
    const ws = await bg.connect();
    await bg.hub(ws, DRIVE);
    assert.equal(plain(await bg.send({ type: "ycode-live:status" })).routes.length, 1, how);
    const before = banners(bg).length;
    if (how === "tab") bg.tabListeners.removed.forEach((fn) => fn(4711));
    else if (how === "socket") ws.onclose({ code: 1006 });
    else await bg.send({ type: "ycode-live:disconnect" });
    await tick();
    assert.deepEqual(plain(await bg.send({ type: "ycode-live:status" })).routes, [], how);
    if (how !== "tab") assert.deepEqual(banners(bg).slice(before), [[4711, null]], how + ": banner removed");
  }
});

test("a navigation repaints the banner on the bound tab only", async () => {
  const bg = loadBackground();
  const ws = await bg.connect();
  await bg.hub(ws, DRIVE);
  const n = banners(bg).length;
  bg.tabListeners.updated.forEach((fn) => fn(5, { status: "complete" }));
  bg.tabListeners.updated.forEach((fn) => fn(4711, { status: "loading" }));
  await tick();
  assert.equal(banners(bg).length, n);
  bg.tabListeners.updated.forEach((fn) => fn(4711, { status: "complete" }));
  await tick();
  assert.deepEqual(banners(bg).slice(n), [[4711, "claude-x"]]);
});

// ---------------------------------------------------------------------------
// One agent, several tabs; each route is per exact tab on the current socket.
// ---------------------------------------------------------------------------

const DRIVE2 = { by_id: true, tab_id: 4712, agent: "claude-x", chat: "chat-123" };
const routesOf = async (bg) => plain(await bg.send({ type: "ycode-live:status" })).routes.map((r) => r.tabId).sort();

test("one agent drives two tabs: both keep banners and chat; the selected third tab has none", async () => {
  const bg = loadBackground();
  const ws = await bg.connect();
  await bg.hub(ws, DRIVE);
  await bg.hub(ws, DRIVE2);
  assert.deepEqual(banners(bg).filter(([, l]) => l), [[4711, "claude-x"], [4712, "claude-x"]]);
  assert.deepEqual(banners(bg).filter(([, l]) => !l), [], "the second tab did not remove the first banner");
  assert.deepEqual(await routesOf(bg), [4711, 4712]);
  assert.deepEqual(bg.focused, []);

  // Each driven tab's note carries the exact chat receipt; the selected tab 9 is refused.
  for (const id of [4711, 4712]) {
    ws.sent.length = 0;
    let resp = null;
    bg.send({ type: "ycode-live:shape", tab: { ...TAB, id }, note: "n", to: "", useRoute: true }).then((r) => { resp = r; });
    await tick();
    assert.equal(ws.sent[0].result.tab_id, id);
    assert.equal(ws.sent[0].result.chat, "chat-123");
    await answer(ws, { nonce: ws.sent[0].result.nonce, ok: true, status: "sent to claude-x's live chat session" });
    await tick();
    assert.equal(resp.ok, true);
    assert.equal(resp.status, "sent to claude-x's live chat session");
  }
  ws.sent.length = 0;
  const third = await bg.send({ type: "ycode-live:shape", tab: { ...TAB, id: 9 }, note: "n", to: "", useRoute: true });
  assert.equal(third.ok, false);
  assert.equal(ws.sent.length, 0);
  // Legacy/manual press on the selected tab: no chat field, manual recipient.
  bg.send({ type: "ycode-live:shape", tab: { ...TAB, id: 9 }, note: "n", to: "codex" });
  await tick();
  assert.equal(ws.sent[0].result.to, "codex");
  assert.equal("chat" in ws.sent[0].result, false);

  // A stale answer marks only the tab it came from.
  ws.sent.length = 0;
  bg.send({ type: "ycode-live:shape", tab: { ...TAB, id: 4712 }, note: "n", to: "", useRoute: true });
  await tick();
  await answer(ws, { nonce: ws.sent[0].result.nonce, ok: false, stale: true, error: "gone" });
  await tick();
  const st = plain(await bg.send({ type: "ycode-live:status" })).routes;
  assert.deepEqual(st.map((r) => [r.tabId, r.stale]).sort(), [[4711, false], [4712, true]]);
});

test("closing one driven tab, then the socket, clear exactly their routes and banners", async () => {
  const bg = loadBackground();
  const ws = await bg.connect();
  await bg.hub(ws, DRIVE);
  await bg.hub(ws, DRIVE2);
  bg.tabListeners.removed.forEach((fn) => fn(4711));
  assert.deepEqual(await routesOf(bg), [4712]);
  const before = banners(bg).length;
  ws.onclose({ code: 1006 });
  await tick();
  assert.deepEqual(await routesOf(bg), []);
  assert.deepEqual(banners(bg).slice(before), [[4712, null]]);
});

test("a late close of an old socket does not clear the newer socket's routes", async () => {
  const bg = loadBackground();
  const old = await bg.connect();
  await bg.send({ type: "ycode-live:disconnect" });
  const fresh = await bg.connect();
  assert.notEqual(old, fresh);
  await bg.hub(fresh, DRIVE);
  old.onclose({ code: 1006 });
  await tick();
  assert.deepEqual(await routesOf(bg), [4711]);
});

test("a failed address or failed action binds no route and paints no banner", async () => {
  const bg = loadBackground();
  const ws = await bg.connect();
  await bg.hub(ws, { by_id: true, tab_id: 999, agent: "claude-x", chat: "chat-123" });
  assert.deepEqual(await routesOf(bg), [], "an unresolved tab binds nothing");
  bg.state.failScript = true;
  await bg.hub(ws, { ...DRIVE, selector: "#x" });
  assert.deepEqual(await routesOf(bg), [], "a failed action binds nothing");
  assert.deepEqual(banners(bg).filter(([, l]) => l), []);
});
