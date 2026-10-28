// frames.js — pure helpers that make a tab's frames look like one page.
//
// background.js runs page.js in every frame (top + iframes, including
// cross-origin ones) and uses these helpers to number elements
// tab-wide and to route an index back to its frame. Kept free of
// chrome.* so the Go tests evaluate the same file in a plain page
// (pkg/browser/live/page_test.go).
//
// Frame order is the contract: the top frame (frameId 0) first, then
// child frames by ascending frameId. extract and click --index both
// use frameOrder, so [N] in a listing is the element click --index N
// hits, wherever it lives.

function frameOrder(frames) {
  return frames
    .filter((f) => f && f.result)
    .sort((a, b) => (a.frameId === 0 ? -1 : b.frameId === 0 ? 1 : a.frameId - b.frameId));
}

// locateIndex maps a tab-wide 1-based index onto { frameId, local } using
// each frame's count() answer; null when out of range.
function locateIndex(counted, n) {
  let base = 0;
  for (const f of frameOrder(counted)) {
    const c = f.result.count || 0;
    if (n <= base + c) return { frameId: f.frameId, local: n - base };
    base += c;
  }
  return null;
}

function totalCount(counted) {
  return frameOrder(counted).reduce((s, f) => s + (f.result.count || 0), 0);
}

// mergeExtract folds per-frame extract answers (each produced with no
// paging) into one listing with tab-wide indexes, then applies the
// caller's offset/limit. Child-frame elements are tagged with their
// frame; child-frame text is appended under a frame header.
function mergeExtract(frames, params) {
  params = params || {};
  const LIMIT = params.limit && params.limit > 0 ? params.limit : 50;
  const OFFSET = params.offset && params.offset > 0 ? params.offset : 0;
  const ordered = frameOrder(frames);
  const top = ordered.find((f) => f.frameId === 0) || ordered[0];
  const recs = [];
  const texts = [];
  let base = 0;
  let viewport = null;
  for (const f of ordered) {
    const r = f.result;
    if (r.error) continue;
    let d = {};
    try { d = JSON.parse(r.data || "{}"); } catch (_) { d = {}; }
    if (f === top) viewport = d.viewport || r.viewport || null;
    for (const e of d.elements || []) {
      recs.push(Object.assign({}, e, {
        index: base + e.index,
        frame_id: f.frameId,
        frame_url: f.frameId === 0 ? undefined : r.url,
      }));
    }
    if (f !== top && (r.content || "").trim()) {
      texts.push(`--- frame ${f.frameId} (${r.url}) ---\n${r.content}`);
    }
    base += r.count || 0;
  }
  const page = recs.slice(OFFSET, OFFSET + LIMIT);
  const lines = page.map((e) =>
    `[${e.index}]${e.visible ? "" : " (hidden)"} ${e.line || `<${e.tag}>${e.text}</${e.tag}>`}` +
    (e.frame_id ? ` (frame ${e.frame_id})` : ""));
  let content = (top && top.result.content) || "";
  if (texts.length) content += "\n\n" + texts.join("\n\n");
  if (content.length > 16000) content = content.slice(0, 16000) + "\n... (truncated)";
  const out = {
    title: top ? top.result.title : "",
    url: top ? top.result.url : "",
    content: content,
    elements: lines.join("\n"),
    total: recs.length,
    truncated: recs.length > OFFSET + LIMIT,
    viewport: viewport,
    frames: ordered.length,
    data: JSON.stringify({ viewport: viewport, elements: page.map((e) => { const c = Object.assign({}, e); delete c.line; return c; }) }),
  };
  if (top && top.result.error) out.error = top.result.error;
  return out;
}
