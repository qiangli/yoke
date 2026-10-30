// guard.js — the page-side half of bashy browser's sensitive-input guard.
//
// It answers ONE question about a field, without ever seeing a value: may
// an agent type into it? A field is guarded when it is a password input,
// carries a sensitive autocomplete token, or when anything that NAMES it —
// name/id, <label>, aria-label/-labelledby/-describedby, placeholder, title,
// or the VISIBLE QUESTION TEXT around it (a Typeform-style screen whose
// heading says "Social Security Number" over a plain text box) — says
// ssn/tax id/ein/routing/bank account/card number/cvv/password/username.
// The field's OWN identity decides; describedby, help text and the
// surrounding question never classify it (the question is only reported).
//
// Loaded before page.js in the same ISOLATED world (background.js injects
// files ["guard.js", "page.js"]); probe/solo evaluate the same bytes in a
// CDP isolated world. The Go gate (pkg/browser/guard) calls guardField
// BEFORE a type and never sends the text; page.js's own type() also
// refuses a guarded field, so a client that skips the gate still cannot
// fill one.
//
// This file is a PROTECTED path (pkg/src manifest): a change merges only
// with a `bashy approve`d src approval. Site skills cannot switch it off;
// there is no parameter that does.
(() => {
  const VERSION = 4;
  if (globalThis.__bashyGuard && globalThis.__bashyGuard.version === VERSION) return;

  // autocomplete tokens that mark credentials, one-time codes, and cards.
  const AUTOCOMPLETE = new Set(["current-password", "new-password", "one-time-code", "username"]);

  // Words that name a sensitive field. Matched on whole words of the
  // normalised text (camelCase and snake_case split), so "accountant" or
  // "cardinal" do not match but "Bank account" and "taxId" do.
  const WORDS = [
    ["ssn", /\b(ssn|social security|social insurance|sin number)\b/],
    ["tax", /\b(tax ?id|tax identification|taxpayer|tax number|tax no|tin|itin|vat id)\b/],
    ["ein", /\b(ein|fein|employer identification)\b/],
    ["routing", /\b(routing|aba|sort code|swift|bic|iban)\b/],
    ["account", /\b((bank|checking|savings|deposit|wire|ach) (account|acct)|(account|acct) (number|num|no))\b/],
    ["card", /\b((credit|debit|payment|bank) card|card (number|no|num|security|verification|expiry|expiration)|cc ?(num|number)|ccnum|cvv|cvc|csc|security code)\b/],
    ["password", /\b(password|passwd|passcode|passphrase|pwd|pass word)\b/],
    ["username", /\b(username|user name|user id|userid|login id|login name)\b/],
    ["otp", /\b(one time code|one time password|otp|verification code|2fa|mfa code)\b/],
  ];

  function words(s) {
    return String(s || "")
      .replace(/([a-z0-9])([A-Z])/g, "$1 $2")
      .replace(/#/g, " no ")
      .replace(/[_\-.:\[\]/]+/g, " ")
      .replace(/\s+/g, " ")
      .trim()
      .toLowerCase();
  }

  function sensitiveWord(s) {
    const w = words(s);
    if (!w) return "";
    for (const [kind, re] of WORDS) if (re.test(w)) return kind;
    return "";
  }

  function visible(el) {
    if (!el || !el.getBoundingClientRect) return false;
    const cs = getComputedStyle(el);
    if (cs.visibility === "hidden" || cs.display === "none") return false;
    const r = el.getBoundingClientRect();
    return r.width > 0 && r.height > 0;
  }

  const FIELD = "input:not([type='hidden']):not([type='button']):not([type='submit']):not([type='reset'])" +
    ":not([type='image']):not([type='checkbox']):not([type='radio']), textarea, select, " +
    "[contenteditable=''], [contenteditable='true'], [role='textbox'], [role='combobox']";

  function textOf(id) {
    const n = id && document.getElementById(id);
    return n ? (n.innerText || n.textContent || "") : "";
  }

  // namesOf: every attribute or element that labels the field directly.
  function namesOf(el) {
    const out = [];
    const push = (src, s) => { if (s && String(s).trim()) out.push([src, String(s).trim()]); };
    push("name", el.getAttribute("name"));
    push("name", el.id);
    if (el.labels) for (const l of el.labels) push("label", l.innerText || l.textContent);
    const wrap = el.closest && el.closest("label");
    if (wrap) push("label", wrap.innerText || wrap.textContent);
    push("label", el.getAttribute("aria-label"));
    for (const id of (el.getAttribute("aria-labelledby") || "").split(/\s+/)) push("label", textOf(id));
    push("label", el.getAttribute("placeholder"));
    push("label", el.getAttribute("title"));
    push("name", el.getAttribute("data-qa"));
    push("name", el.getAttribute("data-testid"));
    return out;
  }

  // SKIP: text that is chrome, not the question (buttons, links, nav,
  // header/footer, scripts, option lists).
  const SKIP = "button, a, [role='button'], [role='link'], nav, header, footer, [role='navigation'], " +
    "[role='banner'], [role='contentinfo'], script, style, noscript, template, select, option, svg";

  // questionOf: the visible text of the question the field answers. It
  // climbs from the field while the ancestor still holds no OTHER visible
  // field — on a one-question-per-screen form that reaches the screen, on
  // a multi-field form it stops at the field's own wrapper — and reads that
  // region's visible text minus chrome.
  function questionOf(el) {
    let region = null;
    for (let n = el.parentElement; n && n !== document.documentElement; n = n.parentElement) {
      const fields = Array.from(n.querySelectorAll(FIELD)).filter((f) => f === el || visible(f));
      if (fields.some((f) => f !== el && !f.contains(el) && !el.contains(f))) break;
      region = n;
      if (n === document.body) break;
    }
    if (!region) return "";
    const parts = [];
    const walker = document.createTreeWalker(region, NodeFilter.SHOW_TEXT);
    let total = 0;
    for (let t = walker.nextNode(); t && total < 600; t = walker.nextNode()) {
      const p = t.parentElement;
      if (!p || p.closest(SKIP) || !visible(p)) continue;
      if (el.contains(t)) continue;
      const s = (t.nodeValue || "").replace(/\s+/g, " ").trim();
      if (!s) continue;
      parts.push(s);
      total += s.length;
    }
    return parts.join(" ").slice(0, 600);
  }

  function describe(el) {
    const tag = el.tagName ? el.tagName.toLowerCase() : "?";
    const type = el.getAttribute && el.getAttribute("type");
    const name = el.getAttribute && (el.getAttribute("name") || el.id);
    return `<${tag}${type ? ` type="${type}"` : ""}${name ? ` name="${name}"` : ""}>`;
  }

  // inspect is the verdict for one element. It reads attributes and page
  // text only — never el.value.
  function inspect(el) {
    const site = location.hostname || location.host || "";
    if (!el || el.nodeType !== 1) return { guarded: false, site };
    // Only something a value can be typed into is a field; a button or a
    // link that says "Close account" is not.
    if (!(el.matches(FIELD) || el.isContentEditable)) return { guarded: false, site };
    const question = questionOf(el);
    const base = { site, question, matched: describe(el) };
    const type = (el.getAttribute("type") || "").toLowerCase();
    if (el.tagName === "INPUT" && type === "password") return Object.assign(base, { guarded: true, reason: "field:type-password" });
    for (const tok of (el.getAttribute("autocomplete") || "").toLowerCase().split(/\s+/)) {
      if (AUTOCOMPLETE.has(tok) || tok.startsWith("cc-")) {
        return Object.assign(base, { guarded: true, reason: "field:autocomplete-" + tok });
      }
    }
    for (const [src, s] of namesOf(el)) {
      const kind = sensitiveWord(s);
      if (kind) return Object.assign(base, { guarded: true, reason: `field:${src}-${kind}` });
    }
    return Object.assign(base, { guarded: false });
  }

  // inspectSelector is the probe/solo entry: the element chromedp's
  // SendKeys/Focus would hit (the first querySelector match), or the
  // focused element when no selector is given.
  function inspectSelector(selector) {
    let el = null;
    if (selector) {
      try { el = document.querySelector(selector); } catch (e) { return { error: `guard: ${JSON.stringify(selector)} is not a valid CSS selector` }; }
      if (!el) return { error: `guard: no element matches selector ${JSON.stringify(selector)}`, notfound: true };
    } else {
      el = document.activeElement;
    }
    return inspect(el);
  }

  // focusForHuman puts the operator's caret in the field and outlines it,
  // so the human types the value straight into the page.
  function focusForHuman(el) {
    try { el.scrollIntoView({ block: "center", inline: "nearest" }); } catch (_) { /* detached */ }
    if (el.focus) el.focus();
    try {
      el.__bashyPrevOutline = [el.style.outline, el.style.outlineOffset];
      el.style.outline = "3px solid #d93025"; el.style.outlineOffset = "2px";
    } catch (_) { /* read-only style */ }
    return { content: "focused", data: JSON.stringify({ matched: describe(el) }) };
  }

  // releaseFocus removes focusForHuman's outline (Done/Skip/Stop).
  function releaseFocus(el) {
    try {
      const prev = el.__bashyPrevOutline || ["", ""];
      el.style.outline = prev[0]; el.style.outlineOffset = prev[1];
      delete el.__bashyPrevOutline;
    } catch (_) { /* read-only style */ }
    return { content: "released", data: JSON.stringify({ matched: describe(el) }) };
  }

  // passwordValues lists the non-empty values of every password input in the
  // document (open shadow roots included). Result sanitization only: callers
  // redact these from what they hand back; the list itself stays in-process.
  function passwordValues() {
    const out = [];
    const walk = (root) => {
      for (const el of root.querySelectorAll("*")) {
        if (el.tagName === "INPUT" && (el.getAttribute("type") || "").toLowerCase() === "password" && el.value) out.push(String(el.value));
        if (el.shadowRoot) walk(el.shadowRoot);
      }
    };
    try { walk(document); } catch (_) { /* detached */ }
    return out;
  }

  globalThis.__bashyGuard = { version: VERSION, inspect, inspectSelector, focusForHuman, releaseFocus, sensitiveWord, passwordValues };
})();
