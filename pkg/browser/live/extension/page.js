// page.js — the page-side core of bashy browser live.
//
// Every DOM read or write the extension performs on a page lives here,
// in one file, so that:
//   - background.js injects it once per frame (ISOLATED world, `files:`)
//     and then calls globalThis.__bashyPage.<fn>(params);
//   - the Go fixture tests load the very same bytes into a headless
//     Chrome isolated world and exercise them against local fixture
//     pages (pkg/browser/live/page_test.go), so a behavior change is
//     tested, not string-matched.
//
// Functions return plain JSON-serializable objects. A failure is
// reported as { error: "..." } rather than thrown, so the caller can
// tell "the page said no" from "injection failed".
(() => {
  const VERSION = 3;
  if (globalThis.__bashyPage && globalThis.__bashyPage.version === VERSION) return;

  // Native controls plus the ARIA widgets component libraries build
  // forms from (Material/Fluent/Headless UI selects, comboboxes, radio
  // groups) and the <label> of a visually hidden radio/checkbox.
  const CANDIDATES = "a, button, input, select, textarea, label, [role='button'], [role='link'], " +
    "[role='combobox'], [role='listbox'], [role='option'], [role='radio'], [role='checkbox'], " +
    "[role='switch'], [role='tab'], [role='menuitem'], [role='menuitemradio'], [aria-haspopup='listbox']";
  const LANDMARKS = "nav, aside, [role='navigation'], [role='complementary']";

  function query(root, sel) {
    try {
      return root.querySelector(sel);
    } catch (e) {
      return { __badSelector: sel, __reason: String((e && e.message) || e) };
    }
  }

  function shown(el) {
    const cs = getComputedStyle(el);
    if (cs.visibility === "hidden" || cs.display === "none" || cs.opacity === "0") return false;
    const r = el.getBoundingClientRect();
    return r.width > 0 && r.height > 0;
  }

  function boxOf(el) {
    const r = el.getBoundingClientRect();
    return { x: Math.round(r.x), y: Math.round(r.y), w: Math.round(r.width), h: Math.round(r.height) };
  }

  // labelOf is the accessible-ish name: visible text, then value,
  // aria-label, title, and an inline <svg><title> — so icon-only
  // buttons (a dialog's X) are listed and addressable by name.
  function labelOf(el) {
    let s = (el.innerText || "").trim() || (el.value || "").trim() ||
      (el.getAttribute("aria-label") || "").trim() || (el.getAttribute("title") || "").trim();
    if (!s) {
      const t = el.querySelector && el.querySelector("svg title, [aria-label], [title], img[alt]");
      if (t) s = (t.textContent || t.getAttribute("aria-label") || t.getAttribute("title") || t.getAttribute("alt") || "").trim();
    }
    return s.replace(/\s+/g, " ");
  }

  function scopeRoot(params) {
    if (!params.scope) return document;
    const s = query(document, params.scope);
    return s && !s.__badSelector ? s : null;
  }

  // candidate drops elements that would only duplicate another entry:
  // a <label> counts only when it stands in for a hidden radio/checkbox
  // (the visible control of a styled group), and an element nested in
  // another candidate widget (a span[role=option] inside a button) is
  // skipped unless it is an option/radio of its own.
  function candidate(el) {
    if (el.tagName === "LABEL") {
      const c = el.control;
      return !!c && (c.type === "radio" || c.type === "checkbox") && !shown(c);
    }
    return true;
  }

  // valueOf is the widget's CURRENT value as a user sees it.
  function valueOf(el) {
    const tag = el.tagName;
    const role = el.getAttribute("role") || "";
    if (tag === "SELECT") {
      const o = el.selectedOptions && el.selectedOptions[0];
      return o ? o.text.trim() : "";
    }
    if (tag === "INPUT" && (el.type === "radio" || el.type === "checkbox")) return el.checked ? "checked" : "unchecked";
    if (tag === "LABEL" && el.control) return el.control.checked ? "checked" : "unchecked";
    const ac = el.getAttribute("aria-checked") || el.getAttribute("aria-selected");
    if (ac) return ac === "true" ? "checked" : "unchecked";
    if (role === "combobox" || el.getAttribute("aria-haspopup")) {
      if ("value" in el && tag === "INPUT") return el.value;
      return (el.getAttribute("aria-valuetext") || el.innerText || "").trim();
    }
    return null;
  }

  // candidates is THE enumeration: extract numbers it, and click/type
  // --index resolve through it, so [N] in a listing is the element an
  // index-addressed action hits.
  function candidates(params) {
    const root = scopeRoot(params) || document;
    const navFilter = !params.scope;
    const includeHidden = !!params.include_hidden;
    const out = [];
    for (const el of root.querySelectorAll(CANDIDATES)) {
      if (!candidate(el)) continue;
      if (navFilter && el.closest && el.closest(LANDMARKS)) continue;
      if (!includeHidden && !shown(el)) continue;
      out.push(el);
    }
    return out;
  }

  function extract(params) {
    params = params || {};
    const SCOPE_SEL = (params.scope || "").trim();
    const MATCH = (params.match_text || params.goal || "").trim();
    const LIMIT = params.limit && params.limit > 0 ? params.limit : 50;
    const OFFSET = params.offset && params.offset > 0 ? params.offset : 0;
    const root = SCOPE_SEL ? scopeRoot(params) : document;
    if (!root) {
      return { title: document.title, url: location.href, content: "", elements: "", error: "extract: scope " + SCOPE_SEL + " not found" };
    }
    const want = MATCH.toLowerCase();
    const matches = [];
    const indexOf = new Map();
    const all = candidates(params);
    for (let i = 0; i < all.length; i++) {
      const el = all[i];
      if (want) {
        const ph = el.getAttribute("placeholder") || "";
        const ar = el.getAttribute("aria-label") || "";
        if (labelOf(el).toLowerCase().indexOf(want) < 0 &&
            ph.toLowerCase().indexOf(want) < 0 &&
            ar.toLowerCase().indexOf(want) < 0) continue;
      }
      matches.push(el);
      indexOf.set(el, i + 1);
    }
    const total = matches.length;
    const slice = matches.slice(OFFSET, OFFSET + LIMIT);
    const lines = [];
    const records = [];
    for (let j = 0; j < slice.length; j++) {
      const el = slice[j];
      const tag = el.tagName.toLowerCase();
      const text = labelOf(el).slice(0, 80);
      const attrs = {};
      const attrPairs = [];
      for (const a of ["type", "placeholder", "href", "name", "value", "role", "aria-label"]) {
        const v = el.getAttribute(a);
        if (v) {
          attrs[a] = String(v).slice(0, 60);
          attrPairs.push(`${a}="${String(v).slice(0, 60)}"`);
        }
      }
      const val = valueOf(el);
      if (val !== null) {
        attrs.current = String(val).slice(0, 60);
        attrPairs.push(`current="${attrs.current}"`);
      }
      if (el.tagName === "SELECT") {
        const opts = Array.from(el.options).slice(0, 12).map((o) => o.text.trim());
        attrs.options = opts.join(" | ");
        attrPairs.push(`options="${attrs.options}${el.options.length > 12 ? " | ..." : ""}"`);
      }
      const vis = shown(el);
      // The index is the CLICK ADDRESS in the full enumeration, so a
      // filtered (--match) listing still prints indexes that click
      // --index resolves to the same element.
      const idx = want ? indexOf.get(el) : OFFSET + j + 1;
      lines.push(`[${idx}]${vis ? "" : " (hidden)"} <${tag} ${attrPairs.join(" ")}>${text}</${tag}>`);
      records.push({ index: idx, tag: tag, text: text, attrs: attrs, visible: vis, box: boxOf(el) });
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
      data: JSON.stringify({ viewport: viewport, elements: records }),
    };
  }

  function resolveTarget(params) {
    if (params.element_id && params.element_id > 0) {
      const flat = candidates(params);
      const el = flat[params.element_id - 1];
      if (!el) return { __miss: `element index ${params.element_id} out of range (extract listed ${flat.length})` };
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
    if (params.match_text) return byText(params);
    return { __miss: "no selector, element index, or match text given" };
  }

  function norm(s) {
    return String(s || "").replace(/\s+/g, " ").trim().toLowerCase();
  }

  function topmostDialog() {
    const open = Array.from(document.querySelectorAll(
      "dialog[open], [role='dialog'], [role='alertdialog'], [aria-modal='true']")).filter(shown);
    return open.length ? open[open.length - 1] : null;
  }

  // reachable: the element's centre is not covered by something else
  // (an overlay, a sticky header).
  function reachable(el) {
    const r = el.getBoundingClientRect();
    const x = r.x + r.width / 2, y = r.y + r.height / 2;
    if (x < 0 || y < 0 || x > innerWidth || y > innerHeight) return true; // off-screen: click scrolls it in
    const hit = document.elementFromPoint(x, y);
    return !hit || hit === el || el.contains(hit) || hit.contains(el);
  }

  // byText resolves click/type --text by RANK, not document order:
  // exact name > prefix > substring; inside the open dialog > elsewhere;
  // buttons/fields > links; main content > nav/aside/header/footer;
  // uncovered > covered. A tie on the best rank is refused with the
  // candidates, so the caller picks with --index instead of hitting a
  // neighbour.
  function byText(params) {
    const want = norm(params.match_text);
    const root = scopeRoot(params) || document;
    const dialog = params.scope ? null : topmostDialog();
    const all = candidates(Object.assign({}, params, { scope: params.scope || "", include_hidden: false }));
    const indexOf = new Map(all.map((el, i) => [el, i + 1]));
    const pool = Array.from(root.querySelectorAll(CANDIDATES)).filter((el) => shown(el));
    const scored = [];
    for (const el of pool) {
      const names = [labelOf(el), el.getAttribute("aria-label"), el.getAttribute("title"), el.getAttribute("placeholder")].map(norm);
      let m = 0;
      for (const n of names) {
        if (!n) continue;
        if (n === want) m = Math.max(m, 3);
        else if (n.startsWith(want)) m = Math.max(m, 2);
        else if (n.indexOf(want) >= 0) m = Math.max(m, 1);
      }
      if (!m) continue;
      const tag = el.tagName.toLowerCase();
      const role = el.getAttribute("role") || "";
      const isLink = tag === "a" || role === "link";
      const inDialog = dialog ? (dialog.contains(el) ? 1 : 0) : 0;
      const inChrome = el.closest("nav, aside, header, footer, [role='navigation'], [role='complementary'], [role='banner'], [role='contentinfo']") ? 0 : 1;
      const key = [inDialog, m, isLink ? 0 : 1, inChrome, reachable(el) ? 1 : 0];
      scored.push({ el, key });
    }
    if (!scored.length) return { __miss: `no element whose text contains ${JSON.stringify(params.match_text)}` };
    const cmp = (a, b) => { for (let i = 0; i < a.key.length; i++) if (a.key[i] !== b.key[i]) return b.key[i] - a.key[i]; return 0; };
    scored.sort(cmp);
    const ties = scored.filter((s) => cmp(s, scored[0]) === 0);
    if (ties.length > 1) {
      const list = ties.slice(0, 6).map((s) => `[${indexOf.get(s.el) || "?"}] ${describe(s.el)}`).join("; ");
      return { __miss: `${ties.length} elements match ${JSON.stringify(params.match_text)} equally (${list}); use --index N or a --scope` };
    }
    return scored[0].el;
  }

  function describe(el) {
    const tag = el.tagName ? el.tagName.toLowerCase() : "?";
    const label = labelOf(el).slice(0, 60);
    return label ? `<${tag}> ${label}` : `<${tag}>`;
  }

  function focusable(el) {
    return el.matches && el.matches("input, textarea, select, [contenteditable=''], [contenteditable='true'], [tabindex]");
  }

  function click(params) {
    const el = resolveTarget(params);
    if (el && el.__miss) return { error: `click: ${el.__miss}` };
    try { el.scrollIntoView({ block: "center", inline: "nearest" }); } catch (_) { /* detached */ }
    // HTMLElement.click() never moves focus; a real click on a field
    // does. Without this a following `type --selector input:focus`
    // (or a keyboard_press) lands on the previous element.
    if (focusable(el) && el.focus) el.focus();
    el.click();
    return { content: "clicked", data: JSON.stringify({ matched: describe(el) }) };
  }

  function currentValue(el) {
    if ("value" in el && !el.isContentEditable) return String(el.value);
    return (el.innerText || el.textContent || "").replace(/\n$/, "");
  }

  // setNative assigns through the prototype's setter. Frameworks that
  // shadow `value` on the instance (React's tracker, Lit, Angular's
  // value accessor) then see the input event as a real change.
  function setNative(el, text) {
    const proto = el instanceof HTMLTextAreaElement ? HTMLTextAreaElement.prototype
      : el instanceof HTMLSelectElement ? HTMLSelectElement.prototype
      : HTMLInputElement.prototype;
    const desc = Object.getOwnPropertyDescriptor(proto, "value");
    if (desc && desc.set) desc.set.call(el, text);
    else el.value = text;
  }

  function insertViaEditing(el, text) {
    // execCommand goes through the browser's editing pipeline: it
    // fires beforeinput/input exactly as typing does, which every
    // framework handles. Returns false where unsupported.
    try {
      if (el.isContentEditable) {
        const sel = getSelection();
        const r = document.createRange();
        r.selectNodeContents(el);
        sel.removeAllRanges();
        sel.addRange(r);
      } else if (el.select) {
        el.select();
      }
      if (text === "") return document.execCommand("delete", false);
      return document.execCommand("insertText", false, text);
    } catch (_) {
      return false;
    }
  }

  function wait(ms) {
    return new Promise((res) => setTimeout(res, ms));
  }

  // type fills the field and VERIFIES it: a framework that rejects or
  // resets the value is reported as an error naming what the field
  // shows, never as "typed".
  async function type(params) {
    const el = resolveTarget(params);
    if (el && el.__miss) return { error: `type: ${el.__miss}` };
    const text = params.text || "";
    try { el.scrollIntoView({ block: "center", inline: "nearest" }); } catch (_) { /* detached */ }
    if (el.focus) el.focus();
    let how = "editing";
    if (!insertViaEditing(el, text) || currentValue(el) !== text) {
      how = "setter";
      if ("value" in el && !el.isContentEditable) {
        setNative(el, text);
      } else {
        el.textContent = text;
      }
      el.dispatchEvent(new InputEvent("input", { bubbles: true, composed: true, inputType: "insertText", data: text }));
    }
    el.dispatchEvent(new Event("change", { bubbles: true }));
    // Let the framework re-render; a controlled field that rejected
    // the value snaps back here.
    await wait(params.settle_ms > 0 ? params.settle_ms : 80);
    const got = currentValue(el);
    const matched = describe(el);
    if (got !== text) {
      return {
        error: `type: value did not stick on ${matched}: wanted ${JSON.stringify(text)}, field shows ${JSON.stringify(got)}`,
        retry_trusted: true,
        data: JSON.stringify({ matched: matched, wanted: text, got: got, how: how }),
      };
    }
    return { content: "typed", data: JSON.stringify({ matched: matched, value: got, how: how }) };
  }

  // readValue re-reads the focused (or addressed) field; background.js
  // uses it to verify a trusted (debugger) insert.
  function readValue(params) {
    const el = (params && (params.element_id || params.selector || params.match_text))
      ? resolveTarget(params) : document.activeElement;
    if (!el || el.__miss) return { error: `read: ${el ? el.__miss : "no focused element"}` };
    return { value: currentValue(el), matched: describe(el) };
  }

  // focusTarget focuses and selects the field's contents so a trusted
  // Input.insertText replaces it.
  function focusTarget(params) {
    const el = resolveTarget(params);
    if (el && el.__miss) return { error: `type: ${el.__miss}` };
    if (el.focus) el.focus();
    try {
      if (el.isContentEditable) {
        const r = document.createRange();
        r.selectNodeContents(el);
        getSelection().removeAllRanges();
        getSelection().addRange(r);
      } else if (el.select) {
        el.select();
      }
    } catch (_) { /* not selectable */ }
    return { ok: true, matched: describe(el) };
  }

  // realClick fires the pointer/mouse sequence a user produces;
  // many listboxes open on mousedown/pointerdown, not on click.
  function realClick(el) {
    try { el.scrollIntoView({ block: "center", inline: "nearest" }); } catch (_) { /* detached */ }
    const r = el.getBoundingClientRect();
    const o = { bubbles: true, cancelable: true, composed: true, view: window, button: 0,
      clientX: r.x + r.width / 2, clientY: r.y + r.height / 2 };
    el.dispatchEvent(new PointerEvent("pointerdown", Object.assign({ pointerType: "mouse", isPrimary: true }, o)));
    el.dispatchEvent(new MouseEvent("mousedown", o));
    if (focusable(el) && el.focus) el.focus();
    el.dispatchEvent(new PointerEvent("pointerup", Object.assign({ pointerType: "mouse", isPrimary: true }, o)));
    el.dispatchEvent(new MouseEvent("mouseup", o));
    el.click();
  }

  function pickByName(els, want) {
    let best = null, bestM = 0;
    for (const el of els) {
      const n = norm(labelOf(el)) || norm(el.getAttribute("value"));
      let m = 0;
      if (n === want) m = 3; else if (n.startsWith(want)) m = 2; else if (n.indexOf(want) >= 0) m = 1;
      if (m > bestM) { best = el; bestM = m; }
    }
    return best;
  }

  function visibleOptions(near) {
    // Options of the popup this widget controls, else any visible ones.
    const id = near && (near.getAttribute("aria-controls") || near.getAttribute("aria-owns"));
    const scope = id ? document.getElementById(id) : null;
    const sel = "[role='option'], [role='menuitem'], [role='menuitemradio'], [role='listbox'] li";
    const pool = Array.from((scope || document).querySelectorAll(sel)).filter(shown);
    return pool.length || !scope ? pool : Array.from(document.querySelectorAll(sel)).filter(shown);
  }

  // select picks an option in a native <select>, a radio group, or a
  // custom listbox/combobox (open it, filter if it is searchable, pick
  // the option), then verifies the widget shows the choice.
  async function select(params) {
    const want = norm(params.option);
    if (!want) return { error: "select: an option is required" };
    const el = resolveTarget(params);
    if (el && el.__miss) return { error: `select: ${el.__miss}` };
    const matched = describe(el);
    const done = (got) => ({ content: "selected", data: JSON.stringify({ matched, selected: got }) });

    if (el.tagName === "SELECT") {
      const o = pickByName(Array.from(el.options), want);
      if (!o) return { error: `select: ${matched} has no option ${JSON.stringify(params.option)} (options: ${Array.from(el.options).map((x) => x.text.trim()).join(" | ")})` };
      if (el.focus) el.focus();
      setNative(el, o.value);
      el.dispatchEvent(new Event("input", { bubbles: true }));
      el.dispatchEvent(new Event("change", { bubbles: true }));
      await wait(50);
      const got = valueOf(el);
      if (norm(got) !== norm(o.text)) return { error: `select: ${matched} shows ${JSON.stringify(got)} after choosing ${JSON.stringify(o.text.trim())}` };
      return done(got);
    }

    // A radio group: the target is the group (or any member); choose
    // the member whose label matches.
    const group = el.closest("[role='radiogroup'], fieldset") || (el.matches("[role='radiogroup'], fieldset") ? el : null);
    if (group) {
      const members = Array.from(group.querySelectorAll("[role='radio'], input[type='radio'], label")).filter((m) => m.tagName !== "INPUT" || shown(m));
      const m = pickByName(members, want);
      if (m) {
        realClick(m);
        await wait(50);
        const input = m.tagName === "LABEL" ? m.control : m;
        const ok = input && (input.checked || input.getAttribute("aria-checked") === "true");
        if (!ok) return { error: `select: clicked ${describe(m)} in ${matched} but it is not checked` };
        return done(labelOf(m));
      }
    }

    // Custom listbox/combobox: open, filter if searchable, pick.
    const input = el.tagName === "INPUT" ? el : el.querySelector("input:not([type='hidden'])");
    realClick(el);
    let opts = [];
    for (let i = 0; i < 20 && !(opts = visibleOptions(el)).length; i++) await wait(50);
    let opt = pickByName(opts, want);
    if (!opt && input) {
      if (input.focus) input.focus();
      insertViaEditing(input, params.option) || (setNative(input, params.option),
        input.dispatchEvent(new InputEvent("input", { bubbles: true, inputType: "insertText", data: params.option })));
      for (let i = 0; i < 30 && !(opt = pickByName(visibleOptions(el), want)); i++) await wait(50);
    }
    if (!opt) {
      const seen = visibleOptions(el).slice(0, 12).map((o) => labelOf(o)).join(" | ");
      return { error: `select: no option ${JSON.stringify(params.option)} in ${matched}` + (seen ? ` (visible: ${seen})` : " (no options opened)") };
    }
    const chosen = labelOf(opt);
    realClick(opt);
    await wait(100);
    const shows = norm(valueOf(el) || labelOf(el) || (input && input.value));
    const picked = opt.getAttribute("aria-selected") === "true" && opt.isConnected;
    if (shows.indexOf(norm(chosen)) < 0 && !picked) {
      return { error: `select: chose ${JSON.stringify(chosen)} but ${matched} shows ${JSON.stringify(shows)}` };
    }
    return done(chosen);
  }

  function scroll(params) {
    let amount = params.amount || 500;
    if (params.direction === "up") amount = -amount;
    window.scrollBy(0, amount);
    return { content: `scrolled to ${window.scrollY}`, data: String(window.scrollY) };
  }

  globalThis.__bashyPage = {
    version: VERSION,
    extract, click, type, select, scroll, readValue, focusTarget,
  };
})();
