// The common Genie button (Sprint 526.B1g): one protected component the
// console injects into every shaped app page it serves — its own launcher
// pages, every registered shaped app (static or server), and candidate
// previews. It draws the sparkle button and opens the wish panel as an
// overlay on the current page; the wish targets the APP (the shaping unit)
// and carries the page and, optionally, an element the person points at.
//
// Everything it needs arrives on its own <script> tag (genie.go writes it):
//   data-base      the launcher's base path, trailing slash (the API lives there)
//   data-app       the shaped app the wish targets
//   data-page      the page/route the person is on, relative to the app
//   data-mode      live | protected | preview
//   data-position  top-right (default) | top-left | bottom-right | bottom-left
//   data-anchor    CSS selector list; the button goes inside the first match
//   data-hidden    "1" keeps the button off this page (landing page excepted)
//   data-landing   "1" on the app's landing page
//   data-candidate the candidate being previewed (preview mode): reviewed in place
//   data-mount     the app's live mount under data-base ("" for the launcher)
//
// In preview mode the component is the in-place review: a bar pinned on top
// of the candidate's own pages carries the wish, "Reviewing candidate <id>",
// what changed in plain words (the agent's coverage list and the changed
// files), Approve (the server runs the trusted verifier first and activates
// only on pass; a refusal shows its reason here), Reject and Back; the
// elements that differ from the live page are outlined (best effort), a
// toggle flips before/after, and the code diff is one click away. The tab's
// session storage remembers the review, so the live page the before-toggle
// shows and a protected page reached from the review both say so.
//
// Plain JS, no framework, no CDN, no dependency on the host page's styles:
// the look is the launcher's design system, carried here so it is the same
// on every app and every OS. The keyboard shortcut (Alt+Shift+G) opens the
// panel even where the button is hidden.
(() => {
  "use strict";
  const me = document.currentScript;
  if (!me || window.__bashyGenie) return;
  window.__bashyGenie = true;

  const d = me.dataset;
  const base = d.base || "/";
  const app = d.app || "";
  const page = d.page || "/";
  const mode = d.mode === "protected" || d.mode === "preview" ? d.mode : "live";
  const corners = { "top-right": "tr", "top-left": "tl", "bottom-right": "br", "bottom-left": "bl" };
  const corner = corners[d.position] || "tr";
  const anchor = d.anchor || "";
  const hidden = d.hidden === "1" && d.landing !== "1";
  const candidate = mode === "preview" ? (d.candidate || "") : "";
  const mount = d.mount || "";
  const SHORTCUT = "Alt+Shift+G";

  const sparkle = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' +
    '<path d="M12 3l1.8 4.6L18.4 9l-4.6 1.8L12 15.4l-1.8-4.6L5.6 9l4.6-1.4z"/>' +
    '<path d="M5 17l.9 2.1L8 20l-2.1.9L5 23l-.9-2.1L2 20l2.1-.9z"/></svg>';

  const css = `
.bgn{--g-bg:#fff;--g-fg:#18181b;--g-muted:#71717a;--g-border:#e4e4e7;--g-primary:#2563eb;--g-primary-fg:#fff;--g-warn:#b45309;--g-shade:rgba(0,0,0,.45);
  font:14px/1.45 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;color:var(--g-fg);box-sizing:border-box}
.bgn.dark{--g-bg:#18181b;--g-fg:#fafafa;--g-muted:#a1a1aa;--g-border:#27272a;--g-primary:#60a5fa;--g-warn:#fbbf24;--g-shade:rgba(0,0,0,.6)}
.bgn *,.bgn *::before,.bgn *::after{box-sizing:inherit}
.bgn-btn{width:36px;height:36px;border:0;background:var(--g-bg);color:var(--g-fg);border-radius:.375rem;cursor:pointer;
  display:inline-flex;align-items:center;justify-content:center;padding:0;margin:0;line-height:1;
  box-shadow:0 1px 2px rgba(0,0,0,.08);border:1px solid var(--g-border)}
.bgn-btn:hover,.bgn-btn:focus-visible{color:var(--g-primary);outline:none;box-shadow:0 0 0 2px var(--g-primary)}
.bgn-btn svg{width:18px;height:18px}
.bgn-btn.bgn-off{opacity:.45;cursor:not-allowed}
.bgn-inline .bgn-btn{background:transparent;border-color:transparent;box-shadow:none}
.bgn-inline .bgn-btn:hover{background:var(--g-bg)}
.bgn-corner{position:fixed;z-index:2147483000}
.bgn-corner.bgn-tr{top:.6rem;right:.6rem}.bgn-corner.bgn-tl{top:.6rem;left:.6rem}
.bgn-corner.bgn-br{bottom:.6rem;right:.6rem}.bgn-corner.bgn-bl{bottom:.6rem;left:.6rem}
.bgn-overlay{position:fixed;inset:0;z-index:2147483001;background:var(--g-shade);display:flex;align-items:flex-start;justify-content:center;padding:4rem 1rem 1rem}
.bgn-overlay[hidden]{display:none}
.bgn-sheet{background:var(--g-bg);color:var(--g-fg);border:1px solid var(--g-border);border-radius:1rem;width:min(34rem,100%);
  box-shadow:0 12px 36px rgba(20,30,60,.25);padding:1rem 1.1rem;display:grid;gap:.6rem}
.bgn-sheet h2{margin:0;font-size:1.05rem;display:flex;align-items:center;gap:.4rem}
.bgn-sheet h2 svg{width:18px;height:18px;color:var(--g-primary)}
.bgn-sheet h2 b{font-weight:600}
.bgn-ctx,.bgn-note,.bgn-status{color:var(--g-muted);font-size:.8rem}
.bgn-note{color:var(--g-warn)}
.bgn-ctx code{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:.78rem;background:rgba(127,127,127,.12);padding:0 .25rem;border-radius:.25rem}
.bgn-chip{display:inline-flex;align-items:center;gap:.3rem;border:1px solid var(--g-border);border-radius:999px;padding:0 .5rem;max-width:100%}
.bgn-chip button{border:0;background:none;color:var(--g-muted);cursor:pointer;font:inherit;padding:0 .1rem}
.bgn-sheet textarea{width:100%;min-height:5.5rem;font:inherit;color:inherit;background:var(--g-bg);border:1px solid var(--g-border);border-radius:.5rem;padding:.5rem;resize:vertical}
.bgn-sheet textarea:focus{outline:none;box-shadow:0 0 0 2px var(--g-primary)}
.bgn-row{display:flex;align-items:center;gap:.5rem;flex-wrap:wrap}
.bgn-row .bgn-sp{flex:1}
.bgn-b{font:inherit;font-size:.82rem;padding:.3rem .75rem;border-radius:.45rem;border:1px solid var(--g-border);background:var(--g-bg);color:var(--g-fg);cursor:pointer}
.bgn-b.bgn-p{background:var(--g-primary);color:var(--g-primary-fg);border-color:transparent}
.bgn-b:disabled{opacity:.55;cursor:default}
.bgn-sheet a{color:var(--g-primary)}
.bgn-pick{position:fixed;z-index:2147483002;pointer-events:none;border:2px solid var(--g-primary);border-radius:.25rem;background:rgba(37,99,235,.12)}
.bgn-picking,.bgn-picking *{cursor:crosshair!important}
.bgn-toast{position:fixed;left:50%;bottom:1.2rem;transform:translateX(-50%);z-index:2147483003;background:var(--g-bg);color:var(--g-fg);
  border:1px solid var(--g-border);border-radius:.5rem;padding:.45rem .8rem;box-shadow:0 6px 20px rgba(0,0,0,.2);font-size:.85rem}
.bgn-bar{position:fixed;top:0;left:0;right:0;z-index:2147482000;background:var(--g-bg);color:var(--g-fg);border-bottom:1px solid var(--g-border);
  box-shadow:0 2px 10px rgba(0,0,0,.12);padding:.4rem .8rem;display:grid;gap:.25rem}
.bgn-bar .bgn-row{gap:.4rem}
.bgn-bar .bgn-tag{font-size:.72rem;text-transform:uppercase;letter-spacing:.06em;color:var(--g-muted);white-space:nowrap}
.bgn-bar code{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:.78rem;background:rgba(127,127,127,.12);padding:0 .25rem;border-radius:.25rem}
.bgn-bar .bgn-wish{font-weight:600;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;max-width:min(42ch,40vw)}
.bgn-bar .bgn-chip{font-size:.72rem;text-transform:uppercase;letter-spacing:.06em;color:var(--g-muted)}
.bgn-bar .bgn-chip.verified{color:#16a34a}
.bgn-bar .bgn-sum{font-size:.8rem;color:var(--g-muted);overflow-wrap:anywhere}
.bgn-bar .bgn-sum b{color:var(--g-fg);font-weight:600}
.bgn-bar .bgn-alert{font-size:.8rem;color:#dc2626;overflow-wrap:anywhere}
.bgn-bar .bgn-b[aria-pressed="true"]{box-shadow:inset 0 0 0 2px var(--g-primary)}
.bgn-bar .bgn-b.bgn-danger{color:#dc2626}
.bgn-changed{outline:2px dashed #2563eb!important;outline-offset:2px}
.bgn-added{outline:2px dashed #16a34a!important;outline-offset:2px}
.bgn-wide{width:min(64rem,100%)}
.bgn-diff{max-height:70vh;overflow:auto;font-size:.78rem}
.bgn-diff pre{margin:0;padding:.5rem;white-space:pre;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;background:rgba(127,127,127,.1);border-radius:.4rem}
.bgn-diff .add{color:#16a34a}.bgn-diff .del{color:#dc2626}.bgn-diff .hunk{color:var(--g-primary)}.bgn-diff .file{font-weight:600}
@media (max-width:640px){.bgn-overlay{padding:1rem .5rem}.bgn-sheet{padding:.8rem}.bgn-bar .bgn-wish{max-width:100%}}`;

  const el = (tag, cls, text) => {
    const n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text !== undefined) n.textContent = text;
    return n;
  };
  const abs = (p) => new URL(p, location.href);
  const api = (rest) => abs(base + "api/shape/" + encodeURIComponent(app) + "/candidates" + (rest || ""));
  const reviewURL = () => abs(base + "shape/" + encodeURIComponent(app) + "/").href;
  // liveURL is the live counterpart of a page of this app; previewURL the
  // same page inside the preview of a candidate.
  const liveURL = (p) => abs(base + mount + (p || page).replace(/^\//, "")).href;
  const previewPrefix = (ofApp, id) => base + "shape/" + encodeURIComponent(ofApp) + "/preview/" + id + "/";
  const previewURL = (id, p) => abs(previewPrefix(app, id) + (p || page).replace(/^\//, "")).href;

  // The tab remembers which candidate is under review, so the live page
  // the before/after toggle shows, and a protected page reached from the
  // review, can say so. Per tab, two hours, cleared when the review ends.
  const MARK = "bashy.genie.review";
  function marker() {
    try {
      const m = JSON.parse(sessionStorage.getItem(MARK) || "null");
      if (!m || !m.id || !m.app || Date.now() - (m.at || 0) > 2 * 3600e3) return null;
      return m;
    } catch (_) { return null; }
  }
  function remember(m) {
    try {
      if (m) sessionStorage.setItem(MARK, JSON.stringify(Object.assign({}, m, { at: Date.now() })));
      else sessionStorage.removeItem(MARK);
    } catch (_) {}
  }

  // The look follows the host page's theme where it says one (the launcher
  // and the desk tools set data-theme on <html>), and the OS otherwise.
  function dark() {
    const root = document.documentElement;
    const t = root.getAttribute("data-theme") || root.dataset.deskTheme || "";
    if (t === "dark") return true;
    if (t === "light") return false;
    if (root.classList.contains("dark")) return true;
    return !!(window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches);
  }
  const theme = (n) => { n.classList.toggle("dark", dark()); return n; };

  const style = el("style");
  style.textContent = css;
  (document.head || document.documentElement).append(style);

  let toastTimer = 0;
  function toast(msg) {
    let t = document.querySelector(".bgn-toast");
    if (!t) { t = el("div", "bgn bgn-toast"); t.setAttribute("role", "status"); document.body.append(t); }
    theme(t);
    t.textContent = msg;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => t.remove(), 3200);
  }

  // ---- the button --------------------------------------------------------
  let btn = null;
  function placeButton() {
    if (hidden) return;
    btn = el("button", "bgn-btn");
    btn.type = "button";
    btn.innerHTML = sparkle;
    btn.setAttribute("data-bashy-genie-btn", mode);
    if (mode === "preview") {
      btn.classList.add("bgn-off");
      btn.title = "You're previewing a candidate";
      btn.setAttribute("aria-label", "Genie is off while you preview a candidate");
      btn.setAttribute("aria-disabled", "true");
    } else {
      btn.title = "Make a wish for " + app + " (" + SHORTCUT + ")";
      btn.setAttribute("aria-label", "Make a wish for " + app);
    }
    btn.addEventListener("click", toggle);
    // While a review bar is shown the button is the bar's icon; otherwise
    // it goes inside the first anchor that matches, or in the corner.
    let host = barSlot;
    for (const sel of host ? [] : anchor.split(",").map((s) => s.trim()).filter(Boolean)) {
      try { host = document.querySelector(sel); } catch (_) { host = null; }
      if (host) break;
    }
    const wrap = el("span", "bgn " + (host ? "bgn-inline" : "bgn-corner bgn-" + corner));
    wrap.append(btn);
    theme(wrap);
    if (host) host.append(wrap);
    else { document.body.append(wrap); nudge(wrap); window.addEventListener("resize", () => nudge(wrap)); }
  }

  // A top corner is often where a page's own sticky bar keeps its controls;
  // the button must not cover them. When the corner is occupied by a header,
  // the button moves just below it — host-agnostic, no per-app knowledge.
  function nudge(wrap) {
    if (corner !== "tr" && corner !== "tl") return;
    wrap.style.top = "";
    const r = wrap.getBoundingClientRect();
    const under = document.elementsFromPoint(r.left + r.width / 2, r.top + r.height / 2)
      .filter((e) => e !== wrap && !wrap.contains(e) && e !== document.body && e !== document.documentElement);
    const bar = under.length ? under[0].closest("header, nav, [role=banner], .desk-appbar, #bar, .bgn-bar") : null;
    if (bar && !bar.contains(wrap)) wrap.style.top = Math.round(bar.getBoundingClientRect().bottom + 8) + "px";
  }

  // ---- the wish panel ----------------------------------------------------
  let overlay = null, ta = null, status = null, send = null, chip = null, element = "";
  let lastFocus = null;

  function build() {
    overlay = el("div", "bgn bgn-overlay");
    overlay.hidden = true;
    overlay.setAttribute("role", "dialog");
    overlay.setAttribute("aria-modal", "true");
    overlay.setAttribute("aria-labelledby", "bgn-title");
    overlay.setAttribute("data-bashy-genie-panel", "");
    const sheet = el("div", "bgn-sheet");
    const h = el("h2");
    h.id = "bgn-title";
    h.innerHTML = sparkle;
    h.append(el("span", "", "What should change in "), el("b", "", app), el("span", "", "?"));
    const ctx = el("div", "bgn-ctx");
    ctx.append(el("span", "", "Page "), Object.assign(el("code", "", page), {}), el("span", "", " · "));
    chip = el("span", "bgn-chip");
    chip.hidden = true;
    ctx.append(chip);
    const hint = el("span", "", "The wish is for the whole app; the page and a pointed-at element are context.");
    ctx.append(hint);
    sheet.append(h, ctx);
    if (mode === "protected") {
      sheet.append(el("div", "bgn-note", "This page is protected: sign-in, pairing, the Shape page, the terminal and accounts are never shaped. " +
        "Your wish is filed as a request for the maintainers instead of building a candidate."));
    }
    ta = el("textarea");
    ta.placeholder = mode === "protected" ? "e.g. a back button on this page" : "e.g. show the newest item first";
    ta.setAttribute("aria-label", "Your wish");
    sheet.append(ta);
    const row = el("div", "bgn-row");
    const pick = el("button", "bgn-b", "Point at an element");
    pick.type = "button";
    pick.addEventListener("click", startPick);
    status = el("span", "bgn-status");
    status.setAttribute("role", "status");
    status.setAttribute("aria-live", "polite");
    const cancel = el("button", "bgn-b", "Cancel");
    cancel.type = "button";
    cancel.addEventListener("click", close);
    send = el("button", "bgn-b bgn-p", mode === "protected" ? "File request" : "Make the wish");
    send.type = "button";
    send.addEventListener("click", submit);
    row.append(pick, el("span", "bgn-sp"), status, cancel, send);
    sheet.append(row);
    const foot = el("div", "bgn-ctx");
    const link = el("a", "", "Wishes & versions");
    link.href = reviewURL();
    foot.append(link, el("span", "", " · " + SHORTCUT + " opens this panel anywhere in the app"));
    sheet.append(foot);
    overlay.append(sheet);
    overlay.addEventListener("click", (e) => { if (e.target === overlay) close(); });
    document.body.append(overlay);
  }

  function setElement(desc) {
    element = desc || "";
    chip.replaceChildren();
    chip.hidden = !element;
    if (!element) return;
    const x = el("button", "", "×");
    x.type = "button";
    x.setAttribute("aria-label", "Forget the pointed-at element");
    x.addEventListener("click", () => setElement(""));
    chip.append(el("span", "", "pointing at "), el("code", "", element), x);
  }

  function open() {
    if (mode === "preview") { toast("You're previewing a candidate — wishes start from the live app."); return; }
    if (!overlay) build();
    theme(overlay);
    lastFocus = document.activeElement;
    overlay.hidden = false;
    status.textContent = "";
    ta.focus();
  }
  function close() {
    if (!overlay || overlay.hidden) return;
    overlay.hidden = true;
    if (lastFocus && lastFocus.focus) lastFocus.focus();
  }
  function toggle() { (overlay && !overlay.hidden) ? close() : open(); }

  async function submit() {
    const request = ta.value.trim();
    if (!request) { ta.focus(); return; }
    send.disabled = true;
    status.textContent = "Sending…";
    try {
      const res = await fetch(api(), {
        method: "POST", credentials: "same-origin",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ request, page, element, protected: mode === "protected" }),
      });
      const text = await res.text();
      if (!res.ok) throw new Error(text.trim() || res.statusText);
      let c = {};
      try { c = JSON.parse(text); } catch (_) {}
      ta.value = "";
      setElement("");
      if (c.status === "requested") {
        status.textContent = "Filed as a request. ";
      } else {
        status.textContent = "Genie is on it (a few minutes). ";
      }
      const a = el("a", "", "Follow it");
      a.href = reviewURL();
      status.append(a);
    } catch (e) {
      status.textContent = e.message || "The wish could not be sent";
    }
    send.disabled = false;
  }

  // ---- pointing at an element --------------------------------------------
  // The panel steps aside, the pointer picks any element on the page, and a
  // short description (tag, id/classes, visible text) is attached as
  // context. Escape cancels.
  let box = null, picking = false;
  function describe(e) {
    const parts = [];
    let n = e;
    for (let i = 0; i < 4 && n && n !== document.body; i++) {
      let s = n.tagName.toLowerCase();
      if (n.id) { parts.unshift(s + "#" + n.id); break; }
      const cls = [...n.classList].filter((c) => !c.startsWith("bgn")).slice(0, 2).join(".");
      if (cls) s += "." + cls;
      parts.unshift(s);
      n = n.parentElement;
    }
    const text = (e.getAttribute("aria-label") || e.getAttribute("placeholder") || e.textContent || "").trim().replace(/\s+/g, " ").slice(0, 60);
    return parts.join(" > ") + (text ? ' "' + text + '"' : "");
  }
  function startPick() {
    if (picking) return;
    picking = true;
    overlay.hidden = true;
    box = el("div", "bgn bgn-pick");
    document.body.append(box);
    document.body.classList.add("bgn-picking");
    toast("Click the element your wish is about (Esc to cancel)");
    document.addEventListener("mousemove", onMove, true);
    document.addEventListener("click", onPick, true);
    document.addEventListener("keydown", onPickKey, true);
  }
  function target(ev) {
    const t = document.elementFromPoint(ev.clientX, ev.clientY);
    return t && !t.closest(".bgn") ? t : null;
  }
  function onMove(ev) {
    const t = target(ev);
    if (!t) { box.style.display = "none"; return; }
    const r = t.getBoundingClientRect();
    Object.assign(box.style, { display: "block", left: r.left + "px", top: r.top + "px", width: r.width + "px", height: r.height + "px" });
  }
  function onPick(ev) {
    ev.preventDefault();
    ev.stopPropagation();
    const t = target(ev);
    stopPick();
    if (t) setElement(describe(t));
  }
  function onPickKey(ev) {
    if (ev.key === "Escape") { ev.preventDefault(); ev.stopPropagation(); stopPick(); }
  }
  function stopPick() {
    picking = false;
    document.removeEventListener("mousemove", onMove, true);
    document.removeEventListener("click", onPick, true);
    document.removeEventListener("keydown", onPickKey, true);
    document.body.classList.remove("bgn-picking");
    if (box) { box.remove(); box = null; }
    overlay.hidden = false;
    ta.focus();
  }

  // ---- keyboard ----------------------------------------------------------
  document.addEventListener("keydown", (ev) => {
    if (ev.altKey && ev.shiftKey && !ev.ctrlKey && !ev.metaKey && ev.code === "KeyG") {
      ev.preventDefault();
      toggle();
    } else if (ev.key === "Escape" && overlay && !overlay.hidden && !picking) {
      close();
    } else if (ev.key === "Escape" && diffSheet && !diffSheet.hidden) {
      diffSheet.hidden = true;
    }
  });

  // ---- the in-place review ------------------------------------------------
  // Inside a preview, a bar pinned on top of the candidate's own page; on a
  // live page of the same app while a review is open (the before/after
  // toggle, or a link that left the preview), and on a protected page
  // reached from a review, a note that says so. The sparkle button sits in
  // the bar's left slot while a bar is shown.
  let barSlot = null, bar = null, diffSheet = null, diffHost = null, baseMargin = null;
  const bodyMargin = () => {
    if (baseMargin === null) baseMargin = parseFloat(getComputedStyle(document.body).marginTop) || 0;
    return baseMargin;
  };

  // pin keeps the page readable under a fixed bar: the body moves down by
  // the bar's height, and any sticky/fixed top bar of the host page (the
  // launcher's header, a desk app bar) sticks below ours instead of under it.
  function pin(b) {
    document.body.prepend(b);
    const fit = () => {
      const h = b.offsetHeight;
      document.body.style.marginTop = (bodyMargin() + h) + "px";
      for (const e of document.querySelectorAll("header, nav, [role=banner], .desk-appbar, #bar")) {
        if (e.closest(".bgn")) continue;
        const cs = getComputedStyle(e);
        if ((cs.position === "sticky" || cs.position === "fixed") && (e.hasAttribute("data-bgn-top") || cs.top === "0px")) {
          e.setAttribute("data-bgn-top", "1");
          e.style.top = h + "px";
        }
      }
    };
    fit();
    window.addEventListener("resize", fit);
    if (window.ResizeObserver) new ResizeObserver(fit).observe(b);
  }
  function unpin(b) {
    b.remove();
    document.body.style.marginTop = baseMargin ? baseMargin + "px" : "";
    for (const e of document.querySelectorAll("[data-bgn-top]")) { e.style.top = ""; e.removeAttribute("data-bgn-top"); }
  }
  const action = (label, cls, fn, aria) => {
    const b = el("button", "bgn-b" + (cls ? " " + cls : ""), label);
    b.type = "button";
    if (aria) b.setAttribute("aria-label", aria);
    b.addEventListener("click", fn);
    return b;
  };

  // reviewBar: the in-place review of `candidate` on this preview page.
  function reviewBar() {
    bar = el("div", "bgn bgn-bar");
    bar.setAttribute("role", "region");
    bar.setAttribute("aria-label", "Candidate review");
    bar.setAttribute("data-bgn-review", candidate);
    theme(bar);
    const head = el("div", "bgn-row");
    barSlot = el("span", "bgn-slot");
    const tag = el("span", "bgn-tag", "Reviewing candidate ");
    tag.append(el("code", "", candidate));
    const wish = el("b", "bgn-wish", "Loading the wish…");
    const chip = el("span", "bgn-chip");
    head.append(barSlot, tag, wish, chip, el("span", "bgn-sp"));
    const sum = el("div", "bgn-sum");
    const alertEl = el("div", "bgn-alert");
    alertEl.setAttribute("role", "alert");
    alertEl.hidden = true;
    const say = (msg) => { alertEl.textContent = msg || ""; alertEl.hidden = !msg; };
    const busy = el("span", "bgn-status");
    busy.setAttribute("role", "status");
    busy.setAttribute("aria-live", "polite");

    let outlined = true, marks = [];
    const outlineBtn = action("Outline changes", "", () => {
      outlined = !outlined;
      outlineBtn.setAttribute("aria-pressed", String(outlined));
      for (const [n, cls] of marks) n.classList.toggle(cls, outlined);
    }, "Outline the elements that differ from your current version");
    outlineBtn.setAttribute("aria-pressed", "true");
    const before = action("Before", "", () => {
      remember({ app, id: candidate, page, compare: true });
      location.assign(liveURL());
    }, "Show your current version of this page instead of the candidate");
    const diffBtn = action("Diff", "", () => showDiff(), "Show the code diff of this candidate");
    const back = action("Back", "", () => { remember(null); location.assign(reviewURL()); }, "Back to the wishes and versions of " + app);
    let armed = 0;
    const reject = action("Reject", "bgn-danger", async () => {
      if (!armed) {
        armed = setTimeout(() => { armed = 0; reject.textContent = "Reject"; }, 5000);
        reject.textContent = "Reject — sure?";
        return;
      }
      clearTimeout(armed); armed = 0;
      setBusy(true, "Rejecting…");
      try {
        const res = await fetch(api("/" + candidate + "/reject"), { method: "POST", credentials: "same-origin" });
        if (!res.ok) throw new Error((await res.text()).trim() || res.statusText);
        remember(null);
        toast("Candidate rejected. Back to your current version.");
        location.assign(liveURL());
      } catch (e) { setBusy(false); say("Could not reject: " + e.message); }
    }, "Reject this candidate");
    const approve = action("Approve", "bgn-p", async () => {
      setBusy(true, "Verifying with the trusted verifier…");
      say("");
      try {
        const res = await fetch(api("/" + candidate + "/approve"), { method: "POST", credentials: "same-origin" });
        const text = await res.text();
        if (res.ok) {
          remember(null);
          toast("Approved: this is now your active version of " + app + ".");
          location.assign(liveURL());
          return;
        }
        let reason = text.trim();
        try { const j = JSON.parse(text); reason = (j.verdict && j.verdict.detail) || j.error || reason; } catch (_) {}
        if (res.status === 409 && !reason) reason = "the trusted verifier refused this candidate";
        say("Not approved: " + (reason || res.statusText));
        load(true);
      } catch (e) { say("Not approved: " + e.message); }
      setBusy(false);
    }, "Approve this candidate: verify it, then make it your active version");
    const buttons = [outlineBtn, before, diffBtn, back, reject, approve];
    head.append(busy, ...buttons);
    bar.append(head, sum, alertEl);
    function setBusy(on, msg) {
      for (const b of buttons) b.disabled = on;
      busy.textContent = on ? (msg || "") : "";
    }

    let diffText = null;
    async function loadDiff() {
      if (diffText !== null) return diffText;
      try {
        const res = await fetch(api("/" + candidate + "/diff"), { credentials: "same-origin" });
        diffText = res.ok ? await res.text() : "";
      } catch (_) { diffText = ""; }
      return diffText;
    }
    async function showDiff() {
      if (!diffSheet) {
        diffSheet = el("div", "bgn bgn-overlay");
        diffSheet.hidden = true;
        diffSheet.setAttribute("role", "dialog");
        diffSheet.setAttribute("aria-label", "Code diff");
        diffSheet.setAttribute("data-bgn-diff", "");
        const sheet = el("div", "bgn-sheet bgn-wide");
        const h = el("h2");
        h.innerHTML = sparkle;
        h.append(el("span", "", "Code diff of candidate "), el("code", "", candidate));
        diffHost = el("div", "bgn-diff");
        const row = el("div", "bgn-row");
        row.append(el("span", "bgn-sp"), action("Close", "", () => { diffSheet.hidden = true; }));
        sheet.append(h, diffHost, row);
        diffSheet.append(sheet);
        diffSheet.addEventListener("click", (e) => { if (e.target === diffSheet) diffSheet.hidden = true; });
        document.body.append(diffSheet);
      }
      theme(diffSheet);
      diffHost.textContent = "Loading…";
      diffSheet.hidden = false;
      const html = await loadDiff();
      // The fragment is rendered and escaped by the console's own API.
      if (html) diffHost.innerHTML = html;
      else diffHost.textContent = "The diff is not available.";
    }

    // filesOf lists the files a rendered diff touches, from its file lines.
    function filesOf(html) {
      const tmp = el("div");
      tmp.innerHTML = html;
      const out = [];
      for (const m of (tmp.textContent || "").matchAll(/^diff --git a\/\S+ b\/(\S+)$/gm)) if (!out.includes(m[1])) out.push(m[1]);
      return out;
    }

    // load fills the bar from the candidate record; keep leaves a message
    // the bar is already showing (a fresh refusal) in place.
    async function load(keep) {
      let c = null, gone = false;
      try {
        const res = await fetch(api("/" + candidate), { credentials: "same-origin" });
        if (res.status === 404) gone = true;
        else if (res.ok) c = await res.json();
      } catch (_) {}
      if (gone) {
        wish.textContent = "This candidate no longer exists";
        chip.textContent = "";
        sum.replaceChildren(el("span", "", "It was approved or rejected elsewhere. "), Object.assign(el("a", "", "Open the live app"), { href: liveURL() }));
        for (const b of [outlineBtn, before, diffBtn, reject, approve]) b.disabled = true;
        remember(null);
        return;
      }
      if (!c) { wish.textContent = "The candidate could not be loaded"; return; }
      wish.textContent = c.request || "";
      wish.title = c.request || "";
      chip.textContent = c.status || "";
      chip.className = "bgn-chip " + (c.status || "");
      const parts = [];
      const cov = c.coverage || {};
      if (cov.covered && cov.covered.length) parts.push(["Does", cov.covered.join("; ")]);
      if (cov.not_covered && cov.not_covered.length) parts.push(["Not done", cov.not_covered.join("; ")]);
      const files = filesOf(await loadDiff());
      if (files.length) parts.push([files.length === 1 ? "Changes 1 file" : "Changes " + files.length + " files", files.slice(0, 6).join(", ") + (files.length > 6 ? ", …" : "")]);
      sum.replaceChildren();
      if (!parts.length) sum.append(el("span", "", "No plain-words summary from the agent; the diff shows the change."));
      parts.forEach(([k, v], i) => {
        if (i) sum.append(el("span", "", " · "));
        sum.append(el("b", "", k + ": "), el("span", "", v));
      });
      if (c.verdict && !c.verdict.pass && !keep) say("Last verification failed" + (c.verdict.detail ? ": " + c.verdict.detail : "") + ". Approve runs it again.");
      if (c.kind === "request" || !c.rev) for (const b of [approve, reject, before, diffBtn]) b.disabled = true;
    }

    // outline marks what differs from the live page, best effort: the
    // live page is fetched and parsed, elements are paired by id or by
    // their tag path, and the innermost changed and outermost added ones
    // get a dashed outline. Scripts, styles and the component's own nodes
    // are ignored; a page that renders itself from an API shows little.
    async function outline() {
      let live;
      try {
        const res = await fetch(liveURL(), { credentials: "same-origin", headers: { Accept: "text/html" } });
        if (!res.ok) return;
        live = new DOMParser().parseFromString(await res.text(), "text/html");
      } catch (_) { return; }
      const SEL = "[id],h1,h2,h3,h4,header,nav,main,footer,section,article,aside,form,fieldset,table,ul,ol,dl,button,a,p,label,input,select,textarea,img,figure,blockquote,pre";
      const ours = (n) => !!n.closest(".bgn, .bgn-bar, [data-bashy-genie]");
      const norm = (n) => {
        const c = n.cloneNode(true);
        for (const x of c.querySelectorAll("script, style, .bgn, .bgn-bar")) x.remove();
        for (const x of [c, ...c.querySelectorAll("[class]")]) {
          x.classList.remove("bgn-changed", "bgn-added");
          if (!x.className) x.removeAttribute("class");
          x.removeAttribute("data-bgn-top");
          if (x.hasAttribute("style") && x.style.top && x.style.length === 1) x.removeAttribute("style");
        }
        if (c.tagName === "SCRIPT" || c.tagName === "STYLE") return "";
        return c.outerHTML.replace(/\s+/g, " ").trim();
      };
      const pathOf = (n, root) => {
        const parts = [];
        for (let e = n; e && e !== root && e.nodeType === 1; e = e.parentElement) {
          if (e.id) { parts.unshift("#" + e.id); break; }
          let i = 1;
          for (let s = e.previousElementSibling; s; s = s.previousElementSibling) if (s.tagName === e.tagName) i++;
          parts.unshift(e.tagName.toLowerCase() + ":" + i);
        }
        return parts.join(">");
      };
      const liveMap = new Map();
      for (const n of live.body.querySelectorAll(SEL)) {
        if (n.closest(".bgn, .bgn-bar") || n.tagName === "SCRIPT" || n.tagName === "STYLE") continue;
        const k = pathOf(n, live.body);
        if (!liveMap.has(k)) liveMap.set(k, norm(n));
      }
      const cands = [...document.body.querySelectorAll(SEL)].filter((n) => !ours(n) && n.tagName !== "SCRIPT" && n.tagName !== "STYLE").slice(0, 800);
      const differs = new Map();
      for (const n of cands) {
        const k = pathOf(n, document.body);
        differs.set(n, liveMap.has(k) ? (liveMap.get(k) !== norm(n) ? "changed" : "") : "added");
      }
      marks = [];
      for (const n of cands) {
        const d = differs.get(n);
        if (!d) continue;
        if (d === "added") {
          if (n.parentElement && n.parentElement.closest(".bgn-added")) continue;
          n.classList.add("bgn-added");
          marks.push([n, "bgn-added"]);
        } else if (!cands.some((m) => m !== n && n.contains(m) && differs.get(m))) {
          n.classList.add("bgn-changed");
          marks.push([n, "bgn-changed"]);
        }
      }
      if (marks.length) sum.append(el("span", "", " · "), el("span", "", marks.length === 1 ? "1 element differs from your current version (outlined)" : marks.length + " elements differ from your current version (outlined)"));
      else outlineBtn.disabled = true;
    }

    pin(bar);
    remember({ app, id: candidate, page, compare: false });
    load().then(outline);
  }

  // noteBar: on a live page of the app while a review is open (before/after,
  // or a link that left the preview), and on a protected page reached from
  // one: this is not the candidate, and the way back.
  function noteBar(m) {
    bar = el("div", "bgn bgn-bar");
    bar.setAttribute("role", "region");
    bar.setAttribute("aria-label", "Candidate review");
    bar.setAttribute("data-bgn-review-note", m.id);
    theme(bar);
    const row = el("div", "bgn-row");
    barSlot = el("span", "bgn-slot");
    const text = el("span", "bgn-sum");
    const id = el("code", "", m.id);
    if (mode === "protected") {
      text.append(el("b", "", "This page is protected"), el("span", "", " and is not part of candidate "), id,
        el("span", "", ": sign-in, pairing, the Shape page, the terminal and accounts are never shaped. You're seeing the live page."));
    } else if (m.compare) {
      text.append(el("b", "", "Before: your current version"), el("span", "", " of this page, compared with candidate "), id, el("span", "", "."));
    } else {
      text.append(el("b", "", "This is your current version"), el("span", "", ", not candidate "), id, el("span", "", ": you left the preview."));
    }
    const done = action("Done", "", () => { remember(null); unpin(bar); bar = null; }, "End the review note");
    const target = m.app === app ? previewURL(m.id, page) : abs(previewPrefix(m.app, m.id)).href;
    const toCand = action(m.compare && mode !== "protected" ? "After" : "Back to the candidate", "bgn-p", () => {
      remember(Object.assign({}, m, { compare: false }));
      location.assign(target);
    }, "Show candidate " + m.id);
    row.append(barSlot, text, el("span", "bgn-sp"), done, toCand);
    bar.append(row);
    pin(bar);
  }

  function start() {
    if (candidate) reviewBar();
    else {
      const m = marker();
      if (m && (mode === "protected" || (mode === "live" && m.app === app))) noteBar(m);
    }
    placeButton();
  }

  if (document.body) start();
  else document.addEventListener("DOMContentLoaded", start);
})();
