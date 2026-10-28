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
  const VERSION = 2;
  if (globalThis.__bashyPage && globalThis.__bashyPage.version === VERSION) return;

  const CANDIDATES = "a, button, input, select, textarea, [role='button'], [role='link']";
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

  // candidates is THE enumeration: extract numbers it, and click/type
  // --index resolve through it, so [N] in a listing is the element an
  // index-addressed action hits.
  function candidates(params) {
    const root = scopeRoot(params) || document;
    const navFilter = !params.scope;
    const includeHidden = !!params.include_hidden;
    const out = [];
    for (const el of root.querySelectorAll(CANDIDATES)) {
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

  function scroll(params) {
    let amount = params.amount || 500;
    if (params.direction === "up") amount = -amount;
    window.scrollBy(0, amount);
    return { content: `scrolled to ${window.scrollY}`, data: String(window.scrollY) };
  }

  globalThis.__bashyPage = {
    version: VERSION,
    extract, click, type, scroll, readValue, focusTarget,
  };
})();
