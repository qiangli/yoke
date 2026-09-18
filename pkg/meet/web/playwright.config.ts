import { defineConfig, devices } from "@playwright/test"

export default defineConfig({
  testDir: "./e2e",

  // The suite starts and stops its own server (e2e/meet-room.spec.ts builds the
  // SPA, builds e2e/serve with -tags meetspa, and runs it on a free port), so
  // there is deliberately no `webServer` block here: the server has to be built
  // from this checkout for the test to be testing this checkout's SPA.

  // Serial, and one worker. The tests share one server and one meet store, and
  // the room list is global state — running them in parallel would make each
  // test's assertions depend on what the others had created by then.
  fullyParallel: false,
  workers: 1,

  // A test that builds the SPA and a Go binary in beforeAll is doing real work
  // before the first assertion; the default 30s is not enough on a cold cache.
  timeout: 90_000,
  expect: { timeout: 15_000 },

  reporter: [["list"]],

  use: {
    // `channel: "chromium"` runs the full browser rather than the separate
    // chrome-headless-shell download, so `playwright install chromium` is the
    // only browser fetch a contributor (or CI) needs.
    ...devices["Desktop Chrome"],
    channel: "chromium",
    trace: "retain-on-failure",
  },
})
