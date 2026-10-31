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
@media (max-width:640px){.bgn-overlay{padding:1rem .5rem}.bgn-sheet{padding:.8rem}}`;

  const el = (tag, cls, text) => {
    const n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text !== undefined) n.textContent = text;
    return n;
  };
  const abs = (p) => new URL(p, location.href);
  const api = () => abs(base + "api/shape/" + encodeURIComponent(app) + "/candidates");
  const reviewURL = () => abs(base + "shape/" + encodeURIComponent(app) + "/").href;

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
    let host = null;
    for (const sel of anchor.split(",").map((s) => s.trim()).filter(Boolean)) {
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
    const bar = under.length ? under[0].closest("header, nav, [role=banner], .desk-appbar, #bar") : null;
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
    }
  });

  if (document.body) placeButton();
  else document.addEventListener("DOMContentLoaded", placeButton);
})();
