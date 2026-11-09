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
//   data-mode      live | protected | preview | draft
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
// In draft mode the component is the EDIT SESSION (S526.B1h): the page is
// the person's private draft of their active version, served at
// shape/<app>/draft/, and the bar on top reads "Editing draft: N wishes ·
// undo · redo · Save · Cancel". Each wish is one stored step on the draft;
// undo and redo move through the steps without re-running Genie (a new
// wish clears the redo list), Ctrl/Cmd-Z and Ctrl/Cmd-Shift-Z work while
// the bar is shown, Save verifies the whole draft once and makes it one
// version listing every wish (a refusal names the failing part and keeps
// the draft open), Cancel discards it. The draft is on the server, so it
// survives a reload and a closed tab: a live page of the app, and a
// protected page, say an edit session is open and lead back into it.
// "Start editing" lives in the wish panel on every live page.
//
// The form's prompt is ONE fixed, generic line for every app and page —
// "What would you like to change, add or make?" — and what fits this app
// is the placeholder: example wishes the server answers at once from what
// it already has (api/shape/<app>/suggest) — the follow-ups Genie listed
// when it finished the last wish, follow-ups derived from that wish, the
// last copy shown, or the app's own from shaped.json. No model call is
// made for them. The first shows when they arrive and the rest rotate
// while the box is empty; with none, the placeholder is a neutral
// "How can I help you today?". No fixed text claims anything app-specific.
//
// There is ONE wish form and no target choice (S526.B1i): a wish that asks
// for a separate new app is understood by Genie, not by this script — the
// server answers the candidate (or the draft the wish was added to) with
// status "new_app" and the app it made, and this script follows: "Genie is
// making <label>…", wait for the wish to land as the new app's first draft
// step, then land in that draft to Save or Cancel like a document. No
// keyword is read here; the intent is Genie's answer only.
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
  const mode = ["protected", "preview", "draft"].includes(d.mode) ? d.mode : "live";
  const corners = { "top-right": "tr", "top-left": "tl", "bottom-right": "br", "bottom-left": "bl" };
  const corner = corners[d.position] || "tr";
  const anchor = d.anchor || "";
  const hidden = d.hidden === "1" && d.landing !== "1";
  const candidate = mode === "preview" ? (d.candidate || "") : "";
  const mount = d.mount || "";
  const SHORTCUT = "Alt+Shift+G";
  // The one fixed prompt of the wish form, and the placeholder until the
  // example wishes arrive, or when there are none (S526.B1g).
  const PROMPT = "What would you like to change, add or make?";
  const NEUTRAL = "How can I help you today?";
  const ROTATE_MS = 6000;

  // The Software Shaping mark ("Flow": two flattened S curves in a diagonal
  // stack), traced from bashy docs/software-shaping-design/monochrome. It
  // fills with currentColor, so it follows the light and dark themes.
  const sparkle = '<svg viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">' +
    '<path d="M15.67 1.18L14.58 1.34L13.69 1.58L12.8 1.94L11.8 2.5L10.42 3.61L7.74 6.5L6.96 7.12L6.33 7.47L5.54 7.75L4.88 7.85L4.44 7.83L3.78 7.68L2.96 7.2L2.68 7.14L2.31 7.29L1.74 7.9L1.35 8.59L1.11 9.33L1 10.14L1.04 10.58L1.21 11.33L1.43 11.86L1.84 12.48L2.36 13.04L3.06 13.54L3.57 13.79L4.31 14.01L5.03 14.11L6.4 13.96L7.18 13.72L7.93 13.39L8.9 12.79L9.85 12.01L12.09 9.61L13.79 8L14.79 7.33L15.73 6.87L16.63 6.58L17.35 6.43L18.14 6.35L19.02 6.39L20.48 6.71L21.27 7.06L22.2 7.64L22.44 7.67L22.63 7.55L22.7 7.32L22.65 6.54L22.27 5.19L21.72 4.12L20.83 3.01L19.59 2.09L18.48 1.57L17.19 1.25L16.41 1.16ZM18.37 10.05L17.61 10.15L16.77 10.39L15.81 10.84L15.05 11.32L13.82 12.31L10.61 15.69L9.24 16.8L8.04 17.45L7.26 17.69L6.24 17.81L5.07 17.68L4.23 17.45L3.54 17.16L2.08 16.25L1.76 16.3L1.67 16.48L1.65 16.89L1.86 18.09L2.3 19.21L2.76 19.99L3.54 20.95L4.16 21.51L4.9 22.02L5.66 22.4L6.69 22.72L7.54 22.84L8.81 22.81L10.2 22.47L11.2 22.01L12.53 21.13L13.15 20.61L15.55 18.15L16.56 17.37L17.4 16.91L18.53 16.53L19.54 16.41L20.51 16.52L21.62 16.93L21.96 16.88L22.18 16.72L22.46 16.35L22.75 15.72L23 14.46L22.93 13.28L22.75 12.7L22.45 12.09L22.21 11.73L21.64 11.12L20.88 10.58L20.21 10.28L19.74 10.15L18.78 10.03Z"/></svg>';

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
.bgn-prompt{font-size:.92rem}
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
.bgn-bar .bgn-count{font-weight:600;white-space:nowrap}
.bgn-bar .bgn-b.bgn-ur{min-width:2.2rem;font-size:1rem;line-height:1;padding:.2rem .4rem}
.bgn-edit{display:flex;align-items:center;gap:.5rem;flex-wrap:wrap;border-top:1px solid var(--g-border);padding-top:.6rem}
.bgn-edit .bgn-note{color:var(--g-muted)}
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
  // The edit session: its API and its pages (the draft served in place of
  // the app, same routes under the draft prefix).
  const draftAPI = (rest) => abs(base + "api/shape/" + encodeURIComponent(app) + "/draft" + (rest || ""));
  const draftURL = (p) => abs(base + "shape/" + encodeURIComponent(app) + "/draft/" + (p || page).replace(/^\//, "")).href;

  // The browser remembers that an edit session is open for this app — in
  // local storage, so it outlives the tab: a live page of the app, and a
  // protected page, then ask the server and say so. Cleared on Save and
  // Cancel, and whenever the server says there is no draft.
  const DRAFT = "bashy.genie.draft";
  function draftMark() {
    try {
      const m = JSON.parse(localStorage.getItem(DRAFT) || "null");
      return m && m.app ? m : null;
    } catch (_) { return null; }
  }
  function rememberDraft(open, ofApp) {
    const a = ofApp || app;
    try {
      if (open) localStorage.setItem(DRAFT, JSON.stringify({ app: a, at: Date.now() }));
      else if ((draftMark() || {}).app === a) localStorage.removeItem(DRAFT);
    } catch (_) {}
  }
  // draftState asks the server for the open draft of an app (this one by
  // default): null when there is none (the marker is dropped then),
  // undefined when unknown.
  async function draftState(ofApp) {
    const a = ofApp || app;
    try {
      const res = await fetch(abs(base + "api/shape/" + encodeURIComponent(a) + "/draft"), { credentials: "same-origin" });
      if (res.status === 404) { rememberDraft(false, a); return null; }
      if (!res.ok) return undefined;
      return await res.json();
    } catch (_) { return undefined; }
  }

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
  // Marked so an in-place draft refresh keeps it while swapping the app's
  // own head nodes.
  style.setAttribute("data-bgn-style", "");
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
    } else if (mode === "draft") {
      btn.title = "Add a wish to your draft of " + app + " (" + SHORTCUT + ")";
      btn.setAttribute("aria-label", "Add a wish to your draft of " + app);
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
  let editBtn = null, editNote = null;

  // refreshEditRow labels the edit button after the open draft, if any.
  async function refreshEditRow() {
    if (!editBtn) return;
    const st = draftMark() ? await draftState() : null;
    if (st) {
      editBtn.textContent = "Resume editing";
      editNote.textContent = "Your draft has " + plural(st.wishes, "wish", "wishes") + "; it is not your active version until you save.";
    } else {
      editBtn.textContent = "Start editing";
      editNote.textContent = "Many wishes in place, undo and redo, then Save or Cancel.";
    }
  }
  const plural = (n, one, many) => n + " " + (n === 1 ? one : many);

  // startEditing opens (or resumes) the edit session and moves to the
  // draft's copy of this page.
  async function startEditing() {
    editBtn.disabled = true;
    status.textContent = "Opening your draft…";
    try {
      const res = await fetch(draftAPI(), { method: "POST", credentials: "same-origin" });
      if (!res.ok) throw new Error((await res.text()).trim() || res.statusText);
      rememberDraft(true);
      location.assign(draftURL());
    } catch (e) {
      status.textContent = e.message || "The draft could not be opened";
      editBtn.disabled = false;
    }
  }

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
    // ONE fixed, generic prompt for every app and page; what fits THIS app
    // is shown as example wishes in the placeholder (loadExamples), never
    // claimed in fixed text. The draft keeps its short note.
    const prompt = el("div", "bgn-prompt", PROMPT);
    prompt.setAttribute("data-bgn-prompt", "");
    if (mode === "draft") ctx.append(el("span", "", "The wish applies on your draft so far; undo and redo from the bar."));
    sheet.append(h, prompt, ctx);
    if (mode === "protected") {
      sheet.append(el("div", "bgn-note", "This page is protected: sign-in, pairing, the Shape page, the terminal and accounts are never shaped. " +
        "Your wish is filed as a request for the maintainers instead of building a candidate — unless it asks for a new app, which Genie then makes."));
    }
    ta = el("textarea");
    ta.placeholder = NEUTRAL;
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
    send = el("button", "bgn-b bgn-p", mode === "protected" ? "File request" : mode === "draft" ? "Add the wish" : "Make the wish");
    send.type = "button";
    send.addEventListener("click", submit);
    row.append(pick, el("span", "bgn-sp"), status, cancel, send);
    sheet.append(row);
    if (mode === "live") {
      // The edit session starts here: many wishes in place, undo/redo,
      // then Save or Cancel. With a draft already open, the way back in.
      const edit = el("div", "bgn-edit");
      edit.setAttribute("data-bgn-edit", "");
      editBtn = el("button", "bgn-b", "Start editing");
      editBtn.type = "button";
      editBtn.setAttribute("aria-label", "Start an edit session: shape " + app + " like a document, with undo, redo, Save and Cancel");
      editBtn.addEventListener("click", startEditing);
      editNote = el("span", "bgn-note", "Many wishes in place, undo and redo, then Save or Cancel.");
      edit.append(editBtn, editNote);
      sheet.append(edit);
    }
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
    refreshEditRow();
    loadExamples();
  }
  function close() {
    if (!overlay || overlay.hidden) return;
    overlay.hidden = true;
    stopRotating();
    if (lastFocus && lastFocus.focus) lastFocus.focus();
  }

  // Example wishes (S526.B1g, revision): asked of the server once per
  // page when the panel first opens — GET api/shape/<app>/suggest?page=…
  // answers {"examples": [...]} at once from what it already holds: the
  // follow-ups Genie listed with the last version (or, in the edit
  // session, with the last applied step), follow-ups derived from that
  // wish, the last copy shown, or the app's declared examples. The server
  // makes no model call for them. The first becomes the placeholder, the
  // rest rotate through it while the box is empty. Nothing is prefilled
  // and nothing is sent until the person types; with no examples the
  // placeholder stays neutral.
  let examples = null, examplesPending = false, rotateTimer = 0, rotateAt = 0;
  const suggestAPI = () => abs(base + "api/shape/" + encodeURIComponent(app) + "/suggest?page=" +
    encodeURIComponent(page) + (mode === "draft" ? "&draft=1" : ""));
  async function loadExamples() {
    if (examples) { startRotating(); return; }
    if (examplesPending) return;
    examplesPending = true;
    try {
      const res = await fetch(suggestAPI(), { credentials: "same-origin" });
      if (!res.ok) throw new Error(res.statusText);
      const body = await res.json();
      const list = Array.isArray(body && body.examples) ? body.examples : [];
      examples = list.filter((x) => typeof x === "string" && x.trim()).map((x) => x.trim()).slice(0, 6);
    } catch (e) {
      examples = [];
    } finally {
      examplesPending = false;
    }
    // An empty answer is not final: a wish may land before the next open,
    // and asking again is one cheap request.
    if (!examples.length) { examples = null; return; }
    if (overlay && !overlay.hidden) startRotating();
  }
  function showExample(i) {
    if (!ta || !examples || !examples.length) return;
    rotateAt = i % examples.length;
    ta.placeholder = examples[rotateAt];
  }
  function startRotating() {
    stopRotating();
    if (!examples || !examples.length) return;
    showExample(0);
    if (examples.length < 2) return;
    rotateTimer = setInterval(() => {
      if (!overlay || overlay.hidden) { stopRotating(); return; }
      if (ta.value) return; // the person is typing: the placeholder is hidden anyway
      showExample(rotateAt + 1);
    }, ROTATE_MS);
  }
  function stopRotating() {
    if (rotateTimer) clearInterval(rotateTimer);
    rotateTimer = 0;
  }
  function toggle() { (overlay && !overlay.hidden) ? close() : open(); }

  // landInNewApp follows a "new_app" answer: the server made the app and
  // ran the wish on it as the first step of an edit session; the panel
  // says so, waits for that step, and lands in the new app's draft — the
  // result shown in place, to Save or Cancel like a document. A first wish
  // that made no step says why here instead.
  async function landInNewApp(na) {
    const label = na.label || na.app;
    status.textContent = "Genie is making " + label + " (a few minutes)…";
    rememberDraft(true, na.app);
    try {
      for (;;) {
        const st = await draftState(na.app);
        if (st === null) throw new Error("the draft of " + label + " is gone");
        if (st && !st.busy) {
          if (st.last_error) throw new Error(label + " was made, but the first wish made no step: " + st.last_error);
          break;
        }
        await new Promise((r) => setTimeout(r, 1000));
      }
      location.assign(abs(base + "shape/" + encodeURIComponent(na.app) + "/draft/").href);
    } catch (e) {
      status.textContent = e.message || "The new app could not be made";
      send.disabled = false;
    }
  }

  // followCandidate watches a wish the live (or protected) page sent until
  // Genie has answered. Only one answer changes anything here: new_app,
  // which this page follows into the new app's draft; the rest is for the
  // Shape page, where the "Follow it" link already points.
  async function followCandidate(id) {
    for (;;) {
      let c;
      try {
        const res = await fetch(api("/" + encodeURIComponent(id)), { credentials: "same-origin" });
        if (res.status === 404) return;
        if (!res.ok) { await new Promise((r) => setTimeout(r, 3000)); continue; }
        c = await res.json();
      } catch (_) { await new Promise((r) => setTimeout(r, 3000)); continue; }
      if (c.status === "new_app" && c.new_app) { send.disabled = true; await landInNewApp(c.new_app); return; }
      if (c.status !== "running" && !c.deciding) return;
      await new Promise((r) => setTimeout(r, 2000));
    }
  }

  async function submit() {
    const request = ta.value.trim();
    if (!request) { ta.focus(); return; }
    send.disabled = true;
    status.textContent = "Sending…";
    if (mode === "draft") { await submitStep(request); return; }
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
      if (c.id) followCandidate(c.id);
    } catch (e) {
      status.textContent = e.message || "The wish could not be sent";
    }
    send.disabled = false;
  }

  // submitStep adds one wish to the draft as a step: posted, then the
  // draft is watched until Genie is done, and the fresh page is swapped
  // in place. A wish that produced no step says why here.
  async function submitStep(request) {
    try {
      const res = await fetch(draftAPI("/wishes"), {
        method: "POST", credentials: "same-origin",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ request, page, element, protected: false }),
      });
      const text = await res.text();
      if (!res.ok) throw new Error(text.trim() || res.statusText);
      ta.value = "";
      setElement("");
      status.textContent = "Genie is on it (a few minutes)…";
      // The bar's watch settles the wish and swaps the result in place;
      // awaiting it shares that one wait instead of settling twice (two
      // loops swap the page twice for one step).
      const st = barState ? await barState.watch() : await settle();
      if (!st) throw new Error("the draft is gone");
      if (st.new_app) { await landInNewApp(st.new_app); return; }
      if (st.last_error) { status.textContent = "No step: " + st.last_error; send.disabled = false; return; }
      close();
      toast("Wish applied to your draft.");
      if (!barState) location.reload();
    } catch (e) {
      status.textContent = e.message || "The wish could not be added";
      send.disabled = false;
    }
  }

  // settle polls the draft until no wish is running. null = no draft.
  async function settle() {
    for (;;) {
      const st = await draftState();
      if (st === null) return null;
      if (st && !st.busy) return st;
      await new Promise((r) => setTimeout(r, 1000));
    }
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
  // typing reports whether the key went to something the person types in:
  // their own undo there is theirs, not the draft's.
  const typing = (ev) => {
    const t = ev.target;
    return !!(t && t.nodeType === 1 && (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName)));
  };
  document.addEventListener("keydown", (ev) => {
    if (ev.altKey && ev.shiftKey && !ev.ctrlKey && !ev.metaKey && ev.code === "KeyG") {
      ev.preventDefault();
      toggle();
    } else if (barState && (ev.ctrlKey || ev.metaKey) && !ev.altKey && (ev.key === "z" || ev.key === "Z" || ev.code === "KeyZ") &&
      !typing(ev) && (!overlay || overlay.hidden)) {
      // Ctrl/Cmd-Z undoes the last wish, Ctrl/Cmd-Shift-Z redoes, while
      // the editing bar is shown.
      ev.preventDefault();
      if (ev.shiftKey) barState.redo(); else barState.undo();
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
  // barState is the editing bar's controller while one is shown (draft
  // mode): undo/redo for the keyboard, watch for a wish in flight.
  let barState = null;
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

  // outlineAgainstLive marks what differs from the live page, best effort:
  // the live page is fetched and parsed, elements are paired by id or by
  // their tag path, and the innermost changed and outermost added ones get
  // a dashed outline. Scripts, styles and the component's own nodes are
  // ignored; a page that renders itself from an API shows little. Returns
  // the [node, class] marks, so a toggle can flip them. Shared by the
  // review (candidate vs current version) and the edit session (draft vs
  // current version).
  async function outlineAgainstLive() {
    let live;
    try {
      const res = await fetch(liveURL(), { credentials: "same-origin", headers: { Accept: "text/html" } });
      if (!res.ok) return [];
      live = new DOMParser().parseFromString(await res.text(), "text/html");
    } catch (_) { return []; }
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
    const marks = [];
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
    return marks;
  }
  const differsText = (n) => n === 1 ? "1 element differs from your current version (outlined)" : n + " elements differ from your current version (outlined)";

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

    // outline marks what differs from the live page (outlineAgainstLive).
    async function outline() {
      marks = await outlineAgainstLive();
      if (marks.length) sum.append(el("span", "", " · "), el("span", "", differsText(marks.length)));
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

  // ---- the edit session ------------------------------------------------------
  // draftBar: on the draft's own pages, the bar "Editing draft: N wishes ·
  // undo · redo · Diff · Cancel · Save", with the sparkle in its slot to add
  // a wish. Undo and redo move the server-side cursor and swap the page in
  // place (the steps are stored; Genie does not run). Save verifies the whole
  // draft once: a refusal is shown here with its reason and the draft
  // stays; a pass lands on the live app, now one version newer. Cancel is
  // armed on the first click and discards on the second.
  function draftBar() {
    bar = el("div", "bgn bgn-bar");
    bar.setAttribute("role", "region");
    bar.setAttribute("aria-label", "Edit session");
    bar.setAttribute("data-bgn-draft", "");
    theme(bar);
    const head = el("div", "bgn-row");
    barSlot = el("span", "bgn-slot");
    const count = el("span", "bgn-count", "Editing draft");
    const wish = el("b", "bgn-wish", "");
    const busy = el("span", "bgn-status");
    busy.setAttribute("role", "status");
    busy.setAttribute("aria-live", "polite");
    const sum = el("div", "bgn-sum");
    const alertEl = el("div", "bgn-alert");
    alertEl.setAttribute("role", "alert");
    alertEl.hidden = true;
    const say = (msg) => { alertEl.textContent = msg || ""; alertEl.hidden = !msg; };

    let st = null, armed = 0;
    const move = async (what) => {
      if (!st || st.busy) return;
      setBusy(true, what === "/undo" ? "Undoing…" : "Redoing…");
      say("");
      try {
        const res = await fetch(draftAPI(what), { method: "POST", credentials: "same-origin" });
        if (!res.ok) throw new Error((await res.text()).trim() || res.statusText);
        await refresh();
      } catch (e) { setBusy(false); say("Could not " + what.slice(1) + ": " + e.message); }
    };
    // The next wish in this draft: a labelled control, not only the sparkle
    // in the slot (operator 2026-10-31: the icon alone was not found).
    const wishBtn = action("Wish", "bgn-p", () => open(), "Add a wish to your draft of " + app + " (" + SHORTCUT + ")");
    wishBtn.setAttribute("data-bgn-draft-wish", "");
    const undoBtn = action("↶", "bgn-ur", () => move("/undo"), "Undo the last wish (Ctrl/Cmd-Z)");
    const redoBtn = action("↷", "bgn-ur", () => move("/redo"), "Redo the undone wish (Ctrl/Cmd-Shift-Z)");
    undoBtn.title = "Undo the last wish (Ctrl/Cmd-Z)";
    redoBtn.title = "Redo the undone wish (Ctrl/Cmd-Shift-Z)";
    const diffBtn = action("Diff", "", () => showDraftDiff(), "Show the code diff of your draft against your current version");
    const cancelBtn = action("Cancel", "bgn-danger", async () => {
      if (!armed) {
        armed = setTimeout(() => { armed = 0; cancelBtn.textContent = "Cancel"; }, 5000);
        cancelBtn.textContent = "Cancel — discard the draft?";
        return;
      }
      clearTimeout(armed); armed = 0;
      setBusy(true, "Discarding the draft…");
      try {
        const res = await fetch(draftAPI("/cancel"), { method: "POST", credentials: "same-origin" });
        if (!res.ok) throw new Error((await res.text()).trim() || res.statusText);
        rememberDraft(false);
        toast("Draft discarded. Your current version is unchanged.");
        location.assign(liveURL());
      } catch (e) { setBusy(false); say("Could not cancel: " + e.message); }
    }, "Cancel the edit session: discard the draft and keep your current version");
    const saveBtn = action("Save", "bgn-p", async () => {
      setBusy(true, "Verifying the whole draft with the trusted verifier…");
      say("");
      try {
        const res = await fetch(draftAPI("/save"), { method: "POST", credentials: "same-origin" });
        const text = await res.text();
        if (res.ok) {
          let saved = {};
          try { saved = JSON.parse(text); } catch (_) {}
          rememberDraft(false);
          toast("Saved: one new version of " + app + " with " + plural(saved.wishes || 0, "wish", "wishes") + ".");
          location.assign(liveURL());
          return;
        }
        let reason = text.trim();
        try { const j = JSON.parse(text); reason = (j.verdict && j.verdict.detail) || j.error || reason; } catch (_) {}
        say("Not saved: " + (reason || res.statusText) + ". The draft stays open: fix it with another wish, undo, or cancel.");
        load(true);
      } catch (e) { say("Not saved: " + e.message); }
      setBusy(false);
    }, "Save the edit session: verify the whole draft once, then make it one new version listing every wish");
    const buttons = [wishBtn, undoBtn, redoBtn, diffBtn, cancelBtn, saveBtn];
    head.append(barSlot, count, wish, el("span", "bgn-sp"), busy, ...buttons);
    bar.append(head, sum, alertEl);
    function setBusy(on, msg) {
      for (const b of buttons) b.disabled = on;
      busy.textContent = on ? (msg || "") : "";
      if (!on && st) render(st);
    }

    // render fills the bar from the draft record.
    function render(state) {
      st = state;
      count.textContent = "Editing draft: " + plural(st.wishes || 0, "wish", "wishes");
      const applied = (st.steps || []).slice(0, st.cursor || 0);
      const last = applied[applied.length - 1];
      wish.textContent = last ? last.request : "";
      wish.title = wish.textContent;
      undoBtn.disabled = !st.can_undo;
      redoBtn.disabled = !st.can_redo;
      saveBtn.disabled = !!st.busy || !(st.wishes > 0);
      wishBtn.disabled = !!st.busy;
      cancelBtn.disabled = !!st.busy;
      diffBtn.disabled = !!st.busy;
      busy.textContent = st.busy && st.running ? "Genie is working on “" + st.running.request + "”…" : "";
      const parts = [];
      if (applied.length) parts.push(["Wishes", applied.map((s, i) => (i + 1) + ". " + s.request).join(" · ")]);
      const undone = (st.steps || []).length - (st.cursor || 0);
      if (undone) parts.push(["Undone", plural(undone, "wish", "wishes") + " (redo brings " + (undone === 1 ? "it" : "them") + " back; a new wish clears " + (undone === 1 ? "it" : "them") + ")"]);
      sum.replaceChildren();
      if (!parts.length) sum.append(el("span", "", "Your private draft of " + app + ": make a wish from the shaping button, as many as you like; nothing changes your current version until you save."));
      parts.forEach(([k, v], i) => {
        if (i) sum.append(el("span", "", " · "));
        sum.append(el("b", "", k + ": "), el("span", "", v));
      });
      if (st.last_error) say("The last wish made no step: " + st.last_error);
      else if (st.verdict && !st.verdict.pass) say("Not saved" + (st.verdict.detail ? ": " + st.verdict.detail : "") + ". The draft stays open: fix it with another wish, undo, or cancel.");
    }

    // watching is the shared wait for the wish in flight: every waiter
    // shares one settle loop and one refresh, so one step never swaps the
    // page twice. Two back-to-back navigations leave the editing bar
    // briefly missing twice, and a poll landing in either gap reads a null
    // bar as a failure.
    let watching = null;
    function watch(onDone) {
      if (!watching) {
        watching = (async () => {
          setBusy(true, "Genie is working on your wish…");
          try {
            const s2 = await settle();
            if (s2 === null) {
              say("This draft no longer exists: it was saved or cancelled elsewhere.");
              for (const b of buttons) b.disabled = true;
              return null;
            }
            if (onDone) onDone(s2);
            if (s2.new_app) {
              // The wish made a new app instead of a step: the panel (if open)
              // is already on its way there; from the bar, go the same way.
              setBusy(true, "Genie is making " + (s2.new_app.label || s2.new_app.app) + "…");
              if (!overlay || overlay.hidden) location.assign(abs(base + "shape/" + encodeURIComponent(s2.new_app.app) + "/draft/").href);
              return s2;
            }
            if (s2.last_error) { render(s2); return s2; }
            await refresh();
            return s2;
          } finally {
            watching = null;
          }
        })();
      } else if (onDone) {
        watching.then((s2) => { if (s2) { try { onDone(s2); } catch (_) {} } });
      }
      return watching;
    }

    // refresh swaps the draft's fresh page in place and refills the bar, so
    // the editing bar (and this component) never disappear mid-step. Only a
    // swap that cannot run falls back to a reload.
    async function refresh() {
      if (!(await refreshDraftPage())) { location.reload(); return; }
      // load leaves the draft-gone message and a disabled bar when the
      // draft vanished mid-step; there is nothing to re-enable then.
      if (!(await load())) return;
      outline();
      setBusy(false);
    }

    // refreshDraftPage fetches this draft page's fresh HTML and swaps the
    // app's own nodes in place, keeping the editing bar, the button, the
    // panel, and this component's style and script alive. Scripts the app
    // ships run again from their fresh copies, the way a reload would run
    // them; the component's own deferred tag is never re-inserted, so it
    // never initializes twice.
    async function refreshDraftPage() {
      let html = "";
      try {
        const res = await fetch(location.href, { credentials: "same-origin", headers: { Accept: "text/html" } });
        if (!res.ok) return false;
        html = await res.text();
      } catch (_) { return false; }
      let doc = null;
      try {
        doc = new DOMParser().parseFromString(html, "text/html");
      } catch (_) { return false; }
      if (!doc || !doc.head || !doc.body) return false;
      try {
        swapHead(doc.head);
        swapBody(doc.body);
      } catch (_) { return false; }
      return true;
    }

    // rerun returns a live copy of a fresh script node: inserted scripts run,
    // parsed ones do not, so the copy is what re-runs the app's scripts.
    function rerun(n) {
      const s = document.createElement("script");
      for (const a of n.attributes) s.setAttribute(a.name, a.value);
      if (n.src) s.async = false;
      else s.textContent = n.textContent;
      return s;
    }

    const ownNode = (n) => n.nodeType === 1 &&
      (n.hasAttribute("data-bgn-style") || n.hasAttribute("data-bashy-genie") ||
        (n.classList && (n.classList.contains("bgn") || n.classList.contains("bgn-bar"))));

    function swapHead(fresh) {
      for (const n of [...document.head.childNodes]) {
        if (!ownNode(n)) n.remove();
      }
      for (const n of [...fresh.childNodes]) {
        if (n.nodeType === 1 && n.tagName === "SCRIPT") document.head.append(rerun(n));
        else document.head.append(n.cloneNode(true));
      }
    }

    function swapBody(fresh) {
      // App nodes go; the bar, the button wrap, the panel, toasts, and the
      // component's own script tag stay.
      for (const n of [...document.body.childNodes]) {
        if (!ownNode(n)) n.remove();
      }
      for (const n of [...fresh.childNodes]) {
        if (ownNode(n)) continue;
        if (n.nodeType === 1 && n.tagName === "SCRIPT") {
          if (n.hasAttribute("data-bashy-genie")) continue;
          document.body.append(rerun(n));
        } else {
          document.body.append(n.cloneNode(true));
        }
      }
    }

    // load fills the bar from the server; keep leaves a fresh message.
    // It answers the draft record, or null when there is none (or it could
    // not be loaded), so a refresh knows whether to re-enable the bar.
    async function load(keep) {
      const s2 = await draftState();
      if (s2 === null) {
        wish.textContent = "";
        count.textContent = "No draft";
        sum.replaceChildren(el("span", "", "This draft no longer exists. "), Object.assign(el("a", "", "Open the live app"), { href: liveURL() }));
        for (const b of buttons) b.disabled = true;
        return null;
      }
      if (!s2) { say("The draft could not be loaded"); return null; }
      const msg = alertEl.textContent;
      render(s2);
      if (keep && msg) say(msg);
      if (s2.busy) watch();
      return s2;
    }

    async function showDraftDiff() {
      if (!diffSheet) {
        diffSheet = el("div", "bgn bgn-overlay");
        diffSheet.hidden = true;
        diffSheet.setAttribute("role", "dialog");
        diffSheet.setAttribute("aria-label", "Code diff");
        diffSheet.setAttribute("data-bgn-diff", "");
        const sheet = el("div", "bgn-sheet bgn-wide");
        const h = el("h2");
        h.innerHTML = sparkle;
        h.append(el("span", "", "Code diff of your draft of "), el("b", "", app));
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
      let html = "";
      try {
        const res = await fetch(draftAPI("/diff"), { credentials: "same-origin" });
        html = res.ok ? await res.text() : "";
      } catch (_) {}
      // The fragment is rendered and escaped by the console's own API.
      if (html) diffHost.innerHTML = html;
      else diffHost.textContent = "The diff is not available.";
    }

    // outline marks what the draft changed against the current version.
    async function outline() {
      const marks = await outlineAgainstLive();
      if (marks.length && st) sum.append(el("span", "", " · "), el("span", "", differsText(marks.length)));
    }

    pin(bar);
    rememberDraft(true);
    barState = { undo: () => move("/undo"), redo: () => move("/redo"), watch };
    load().then(outline);
  }

  // draftNoteBar: on a live page of the app while a draft is open (the
  // person left the draft, or came back after closing the tab), and on a
  // protected page: this is not the draft, and the way back into it.
  function draftNoteBar(m, st) {
    bar = el("div", "bgn bgn-bar");
    bar.setAttribute("role", "region");
    bar.setAttribute("aria-label", "Edit session");
    bar.setAttribute("data-bgn-draft-note", m.app);
    theme(bar);
    const row = el("div", "bgn-row");
    const text = el("span", "bgn-sum");
    const n = plural(st.wishes || 0, "wish", "wishes");
    if (mode === "protected") {
      text.append(el("b", "", "This page is protected"), el("span", "", " and is not part of your draft of "), el("code", "", m.app),
        el("span", "", ": sign-in, pairing, the Shape page, the terminal and accounts are never shaped. A wish made here is filed as a request."));
    } else {
      text.append(el("b", "", "This is your current version"), el("span", "", " of " + app + ". Your draft has " + n + " and is not active until you save it."));
    }
    const done = action("Done", "", () => {
      try { sessionStorage.setItem(DRAFT + ".quiet", m.app); } catch (_) {}
      unpin(bar); bar = null;
    }, "Hide this note for now");
    const target = m.app === app && mode !== "protected" ? draftURL() : abs(base + "shape/" + encodeURIComponent(m.app) + "/draft/").href;
    const resume = action("Resume editing", "bgn-p", () => location.assign(target), "Back into your draft of " + m.app);
    row.append(text, el("span", "bgn-sp"), done, resume);
    bar.append(row);
    pin(bar);
  }

  // ---- self-repair -------------------------------------------------------
  // S549.R1: when the app breaks for this person, the server records an
  // incident and this component follows it. Page errors — a failed
  // same-origin asset load, an uncaught script error, an unhandled
  // rejection, a same-app api/* fetch answering 5xx with an incident id —
  // are reported once per signature per page load to
  // api/shape/<app>/incidents, except a script error or rejection within
  // 10s of a failed fetch: that is the fetch's fallout, already covered by
  // its incident, and is not reported separately. The overlay below follows
  // the incident:
  // it waits while repairing, reloads when a fix is activated, and tells
  // the truth when the fix failed (the previous version is still active),
  // was refused (waiting for review), or is outside bashy's control.
  // Only live pages report: a preview's or draft's errors are not the
  // active version's. A swap or navigation that aborts in-flight loads is
  // never reported: those aborts are the navigation, not an incident.
  const repairAPI = (rest) => abs(base + "api/shape/" + encodeURIComponent(app) + "/incidents" + (rest || ""));
  const reportedSigs = new Set();
  const repairSig = (kind, message, url) => kind + "\n" + (message || "") + "\n" + (url || "");
  let repairOverlay = null, repairText = null, repairReview = null;
  let repairId = null, repairTimer = 0;

  function stopRepairPoll() {
    if (repairTimer) clearInterval(repairTimer);
    repairTimer = 0;
  }

  // paintRepair renders one incident state. Activated reloads the page to
  // the fixed version; the terminal states stop the poll and stay visible
  // until dismissed.
  function paintRepair(state, id) {
    if (!repairOverlay) return;
    repairOverlay.setAttribute("data-incident", id);
    repairOverlay.setAttribute("data-state", state);
    repairReview.hidden = true;
    if (state === "activated") {
      stopRepairPoll();
      location.reload();
      return;
    }
    if (state === "failed") {
      repairText.textContent = "Bashy could not fix the error. The previous version is still active. (Incident " + id + ")";
      stopRepairPoll();
      return;
    }
    if (state === "refused") {
      repairText.textContent = "A fix is ready, but it changes the app, so it is waiting for your review instead of going live on its own. (Incident " + id + ")";
      repairReview.href = reviewURL();
      repairReview.hidden = false;
      stopRepairPoll();
      return;
    }
    if (state === "external") {
      repairText.textContent = "That error is outside Bashy's control, so there is nothing to fix here. (Incident " + id + ")";
      stopRepairPoll();
      return;
    }
    repairText.textContent = "Oops, I saw that error and am fixing it, please wait…";
  }

  async function pollRepair() {
    if (!repairId) return;
    let j = null;
    try {
      const res = await origFetch(repairAPI("/" + encodeURIComponent(repairId)), { credentials: "same-origin" });
      if (res.status === 404) { stopRepairPoll(); if (repairOverlay) repairOverlay.hidden = true; return; }
      if (!res.ok) return;
      j = await res.json();
    } catch (_) { return; }
    if (j && j.state) paintRepair(j.state, repairId);
  }

  function startRepairPoll() {
    stopRepairPoll();
    repairTimer = setInterval(pollRepair, 2000);
  }

  function showRepairOverlay(id, state) {
    if (!id) return;
    if (repairOverlay && repairId !== id) {
      stopRepairPoll();
      repairOverlay.remove();
      repairOverlay = null;
    }
    repairId = id;
    if (!repairOverlay) {
      repairOverlay = el("div", "bgn bgn-overlay");
      repairOverlay.setAttribute("data-bgn-repair", "");
      repairOverlay.setAttribute("role", "alert");
      const sheet = el("div", "bgn-sheet");
      const h = el("h2");
      h.innerHTML = sparkle;
      h.append(el("span", "", "Something broke"));
      repairText = el("div", "bgn-prompt");
      repairText.setAttribute("data-bgn-repair-text", "");
      const row = el("div", "bgn-row");
      repairReview = el("a", "", "Review the fix");
      repairReview.setAttribute("data-bgn-repair-review", "");
      repairReview.hidden = true;
      const dismiss = el("button", "bgn-b", "Dismiss");
      dismiss.type = "button";
      dismiss.addEventListener("click", () => { stopRepairPoll(); repairOverlay.hidden = true; });
      row.append(el("span", "bgn-sp"), repairReview, dismiss);
      sheet.append(h, repairText, row);
      repairOverlay.append(sheet);
      theme(repairOverlay);
      (document.body || document.documentElement).append(repairOverlay);
    }
    repairOverlay.hidden = false;
    paintRepair(state || "repairing", id);
    if (repairTimer === 0) startRepairPoll();
  }

  // unloading is set when the page starts going away: a swap or a
  // navigation aborts in-flight asset loads and fetches, and those aborts
  // surface as element error events and aborted-fetch rejections. None of
  // them is an incident, so nothing is reported from here on. A bfcache
  // restore clears it: the page is alive again.
  let unloading = false;
  window.addEventListener("pagehide", () => { unloading = true; }, true);
  window.addEventListener("pageshow", (ev) => { if (ev && ev.persisted) unloading = false; }, true);

  // fetchIncidentAt is when an app api/* fetch last produced an incident
  // id (noteFetchResponse, provisional on the 5xx itself and confirmed when
  // its JSON names the incident). Page code that breaks BECAUSE that fetch
  // failed — an uncaught error or an unhandled rejection within
  // CAUSE_WINDOW_MS — is caused by the recorded incident, not a new one, so
  // it is not reported separately: the overlay already follows the fetch
  // incident. Suppression (rather than reporting with caused_by and folding
  // server-side) because the follow-on carries no independent signal, needs
  // no server state, and cannot open a second repair for one root cause.
  // The early bootstrap (the synchronous script the server puts first in
  // <head>) stamps the same time on window.__bashyFetchIncidentAt when the
  // app fetches before this deferred component wraps fetch; this component
  // reads that slot on load and re-checks it on every report, so an early
  // fetch's fallout is covered too. A report that still reaches the server
  // folds there (incidentStore.Observe), the robust backstop.
  let fetchIncidentAt = 0;
  try { fetchIncidentAt = +window.__bashyFetchIncidentAt || 0; } catch (_) {}
  const CAUSE_WINDOW_MS = 10000;

  function setFetchIncidentAt(v) {
    fetchIncidentAt = v;
    try { window.__bashyFetchIncidentAt = v; } catch (_) {}
  }

  async function reportRepair(kind, path, message, stack, url, offline) {
    if (unloading) return null;
    if (mode !== "live" || (kind !== "asset" && kind !== "script")) return null;
    // A script error or rejection right after a failed fetch is the fetch's
    // fallout, not a new incident. Asset loads are independent (a broken
    // image is not caused by a failed api call) and offline reports are
    // outside bashy's control, so only non-offline script reports wait.
    // The bootstrap may have stamped the slot after this component loaded
    // (its wrapped fetch resolves later), so re-check it here.
    try { if (+window.__bashyFetchIncidentAt > fetchIncidentAt) fetchIncidentAt = +window.__bashyFetchIncidentAt; } catch (_) {}
    if (kind === "script" && !offline && Date.now() - fetchIncidentAt < CAUSE_WINDOW_MS) return null;
    const sig = repairSig(kind, message, url);
    if (reportedSigs.has(sig)) return null;
    reportedSigs.add(sig);
    try {
      const res = await origFetch(repairAPI(), {
        method: "POST", credentials: "same-origin",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ kind, path, message, stack, url, offline: !!offline }),
      });
      if (!res.ok) return null;
      const body = await res.json();
      if (body && body.incident) showRepairOverlay(body.incident, body.state);
      return body;
    } catch (_) { return null; }
  }

  function sameOrigin(u) {
    try { return new URL(u, location.href).origin === location.origin; }
    catch (_) { return false; }
  }

  // isAppAPI is a same-origin call to the app's own api/* — under its mount
  // or any prefix above it — but never the shaping API itself, whose own
  // failures must not start repairs.
  function isAppAPI(u) {
    let p;
    try { p = new URL(u, location.href); }
    catch (_) { return false; }
    if (p.origin !== location.origin) return false;
    if (p.pathname.indexOf("/api/shape/") !== -1) return false;
    return /(^|\/)api\//.test(p.pathname);
  }

  // A fetch that answered 5xx may carry the server's incident: follow it.
  // The window opens on the 5xx itself, synchronously — the wrapper runs
  // before the page's own continuation, so page code breaking on the
  // failure is already covered — and is confirmed when the JSON names the
  // incident (reverted when it names none, so a bare 5xx never suppresses).
  function noteFetchResponse(input, res) {
    try {
      if (!isAppAPI(input) || !res || res.status < 500) return;
      const prev = fetchIncidentAt;
      setFetchIncidentAt(Date.now());
      res.clone().json().then((j) => {
        if (j && j.incident) {
          setFetchIncidentAt(Date.now());
          showRepairOverlay(j.incident, j.state);
        } else {
          setFetchIncidentAt(prev);
        }
      }).catch(() => { setFetchIncidentAt(prev); });
    } catch (_) {}
  }

  // A fetch that never answered while offline is outside bashy's control:
  // report it once so the page says so instead of failing silently.
  function noteFetchError(input, err) {
    try {
      if (!isAppAPI(input)) return;
      const u = String((input && input.url) || input || "");
      if (u.indexOf("/api/shape/") !== -1) return;
      if (typeof navigator !== "undefined" && !navigator.onLine) {
        reportRepair("script", page, "The page is offline, so this request could not be sent: " + u, "", u, true);
      }
    } catch (_) {}
  }

  const origFetch = window.fetch ? window.fetch.bind(window) : null;
  if (origFetch) {
    window.fetch = function (input, init) {
      return origFetch(input, init).then(
        (res) => { noteFetchResponse(input, res); return res; },
        (err) => { noteFetchError(input, err); throw err; }
      );
    };
  }

  // Failing same-origin app assets and uncaught page errors, in the capture
  // phase so a failed <script>/<link>/<img> load is seen with its element.
  // Cross-origin failures name their URL; the server marks them external.
  window.addEventListener("error", (ev) => {
    try {
      if (mode !== "live") return;
      const t = ev && ev.target;
      if (t && t !== window && t.tagName && /^(SCRIPT|LINK|IMG|VIDEO|AUDIO|SOURCE|TRACK|EMBED)$/.test(t.tagName)) {
        const u = String(t.currentSrc || t.src || t.href || "");
        const cross = u !== "" && !sameOrigin(u);
        reportRepair("asset", page,
          cross ? "An asset outside this app failed to load: " + u : "An asset failed to load: " + u,
          "", u, false);
        return;
      }
      const msg = String((ev && ev.message) || "Script error") +
        (ev && ev.filename ? " @ " + ev.filename + ":" + (ev.lineno || 0) : "");
      const stack = (ev && ev.error && ev.error.stack) || "";
      reportRepair("script", page, msg, String(stack), (ev && ev.filename) || "", false);
    } catch (_) {}
  }, true);
  window.addEventListener("unhandledrejection", (ev) => {
    try {
      if (mode !== "live") return;
      const r = ev && ev.reason;
      // An aborted fetch (a swap or navigation cut it off, or the app
      // cancelled it) is not an incident either, with or without pagehide.
      if (r && r.name === "AbortError") return;
      const msg = (r instanceof Error ? (r.message || "Unhandled rejection") : "Unhandled rejection: " + String(r));
      reportRepair("script", page, String(msg), (r instanceof Error && r.stack) || "", "", false);
    } catch (_) {}
  });

  function start() {
    if (mode === "draft") draftBar();
    else if (candidate) reviewBar();
    else {
      const m = marker();
      if (m && (mode === "protected" || (mode === "live" && m.app === app))) noteBar(m);
      else {
        const dm = draftMark();
        let quiet = false;
        try { quiet = !!dm && sessionStorage.getItem(DRAFT + ".quiet") === dm.app; } catch (_) {}
        if (dm && !quiet && (mode === "protected" || (mode === "live" && dm.app === app))) {
          draftState(dm.app).then((st) => { if (st && !bar) draftNoteBar(dm, st); });
        }
      }
    }
    placeButton();
    // The fixing page names its incident on window.__bashyIncident: follow
    // it so the page reloads when the repair lands.
    try {
      if (mode === "live" && typeof window.__bashyIncident === "string" && window.__bashyIncident) {
        showRepairOverlay(window.__bashyIncident, "repairing");
      }
    } catch (_) {}
  }

  if (document.body) start();
  else document.addEventListener("DOMContentLoaded", start);
})();
