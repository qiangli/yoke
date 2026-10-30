// S536.4 stitch behavioral fixtures — no Chrome.
//
// Loads the real background.js (+ frames.js) into a vm sandbox with a
// stubbed chrome API and a scripted fake page, then drives takeScreenshot
// / stitchFullPage through the paths the review flagged: debugger-acquire
// failure, a hanging capture past the stitch timeout, a mid-stitch tile
// failure, panel-window pinning on a tall page, and late attach
// completion. Every fixture asserts OBSERVED behavior (restoration calls
// with saved nonzero positions, no scroll/capture mutations after cancel,
// explicit failure instead of a downgraded full_page success) — never
// implementation source strings.
//
// Gate:
//
//	node --test pkg/browser/live/extension/stitch_test.cjs
//
// (Short unit: no browser, no network. The Chrome-backed extent/content
// proof stays in TestExtensionE2EEffects / TestSoloEffectsClickTextScrollCapture,
// run by the manager on the remote host.)
"use strict";

const { test } = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

const DIR = __dirname;

// ---------------------------------------------------------------------------
// Fake page: models window scroll, panel scrollTops and the fixed banner
// behind the shotLayout/shotSet/shotRestore contract. Starts at NONZERO
// positions unless the scenario says otherwise, so a restore that lands
// at 0,0 fails instead of passing silently.
// ---------------------------------------------------------------------------
function makePageModel(spec) {
  const st = {
    winX: spec.winX ?? 0,
    winY: spec.winY ?? 0,
    bannerHidden: false,
    panels: (spec.panels ?? []).map((p) => ({ ...p, pst: p.pst ?? 0, psl: 0 })),
    saved: null,
    calls: [], // {fn, arg}
  };
  const clampWin = (y) => {
    const max = Math.max(spec.docH - spec.vh, 0);
    return Math.min(Math.max(y || 0, 0), max);
  };
  return {
    state: st,
    shotLayout() {
      st.saved = {
        x: st.winX, y: st.winY,
        panels: st.panels.map((p) => ({ st: p.pst, sl: p.psl })),
      };
      st.bannerHidden = true;
      st.calls.push({ fn: "shotLayout", arg: null });
      return {
        dpr: 1, vw: spec.vw, vh: spec.vh, docW: spec.docW, docH: spec.docH,
        x: st.saved.x, y: st.saved.y,
        panels: st.panels.map((p) => ({
          x: p.x, y: p.y, w: p.w, h: p.h, sh: p.sh, sw: p.w,
          st: p.pst, sl: p.psl,
        })),
      };
    },
    shotSet(s) {
      s = s || {};
      st.calls.push({ fn: "shotSet", arg: { ...s } });
      st.winX = s.x || 0;
      st.winY = clampWin(s.y);
      let pst = -1;
      if (s.p >= 0 && st.panels[s.p]) {
        const p = st.panels[s.p];
        p.pst = Math.min(Math.max(s.pt || 0, 0), Math.max(p.sh - p.h, 0));
        pst = p.pst;
      }
      return { x: st.winX, y: st.winY, pst };
    },
    shotRestore(s) {
      st.calls.push({ fn: "shotRestore", arg: s === undefined ? undefined : JSON.parse(JSON.stringify(s)) });
      const use = s || st.saved || { x: 0, y: 0, panels: [] };
      st.winX = use.x || 0;
      st.winY = use.y || 0;
      (use.panels || []).forEach((sp, i) => {
        if (st.panels[i]) st.panels[i].pst = sp.st || 0;
      });
      st.bannerHidden = false;
      return { ok: true };
    },
  };
}

// ---------------------------------------------------------------------------
// Sandbox: loads background.js with per-scenario chrome hooks.
// ---------------------------------------------------------------------------
function loadExtension(hooks) {
  hooks = hooks || {};
  const events = { attach: 0, detach: 0, sendCommand: [] };
  const chrome = {
    runtime: {
      onMessage: { addListener() {} },
      onStartup: { addListener() {} },
      onInstalled: { addListener() {} },
      onConnect: { addListener() {} },
      getManifest: () => ({ version: "0.10.0", permissions: [] }),
      reload() {},
    },
    storage: {
      local: {
        get(keys, cb) {
          const r = Promise.resolve({});
          if (typeof cb === "function") r.then((v) => cb(v));
          return r;
        },
        set() {},
      },
    },
    alarms: {
      create() {}, clear() {}, onAlarm: { addListener() {} },
    },
    tabs: {
      get: async (id) => ({ id, windowId: 1, url: "http://fixture/fx", title: "fx" }),
      query: async () => [],
      create: async () => ({ id: 9, windowId: 1 }),
      update: async (id) => ({ id, windowId: 1 }),
      goBack: async () => {},
      captureVisibleTab: async () => { throw new Error("not used in stitch scenarios"); },
      onUpdated: { addListener() {}, removeListener() {} },
    },
    windows: { update: async () => ({}) },
    scripting: {
      executeScript: async (opts) => hooks.scripting(opts),
    },
    debugger: {
      attach: async (target, version) => {
        events.attach++;
        if (hooks.attach) await hooks.attach(target, version);
      },
      sendCommand: async (target, method, params) => {
        events.sendCommand.push(method);
        if (hooks.sendCommand) return hooks.sendCommand(target, method, params);
        return {};
      },
      detach: async (target) => {
        events.detach++;
        if (hooks.detach) await hooks.detach(target);
      },
      onDetach: { addListener() {} },
      onEvent: { addListener() {} },
    },
    cookies: { getAll: async () => [] },
  };

  const sandbox = {
    chrome,
    console,
    setTimeout, clearTimeout, setInterval, clearInterval,
    atob: (s) => Buffer.from(s, "base64").toString("binary"),
    btoa: (s) => Buffer.from(s, "binary").toString("base64"),
    Blob,
    createImageBitmap: async () => ({ ...(hooks.bitmap ? hooks.bitmap() : { width: 100, height: 50 }) }),
    OffscreenCanvas: class {
      constructor(w, h) { this.width = w; this.height = h; this.draws = []; }
      getContext() {
        const self = this;
        return { drawImage: (...a) => self.draws.push(a) };
      }
      async convertToBlob() {
        return { arrayBuffer: async () => new Uint8Array([1, 2, 3, 4, 5, 6, 7, 8]).buffer };
      }
    },
    WebSocket: class { constructor() {} close() {} send() {} },
  };
  if (hooks.globals) Object.assign(sandbox, hooks.globals);
  sandbox.globalThis = sandbox;
  sandbox.window = sandbox;
  vm.createContext(sandbox);
  sandbox.importScripts = (name) => {
    vm.runInContext(fs.readFileSync(path.join(DIR, name), "utf8"), sandbox, { filename: name });
  };
  vm.runInContext(fs.readFileSync(path.join(DIR, "frames.js"), "utf8"), sandbox, { filename: "frames.js" });
  vm.runInContext(fs.readFileSync(path.join(DIR, "background.js"), "utf8"), sandbox, { filename: "background.js" });
  return { sandbox, events };
}

// Default scripting hook: files[] injection is a no-op success; a two-arg
// __bashyPage call routes to the fake page model; anything else answers the
// viewport / settle probes the capture path makes.
function scriptingHook(page, failure) {
  return async (opts) => {
    if (opts.files) return [{}];
    const args = opts.args || [];
    if (args.length === 2 && typeof args[0] === "string") {
      const [fn, params] = args;
      if (failure && failure[fn]) throw new Error(failure[fn]);
      return [{ result: page[fn](params) }];
    }
    if (args.length === 1) return [{ result: true }]; // settle barrier
    return [{ result: { width: 100, height: 50, dpr: 1 } }]; // viewportOf
  };
}

const TALL = {
  vw: 100, vh: 50, docW: 100, docH: 300, winX: 0, winY: 120,
  panels: [
    { x: 0, y: 40, w: 100, h: 30, sh: 120, pst: 17 },
    { x: 0, y: 200, w: 100, h: 30, sh: 90, pst: 5 },
  ],
};

function withTimeoutGuard(p, ms, label) {
  let timer;
  const bell = new Promise((_, reject) => {
    timer = setTimeout(() => reject(new Error(label + ": hung (never settled)")), ms);
  });
  return Promise.race([p, bell]).finally(() => clearTimeout(timer));
}

function shotSets(page) {
  return page.state.calls.filter((c) => c.fn === "shotSet").map((c) => c.arg);
}

function restores(page) {
  return page.state.calls.filter((c) => c.fn === "shotRestore");
}

// ---------------------------------------------------------------------------
// 1. Debugger-acquire failure restores the layout and releases nothing.
// ---------------------------------------------------------------------------
test("attach failure restores the hidden page and holds no debugger ref", async () => {
  const page = makePageModel(TALL);
  const { sandbox, events } = loadExtension({
    scripting: scriptingHook(page),
    attach: async () => { throw new Error("No debugger target"); },
  });
  await assert.rejects(sandbox.stitchFullPage(7), /No debugger target/);
  assert.equal(restores(page).length, 1, "layout must be restored even when attach fails");
  const saved = restores(page)[0].arg;
  assert.equal(saved.y, 120, "restore carries the saved nonzero window scroll, not 0");
  assert.equal(saved.panels[0].st, 17, "restore carries the saved panel scroll, not 0");
  assert.equal(page.state.bannerHidden, false, "fixed banner is reshown");
  assert.equal(events.detach, 0, "an unheld attach must not be released");
});

// ---------------------------------------------------------------------------
// 2. A capture that hangs past the stitch budget settles promptly once
// cancelled: no further scroll/capture mutations, positions restored,
// and the debugger ref balanced.
// ---------------------------------------------------------------------------
test("timed-out stitch stops mutating and restores before takeScreenshot returns", async () => {
  const page = makePageModel(TALL);
  let captures = 0;
  const { sandbox, events } = loadExtension({
    globals: { __bashyStitchTimeoutMs: 150 },
    scripting: scriptingHook(page),
    sendCommand: async (target, method) => {
      if (method !== "Page.captureScreenshot") return {};
      captures++;
      if (captures === 1) return { data: "AA==" };
      return new Promise(() => {}); // second tile hangs forever
    },
    bitmap: () => ({ width: 100, height: 50 }),
  });
  const shot = withTimeoutGuard(
    sandbox.takeScreenshot(7, { full_page: true, settle_ms: 0 }), 5000, "takeScreenshot");
  await assert.rejects(shot, /stitch/i);
  const sets = shotSets(page);
  assert.ok(sets.length <= 3, `no scroll mutations after cancel (shotSets: ${sets.length})`);
  assert.equal(restores(page).length, 1, "exactly one restore runs before return");
  assert.equal(page.state.winY, 120, "window scroll restored to the saved nonzero offset");
  assert.equal(page.state.panels[0].pst, 17, "panel scroll restored to the saved nonzero offset");
  assert.equal(page.state.bannerHidden, false, "fixed banner is reshown after timeout");
  assert.equal(events.attach, events.detach, "every debugger attach is released");
});

// ---------------------------------------------------------------------------
// 3. A mid-stitch tile failure is an explicit error, never a
// full_page:true fallback missing panel rows.
// ---------------------------------------------------------------------------
test("failed stitch is an explicit error, not a silent full-page success", async () => {
  const page = makePageModel(TALL);
  let captures = 0;
  const { sandbox } = loadExtension({
    globals: { __bashyStitchTimeoutMs: 10000 },
    scripting: scriptingHook(page),
    sendCommand: async (target, method) => {
      if (method !== "Page.captureScreenshot") return {};
      captures++;
      // Only the stitch's second tile fails; the single-capture fallback
      // WOULD succeed — so resolving here proves the silent downgrade.
      if (captures === 2) throw new Error("Target crashed");
      return { data: "AA==" };
    },
    bitmap: () => ({ width: 100, height: 50 }),
  });
  await assert.rejects(
    withTimeoutGuard(sandbox.takeScreenshot(7, { full_page: true, settle_ms: 0 }), 5000, "takeScreenshot"),
    /full_page stitch failed.*Target crashed/,
    "the caller sees the failure instead of a downgraded full_page:true image",
  );
  assert.equal(restores(page).length, 1, "the failed stitch still restores");
  assert.equal(page.state.winY, 120, "window scroll restored after failure");
});

// ---------------------------------------------------------------------------
// 4. Tall page, nonzero panels: the window stays pinned on each panel box
// (never p.y + content offset) and everything is restored on success.
// ---------------------------------------------------------------------------
test("panel tiling pins the window on the panel box and restores nonzero positions", async () => {
  const page = makePageModel(TALL);
  const { sandbox, events } = loadExtension({
    globals: { __bashyStitchTimeoutMs: 10000 },
    scripting: scriptingHook(page),
    sendCommand: async () => ({ data: "AA==" }),
    bitmap: () => ({ width: 100, height: 50 }),
  });
  const out = await withTimeoutGuard(
    sandbox.takeScreenshot(7, { full_page: true, settle_ms: 0 }), 5000, "takeScreenshot");
  assert.ok(out && out.image, "stitched capture resolves an image");
  const info = JSON.parse(out.data);
  assert.equal(info.capture, "stitched_full_page");
  assert.equal(info.panels, 2);
  // Window rows for panel tiles: clamped panel-box rows
  // (min(max(p.y,0), docH-vh)), i.e. 40 and 200 — never p.y + st.
  const panelSets = shotSets(page).filter((s) => s.p >= 0);
  assert.ok(panelSets.length > 0, "panel tiles were captured");
  for (const s of panelSets) {
    const box = TALL.panels[s.p];
    const want = Math.min(Math.max(box.y, 0), TALL.docH - TALL.vh);
    assert.equal(s.y, want, `panel ${s.p} window row ${s.y}, want pinned ${want}`);
  }
  assert.equal(page.state.winY, 120, "window scroll restored to nonzero");
  assert.equal(page.state.panels[0].pst, 17, "panel 0 scroll restored to nonzero");
  assert.equal(page.state.panels[1].pst, 5, "panel 1 scroll restored to nonzero");
  assert.equal(page.state.bannerHidden, false, "fixed banner is reshown");
  assert.equal(events.attach, events.detach, "debugger attach balanced on success");
});

// ---------------------------------------------------------------------------
// 5. A late attach completion releases only its own ref.
// ---------------------------------------------------------------------------
test("late attach completion after cancel releases only its own ref", async () => {
  const page = makePageModel(TALL);
  let releaseAttach;
  const attachGate = new Promise((r) => { releaseAttach = r; });
  const { sandbox, events } = loadExtension({
    globals: { __bashyStitchTimeoutMs: 150 },
    scripting: scriptingHook(page),
    attach: () => attachGate, // hangs until the test releases it
    sendCommand: async (target, method) => {
      if (method !== "Page.captureScreenshot") return {}; // e.g. Page.enable
      return new Promise(() => {}); // tiles hang: the timeout must still win
    },
  });
  const shot = withTimeoutGuard(
    sandbox.takeScreenshot(7, { full_page: true, settle_ms: 0 }), 5000, "takeScreenshot");
  await assert.rejects(shot, /stitch/i);
  const detachesBefore = events.detach;
  releaseAttach(); // the abandoned attach completes late
  await new Promise((r) => setTimeout(r, 100));
  assert.equal(events.detach, detachesBefore + 1, "exactly the late completion's own ref is released");
  assert.equal(restores(page).length, 1, "one restore, no double restore from the late attach");
});

// ---------------------------------------------------------------------------
// 6. No panels: stitch reports null and restores the layout hiding.
// ---------------------------------------------------------------------------
test("panel-free page stitches null and unhides fixed elements", async () => {
  const page = makePageModel({ vw: 100, vh: 50, docW: 100, docH: 300, winY: 60, panels: [] });
  const { sandbox, events } = loadExtension({ scripting: scriptingHook(page) });
  const out = await sandbox.stitchFullPage(7);
  assert.equal(out, null, "nothing to stitch");
  assert.equal(restores(page).length, 1, "the layout's fixed-element hiding is undone");
  assert.equal(page.state.bannerHidden, false);
  assert.equal(events.attach, 0, "no debugger attach without panels");
  assert.equal(events.detach, 0);
});
