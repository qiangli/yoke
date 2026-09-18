// The steward board page.
//
// A full browser page like the terminal and the message board, and its
// <base href> is the LAUNCHER's, so "api/sprint" and "./" resolve correctly at /
// on loopback and under outpost's /matrix/h/<host>/app/<name>/ prefix alike.
//
// READ-ONLY BY CONSTRUCTION. `board` is the one work verb the atlas marks
// CapReadOnly — it reports across the machine but never starts, merges, or
// kills work — so this page has no action anywhere on it. In particular it must
// never touch a sprint lease: a browser tab left open would otherwise look like
// a working conductor.
const url = (p) => new URL(p, document.baseURI);

try {
  const cfg = JSON.parse(localStorage.getItem("bashy.apps.config") || "{}");
  if (cfg.theme && cfg.theme !== "system") document.documentElement.setAttribute("data-theme", cfg.theme);
} catch (_) {}

const $ = (id) => document.getElementById(id);
const el = (tag, cls, text) => {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text !== undefined) n.textContent = text;
  return n;
};

// state is what a refresh must NOT throw away. The page re-renders every 15s
// with replaceChildren, so anything a reader opened lives here or it collapses
// under them mid-read: `open` is the panels, `stories`/`cont` are a sprint
// card's two disclosures, and `story` is the one story whose body is showing.
const state = { all: false, open: {}, stories: {}, cont: {}, story: {}, runs: [], runbook: "" };

// writeHash mirrors the two things a reader can deep-link into the URL: the
// history toggle and the open runbook. Keep it the one writer so a reload
// lands the reader where they were.
function writeHash() {
  const q = new URLSearchParams();
  if (state.all) q.set("all", "1");
  if (state.runbook) q.set("runbook", state.runbook);
  const h = q.toString();
  history.replaceState(null, "", h ? "#" + h : "#");
}

// disclose wires a toggle button to its body and REMEMBERS the answer under
// key, so the next render restores it. Every disclosure on this page goes
// through it — the story list forgot to, which is why an opened story list
// collapsed on the next poll while the story body it was opened for stayed up.
function disclose(btn, body, bag, key) {
  const open = !!bag[key];
  body.hidden = !open;
  btn.classList.toggle("open", open);
  btn.addEventListener("click", () => {
    body.hidden = !body.hidden;
    bag[key] = !body.hidden;
    btn.classList.toggle("open", !body.hidden);
  });
}

// ---- formatting --------------------------------------------------------------

function dur(secs) {
  if (!secs || secs < 0) return "";
  if (secs < 60) return secs + "s";
  if (secs < 3600) return Math.round(secs / 60) + "m";
  if (secs < 86400) return (secs / 3600).toFixed(1) + "h";
  return Math.round(secs / 86400) + "d";
}

// stateClass maps a run/sprint state onto the three things a steward is
// actually sorting for: needs me, running, finished. Anything unrecognised
// stays neutral rather than being guessed into a bucket.
const NEEDS = new Set(["submitted", "review", "failed", "blocked"]);
const LIVE = new Set(["working", "doing", "allocated", "running"]);
function stateClass(s) {
  s = (s || "").toLowerCase();
  if (NEEDS.has(s)) return "needs";
  if (LIVE.has(s)) return "live";
  if (["done", "closed", "cancelled", "canceled", "merged", "abandoned", "killed", "no-op"].includes(s)) return "past";
  return "";
}

// ---- rendering ---------------------------------------------------------------

function stat(label, value, cls) {
  const n = el("div", "bd-stat" + (cls ? " " + cls : ""));
  n.append(el("div", "v", String(value)));
  n.append(el("div", "k", label));
  return n;
}

// The Sprint and Meet apps share the launcher's <base href>, including when
// the launcher itself is reached through an outpost proxy. Build links from
// that base rather than spelling a root-relative /meet/ that would jump out of
// the mounted app on a remote host.
function meetHref(kind, ref) {
  const target = url("meet/");
  target.searchParams.set(kind, ref);
  return target.href;
}

// The draft for an EXISTING sprint that has nobody. It carries the sprint's id
// and title because the agent on the other end has neither, and it names the
// command that actually installs a manager rather than describing the goal in
// the abstract. Like every draft on this page it is EDITABLE and is never sent
// by opening the link.
function unassignedDraft(sp) {
  return "Sprint #" + sp.id + " (" + (sp.title || "untitled") +
    ") has no project manager, so nobody is accountable for it and no name can be addressed. " +
    "Help me pick a suitable agent from `bashy agents list` and install it with " +
    "`bashy sprint take " + sp.id + " --owner <agent>`.";
}

const NEW_SPRINT_DRAFT = "Create a new sprint. Help me define its title, project manager, scope, stories, acceptance criteria, and gate, then use bashy sprint to create and start it.";

// THE COUNTERPART IS CHOSEN, NOT ASSUMED, and that is the whole answer to
// "which agent does a new sprint talk to". A 1:1 needs a counterpart and a
// sprint that does not exist yet has no manager by definition — so routing to
// a steward seat or a hardcoded default would name an agent the operator never
// picked. `chat=1` opens Chat's agent PICKER and the chosen agent becomes the
// 1:1, with the draft carried into it intact. The chooser IS the 1:1 path
// here; what was wrong was the glyph in front of it.
function newSprintLink() {
  const a = conversationLink("chat", "1", "Create a new sprint: chat 1:1 with an agent");
  const target = new URL(a.href);
  target.searchParams.set("draft", NEW_SPRINT_DRAFT);
  a.href = target.href;
  a.classList.add("new-sprint-link");
  a.append(el("span", null, "New sprint"));
  return a;
}

// THE GLYPH MUST AGREE WITH THE DESTINATION, and it did not.
//
// Two marks, and which one applies is a question about the DESTINATION, not
// about the parameter name. A hash is the channel mark — this page already
// uses it for "Everyone" in the composer's recipient menu — and a Meet room
// IS a channel, so `room` keeps it. A speech bubble is the conversation mark.
//
// `chat` fell through to the hash because the branch tested only for `dm`. So
// the one control that starts a CONVERSATION with an agent was drawn as a
// broadcast marker: the icon said "everyone", the link meant "one agent".
const CHANNEL_MARK = "M4 9h16M4 15h16M10 3 8 21M16 3l-2 18";
const CONVERSATION_MARK = "M21 15a4 4 0 0 1-4 4H8l-5 3V7a4 4 0 0 1 4-4h10a4 4 0 0 1 4 4z";

function conversationLink(kind, ref, label) {
  const a = el("a", "conversation-link");
  a.href = meetHref(kind, ref);
  a.title = label;
  a.setAttribute("aria-label", label);
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("viewBox", "0 0 24 24");
  svg.setAttribute("aria-hidden", "true");
  svg.setAttribute("fill", "none");
  svg.setAttribute("stroke", "currentColor");
  svg.setAttribute("stroke-width", "1.9");
  svg.setAttribute("stroke-linecap", "round");
  svg.setAttribute("stroke-linejoin", "round");
  const path = document.createElementNS("http://www.w3.org/2000/svg", "path");
  path.setAttribute("d", kind === "room" ? CHANNEL_MARK : CONVERSATION_MARK);
  svg.append(path);
  a.append(svg);
  return a;
}

function renderSummary(d) {
  const s = d.summary || {};
  const host = d.resources || {};
  const cells = [
    stat("sprints", d.sprint_total, ""),
    stat("runs", d.run_total, ""),
    stat("needs steward", s.needs_steward, s.needs_steward ? "needs" : ""),
    stat("in flight", s.in_flight, s.in_flight ? "live" : ""),
    stat("todos", s.todos, ""),
    stat("median eta", dur(s.eta_median_seconds) || "—", ""),
  ];
  if (host.cpu) cells.push(stat("cpu", Math.round(host.cpu.usage_percent) + "%", ""));
  if (host.memory) cells.push(stat("memory", host.memory.used_percent + "%", ""));
  if (host.disks && host.disks.length) {
    const worst = host.disks.reduce((n, disk) => Math.max(n, Number(disk.used_percent) || 0), 0);
    cells.push(stat("disk", Math.round(worst) + "%", ""));
  }
  $("bd-summary").replaceChildren(...cells);

  const u = d.utilization;
  const v = $("bd-verdict");
  if (u && u.verdict) {
    v.textContent = u.verdict;
    v.className = u.verdict === "SATURATED" ? "needs" : "";
    v.title = u.reason || "";
  } else {
    v.textContent = "";
    v.title = "";
  }
}

function renderWarnings(d) {
  const host = $("bd-warnings");
  const ws = d.warnings || [];
  host.hidden = ws.length === 0;
  if (!ws.length) return;
  // A source that failed is reported, never swallowed: a board missing a whole
  // source silently looks exactly like a board with nothing in it.
  host.replaceChildren(
    el("strong", null, ws.length + " source warning" + (ws.length === 1 ? "" : "s")),
    ...ws.map((w) => el("div", "w", w)),
  );
}

function card(c) {
  const n = el("article", "bd-card " + stateClass(c.state));
  const head = el("header");
  head.append(el("span", "id", "#" + c.id));
  head.append(el("span", "st", c.state || ""));
  if (c.tool) head.append(el("span", "tool", c.tool + (c.band ? " L" + c.band : "")));
  head.append(el("span", "spacer"));
  if (c.elapsed_seconds) head.append(el("span", "t", dur(c.elapsed_seconds)));
  n.append(head);
  n.append(el("p", "label", c.label || ""));
  const meta = [];
  if (c.scope) meta.push(c.scope);
  if (c.unmerged_commits) meta.push(c.unmerged_commits + " unmerged");
  if (c.salvageable) meta.push("salvageable");
  if (c.stale) meta.push("stale");
  if (meta.length) n.append(el("div", "meta", meta.join(" · ")));
  return n;
}

// Conductors write the continuity brief as blank-line-separated paragraphs led
// by an ALL-CAPS label — STATE:, BLOCKED, NEXT ACTION:, GATES GREEN:. Sprint 84
// is 17.9 kB and nine such sections. As one <pre> that is a wall nobody reads;
// as its sections it is the status report it was written as. A short
// single-paragraph brief (most sprints) has no labels and falls through as one
// plain block, which is right for it.
const SECTION_LABEL = /^([A-Z][A-Z0-9 ,/+&()'-]{2,40}):\s*/;

function continuitySections(text) {
  return String(text || "")
    .split(/\n\s*\n/)
    .map((p) => p.trim())
    .filter(Boolean)
    .map((p) => {
      const m = SECTION_LABEL.exec(p);
      return m ? { label: m[1], body: p.slice(m[0].length) } : { label: "", body: p };
    });
}

// The linked runs are the sprint's ITEMS. They were rendered as a bare count
// ("21 runs"), which says a sprint is busy without saying what it spans — and
// sprint 82's 21 refs cross several repos, which is the useful part.
function runRefsEl(refs) {
  const wrap = el("div", "refs");
  wrap.append(el("span", "k", "runs"));
  const shown = refs.slice(0, 24);
  for (const r of shown) wrap.append(el("span", "ref", (r.repo || "?") + "#" + r.id));
  if (refs.length > shown.length) {
    wrap.append(el("span", "more", "+" + (refs.length - shown.length) + " more"));
  }
  return wrap;
}

// The RUNBOOKS a story cites are its procedure — the story carries this
// iteration's values, the runbook the reusable steps (`Runbook: [[kb:<slug>]]`
// in the body). Only kb refs of type runbook are runbooks; a cited lesson is
// not one and must not be labelled one. A dangling kb ref is shown in the
// warning colour so a typo in a slug is visible rather than silently absent.
// runbookChip is the one shape a runbook is named by on this page: the
// ring-local seq (`#12`, the handle a human types as kb:12) beside the slug.
// A slug is a sentence in kebab-case and long ones used to stretch the chip row
// across the card, so the slug is CLIPPED to a fixed width with an ellipsis
// (app.css .ref .slug) and the tooltip carries the complete kb:<slug> — hover
// to read what the chip cut off. A page written before seqs were minted has
// none and shows the slug alone.
function runbookChip(slug, seq, tip, cls) {
  const chip = el("button", "ref link rb" + (cls ? " " + cls : ""));
  chip.type = "button";
  if (seq) chip.append(el("span", "seq", "#" + seq), " ");
  const s = el("span", "slug", slug);
  chip.append(s);
  chip.title = ["kb:" + slug + (seq ? " (kb:" + seq + ")" : "")].concat(tip.filter(Boolean)).join(" — ");
  chip.addEventListener("click", () => openRunbook(slug));
  return chip;
}

function runbookRefsEl(outbound) {
  const refs = (outbound || []).filter((r) => typeof r.ref === "string" && r.ref.startsWith("kb:")
    && (r.type === "runbook" || r.status === "dangling"));
  if (!refs.length) return null;
  const wrap = el("div", "refs");
  wrap.append(el("span", "k", "runbooks"));
  for (const r of refs) {
    const slug = r.ref.slice(3);
    if (r.status === "dangling") {
      const chip = el("span", "ref needs", slug);
      chip.title = "kb:" + slug + " — not found in the tracked repos' kb";
      wrap.append(chip);
      continue;
    }
    wrap.append(runbookChip(slug, r.seq, [r.title], ""));
  }
  return wrap;
}

// The linked STORIES are what a sprint is FOR, and this page showed none of
// them: the overview payload carried sprints and runs and dropped todos
// entirely, so every card rendered as a title with no work under it. A sprint
// whose stories are invisible reads as an empty sprint.
//
// Rendered in two registers, both reusing the card's existing classes so this
// costs no CSS: an always-visible chip line (what runRefsEl does for runs, so a
// scan sees at once that a card has seven stories, not zero) and a collapsible
// list carrying each story's status, priority and title.
// A STORY BODY IS ORDINARY MARKDOWN, and it was being read as a continuity
// brief. continuitySections below splits on blank lines and treats a leading
// ALL-CAPS token before a colon as a section label — exactly right for the
// brief a conductor writes, and wrong for everything else. Pushed through it, a
// story body lost its headings, its lists and its code fences, and any
// paragraph that happened to open with capitals and a colon was silently
// relabelled. The bodies in this repo carry indented command transcripts;
// those were the worst casualties.
//
// So the two callers are separated. The card's continuity keeps the splitter.
// A story body gets this: a small block renderer, no dependency, no framework.
//
// SAFE BY CONSTRUCTION. Bodies are written by agents, so nothing here ever
// touches innerHTML — every character reaches the page as a text node. That is
// not a hardening pass bolted on afterwards; it is why this is a DOM builder
// rather than a markdown-to-HTML string.
const FENCE = /^\s*```/;
const HEADING = /^(#{1,6})\s+(.*)$/;
const BULLET = /^\s*([-*+]|\d+[.)])\s+/;

function storyBodyEl(text) {
  const host = el("div", "story-body");
  const lines = String(text || "").split("\n");
  let i = 0;
  // A block is emitted only when its shape is recognised; anything else falls
  // through to a paragraph, which is what unstructured prose should be.
  while (i < lines.length) {
    const line = lines[i];
    if (FENCE.test(line)) {
      // A FENCE IS VERBATIM to its closer, including blank lines and the
      // indentation that carries meaning inside it. An unterminated fence runs
      // to the end of the body rather than being abandoned — a truncated code
      // block is the failure this story is about.
      const buf = [];
      i++;
      while (i < lines.length && !FENCE.test(lines[i])) buf.push(lines[i++]);
      if (i < lines.length) i++;
      host.append(el("pre", "story-code", buf.join("\n")));
      continue;
    }
    const h = HEADING.exec(line);
    if (h) {
      host.append(el("div", "story-h h" + h[1].length, h[2].trim()));
      i++;
      continue;
    }
    if (BULLET.test(line)) {
      const list = el("ul", "story-list");
      while (i < lines.length && BULLET.test(lines[i])) {
        list.append(el("li", null, lines[i].replace(BULLET, "")));
        i++;
      }
      host.append(list);
      continue;
    }
    if (/^\s{4,}\S/.test(line)) {
      // An INDENTED BLOCK is a transcript. Keeping it as one <pre> preserves
      // the alignment that makes command output readable; splitting it into
      // paragraphs is what destroyed it.
      const buf = [];
      while (i < lines.length && (/^\s{4,}/.test(lines[i]) || lines[i].trim() === "")) {
        // A trailing run of blank lines belongs to the gap after the block,
        // not inside it.
        if (lines[i].trim() === "" && !/^\s{4,}\S/.test(lines[i + 1] || "")) break;
        buf.push(lines[i++]);
      }
      host.append(el("pre", "story-code", buf.join("\n")));
      continue;
    }
    if (line.trim() === "") {
      i++;
      continue;
    }
    // A PARAGRAPH is its consecutive non-blank lines, joined — markdown's own
    // rule, and the one that keeps a wrapped sentence a sentence.
    const buf = [];
    while (i < lines.length && lines[i].trim() !== "" && !FENCE.test(lines[i]) &&
           !HEADING.test(lines[i]) && !BULLET.test(lines[i]) && !/^\s{4,}\S/.test(lines[i])) {
      buf.push(lines[i].trim());
      i++;
    }
    host.append(el("p", "story-p", buf.join(" ")));
  }
  if (!host.childNodes.length) host.append(el("p", "story-p", "(no body)"));
  return host;
}

// WHO IS ACTUALLY RUNNING THIS, and the honest answer is often "cannot tell".
//
// A weave run does NOT record the story it executes — board.Run carries
// agent/model/band/state and a sprint id, board.Todo carries an assignee, and
// nothing joins them. So the only correlation available is by AGENT IDENTITY
// within the sprint, and it is reported as a correlation, never as a fact the
// records assert.
//
// Its limit is stated rather than hidden: if one agent holds two runs in the
// same sprint, which run belongs to which story is genuinely unknown, and
// saying "ambiguous" is the only truthful answer. Picking the first would be a
// guess an operator would then act on.
function runsForStory(story) {
  const who = String(story.assignee || "").trim().toLowerCase();
  if (!who) return [];
  return (state.runs || []).filter((r) =>
    String(r.agent || "").trim().toLowerCase() === who &&
    (!story.sprint || !r.sprint_id || r.sprint_id === story.sprint));
}

// workerRows renders the worker line and, when a run can be correlated, what
// that run is doing. Every branch says which of the three states it is in.
function workerRows(d) {
  const rows = [];
  const who = d.assignee && !/^(unassigned|-|none)$/i.test(d.assignee) ? d.assignee : "";
  if (!who) {
    const row = el("div", "sec");
    row.append(el("div", "sec-k", "worker"));
    row.append(el("div", "sec-v", "unassigned — nobody is working this story"));
    rows.push(row);
    return rows;
  }
  const row = el("div", "sec");
  row.append(el("div", "sec-k", "worker"));
  row.append(el("div", "sec-v", "@" + who));
  rows.push(row);

  const runs = runsForStory(d);
  const run = el("div", "sec");
  run.append(el("div", "sec-k", "run"));
  if (runs.length === 1) {
    const r = runs[0];
    const parts = [
      (r.repo || "?") + "#" + r.id,
      r.state || "",
      [r.tool, r.model].filter(Boolean).join(":"),
      r.band ? "L" + r.band : "",
      r.elapsed_seconds ? dur(r.elapsed_seconds) : "",
      r.stale ? "STALE" : "",
    ].filter(Boolean);
    run.append(el("div", "sec-v " + stateClass(r.state), parts.join(" · ")));
  } else if (runs.length > 1) {
    // AMBIGUOUS, and named as such. Two runs under one agent in one sprint
    // cannot be told apart from here.
    run.append(el("div", "sec-v needs",
      runs.length + " runs held by @" + who + " in this sprint — cannot tell which is this story"));
  } else {
    // NOT A FAILURE. A story can be assigned and worked without a weave run at
    // all, and a run does not name its story, so "no run" is a report about the
    // records rather than about the work.
    run.append(el("div", "sec-v", "no run correlated — a run does not record the story it executes"));
  }
  rows.push(run);
  return rows;
}

// storyDetail fetches ONE story's full record. Cached per id for the life of
// the page: a body does not change under a reader, and a second click on the
// same story should not re-hit the host.
const storyCache = new Map();
async function storyDetail(id) {
  if (storyCache.has(id)) return storyCache.get(id);
  const p = fetch(url("api/sprint/story/" + encodeURIComponent(id)))
    .then(async (r) => {
      const d = await r.json().catch(() => ({}));
      if (!r.ok) throw new Error(d.error || "HTTP " + r.status);
      return d;
    });
  storyCache.set(id, p);
  // A failed fetch must not poison the cache — the next click should retry
  // rather than replay the error forever.
  p.catch(() => storyCache.delete(id));
  return p;
}

// openStory renders a story's detail INTO an already-visible container. It is
// the one place the page shows a body, so both entry points (a chip and a
// list row) land here and agree.
async function openStory(id, host, sprintID) {
  // Remember it. The page reloads every 15s and re-renders with
  // replaceChildren, so without this a reader's open story vanishes
  // mid-sentence — the panels already solve the same problem with state.open.
  if (sprintID) state.story[sprintID] = id;
  host.hidden = false;
  host.replaceChildren(el("div", "sec-v", "loading #" + id + " …"));
  try {
    const d = await storyDetail(id);
    const rows = [];
    // "unassigned" is todo's placeholder for "nobody", so rendering it as
    // "@unassigned" invents a person. An absent owner shows as nothing.
    const owner = d.assignee && !/^(unassigned|-|none)$/i.test(d.assignee) ? "@" + d.assignee : "";
    const meta = [d.status, d.priority, owner,
      d.sprint ? "sprint #" + d.sprint : "", d.scope]
      .filter(Boolean).join(" · ");
    const head = el("div", "sec");
    head.append(el("div", "sec-k " + stateClass(d.status), "#" + (d.seq || d.id)));
    head.append(el("div", "sec-v", d.title || ""));
    rows.push(head);
    if (meta) {
      const m = el("div", "sec");
      m.append(el("div", "sec-k", "meta"));
      m.append(el("div", "sec-v", meta));
      rows.push(m);
    }
    rows.push(...workerRows(d));
    const rb = runbookRefsEl(d.outbound);
    if (rb) rows.push(rb);
    // THE BODY, whole and in its own shape. Never truncated — a long record
    // scrolls inside its pane, because a detail view that quietly stops
    // mid-sentence is the defect class this page exists to report on.
    rows.push(storyBodyEl(d.body));
    host.replaceChildren(...rows);
  } catch (e) {
    // Name the failure. A detail pane that silently shows nothing is the same
    // defect class this board is meant to report on.
    const row = el("div", "sec");
    row.append(el("div", "sec-k needs", "error"));
    row.append(el("div", "sec-v", String(e.message || e)));
    host.replaceChildren(row);
  }
}

function storiesEl(stories, detailHost, sprintID) {
  const wrap = el("div", "refs");
  wrap.append(el("span", "k", "stories"));
  const shown = stories.slice(0, 24);
  for (const t of shown) {
    // A button, not a span: a thing that responds to a click has to be
    // reachable by keyboard and announce itself as activatable.
    const stage = storyStage(t);
    const chip = el("button", "ref link stage-" + stage + " " + STAGE_CLASS[stage],
      "#" + (t.number || t.id));
    chip.type = "button";
    // The tooltip names the stage AND its evidence, so a colour never has to be
    // decoded from memory.
    chip.title = [t.priority, stage, t.assignee ? "@" + t.assignee : "", t.title]
      .filter(Boolean).join(" · ");
    chip.addEventListener("click", () => openStory(t.id, detailHost, sprintID));
    wrap.append(chip);
  }
  if (stories.length > shown.length) {
    wrap.append(el("span", "more", "+" + (stories.length - shown.length) + " more"));
  }
  return wrap;
}

// A STORY HAS STAGES, and the card knew only two of them. storyIsClosed sorted
// every story into done-or-not, so one with a worker on it right now rendered
// identically to one nobody had touched — and "what is actually moving?" is the
// question a scan of this board is asking.
//
// Five stages, each sourced from a field that actually exists. There is no
// guessing here and there deliberately cannot be: a weave run does not name the
// story it executes, so a story's worker is read from the STORY's own assignee
// and never inferred from a nearby run.
//
//   closed     done / closed / cancelled
//   needs      submitted / review / failed / blocked — waiting on a person
//   working    doing — someone is on it
//   assigned   named owner, not started yet
//   unstarted  nobody, nothing
function storyStage(t) {
  const st = String(t.status || "").toLowerCase();
  if (storyIsClosed(t)) return "closed";
  if (NEEDS.has(st)) return "needs";
  if (LIVE.has(st)) return "working";
  return t.assignee ? "assigned" : "unstarted";
}

// The stage's CSS class. `needs`, `live` and `past` already exist and are
// reused rather than duplicated; the two new ones are the states the card
// could not previously express.
const STAGE_CLASS = {
  closed: "past",
  needs: "needs",
  working: "live",
  assigned: "assigned",
  unstarted: "unstarted",
};

function storyIsClosed(story) {
  return ["done", "closed", "cancelled", "canceled"].includes(String(story.status || "").toLowerCase());
}

function storyStats(sp, stories) {
  const derivedClosed = stories.filter(storyIsClosed).length;
  const total = Number.isInteger(sp.story_total) ? sp.story_total : stories.length;
  const closed = Number.isInteger(sp.story_closed) ? sp.story_closed : derivedClosed;
  const open = Number.isInteger(sp.story_open) ? sp.story_open : Math.max(0, total - closed);
  return { total, closed, open };
}

function storyListEl(sp, stories, detailHost, sprintID) {
  const stats = storyStats(sp, stories);
  const btn = el("button", "more", "stories — " + stats.open + " open · " +
    stats.closed + " closed of " + stats.total);
  btn.type = "button";
  const body = el("div", "continuity");
  const groups = [
    ["open", stories.filter((t) => !storyIsClosed(t))],
    ["closed", stories.filter(storyIsClosed)],
  ];
  for (const [label, items] of groups) {
    if (!items.length) continue;
    body.append(el("div", "story-group", label + " — " + items.length));
    for (const t of items) {
      const stage = storyStage(t);
      const row = el("button", "sec link stage-" + stage + " " + STAGE_CLASS[stage]);
      row.type = "button";
      row.append(el("div", "sec-k " + STAGE_CLASS[stage],
        "#" + (t.number || t.id) + (t.priority ? " " + t.priority : "")));
      row.append(el("div", "sec-v", t.title || ""));
      // The worker is named on the ROW, not only in the detail: an operator
      // scanning for "who has what" should not have to open seven stories.
      if (t.assignee) row.append(el("div", "sec-who", "@" + t.assignee));
      row.addEventListener("click", () => openStory(t.id, detailHost, sprintID));
      body.append(row);
    }
  }
  disclose(btn, body, state.stories, sprintID);
  return [btn, body];
}

// A SPRINT card. The sprints are the reason this page exists — the lanes below
// are runs, and a run is the execution of one item, not the time-boxed set a
// conductor drives.
function sprintEl(sp, stories) {
  stories = stories || [];
  const n = el("article", "bd-sprint " + stateClass(sp.column));
  const head = el("header");
  head.append(el("span", "id", "#" + sp.id));
  head.append(el("span", "st", sp.column || ""));
  if (sp.epic) head.append(el("span", "epic", sp.epic));
  head.append(el("span", "spacer"));
  if (sp.gate_state) {
    // The gate is the whole point of a sprint: entity.Sprint.CanConverge is
    // false without one, so its state is never decoration.
    head.append(el("span", "gate " + (sp.gate_state === "complete" ? "past" : "needs"), sp.gate_state));
  }
  // Progress belongs in the HEAD, not only on the disclosure that reveals the
  // list. Whether a sprint has three stories left or thirty is the question a
  // scan of the column is asking, and it should not cost a click per card.
  const headStats = storyStats(sp, stories || []);
  if (headStats.total) {
    const chip = el("span", "stories" + (headStats.open ? "" : " past"),
      headStats.open + " open / " + headStats.closed + " closed");
    chip.title = headStats.closed + " of " + headStats.total + " stories complete";
    head.append(chip);
  }
  n.append(head);

  // THE PLAN. A sprint's spec/handoff document is on the record — `sprint show`
  // prints it as "spec:" — and the card showed nothing, so the one document
  // that says what the sprint is FOR was invisible from the browser.
  //
  // RENDERED AS A REFERENCE, NOT AN ANCHOR, and that is the decision rather
  // than an omission. SpecRef is REPO-RELATIVE ("docs/plan.md") and a sprint
  // spans repos by definition, so there is no one root to resolve it against;
  // the Files panel is optional, scoped to home by default, and this page is
  // reached both on loopback and under outpost's /matrix/h/<host>/app/<name>/
  // prefix. An href built from any of that is a link that works on one host and
  // 404s on another, which is worse than a path — and inventing a file-serving
  // route for it is a data plane this read-only page must not grow.
  //
  // So: the path, selectable and labelled, next to the title where the reader
  // already is. A sprint with no spec gains no row at all.
  const planRow = (sp) => {
    if (!sp.spec_ref) return null;
    const row = el("div", "meta plan");
    row.append(el("span", "k", "plan"));
    const ref = el("code", "plan-ref", sp.spec_ref);
    // Selectable by click, so copying it costs one gesture and no dependency.
    ref.tabIndex = 0;
    ref.title = "The sprint's plan document, relative to its repo — select to copy";
    ref.addEventListener("click", () => {
      const sel = window.getSelection();
      const range = document.createRange();
      range.selectNodeContents(ref);
      sel.removeAllRanges();
      sel.addRange(range);
    });
    row.append(ref);
    return row;
  };

  const title = el("div", "sprint-title");
  title.append(el("p", "label", sp.title || ""));
  if (sp.meet_room_ref) {
    title.append(conversationLink(
      "room",
      sp.meet_room_ref,
      "Open sprint " + sp.id + " Meet room",
    ));
  }
  n.append(title);

  // WHO IS ACCOUNTABLE — and an UNOWNED sprint is not a sprint with one less
  // field, it is a sprint that CANNOT BE ADDRESSED. Rendering absence as
  // absence hid the single most actionable fact on the card, on the one
  // surface that sees every sprint at once.
  //
  // Three states, and they are not two: an owned sprint names its manager, a
  // STALE lease names a manager nothing is currently driving, and an unowned
  // sprint names nobody. The stale label was already here and keeps its own
  // wording — a dead conductor is not the same problem as no conductor.
  const manager = sp.manager || sp.conductor || sp.lease_holder;
  const meta = el("div", "meta manager" + (manager ? "" : " unassigned"));
  if (manager) {
    meta.append(document.createTextNode(
      (sp.lease_stale ? "lease STALE — " : "project manager ") + manager,
    ));
    meta.append(conversationLink("dm", manager, "Chat 1:1 with " + manager));
  } else {
    meta.append(document.createTextNode("project manager — unassigned"));
    // The same control as New sprint, for the same reason: staffing a sprint
    // is a conversation with an agent, and the operator should not have to
    // leave the board to have it. chat=1 opens Chat's picker, so the
    // counterpart is CHOSEN rather than assumed — this sprint has no manager
    // by definition, so there is nobody to address yet.
    const a = conversationLink("chat", "1", "Assign a project manager to sprint " + sp.id);
    const target = new URL(a.href);
    target.searchParams.set("draft", unassignedDraft(sp));
    a.href = target.href;
    a.classList.add("assign-manager-link");
    meta.append(a);
  }
  n.append(meta);

  const plan = planRow(sp);
  if (plan) n.append(plan);

  const refs = sp.run_refs || [];
  if (refs.length) n.append(runRefsEl(refs));

  if (stories.length) {
    // One detail pane per card, shared by both entry points, so clicking a
    // second story replaces the first rather than stacking panes nobody closes.
    const detail = el("div", "continuity story-detail");
    detail.hidden = true;
    n.append(storiesEl(stories, detail, sp.id));
    n.append(...storyListEl(sp, stories, detail, sp.id));
    n.append(detail);
    // Re-open whatever the reader had open before the last refresh. The body
    // is cached per id, so this costs no request.
    const wasOpen = state.story[sp.id];
    if (wasOpen && stories.some((t) => t.id === wasOpen)) {
      openStory(wasOpen, detail, sp.id);
    }
  }

  const sections = continuitySections(sp.continuity);
  if (sections.length) {
    const labelled = sections.filter((x) => x.label).length;
    const btn = el("button", "more", "continuity — " +
      (labelled ? labelled + " sections" : sections.length + " note" + (sections.length === 1 ? "" : "s")));
    btn.type = "button";
    const body = el("div", "continuity");
    for (const sec of sections) {
      const row = el("div", "sec");
      if (sec.label) row.append(el("div", "sec-k", sec.label));
      row.append(el("div", "sec-v", sec.body));
      body.append(row);
    }
    disclose(btn, body, state.cont, sp.id);
    n.append(btn, body);
  }
  return n;
}

// storiesBySprint indexes the todos onto their card. A story whose sprint is 0
// is UNLINKED, which is a perfectly ordinary item — todo does not require a
// sprint — so it simply does not appear under any card.
function storiesBySprint(todos) {
  const by = new Map();
  for (const t of todos || []) {
    const id = t.sprint_id;
    if (!id) continue;
    if (!by.has(id)) by.set(id, []);
    by.get(id).push(t);
  }
  return by;
}

function renderSprints(d) {
  const host = $("bd-sprints");
  const sps = d.sprints || [];
  const by = storiesBySprint(d.todos);
  const h = el("header");
  h.append(el("span", "t", "Sprints"));
  h.append(el("span", "n", sps.length + (d.all ? "" : " of " + d.sprint_total)));
  h.append(newSprintLink());
  const body = el("div", "cards");
  if (!sps.length) {
    body.append(el("p", "empty", d.all ? "No sprints." : "No sprint is open. Tick “include history” for the finished ones."));
  } else {
    for (const sp of sps) body.append(sprintEl(sp, by.get(sp.id)));
  }
  host.replaceChildren(h, body);
}

function renderLanes(d) {
  const host = $("bd-lanes");
  const lanes = d.lanes || [];
  if (!lanes.length) {
    host.replaceChildren(el("p", "empty", "No runs."));
    return;
  }
  host.replaceChildren(...lanes.map((lane) => {
    const col = el("section", "bd-lane");
    const h = el("header");
    h.append(el("span", "t", lane.title));
    h.append(el("span", "n", String((lane.cards || []).length)));
    col.append(h);
    const body = el("div", "cards");
    for (const c of lane.cards || []) body.append(card(c));
    if (lane.dropped) body.append(el("p", "dropped", "+" + lane.dropped + " more"));
    if (!(lane.cards || []).length) body.append(el("p", "empty", "empty"));
    col.append(body);
    return col;
  }));
}

function table(columns, rows) {
  const t = el("table");
  const thead = el("thead");
  const hr = el("tr");
  for (const c of columns || []) hr.append(el("th", null, c));
  thead.append(hr);
  t.append(thead);
  const tb = el("tbody");
  for (const r of rows || []) {
    const tr = el("tr");
    for (const cell of r) tr.append(el("td", null, cell));
    tb.append(tr);
  }
  t.append(tb);
  const wrap = el("div", "tw");
  wrap.append(t);
  return wrap;
}

async function loadPanelRows(p, body, more) {
  // The full row set is fetched only when a reader opens the panel. The dag
  // panel alone is 8,779 rows / 1.5 MB here; shipping that on every poll is how
  // a read panel turns into a data plane.
  more.disabled = true;
  more.textContent = "loading…";
  try {
    const d = await fetch(url("api/sprint/panel/" + encodeURIComponent(p.id) + "?limit=500")).then((r) => r.json());
    body.replaceChildren(table(d.columns, d.rows));
    if (d.row_total > (d.rows || []).length) {
      body.append(el("p", "dropped", "showing " + d.rows.length + " of " + d.row_total));
    }
    more.remove();
  } catch (_) {
    more.disabled = false;
    more.textContent = "load all — retry";
  }
}

function renderPanels(d) {
  const host = $("bd-panels");
  host.replaceChildren(...(d.panels || []).map((p) => {
    const sec = el("section", "bd-panel");
    const h = el("button", "bd-panel-head");
    h.type = "button";
    h.append(el("span", "t", p.title));
    h.append(el("span", "c", p.collapsed || ""));
    h.append(el("span", "spacer"));
    h.append(el("span", "n", p.row_total ? String(p.row_total) : ""));
    sec.append(h);

    const body = el("div", "bd-panel-body");
    body.hidden = !state.open[p.id];
    if (!p.row_total) {
      body.append(el("p", "empty", "nothing to show"));
    } else {
      body.append(table(p.columns, p.rows));
      if (p.row_total > (p.rows || []).length) {
        const more = el("button", "btn", "load all " + p.row_total);
        more.type = "button";
        more.addEventListener("click", () => loadPanelRows(p, body, more));
        body.append(more);
      }
    }
    h.addEventListener("click", () => {
      body.hidden = !body.hidden;
      state.open[p.id] = !body.hidden;
    });
    sec.append(body);
    return sec;
  }));
}

function renderMeta(d) {
  $("bd-age").textContent = d.age_seconds > 0 ? "collected " + dur(d.age_seconds) + " ago" : "just collected";
  $("bd-scope").textContent = d.all
    ? "everything, history included — " + d.sprint_total + " sprints, " + d.run_total + " runs"
    : d.sprints.length + " of " + d.sprint_total + " sprints and " +
      d.runs.length + " of " + d.run_total + " runs are live";
  $("bd-foot").textContent =
    d.title + " · " + d.scope + " · re-collected at most every " + dur(d.ttl_seconds) +
    " (it forks a subprocess per weave queue root, so it is not free). " +
    "This view never starts, merges, kills, or leases anything — `bashy sprint board` in a browser.";
}

// ---- loading -----------------------------------------------------------------

let inflight = null;

async function load() {
  if (inflight) return;
  const q = state.all ? "?all=1" : "";
  inflight = fetch(url("api/sprint" + q)).then((r) => r.json());
  let d;
  try {
    d = await inflight;
  } catch (_) {
    return;
  } finally {
    inflight = null;
  }
  if (d.error) {
    $("bd-summary").replaceChildren(el("p", "empty", d.error));
    return;
  }
  renderWarnings(d);
  renderSummary(d);
  state.runs = d.runs || [];
  renderSprints(d);
  renderRunbooks();
  renderLanes(d);
  renderPanels(d);
  renderMeta(d);
}

// runbookDetail fetches ONE runbook page, cached per slug like storyDetail:
// the poll re-renders the section every 15s and must not re-read the page.
// (Server-side the read is Store.Load, never RecordOpen, for the same reason.)
const runbookCache = new Map();
async function runbookDetail(slug) {
  if (runbookCache.has(slug)) return runbookCache.get(slug);
  const p = fetch(url("api/sprint/runbook/" + encodeURIComponent(slug)))
    .then(async (r) => {
      const d = await r.json().catch(() => ({}));
      if (!r.ok) throw new Error(d.error || "HTTP " + r.status);
      return d;
    });
  runbookCache.set(slug, p);
  p.catch(() => runbookCache.delete(slug));
  return p;
}

// openRunbook renders a runbook's body into the Runbooks section's one pane.
// Both entry points — a story's runbook chip and the section's own list —
// land here, so the page has exactly one place a procedure is shown.
async function openRunbook(slug) {
  state.runbook = slug;
  writeHash();
  const host = $("bd-runbooks").querySelector(".story-detail");
  if (!host) return;
  host.hidden = false;
  host.replaceChildren(el("div", "sec-v", "loading kb:" + slug + " …"));
  try {
    const d = await runbookDetail(slug);
    const head = el("div", "sec");
    head.append(el("div", "sec-k", "kb:" + d.slug + (d.seq ? "  ·  #" + d.seq : "")));
    head.append(el("div", "sec-v", d.title || ""));
    const meta = [d.status, d.ring, (d.tags || []).join(", ")].filter(Boolean).join(" · ");
    const m = el("div", "sec");
    m.append(el("div", "sec-k", "meta"));
    m.append(el("div", "sec-v", meta));
    host.replaceChildren(head, m, storyBodyEl(d.body));
  } catch (e) {
    const row = el("div", "sec");
    row.append(el("div", "sec-k needs", "error"));
    row.append(el("div", "sec-v", String(e.message || e)));
    host.replaceChildren(row);
  }
}

// renderRunbooks is the one Runbooks section: the runbook-typed kb pages of
// every repo a live sprint tracks (plus the host ring), as chips, over one
// shared detail pane. An open runbook survives the poll because openRunbook
// is replayed from state after each render — from cache, so no refetch.
async function renderRunbooks() {
  let d;
  try {
    const r = await fetch(url("api/sprint/runbooks"));
    d = await r.json();
    if (!r.ok) throw new Error(d.error || "HTTP " + r.status);
  } catch (e) {
    $("bd-runbooks").replaceChildren(el("p", "empty", String(e.message || e)));
    return;
  }
  const rows = d.runbooks || [];
  const card = el("article", "bd-sprint");
  if (!rows.length) {
    card.append(el("p", "empty", "No runbooks — author one with: bashy kb add --type runbook"));
  } else {
    const wrap = el("div", "refs");
    wrap.append(el("span", "k", "runbooks"));
    for (const rb of rows) {
      wrap.append(runbookChip(rb.slug, rb.seq,
        [[rb.title, rb.description, rb.ring].filter(Boolean).join(" · ")],
        rb.slug === state.runbook ? "open" : ""));
    }
    card.append(wrap);
  }
  const detail = el("div", "continuity story-detail");
  detail.hidden = true;
  card.append(detail);
  $("bd-runbooks").replaceChildren(card);
  if (state.runbook) openRunbook(state.runbook);
}

function init() {
  const params = new URLSearchParams(location.hash.replace(/^#/, ""));
  state.all = params.get("all") === "1";
  state.runbook = params.get("runbook") || "";
  $("f-all").checked = state.all;
  $("f-all").addEventListener("change", () => {
    state.all = $("f-all").checked;
    writeHash();
    load();
  });
  $("f-refresh").addEventListener("click", load);

  load();
  // Half the server's TTL: often enough that the age line stays small, rarely
  // enough that the poll itself is never what triggers a collect.
  setInterval(load, 15000);
}

init();
