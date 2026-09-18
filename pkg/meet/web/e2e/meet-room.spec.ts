import { execFileSync, spawn, type ChildProcess } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import net from "node:net";
import { appendFileSync, cpSync, readFileSync, writeFileSync } from "node:fs";

import { expect, test, type Page } from "@playwright/test";

const webDir = path.resolve(import.meta.dirname, "..");
const repoRoot = path.resolve(webDir, "../../..");
// Deterministic fixtures, not fleet members: addressing a real agent would exec
// its CLI, which needs a network and a key and answers differently every run.
// See e2e/fleet/ — the launch template echoes the prompt back.
const primaryAgent = "echoback-fixed";
const invitedAgent = "echoback-alt";
const facilitatorAgent = "echoback-facilitator";

let server: ChildProcess | undefined;
let baseURL = "";
let meetDir = "";
let binDir = "";
let serverOutput = "";
let fleetDir = "";

/** materializeFleet copies the fixture fleet into a temp dir and renders the
 * tool template, which needs the absolute path of the echoback script. */
function materializeFleet(): string {
  const dir = mkdtempSync(path.join(tmpdir(), "bashy-meet-fleet-"));
  const src = path.join(import.meta.dirname, "fleet");
  cpSync(src, dir, { recursive: true });
  const tmpl = path.join(dir, "tools", "echoback.yaml.tmpl");
  writeFileSync(
    path.join(dir, "tools", "echoback.yaml"),
    readFileSync(tmpl, "utf8").replaceAll("__DIR__", dir),
  );
  rmSync(tmpl, { force: true });
  return dir;
}

test.describe.configure({ mode: "serial" });

test.beforeAll(async () => {
  test.setTimeout(180_000);

  fleetDir = materializeFleet();
  binDir = mkdtempSync(path.join(tmpdir(), "bashy-meet-bin-"));
  meetDir = mkdtempSync(path.join(tmpdir(), "bashy-meet-state-"));
  const binary = path.join(binDir, "bashy-meet-e2e");

  execFileSync("npm", ["run", "build"], {
    cwd: webDir,
    stdio: "inherit",
  });
  // The harness's own server (pkg/meet/web/e2e/serve), not a product binary:
  // cmd/coreutils is the multi-call applet binary and has no meet command, and
  // `bashy` lives in a sibling repo that is not present in a coreutils
  // checkout. Building here is also what makes the test test THIS SPA — the
  // meetspa tag embeds the dist that `npm run build` just produced.
  execFileSync(
    "go",
    ["build", "-tags", "meetspa", "-o", binary, "./pkg/meet/web/e2e/serve"],
    {
      cwd: repoRoot,
      stdio: "inherit",
    },
  );

  const port = await freePort();
  baseURL = `http://127.0.0.1:${port}`;
  server = spawn(
    binary,
    ["-addr", `127.0.0.1:${port}`],
    {
      // A browser-created room defaults its minutes to ./docs/meetings. Keep
      // that real behavior inside the disposable test root: closing a room in
      // an end-to-end test must never dirty the source checkout.
      cwd: meetDir,
      env: {
        ...process.env,
        BASHY_MEET_DIR: meetDir,
        BASHY_FLEET_DIR: fleetDir,
      },
      stdio: ["ignore", "pipe", "pipe"],
    },
  );
  server.stdout?.on("data", (chunk) => {
    serverOutput += chunk.toString();
  });
  server.stderr?.on("data", (chunk) => {
    serverOutput += chunk.toString();
  });

  await waitForHealth();
});

test.afterAll(async () => {
  server?.kill();
  rmSync(meetDir, { recursive: true, force: true });
  rmSync(binDir, { recursive: true, force: true });
  rmSync(fleetDir, { recursive: true, force: true });
});

test("renders room list from the real server and opens a live observe socket", async ({ page }) => {
  const first = unique("Browser seeded alpha");
  const second = unique("Browser seeded beta");
  const seeded = await createRoom(first, [primaryAgent]);
  await postMessage(seeded.id, "e2e-human", "seeded transcript from setup");
  await createRoom(second, [primaryAgent]);

  await openMeet(page);

  await expect(
    page.getByRole("button", { name: new RegExp(first) }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: new RegExp(second) }),
  ).toBeVisible();
  await expect(page.getByText("Demo workspace")).toHaveCount(0);
  const sidebar = page.locator("aside").first();
  await expect(sidebar.locator('[title="open"]')).toBeVisible();
  await expect(page).toHaveTitle("Meet — bashy");
  await expect(page.getByText("bashymeet", { exact: true })).toBeVisible();
  await expect(page.getByText(/Relay/)).toHaveCount(0);
  await expect(page.getByRole("link", { name: "Back to all apps" })).toBeVisible();
});

test("a room deep link opens the requested sprint conversation", async ({ page }) => {
  await createRoom(unique("Different room"), [primaryAgent]);
  const topic = unique("Sprint linked room");
  const room = await createRoom(topic, [primaryAgent]);

  await page.goto(`${baseURL}/?room=${encodeURIComponent(room.id)}`);

  await expect(page.locator("main header").getByText(topic, { exact: true })).toBeVisible();
  await expect(page).toHaveURL(new RegExp(`room=${encodeURIComponent(room.id)}`));
});

test("a manager chat deep link creates and opens the durable 1:1", async ({ page }) => {
  await page.goto(`${baseURL}/?dm=${encodeURIComponent(primaryAgent)}`);

  await expect(page.getByRole("heading", { name: primaryAgent })).toBeVisible();
  await expect(page.getByRole("tab", { name: /Chat/ })).toHaveAttribute("aria-selected", "true");
  await expect(page).toHaveURL(new RegExp(`dm=${primaryAgent}`));
});

// A DRAFT IS A SUGGESTION, and one keystroke takes it.
//
// It used to seed the textarea's value, which made it indistinguishable from
// typing: already what Enter would send, and an operator who wanted their own
// message had to select and delete a paragraph somebody else wrote.
test("a suggested draft is ghost text until Tab accepts it", async ({ page }) => {
  const draft = "Create a new sprint with an explicit project manager.";
  await page.goto(`${baseURL}/?mock=0&chat=1&draft=${encodeURIComponent(draft)}`);

  // The agent picker is modal, so while it is open the tabs behind it are
  // intentionally hidden from the accessibility tree. Assert the selected tab
  // after choosing the recipient.
  const choices = page.getByRole("menuitem", { name: new RegExp(primaryAgent) });
  await expect(choices.first()).toBeVisible();
  await choices.first().click();
  await expect(page.getByRole("tab", { name: /Chat/ })).toHaveAttribute("aria-selected", "true");

  const box = page.getByLabel(`Message ${primaryAgent}`);
  // NOT A VALUE. The suggestion is offered, not installed.
  await expect(box).toHaveValue("");
  await expect(box).toHaveAttribute("placeholder", draft);
  // And it SAYS it is takeable — a placeholder alone does not tell anyone that.
  await expect(page.getByText(/press Tab or Space to use it/)).toBeVisible();

  await box.press("Tab");
  await expect(box).toHaveValue(draft);
  // Spent: the hint is gone, because there is nothing left to accept.
  await expect(page.getByText(/press Tab or Space to use it/)).toHaveCount(0);
});

// SPACE TAKES IT TOO, and typing declines it. The two are one test because the
// rule that makes Space safe is that there is no ghost once anything is typed.
test("Space accepts a suggestion, and typing discards it", async ({ page }) => {
  const draft = "Create a new sprint with an explicit project manager.";
  await page.goto(`${baseURL}/?mock=0&chat=1&draft=${encodeURIComponent(draft)}`);
  const choices = page.getByRole("menuitem", { name: new RegExp(primaryAgent) });
  await expect(choices.first()).toBeVisible();
  await choices.first().click();

  const box = page.getByLabel(`Message ${primaryAgent}`);
  await box.press(" ");
  await expect(box).toHaveValue(draft);

  // A second visit, declined by typing. The ghost does not merge with what was
  // typed and does not come back when the box is emptied again.
  await page.goto(`${baseURL}/?mock=0&chat=1&draft=${encodeURIComponent(draft)}`);
  await expect(choices.first()).toBeVisible();
  await choices.first().click();
  const box2 = page.getByLabel(`Message ${primaryAgent}`);
  await box2.fill("my own words");
  await expect(box2).toHaveValue("my own words");
  await box2.fill("");
  await expect(box2).not.toHaveAttribute("placeholder", draft);
  // Space is an ordinary character again the moment there is no ghost.
  await box2.fill("a");
  await box2.press(" ");
  await expect(box2).toHaveValue("a ");
});

// ARRIVING AT A PAGE MUST NEVER SEND. A draft is a draft until a person takes
// it, and Enter on an unaccepted ghost is not taking it.
test("Enter on an unaccepted suggestion sends nothing", async ({ page }) => {
  const draft = "Create a new sprint with an explicit project manager.";
  await page.goto(`${baseURL}/?mock=0&chat=1&draft=${encodeURIComponent(draft)}`);
  const choices = page.getByRole("menuitem", { name: new RegExp(primaryAgent) });
  await expect(choices.first()).toBeVisible();
  await choices.first().click();

  const box = page.getByLabel(`Message ${primaryAgent}`);
  await box.press("Enter");
  await expect(box).toHaveValue("");
  // Nothing reached the transcript, and the suggestion is still on offer.
  await expect(page.getByText(draft, { exact: true })).toHaveCount(0);
  await expect(box).toHaveAttribute("placeholder", draft);
});

test("creates a room from the sidebar and selects it with one agent seated", async ({ page }) => {
  const topic = unique("Browser created room");
  await openMeet(page);

  await page.getByRole("button", { name: "New meeting" }).click();
  await page.getByLabel("Topic").fill(topic);
  await page.getByLabel("Facilitator").selectOption(facilitatorAgent);
  await page.getByLabel("Participants").selectOption(primaryAgent);
  await page.getByRole("button", { name: "Open meeting" }).click();

  const roomButton = page.getByRole("button", { name: new RegExp(topic) });
  const sidebar = page.locator("aside").first();
  await expect(roomButton).toBeVisible();
  // A prior durable DM may also contain the agent name in its channel title;
  // the roster entry itself is the exact-name element this assertion means.
  await expect(sidebar.getByText(primaryAgent, { exact: true })).toBeVisible();
  await expect(page.getByLabel("Message the room")).toBeEnabled();
});

test("invites an agent from room details and shows it in the roster", async ({ page }) => {
  const topic = unique("Browser invite room");
  await openMeet(page);
  await createRoomFromUI(page, topic, primaryAgent);

  // RoomDetails is mounted twice — the desktop panel and the mobile sheet — so
  // "Invite" matches two buttons, so target the visible desktop details copy.
  const inviteField = page.getByLabel("Agent to invite").first();
  await inviteField.selectOption(invitedAgent);
  await page.getByRole("button", { name: "Invite", exact: true }).first().click();

  const sidebar = page.locator("aside").first();
  await expect(sidebar.getByText(invitedAgent)).toBeVisible();

  await page.getByRole("button", { name: `Remove ${invitedAgent}` }).first().click();
  await expect(sidebar.getByText(invitedAgent)).toHaveCount(0);
});

// The Room actions menu — round / poll / ask / converge / mark, plus the two
// menu items that only pointed elsewhere — is HIDDEN from the web UI. It was
// built for a facilitator driving a floor, and a person typing in a chat window
// is not that; the verbs stay on the CLI, where the conductor uses them.
//
// Asserted as an ABSENCE on purpose. Nothing else fails when a menu quietly
// comes back, and "for now" is exactly the kind of decision that gets undone by
// a merge nobody read.
test("the room actions menu is not offered in the browser", async ({ page }) => {
  const topic = unique("Browser no-actions room");
  await openMeet(page);
  await createRoomFromUI(page, topic, primaryAgent);

  await expect(page.getByRole("button", { name: "Room actions" })).toHaveCount(0);
  for (const gone of ["Hear from everyone", "Take a quick pulse", "Ask the room",
    "Find common ground", "Record decision", "Record action item", "Add agenda item"]) {
    await expect(page.getByRole("menuitem", { name: new RegExp(gone) })).toHaveCount(0);
  }
});

test("reflects room closure and reopening without a reload", async ({ page }) => {
  const topic = unique("Browser lifecycle room");
  await openMeet(page);
  await createRoomFromUI(page, topic, primaryAgent);

  await page.getByRole("button", { name: "Close room" }).first().click();
  await page.getByRole("dialog").getByRole("button", { name: "Close room" }).click();
  await expect(page.getByLabel("Message the room")).toBeDisabled();
  const reopen = page.getByRole("button", { name: "Open room" }).first();
  await expect(reopen).toBeEnabled();
  await reopen.click();
  await expect(page.getByLabel("Message the room")).toBeEnabled();

  // Closing ends the room's WebSocket normally. Reopening must establish a
  // fresh observe stream; otherwise this reply exists only after a page reload.
  await page.getByLabel("Message the room").fill(`@${primaryAgent} after reopen`);
  await page.getByRole("button", { name: "Send message" }).click();
  await expect(page.getByText(/Current question: after reopen/).first()).toBeVisible({
    timeout: 60_000,
  });
});

test("posts a human message through the composer and reloads it from the transcript", async ({ page }) => {
  const topic = unique("Browser transcript room");
  const message = unique("human browser message");
  await openMeet(page);
  await createRoomFromUI(page, topic, primaryAgent);

  // Explicitly to the whole room: the composer's default recipient is the
  // room's owner, and a message to an owner runs that agent's turn rather than
  // landing in the transcript as a human contribution.
  await selectRecipient(page, "Everyone");
  await page.getByLabel("Message the room").fill(message);
  await page.getByRole("button", { name: "Send message" }).click();

  await expect(page.getByText(message)).toBeVisible();
  await page.reload();
  await expect(page.getByText(message)).toBeVisible();
});

test("normalizes a legacy transport turn and reveals raw JSON only on request", async ({ page }) => {
  const topic = unique("Browser legacy transport");
  const seeded = await createRoom(topic, [primaryAgent]);
  const prose = unique("legacy codex answer");
  const raw = [
    JSON.stringify({ type: "thread.started", thread_id: "thread-e2e" }),
    JSON.stringify({
      type: "item.completed",
      item: { id: "item-e2e", type: "agent_message", text: prose },
    }),
    JSON.stringify({
      type: "turn.completed",
      usage: { input_tokens: 10, output_tokens: 3 },
    }),
  ].join("\n");
  appendFileSync(
    path.join(meetDir, seeded.id, "transcript.jsonl"),
    `${JSON.stringify({
      round: 1,
      speaker: primaryAgent,
      role: "participant",
      kind: "turn",
      text: raw,
      ts: new Date().toISOString(),
    })}\n`,
  );

  await openMeet(page);
  await page.getByRole("button", { name: new RegExp(topic) }).click();
  await expect(page.getByText(prose, { exact: true })).toBeVisible();
  await expect(page.getByText(/thread\.started/)).toHaveCount(0);
  await expect(page.getByText(/Raw transport/)).toHaveCount(0);

  await page.getByRole("button", { name: "Show messages as JSON" }).click();
  await expect(page.getByText(/Raw transport \(3 lines\)/)).toBeVisible();
  await page.getByText(/Raw transport \(3 lines\)/).click();
  await expect(page.getByText(/thread\.started/)).toBeVisible();

  await page.getByRole("button", { name: "Show messages as text" }).click();
  await expect(page.getByText(/Raw transport/)).toHaveCount(0);
});

// THE TOGGLE HAS TO DO SOMETHING ON AN ORDINARY MESSAGE.
//
// The test above is the only one the button used to pass, and it passes by
// SEEDING a record an older build would have written. `raw` reaches the client
// only when render-time normalization changed the stored text, and every
// capture seam now normalizes before writing — so on every conversation this
// build produces, pressing JSON did nothing at all. The view now switches each
// message to the record it was rendered from, which always exists.
test("the JSON toggle switches an ordinary message to its record and back", async ({ page }) => {
  const topic = unique("Browser JSON view");
  await createRoom(topic, [primaryAgent]);
  const message = unique("plain prose with no transport");
  await openMeet(page);
  await page.getByRole("button", { name: new RegExp(topic) }).click();
  await page.getByLabel("Message the room").fill(message);
  await page.getByRole("button", { name: "Send message" }).click();
  // The staffed agent echoes the prompt back, so the message text appears in
  // its reply too: assert on the human's own message, not on the word anywhere.
  const posted = page.getByText(message, { exact: true }).first();
  await expect(posted).toBeVisible();

  await page.getByRole("button", { name: "Show messages as JSON" }).click();
  const record = page.locator("[data-event-json]").first();
  await expect(record).toBeVisible();
  await expect(record).toContainText(`"text": "${message}"`);
  await expect(record).toContainText('"kind"');

  await page.getByRole("button", { name: "Show messages as text" }).click();
  await expect(page.locator("[data-event-json]")).toHaveCount(0);
  await expect(posted).toBeVisible();
});

// The one that needed a deterministic agent: addressing runs a real turn through
// chat.Invoke, so the fixture in e2e/fleet stands in for an agent CLI and echoes
// the prompt back. What is asserted is the browser, not the API — the reply has
// to arrive over /observe and be painted, which is the half no Go test can see.
// This is the assertion the other four cannot make: they prove the room can be
// listed, opened, staffed and posted to, but only this one proves an AGENT
// ANSWERS and a human sees it. Two defects hid behind it, both silent, and both
// found only by running the whole chain in a browser:
//
//   1. liveEventSchema required role/status, which LiveEvent marks omitempty —
//      so the SPA received every live frame and discarded all of them.
//   2. the fixture's `#!/usr/bin/env bash` shebang was intercepted by the agent
//      shell shim (see e2e/fleet/echoback), so the turn re-executed the harness
//      server instead of the fixture and never returned.
//
// Neither showed up as an error anywhere. Keep this test running.
test("addressed agent replies render in the browser", async ({ page }) => {
  const topic = unique("Browser reply room");
  await openMeet(page);
  await createRoomFromUI(page, topic, primaryAgent);

  await page.getByLabel("Message the room").fill(`@${primaryAgent} say hello`);
  await page.getByRole("button", { name: "Send message" }).click();

  // ECHO[<model>] is the fixture's signature; the agent's turn is what renders.
  await expect(page.getByText(/ECHO\[fixed\]/).first()).toBeVisible({
    timeout: 60_000,
  });
});

test("opens a Chat-backed direct message and streams its reply", async ({ page }) => {
  await createDM(primaryAgent);
  await openMeet(page);
  await openChat(page, primaryAgent);
  await expect(page.getByText("Direct message", { exact: true }).first()).toBeVisible();

  await page.locator("textarea").fill("say hello privately");
  await page.getByRole("button", { name: "Send message" }).click();
  await expect(page.getByText(/ECHO\[fixed\]/).first()).toBeVisible({ timeout: 60_000 });
});

test("two agent Chats keep their messages in separate conversations", async ({ page }) => {
  const primaryMessage = unique("primary sprint instruction");
  const alternateMessage = unique("alternate sprint instruction");
  await createDM(primaryAgent);
  await createDM(invitedAgent);
  await openMeet(page);

  await openChat(page, primaryAgent);
  await page.locator("textarea").fill(primaryMessage);
  await page.getByRole("button", { name: "Send message" }).click();
  await expect(page.getByText(new RegExp(`ECHO\\[fixed\\].*${primaryMessage}`)).first()).toBeVisible({
    timeout: 60_000,
  });

  await openChat(page, invitedAgent);
  await expect(page.getByText(primaryMessage, { exact: true })).toHaveCount(0);
  await page.locator("textarea").fill(alternateMessage);
  await page.getByRole("button", { name: "Send message" }).click();
  await expect(page.getByText(new RegExp(`ECHO\\[fixed\\].*${alternateMessage}`)).first()).toBeVisible({
    timeout: 60_000,
  });

  await openChat(page, primaryAgent);
  await expect(page.getByText(primaryMessage, { exact: true })).toBeVisible();
  await expect(page.getByText(alternateMessage, { exact: true })).toHaveCount(0);
});

// EXPECTED-FAIL, and the marker is the alarm. Measured 2026-09-07 (story
// 86a6cc12c1e4, follow-on 041bf670): the sampled `<pre>` can never be observed
// because chat.Invoke does not stream. It buffers the whole turn and writes
// opt.Stream ONCE at process exit, so all five sampled frames reach the browser
// in a 0.3ms burst 2.9s after the turn starts, and the turn-end frame unmounts
// the card 23ms later. The counters above it come from the `speaking` frame and
// are all zero, which is why the three assertions before line 475 pass on an
// EMPTY card — the case is kept in full, and running, for exactly that reason.
//
// This is `test.fail()` rather than a `--grep-invert` because the suite must
// keep running it: docs/fleet-evidence-invariant.md forbids reaching a green
// state through the ABSENCE of evidence, and a permanently excluded case is
// that absence. When 041bf670 lands, this case starts PASSING, `test.fail()`
// turns the run red, and whoever fixed the stream deletes this marker. That
// red is the intended notification, not a regression.
test("a long Chat reply shows bounded real output and cumulative progress", async ({ page }) => {
  test.fail();
  await createDM(primaryAgent);
  await openMeet(page);
  await openChat(page, primaryAgent);

  await page.locator("textarea").fill("show sampled progress");
  await page.getByRole("button", { name: "Send message" }).click();

  const progress = page.locator("[data-live-progress]");
  await expect(progress).toBeVisible({ timeout: 60_000 });
  await expect(progress).toContainText(/lines observed/);
  await expect(progress).toContainText(/B/);
  await expect(progress).toContainText(/elapsed/);
  await expect(page.locator("pre").filter({ hasText: /progress line/ })).toBeVisible();

  // Completion replaces the sampled card with the full, durable answer.
  await expect(page.getByText(/ECHO\[fixed\].*show sampled progress/).first()).toBeVisible({ timeout: 60_000 });
  await expect(progress).toHaveCount(0);
});

test("a long Chat turn visibly shows the agent working", async ({ page }) => {
  await createDM(primaryAgent);
  await openMeet(page);
  await openChat(page, primaryAgent);
  await page.route("**/api/dms/*/messages", async (route) => {
    await route.fulfill({
      status: 202,
      contentType: "application/json",
      body: JSON.stringify({ agent: primaryAgent, status: "working" }),
    });
  }, { times: 1 });
  await page.locator("textarea").fill("take your time");
  await page.getByRole("button", { name: "Send message" }).click();
  await expect(page.getByLabel(`${primaryAgent} is typing`)).toBeVisible();
  await expect(page.getByText(/reply will appear here/i)).toBeVisible();
});

test("Meet tabs navigate between channels and Chat direct messages", async ({ page }) => {
  const topic = unique("Meet tab channel");
  await createRoom(topic, [primaryAgent]);
  await createDM(primaryAgent);
  await openMeet(page);

  await openChat(page, primaryAgent);
  await expect(page.getByText("Past conversations", { exact: true }).first()).toBeVisible();
  await expect(page.getByText("Meetings", { exact: true }).first()).toHaveCount(0);
  await expect(page.getByRole("heading", { name: primaryAgent })).toBeVisible();
  await expect(page.locator("textarea")).toBeEnabled();

  await page.getByRole("tab", { name: /Meet/ }).click();
  await expect(page.getByText("Meetings", { exact: true }).first()).toBeVisible();
  await expect(page.getByText("Past conversations", { exact: true }).first()).toHaveCount(0);
  await expect(page.getByLabel("Message the room")).toBeEnabled();
});

test("an empty legacy DM transcript never renders raw schema JSON", async ({ page }) => {
  await createDM(primaryAgent);
  await openMeet(page);
  await page.route("**/api/dms/echoback-fixed", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        state: {
          agent: primaryAgent,
          human: "e2e-human",
          created: new Date().toISOString(),
          updated: new Date().toISOString(),
        },
        events: null,
      }),
    });
  }, { times: 1 });
  await openChat(page, primaryAgent);
  await expect(page.locator("textarea")).toBeEnabled();
  await expect(page.getByText(/expected.*array|invalid_type/i)).toHaveCount(0);
});

test("routes an unoccupied permanent role instead of silently posting it", async ({ page }) => {
  const topic = unique("Browser lazy role room");
  await openMeet(page);
  await createRoomFromUI(page, topic, primaryAgent);

  await page.route("**/api/rooms/*/address", async (route) => {
    await route.fulfill({
      status: 202,
      contentType: "application/json",
      body: JSON.stringify({ job: "lazy-steward", room: "steward" }),
    });
  }, { times: 1 });
  const requestPromise = page.waitForRequest((request) =>
    request.url().endsWith("/address"),
  );

  await page.getByLabel("Message the room").fill("@steward what is the hostname?");
  await page.getByRole("button", { name: "Send message" }).click();

  const request = await requestPromise;
  expect(request.postDataJSON()).toEqual({
    agent: "steward",
    text: "what is the hostname?",
  });
  // Addressing is asynchronous, so the composer reports the accepted turn. The
  // original @steward text must never be recorded as a plain human message.
  await expect(page.getByText(/message to steward was accepted/i)).toBeVisible();
  await expect(page.getByText("@steward what is the hostname?", { exact: true })).toHaveCount(0);
});

// The room's OWNER is preselected, and in a sprint's room that owner is its
// project manager. This is not a nicety: unaddressed mail in such a room
// already lands on that seat server-side, so a composer defaulting to nobody
// disagreed with the room it was typing into — the room had someone
// accountable and the UI would not say who.
test("the composer addresses the room owner by default and says so", async ({ page }) => {
  const topic = unique("Browser owner default room");
  await openMeet(page);
  await createRoomFromUI(page, topic, primaryAgent);

  await expect(
    page.getByRole("button", { name: `Recipient: ${facilitatorAgent} · facilitator` }),
  ).toBeVisible();
  // And the roster says the same thing, so the two lists cannot disagree about
  // who is accountable.
  const sidebar = page.locator("aside").first();
  await expect(sidebar.getByText("facilitator").first()).toBeVisible();
});

// A changed recipient must SURVIVE. The pick is per room and per browser —
// two people in one room may each be talking to a different agent — so it is
// stored locally rather than written onto shared room state.
test("a changed recipient persists across a reload", async ({ page }) => {
  const topic = unique("Browser recipient persist room");
  await openMeet(page);
  await createRoomFromUI(page, topic, primaryAgent);

  await selectRecipient(page, primaryAgent);
  await expect(page.getByRole("button", { name: `Recipient: ${primaryAgent}` })).toBeVisible();

  await page.reload();
  await expect(page.getByRole("button", { name: `Recipient: ${primaryAgent}` })).toBeVisible();

  // "Everyone" is a real choice, not the absence of one, so it persists too.
  await selectRecipient(page, "Everyone");
  await page.reload();
  await expect(page.getByRole("button", { name: "Recipient: Everyone" })).toBeVisible();
});

// "Everyone" must put the message in every participant's INBOX, not merely in
// the transcript.
//
// The composer could not express an addressee at all: /post dropped `to`, so
// the room's own web UI could only ever post mail addressed to nobody —
// present in the transcript, in no participant's actionable inbox, waking no
// one. From outside that is indistinguishable from every agent ignoring you.
test("Everyone addresses the whole room rather than nobody", async ({ page }) => {
  const topic = unique("Browser broadcast room");
  const message = unique("broadcast to the room");
  await openMeet(page);
  await createRoomFromUI(page, topic, primaryAgent);

  await selectRecipient(page, "Everyone");
  const posted = page.waitForRequest((r) => r.url().endsWith("/post"));
  await page.getByLabel("Message the room").fill(message);
  await page.getByRole("button", { name: "Send message" }).click();

  const request = await posted;
  if (request.postDataJSON().to !== "all") {
    throw new Error(
      `the composer posted to ${JSON.stringify(request.postDataJSON().to)}; ` +
        `an empty addressee reaches no inbox at all`,
    );
  }
  await expect(page.getByText(message).first()).toBeVisible();
});

// EVERY MESSAGE IN A ROOM SAYS FROM AND TO.
//
// A room is many-to-many: "@codex said this" does not say whether it was asked
// of the project manager, of one participant, or of everybody — and until this
// landed, addressing an agent recorded ONLY the reply, so the human's own
// message vanished and the room showed answers to questions nobody could see.
test("a room message names both its sender and its addressee", async ({ page }) => {
  const topic = unique("Browser addressing room");
  const broadcast = unique("everyone please read this");
  const question = unique("addressed question");
  await openMeet(page);
  await createRoomFromUI(page, topic, primaryAgent);

  // A broadcast: from the human, to everyone.
  await selectRecipient(page, "Everyone");
  await page.getByLabel("Message the room").fill(broadcast);
  await page.getByRole("button", { name: "Send message" }).click();
  await expect(page.locator("article", { hasText: broadcast })).toContainText("everyone");

  // A directed question. The QUESTION has to be on screen, addressed to the
  // agent — not only the answer to it.
  await page.getByLabel("Message the room").fill(`@${primaryAgent} ${question}`);
  await page.getByRole("button", { name: "Send message" }).click();
  const asked = page
    .locator("article", { hasText: question })
    .filter({ hasNotText: "ECHO[" });
  await expect(asked).toBeVisible({ timeout: 60_000 });
  await expect(asked.locator("[data-message-header]")).toContainText(`@${primaryAgent}`);

  // The reply is from the agent, and it is addressed to nobody: a turn is
  // shared room history, which is what stops a reply from waking anything.
  const reply = page
    .locator("article", { hasText: question })
    .filter({ hasText: "ECHO[" });
  await expect(reply).toBeVisible({ timeout: 60_000 });
  const replyHeader = reply.locator("[data-message-header]");
  await expect(replyHeader).toContainText(`@${primaryAgent}`);
  await expect(replyHeader).toContainText("the room");
});

// THE ROLE BADGE MUST SAY SOMETHING TRUE ABOUT THIS SPEAKER.
//
// Every agent turn is recorded with role "participant" (session.go), so the
// badge read "participant" beside every name in every room and distinguished
// nobody. The room already knows who its facilitator is; the badge now says so.
test("a message badge names the speaker's seat, not the generic role", async ({ page }) => {
  const topic = unique("Browser seat badge room");
  const question = unique("facilitator question");
  await openMeet(page);
  await createRoomFromUI(page, topic, primaryAgent);

  await page.getByLabel("Message the room").fill(`@${facilitatorAgent} ${question}`);
  await page.getByRole("button", { name: "Send message" }).click();

  const reply = page
    .locator("article", { hasText: question })
    .filter({ hasText: "ECHO[" });
  await expect(reply).toBeVisible({ timeout: 60_000 });
  // Scoped to the attribution line: the agent's own answer quotes the word
  // "participant" back (it is in the turn prompt), so an article-wide
  // assertion would prove nothing about the badge.
  const header = reply.locator("[data-message-header]");
  await expect(header).toContainText("facilitator");
  await expect(header).not.toContainText("participant");
});

// A 1:1 STATES ITS RECIPIENT AND OFFERS ONE ACTION: SENDING.
//
// The composer does not parse "@name" in a chat (see submit), so the mention
// button offered a syntax that would have been sent verbatim as prose. What a
// chat needs in that slot is the one thing a room puts there: who this goes to,
// named the same way — "@agent". Start work went the same way for a harder
// reason: the server refuses it outside proven containment, so it was a control
// that could only fail, in the surface whose one useful mode is the message.
test("a chat names its agent and offers no action but sending", async ({ page }) => {
  await createDM(primaryAgent);
  await openMeet(page);
  await openChat(page, primaryAgent);

  await expect(page.getByText(`@${primaryAgent}`).first()).toBeVisible();
  await expect(page.getByRole("button", { name: "Send message" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Start work" })).toHaveCount(0);
  // The mention button is gone from BOTH surfaces now — the recipient control
  // is the one place a message is addressed — so this guards against it coming
  // back here rather than distinguishing a chat from a room.
  await expect(page.getByRole("button", { name: "Mention an agent" })).toHaveCount(0);
  await expect(page.getByRole("button", { name: /^Recipient: / })).toHaveCount(0);
});

async function selectRecipient(page: Page, name: string) {
  await page.getByRole("button", { name: /^Recipient: / }).click();
  await page.getByRole("menuitem", { name: new RegExp(name) }).click();
}

/** openMeet opens the app.
 *
 * There is no `hold` parameter any more. The composer used to withhold a
 * clicked message for a few seconds so that a cancel could truthfully say
 * "not sent", and every test that was about something else had to switch that
 * off. The send is plain again (story e9f5d325eded), so opening the app is
 * just opening the app.
 */
async function openMeet(page: Page) {
  await page.goto(`${baseURL}/?mock=0`);
  await expect(page.getByText("bashymeet", { exact: true })).toBeVisible();
}

async function openChat(page: Page, agent: string) {
  await page.getByRole("tab", { name: /Chat/ }).click();
  const dmButton = page.locator("aside").first().getByRole("button", { name: new RegExp(agent) });
  const newChat = page.getByRole("menuitem", { name: new RegExp(agent) });
  await Promise.race([dmButton.waitFor(), newChat.waitFor()]);
  if (await newChat.isVisible()) await newChat.click();
  else await dmButton.click();
  await expect(page.getByRole("heading", { name: agent })).toBeVisible();
}

async function createRoomFromUI(page: Page, topic: string, agents: string) {
  await page.getByRole("button", { name: "New meeting" }).click();
  await page.getByLabel("Topic").fill(topic);
  await page.getByLabel("Facilitator").selectOption(facilitatorAgent);
  await page.getByLabel("Participants").selectOption(agents.split(/[,\s]+/).filter(Boolean));
  await page.getByRole("button", { name: "Open meeting" }).click();
  await expect(
    page.getByRole("button", { name: new RegExp(topic) }),
  ).toBeVisible();
}

async function createRoom(
  topic: string,
  participants: string[],
): Promise<{ id: string }> {
  const created = await api("api/rooms", {
    method: "POST",
    body: JSON.stringify({
      topic,
      participants,
      owner: facilitatorAgent,
      human: "e2e-human",
    }),
  });
  if (
    !created ||
    typeof created !== "object" ||
    !("id" in created) ||
    typeof created.id !== "string"
  ) {
    throw new Error(
      `create room response did not include an id: ${JSON.stringify(created)}`,
    );
  }
  return { id: created.id };
}

async function createDM(agent: string): Promise<void> {
  await api("api/dms", {
    method: "POST",
    body: JSON.stringify({ agent }),
  });
}

async function postMessage(ref: string, author: string, text: string) {
  return api(`api/rooms/${encodeURIComponent(ref)}/post`, {
    method: "POST",
    body: JSON.stringify({ author, text }),
  });
}

async function api(pathname: string, init?: RequestInit): Promise<unknown> {
  const response = await fetch(new URL(pathname, baseURL), {
    ...init,
    headers: {
      "Content-Type": "application/json",
      ...init?.headers,
    },
  });
  if (!response.ok) {
    throw new Error(
      `${init?.method ?? "GET"} ${pathname} failed with ${response.status}: ${await response.text()}`,
    );
  }
  if (response.status === 204) return undefined;
  return response.json();
}

async function waitForHealth() {
  const deadline = Date.now() + 15_000;
  let lastError: unknown;
  while (Date.now() < deadline) {
    if (server?.exitCode !== null) {
      throw new Error(`meet serve exited early with ${server?.exitCode}\n${serverOutput}`);
    }
    try {
      const response = await fetch(new URL("healthz", baseURL));
      if (response.ok) return;
    } catch (error) {
      lastError = error;
    }
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  throw new Error(`meet serve did not become healthy: ${String(lastError)}\n${serverOutput}`);
}

function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const probe = net.createServer();
    probe.on("error", reject);
    probe.listen(0, "127.0.0.1", () => {
      const address = probe.address();
      probe.close(() => {
        if (typeof address === "object" && address) {
          resolve(address.port);
        } else {
          reject(new Error("could not allocate a TCP port"));
        }
      });
    });
  });
}

function unique(prefix: string) {
  return `${prefix} ${Date.now()}-${Math.random().toString(16).slice(2)}`;
}

// --- The send is plain -------------------------------------------------------

// THE GATE FOR STORY e9f5d325eded. A hold-then-cancel-then-recall machine sat
// on this control and never worked; it is removed, and these are the
// assertions that keep it removed.
//
// The point is not that the old controls are absent from one render — it is
// that the send path no longer has a state in which they could appear. So the
// test sends TWICE and checks after each: the second send is what caught the
// original defect, where a delivered message left a pending record behind and
// the composer refused everything typed after it.
test("sending is one act: no hold, no cancel, no recall", async ({ page }) => {
  const topic = unique("Browser plain send room");
  const first = unique("the first message");
  const second = unique("the second message");
  await openMeet(page);
  await createRoomFromUI(page, topic, primaryAgent);
  await selectRecipient(page, "Everyone");

  const send = page.getByRole("button", { name: "Send message" });
  await page.getByLabel("Message the room").fill(first);
  await send.click();

  // DELIVERED, not held: the message is in the transcript with no countdown in
  // front of it.
  await expect(page.getByText(first).first()).toBeVisible();
  await expect(page.getByText(/Sending in \d+s/)).toHaveCount(0);
  await expect(page.getByRole("button", { name: /^Cancel send/ })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Recall message" })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Post retraction" })).toHaveCount(0);

  // ACCEPTED, and said so. "Delivered to everyone" is the honest claim for a
  // broadcast: it reached every participant's inbox, which is not the same as
  // anyone having answered.
  await expect(
    page.getByText(/Delivered to everyone in the room/),
  ).toBeVisible();

  // THE SECOND MESSAGE. Nothing from the first is still holding the control.
  await expect(send).toBeVisible();
  await page.getByLabel("Message the room").fill(second);
  await send.click();
  await expect(page.getByText(second).first()).toBeVisible();
  await expect(page.getByRole("button", { name: "Recall message" })).toHaveCount(0);

  // Both survive a reload: the transcript is the record, and neither message
  // was withheld, superseded or retracted.
  await page.reload();
  await expect(page.getByText(first).first()).toBeVisible();
  await expect(page.getByText(second).first()).toBeVisible();
  await expect(page.getByText("Retracted")).toHaveCount(0);
});

// A 1:1 is the other synchronous path, and it carried the same machine. Its
// accepted indicator is the one that matters most: a Chat turn takes minutes,
// so between the click and the reply the banner is all the sender has.
test("a chat send is accepted immediately and shows the reply when it lands", async ({ page }) => {
  const message = unique("plain chat send");
  await openMeet(page);
  await openChat(page, primaryAgent);

  await page.getByLabel(new RegExp(`Message ${primaryAgent}`)).fill(message);
  await page.getByRole("button", { name: "Send message" }).click();

  await expect(page.getByText(message).first()).toBeVisible();
  await expect(
    page.getByText(new RegExp(`Your message to ${primaryAgent} was accepted`)),
  ).toBeVisible();
  await expect(page.getByRole("button", { name: /^Cancel send/ })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Recall message" })).toHaveCount(0);
});
