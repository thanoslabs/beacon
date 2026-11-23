import { defineConfig, devices } from "@playwright/test";

// The browser tests use the real binary, with the web interface embedded,
// from outside: a browser, and the webhook as a source would call it. The
// address is the one Dex is configured to send people back to.
export const baseURL = "http://127.0.0.1:18080";

export default defineConfig({
  testDir: "./e2e",
  // One Beacon and one database for the run: the tests share them, so they
  // run one after the other, each on things it names itself.
  workers: 1,
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  // A test that fails now and then is a broken test. Nothing is retried.
  retries: 0,
  reporter: process.env.CI ? [["list"], ["html", { open: "never" }]] : [["list"]],
  timeout: 60_000,
  expect: { timeout: 15_000 },
  use: {
    baseURL,
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
    // The same everywhere, so that what is written on a page does not
    // depend on the machine.
    locale: "en-GB",
    timezoneId: "America/Toronto",
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
  webServer: {
    command: "node e2e/serve.mjs",
    url: `${baseURL}/readyz`,
    reuseExistingServer: false,
    timeout: 60_000,
    gracefulShutdown: { signal: "SIGTERM", timeout: 15_000 },
    stdout: "pipe",
    stderr: "pipe",
  },
});
