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

function loadBackground() {
  const sockets = [];
  const timers = []; // {fn, ms, cleared}
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
    tabs: { get: async (id) => ({ id }), query: async () => [], onUpdated: event, onRemoved: event, onActivated: event },
    windows: { update: async () => ({}) },
    scripting: { executeScript: async () => [{}] },
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
  return {
    sockets, timers, send,
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
        if (msg.type === "ycode-live:shape") {
          if (opts.shapeThrows) throw new Error(opts.shapeThrows);
          return opts.shape;
        }
        return { connected: true, port: 58082 };
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
    tabs: { query: async () => opts.tabs ?? [TAB] },
    windows: {
      getLastFocused: async (q) => { opts.windowQuery = q; return opts.lastFocused; },
      create: async () => ({}),
    },
  };
  const sandbox = {
    chrome,
    document: {
      getElementById: (id) => els[id] || null,
      createElement: () => { throw new Error("no debug rows in these fixtures"); },
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
  return { els, markup, stored, shapeMessages, opts };
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
