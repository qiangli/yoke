// page.js — the page-side core of bashy browser live.
//
// Every DOM read or write the extension performs on a page lives here,
// in one file, so that:
//   - background.js injects it once per frame (ISOLATED world, `files:`,
//     after guard.js — the sensitive-input field check it relies on) and
//     then calls globalThis.__bashyPage.<fn>(params);
//   - the Go fixture tests load the very same bytes into a headless
//     Chrome isolated world and exercise them against local fixture
//     pages (pkg/browser/live/page_test.go), so a behavior change is
//     tested, not string-matched.
//
// Functions return plain JSON-serializable objects. A failure is
// reported as { error: "..." } rather than thrown, so the caller can
// tell "the page said no" from "injection failed".
(() => {
  const VERSION = 16;
  if (globalThis.__bashyPage && globalThis.__bashyPage.version === VERSION) return;

  // Native controls plus the ARIA widgets component libraries build
  // forms from (Material/Fluent/Headless UI selects, comboboxes, radio
  // groups) and the <label> of a visually hidden radio/checkbox.
  const CANDIDATES = "a, button, input, select, textarea, label, [role='button'], [role='link'], " +
    "[role='combobox'], [role='listbox'], [role='option'], [role='radio'], [role='checkbox'], " +
    "[role='switch'], [role='tab'], [role='menuitem'], [role='menuitemradio'], [aria-haspopup='listbox']";
  const LANDMARKS = "nav, aside, [role='navigation'], [role='complementary']";
  // Indexes are ephemeral addresses. Keep the identity captured by the last
  // extract so a later insertion cannot redirect a write to another field.
  const extracted = new Map();

  function deepElements(root, selector) {
    const found = [];
    function walk(node) {
      for (const el of node.querySelectorAll("*")) {
        if (el.matches(selector)) found.push(el);
        if (el.shadowRoot) walk(el.shadowRoot);
        if (globalThis.__bashyCDPTraverseFrames && (el.localName === "iframe" || el.localName === "frame")) {
          try { if (el.contentDocument) walk(el.contentDocument); } catch (_) { /* cross-origin frame */ }
        }
      }
    }
    walk(root);
    return found;
  }

  function fieldLabel(el) {
    const labelled = (el.getAttribute("aria-labelledby") || "").split(/\s+/).filter(Boolean)
      .map((id) => el.getRootNode().getElementById && el.getRootNode().getElementById(id))
      .filter(Boolean).map((node) => node.textContent.trim()).join(" ");
    const labels = el.labels && Array.from(el.labels).map((node) => node.textContent.trim()).join(" ");
    return (el.getAttribute("aria-label") || labelled || labels ||
      (el.closest("label") && el.closest("label").textContent) ||
      el.getAttribute("placeholder") || el.getAttribute("title") || "").replace(/\s+/g, " ").trim();
  }

  function cssEscape(value) {
    return globalThis.CSS && CSS.escape ? CSS.escape(value) : String(value).replace(/[^a-zA-Z0-9_-]/g, (c) => "\\" + c.codePointAt(0).toString(16) + " ");
  }

  // A selector is stable across sibling insertions when the field has an id.
  // For unmarked controls, the structural path remains a useful explicit
  // address and identity checks protect index writes if that path shifts.
  function selectorOf(el) {
    const parts = [];
    let cur = el;
    while (cur && cur.nodeType === Node.ELEMENT_NODE) {
      const root = cur.getRootNode();
      let anchored = false;
      if (cur.id && root.querySelectorAll("#" + cssEscape(cur.id)).length === 1) {
        parts.unshift("#" + cssEscape(cur.id));
        anchored = true;
      } else if (cur.getAttribute("name") && root.querySelectorAll(`${cur.localName}[name=${JSON.stringify(cur.getAttribute("name"))}]`).length === 1) {
        parts.unshift(`${cur.localName}[name=${JSON.stringify(cur.getAttribute("name"))}]`);
        anchored = true;
      } else {
        const tag = cur.localName;
        const siblings = cur.parentNode && cur.parentNode.children ? Array.from(cur.parentNode.children).filter((x) => x.localName === tag) : [];
        parts.unshift(tag + (siblings.length > 1 ? `:nth-of-type(${siblings.indexOf(cur) + 1})` : ""));
      }
      if (anchored || !cur.parentElement) {
        if (root.nodeType === Node.DOCUMENT_FRAGMENT_NODE && root.host) {
          parts.unshift(">>>");
          cur = root.host;
          continue;
        }
        break;
      }
      cur = cur.parentElement;
    }
    const local = parts.join(" ").replace(/\s*>>>\s*/g, " >>> ");
    const frame = el.ownerDocument && el.ownerDocument.defaultView && el.ownerDocument.defaultView.frameElement;
    return frame && globalThis.__bashyCDPTraverseFrames ? `framecss:${selectorOf(frame)}::${local}` : local;
  }

  function identityOf(el) {
    const tag = el.localName;
    const name = el.getAttribute("name") || "";
    const label = fieldLabel(el);
    // HTML IDs are scoped to their document/root and need not be unique.
    // Keep familiar IDs only when they uniquely identify a top-document
    // element; qualify shadow, duplicate-ID and traversed-frame fields.
    const uniqueDocumentID = el.id && el.getRootNode() === document &&
      document.querySelectorAll("#" + cssEscape(el.id)).length === 1;
    const stable_id = uniqueDocumentID ? el.id : selectorOf(el);
    return { tag, name, label, stable_id };
  }

  function sameIdentity(a, b) {
    return a && b && a.tag === b.tag && a.name === b.name && a.label === b.label && a.stable_id === b.stable_id;
  }

  function resolveSelector(root, selector) {
    const s = String(selector || "").trim();
    if (s.startsWith("framecss:")) {
      const cut = s.lastIndexOf("::");
      if (cut < 0) return [];
      const frames = resolveSelector(root, s.slice(9, cut));
      return frames.flatMap((frame) => {
        try { return frame.contentDocument ? resolveSelector(frame.contentDocument, s.slice(cut + 2)) : []; }
        catch (_) { return []; }
      });
    }
    if (s.startsWith("text=") || s.startsWith("label=")) {
      const want = norm(s.slice(s.indexOf("=") + 1));
      const pool = deepElements(root, CANDIDATES);
      if (s.startsWith("label=")) {
        const matches = pool.filter((el) => norm(fieldLabel(el)).includes(want))
          .map((el) => el.tagName === "LABEL" ? el.control : el).filter(Boolean);
        return [...new Set(matches)];
      }
      return [...new Set(pool.filter((el) => norm(labelOf(el) || el.textContent).includes(want))
        .map((el) => el.tagName === "LABEL" && el.control ? el.control : el))];
    }
    if (s.startsWith("xpath=") || s.startsWith("/") || s.startsWith("(")) {
      const expr = s.startsWith("xpath=") ? s.slice(6) : s;
      const found = [];
      function evaluate(node) {
        // Chrome rejects a ShadowRoot as an XPath context. Evaluate a
        // detached mirror and map its elements back to the open root.
        let context = node, doc = node.nodeType === Node.DOCUMENT_NODE ? node : (node.ownerDocument || document), original = null, mirrored = null;
        if (node.nodeType === Node.DOCUMENT_FRAGMENT_NODE && node.host) {
          doc = document.implementation.createHTMLDocument("");
          const clone = (from, to) => {
            for (const child of from.children) {
              const copy = child.cloneNode(false);
              to.appendChild(copy);
              original.push(child); mirrored.push(copy);
              clone(child, copy);
            }
          };
          original = []; mirrored = [];
          clone(node, doc.body);
          context = doc;
        }
        const result = doc.evaluate(expr, context, null, XPathResult.ORDERED_NODE_SNAPSHOT_TYPE, null);
        for (let i = 0; i < result.snapshotLength; i++) {
          const el = result.snapshotItem(i);
          const mapped = mirrored ? original[mirrored.indexOf(el)] : el;
          if (mapped && mapped.nodeType === Node.ELEMENT_NODE) found.push(mapped);
        }
      }
      function walk(node) {
        evaluate(node);
        for (const el of node.querySelectorAll("*")) {
          if (el.shadowRoot) walk(el.shadowRoot);
          if (globalThis.__bashyCDPTraverseFrames && (el.localName === "iframe" || el.localName === "frame")) {
            try { if (el.contentDocument) walk(el.contentDocument); } catch (_) { /* cross-origin frame */ }
          }
        }
      }
      walk(root);
      return [...new Set(found)];
    }
    // Explicit shadow boundary syntax. An ordinary CSS selector searches
    // every open root, so callers need not know whether a widget is shadowed.
    const stages = s.split(/\s*>>>\s*/);
    let roots = [root];
    for (let i = 0; i < stages.length; i++) {
      const matches = roots.flatMap((r) => i ? Array.from(r.querySelectorAll(stages[i])) : deepElements(r, stages[i]));
      if (i === stages.length - 1) return matches;
      roots = matches.map((el) => el.shadowRoot).filter(Boolean);
    }
    return [];
  }

  function shown(el) {
    const cs = el.ownerDocument.defaultView.getComputedStyle(el);
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

  // nameOf is labelOf without the field's value, for guarded fields.
  function nameOf(el) {
    return ((el.getAttribute("aria-label") || "").trim() || (el.getAttribute("title") || "").trim() ||
      (el.getAttribute("placeholder") || "").trim()).replace(/\s+/g, " ");
  }

  function scopeRoot(params) {
    if (!params.scope) return document;
    try { return resolveSelector(document, params.scope)[0] || null; }
    catch (_) { return null; }
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
    for (const el of deepElements(root, CANDIDATES)) {
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
    extracted.set(JSON.stringify([params.scope || "", !!params.include_hidden]), all.map(identityOf));
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
      // A password input never shows its value in a listing; other
      // fields are read as-is.
      const g = guardOf(el);
      const pw = isPasswordInput(el);
      const text = (pw ? nameOf(el) : labelOf(el)).slice(0, 80);
      const attrs = {};
      const attrPairs = [];
      for (const a of ["type", "placeholder", "href", "name", "value", "role", "aria-label"]) {
        const v = el.getAttribute(a);
        if (v && !(pw && a === "value")) {
          attrs[a] = String(v).slice(0, 60);
          attrPairs.push(`${a}="${String(v).slice(0, 60)}"`);
        }
      }
      if (g.guarded) {
        attrs.guarded = g.reason;
        attrPairs.push(`guarded="${g.reason}"`);
      }
      const val = pw ? null : valueOf(el);
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
      const identity = identityOf(el);
      const selector = selectorOf(el);
      lines.push(`[${idx}]${vis ? "" : " (hidden)"} ${selector} <${tag} ${attrPairs.join(" ")}>${text}</${tag}>`);
      const line = `<${tag} ${attrPairs.join(" ")}>${text}</${tag}>`;
      records.push({ index: idx, tag: tag, text: text, attrs: attrs, visible: vis, box: boxOf(el), line: line,
        stable_id: identity.stable_id, label: identity.label, selector: selector, name: identity.name });
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
      count: all.length,
      truncated: total > (OFFSET + LIMIT),
      viewport: viewport,
      data: JSON.stringify({ viewport: viewport, elements: records }),
    };
  }

  // opts.visibleOnly refuses a selector that matches only hidden
  // elements. Explicit --include-hidden still permits hidden controls.
  function resolveTarget(params, opts) {
    if (params.element_id && params.element_id > 0) {
      const flat = candidates(params);
      const el = flat[params.element_id - 1];
      if (!el) return { __miss: `element index ${params.element_id} out of range (extract listed ${flat.length})`, __notfound: true };
      if (opts && opts.write) {
        const identities = extracted.get(JSON.stringify([params.scope || "", !!params.include_hidden]));
        const expected = params.expected_identity || (identities && identities[params.element_id - 1]);
        if (!sameIdentity(expected, identityOf(el))) return {
          __miss: `guard:fill-mismatch: index ${params.element_id} no longer identifies the extracted field`,
          __mismatch: true,
        };
      }
      return el;
    }
    if (params.selector) {
      const scoped = scopeRoot(params);
      let matches;
      try { matches = resolveSelector(scoped || document, params.selector); }
      catch (e) {
        const numeric = /^[0-9]+$/.test(params.selector);
        return {
          __miss: numeric
            ? `${JSON.stringify(params.selector)} is not a CSS selector; pass an element index as element_id (bashy browser click --index ${params.selector})`
            : `${JSON.stringify(params.selector)} is not a valid selector: ${String((e && e.message) || e)}`,
        };
      }
      const el = matches[0];
      if (!el) return { __miss: `no element matches selector ${JSON.stringify(params.selector)}`, __notfound: true };
      // Prefer the first VISIBLE match: a selector like input[type=text]
      // also matches inputs in collapsed/display:none steps.
      if (!params.include_hidden && !shown(el)) {
        const vis = matches.find(shown);
        if (vis) return vis;
        if (opts && opts.visibleOnly) {
          return { __miss: `selector ${JSON.stringify(params.selector)} matches only hidden elements (pass --include-hidden to act on one anyway)`, __notfound: true };
        }
      }
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
    const pool = deepElements(root, CANDIDATES).filter((el) => shown(el));
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
    if (!scored.length) return { __miss: `no element whose text contains ${JSON.stringify(params.match_text)}`, __notfound: true };
    const cmp = (a, b) => { for (let i = 0; i < a.key.length; i++) if (a.key[i] !== b.key[i]) return b.key[i] - a.key[i]; return 0; };
    scored.sort(cmp);
    const ties = scored.filter((s) => cmp(s, scored[0]) === 0);
    if (ties.length > 1) {
      const list = ties.slice(0, 6).map((s) => `[${indexOf.get(s.el) || "?"}] ${describe(s.el)}`).join("; ");
      return { __miss: `${ties.length} elements match ${JSON.stringify(params.match_text)} equally (${list}); use --index N or a --scope` };
    }
    return scored[0].el;
  }

  // miss reports an unresolved target; notfound tells background.js to
  // try the next frame rather than fail.
  function miss(verb, el) {
    return { error: `${verb}: ${el.__miss}`, notfound: !!el.__notfound };
  }

  // count is this frame's enumeration size, so background.js can map a
  // tab-wide index onto (frame, local index) exactly as extract numbers.
  function count(params) {
    params = params || {};
    if (params.scope && !scopeRoot(params)) return { count: 0 };
    return { count: candidates(params).length };
  }

  function describe(el) {
    const tag = el.tagName ? el.tagName.toLowerCase() : "?";
    // labelOf reads a field's value; a guarded field is described by its
    // name only, so no message echoes what is in it.
    const label = (guardOf(el).guarded ? nameOf(el) : labelOf(el)).slice(0, 60);
    return label ? `<${tag}> ${label}` : `<${tag}>`;
  }

  function focusable(el) {
    return el.matches && el.matches("input, textarea, select, [contenteditable=''], [contenteditable='true'], [tabindex]");
  }

  function click(params) {
    const el = resolveTarget(params, { visibleOnly: true, write: true });
    if (el && el.__miss) return miss("click", el);
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
    const view = el.ownerDocument.defaultView;
    const proto = el instanceof view.HTMLTextAreaElement ? view.HTMLTextAreaElement.prototype
      : el instanceof view.HTMLSelectElement ? view.HTMLSelectElement.prototype
      : view.HTMLInputElement.prototype;
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
    const el = resolveTarget(params, { visibleOnly: true, write: true });
    if (el && el.__miss) return miss("type", el);
    // The sensitive-input guard, enforced here too: a client that skipped
    // bashy's gate still cannot fill a guarded field. The refusal never
    // echoes the text.
    const g = guardOf(el);
    if (g.guarded || g.error) {
      return { error: `type: refused by the sensitive-input guard (${g.reason || g.error}) on ${describe(el)}; a human must type this field (bashy browser routes it to the operator)`, guarded: true, reason: g.reason || "" };
    }
    return typeInto(el, params);
  }

  // guardFill types an OPERATOR's answer the Go guard admitted for this
  // field (the hub checks the admission before this runs), past the field
  // check that stops an agent's own text, and VERIFIES it here, where the
  // value already is. Nothing it returns carries the value — not the
  // result, not an error, not a hash: only match/mismatch and two lengths.
  async function guardFill(params) {
    const el = resolveTarget(params, { visibleOnly: true, write: true });
    if (el && el.__miss) return miss("guard_fill", el);
    const r = await typeInto(el, params);
    if (r.error) return { error: `guard_fill: the answer did not stick on ${nameOf(el) || el.tagName.toLowerCase()}`, retry_trusted: !!r.retry_trusted };
    return Object.assign({ content: "filled" }, fillVerdict(el, params.text || ""));
  }

  // guardVerify compares the addressed field with the operator's answer
  // after a fill (background.js runs it once the synthetic and, if needed,
  // trusted insert are done): the verdict and the lengths, never the value.
  function guardVerify(params) {
    const el = resolveTarget(params, { visibleOnly: true, write: true });
    if (el && el.__miss) return miss("guard_fill", el);
    return fillVerdict(el, params.text || "");
  }

  function fillVerdict(el, text) {
    const got = currentValue(el);
    return { verified: got === text ? "match" : "mismatch", field_len: got.length, value_len: text.length };
  }

  async function typeInto(el, params) {
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
    const el = resolveTarget(params, { visibleOnly: true, write: true });
    if (el && el.__miss) return miss("type", el);
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
    const el = resolveTarget(params, { visibleOnly: true, write: true });
    if (el && el.__miss) return miss("select", el);
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

  // guardOf is guard.js's verdict on el; a page.js without guard.js loaded
  // refuses rather than guessing.
  // Reads are unmasked except a password input's value.
  function isPasswordInput(el) {
    return !!el && el.tagName === "INPUT" && (el.getAttribute("type") || "").toLowerCase() === "password";
  }

  function guardOf(el) {
    if (!globalThis.__bashyGuard) return { guarded: true, reason: "field:guard-missing", error: "guard.js not loaded" };
    return globalThis.__bashyGuard.inspect(el);
  }

  // guardField is the Go gate's preflight: the verdict on the field a
  // type/keypress would hit, resolved exactly as type resolves it (the
  // focused element when no target is given). It is never sent the text.
  function guardField(params) {
    params = params || {};
    let el;
    if (params.element_id || params.selector || params.match_text) {
      el = resolveTarget(params, { visibleOnly: true, write: true });
      if (el && el.__miss) return miss("guard", el);
    } else {
      // The focused element. Focus inside a child frame answers from that
      // frame: here the active element is the <iframe>, so defer to it.
      el = document.activeElement;
      const top = window.top === window;
      if (el && (el.tagName === "IFRAME" || el.tagName === "FRAME")) return { error: "guard: focus is in a child frame", notfound: true };
      if (!top && (!el || el === document.body)) return { error: "guard: this frame holds no focus", notfound: true };
    }
    return guardOf(el);
  }

  // guardFocus focuses the field for the HUMAN path: the operator types
  // the guarded value into it themselves.
  function guardFocus(params) {
    const el = resolveTarget(params || {}, { visibleOnly: true, write: true });
    if (el && el.__miss) return miss("guard_focus", el);
    if (!globalThis.__bashyGuard) return { error: "guard_focus: guard.js not loaded" };
    if ((params || {}).release) return globalThis.__bashyGuard.releaseFocus(el);
    return globalThis.__bashyGuard.focusForHuman(el);
  }

  // probe counts this frame's matches for selector: all of them, and the
  // ones a user could see. wait_for_selector and keyboard_press route
  // through it so they share click/type's selector grammar.
  function probe(params) {
    let matches;
    try { matches = resolveSelector(document, params.selector); }
    catch (e) { return { error: `${JSON.stringify(params.selector)} is not a valid selector: ${String((e && e.message) || e)}` }; }
    return { matches: matches.length, visible: matches.filter(shown).length };
  }

  // dispatch fires a named event at window, document or the first element
  // matching selector. Nothing is compiled from a string (CSP-safe).
  function dispatch(params) {
    const name = String(params.event || "");
    let detail = null;
    if (params.detail) { try { detail = JSON.parse(params.detail); } catch (_) { detail = params.detail; } }
    let target = window, where = "window";
    if (params.selector === "document") { target = document; where = "document"; }
    else if (params.selector) {
      let matches;
      try { matches = resolveSelector(document, params.selector); }
      catch (e) { return { error: `dispatch_event: ${JSON.stringify(params.selector)} is not a valid selector` }; }
      if (!matches.length) return { error: `dispatch_event: no element matches ${JSON.stringify(params.selector)}`, notfound: true };
      target = matches[0]; where = params.selector;
    }
    const ev = detail === null
      ? new Event(name, { bubbles: true, cancelable: true })
      : new CustomEvent(name, { bubbles: true, cancelable: true, detail });
    const notCancelled = target.dispatchEvent(ev);
    return {
      content: `dispatched ${name} on ${where}`,
      data: JSON.stringify({ event: name, target: where, default_prevented: !notCancelled }),
    };
  }

  function deepActive() {
    let active = document.activeElement;
    while (active && active.shadowRoot && active.shadowRoot.activeElement) active = active.shadowRoot.activeElement;
    return active;
  }

  // keySynthetic is the untrusted fallback for keyboard_press: it targets the
  // deepest focused element (through open shadow roots), never the shadow host.
  function keySynthetic(params) {
    const key = params.key;
    const mods = new Set((params.modifiers || []).map((m) => String(m).toLowerCase()));
    const opts = {
      key: key,
      code: key.length === 1 ? "Key" + key.toUpperCase() : key,
      bubbles: true,
      cancelable: true,
      composed: true,
      shiftKey: mods.has("shift"),
      ctrlKey: mods.has("control") || mods.has("ctrl"),
      altKey: mods.has("alt"),
      metaKey: mods.has("meta") || mods.has("cmd") || mods.has("command"),
    };
    const target = deepActive() || document.body;
    target.dispatchEvent(new KeyboardEvent("keydown", opts));
    target.dispatchEvent(new KeyboardEvent("keypress", opts));
    if (key === "Enter" && target.form) {
      try { target.form.requestSubmit ? target.form.requestSubmit() : target.form.submit(); } catch (_) { /* ignore */ }
    }
    target.dispatchEvent(new KeyboardEvent("keyup", opts));
    return { ok: true };
  }

  // keyFocus focuses the single visible match of selector and confirms it
  // holds focus; the caller sends no key unless this answers ok.
  function keyFocus(params) {
    let matches;
    try { matches = resolveSelector(document, params.selector); }
    catch (e) { return { error: `keyboard_press: ${JSON.stringify(params.selector)} is not a valid selector` }; }
    const visible = matches.filter(shown);
    if (visible.length !== 1) return { error: `keyboard_press: selector ${JSON.stringify(params.selector)} matches ${visible.length} visible elements` };
    const el = visible[0];
    if (el.focus) el.focus();
    if (deepActive() !== el) return { error: `keyboard_press: ${JSON.stringify(params.selector)} did not take focus` };
    return { ok: true };
  }

  function scroll(params) {
    let amount = params.amount || 500;
    if (params.direction === "up") amount = -amount;
    window.scrollBy(0, amount);
    return { content: `scrolled to ${window.scrollY}`, data: String(window.scrollY) };
  }

  globalThis.__bashyPage = {
    version: VERSION,
    extract, count, click, type, select, scroll, readValue, focusTarget, guardField, guardFocus, guardFill, guardVerify,
    resolveSelector, probe, dispatch, keyFocus, keySynthetic,
  };
})();
