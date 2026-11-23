// Takes the screenshots that the documentation shows, of the real app with
// the test users and nothing else. It runs only when asked:
//
//   BEACON_SCREENSHOTS=1 npx playwright test screenshots
//
// and writes to docs/images/web. Look at them before committing.
import { resolve } from "node:path";
import type { Page } from "@playwright/test";
import { deliver, expect, looks, signIn, signOut, someoneOnCall, test } from "./helpers";

const directory = resolve(import.meta.dirname, "../../docs/images/web");

/** One picture of the page as it is, in each look. */
export async function capture(page: Page, name: string): Promise<void> {
  for (const look of looks.filter((each) => each.name !== "phone-dark")) {
    await page.setViewportSize(look.viewport);
    await page.emulateMedia({ colorScheme: look.colorScheme });
    // As tall as the page, so that the picture shows all of it as a screen
    // of that height would.
    const height = await page.evaluate(() => document.documentElement.scrollHeight);
    await page.setViewportSize({ width: look.viewport.width, height: Math.max(height, look.viewport.height) });
    await page.screenshot({ path: resolve(directory, `${name}-${look.name}.png`) });
  }
  await page.setViewportSize(looks[0].viewport);
  await page.emulateMedia({ colorScheme: looks[0].colorScheme });
}

test.describe("screenshots", () => {
  test.skip(!process.env.BEACON_SCREENSHOTS, "set BEACON_SCREENSHOTS=1 to take the screenshots of the documentation");

  test("sign-in and account", async ({ page }) => {
    await page.goto("/");
    await expect(page.getByRole("link", { name: "Sign in with your identity provider" })).toBeVisible();
    await capture(page, "sign-in");

    await signIn(page, "ada", "/account");
    await expect(page.getByRole("heading", { level: 1, name: "Account" })).toBeVisible();
    await signOut(page);
  });

  test("alerts", async ({ page, request }) => {
    await signIn(page, "ada");
    // A few alerts in different states, as a source would send them.
    const disk = await deliver(request, {
      labels: { alertname: "DiskFull", host: "db1", service: "postgres", severity: "critical" },
      annotations: { summary: "Disk is full on db1", description: "/var/lib/postgresql has 2% free", runbook_url: "https://runbooks.example.com/disk-full" },
    });
    await deliver(request, { labels: { alertname: "HighLatency", service: "checkout", severity: "warning" }, annotations: { summary: "p99 latency of checkout is above 2s" } });
    await deliver(request, { labels: { alertname: "CertificateExpiresSoon", host: "www", severity: "info" }, annotations: { summary: "The certificate of www expires in 14 days" } });
    await deliver(request, { labels: { alertname: "QueueBacklog", service: "mailer", severity: "warning" }, annotations: { summary: "12,000 messages are waiting" } });

    await page.goto("/alerts");
    await page.getByRole("button", { name: "Acknowledge HighLatency" }).click();
    await page.getByRole("button", { name: "Snooze QueueBacklog" }).click();
    await page.getByRole("dialog").getByRole("button", { name: "Snooze" }).click();
    await expect(page.getByRole("listitem").filter({ hasText: "QueueBacklog" })).toContainText("Snoozed");
    await expect(page.getByRole("listitem").filter({ hasText: "HighLatency" })).toContainText("Acknowledged");
    await capture(page, "alerts");

    await page.goto(`/alerts/${disk}`);
    await expect(page.getByRole("region", { name: "Timeline" })).toContainText("Escalation step 1 of 2");
    await page.getByRole("button", { name: "Acknowledge DiskFull" }).click();
    await expect(page.getByRole("region", { name: "Timeline" })).toContainText("Acknowledged by Ada Example");
    await capture(page, "alert");
    await signOut(page);
  });

  test("on call and a schedule", async ({ page }) => {
    // Both members of the rotation have signed in, so both take their turns.
    // Ada was first, so the rotation began with her.
    await signIn(page, "ada");
    expect(await someoneOnCall(page, "platform-primary")).toBe("ada@example.com");
    await signOut(page);
    await signIn(page, "grace");
    await signOut(page);
    await signIn(page, "ada");
    // Grace covers a day of Ada's week.
    const made = await page.request.post("/api/v1/schedules/platform-primary/overrides", {
      data: { user: "grace@example.com", duration: "1d", reason: "Ada is at the dentist" },
    });
    expect(made.status()).toBe(201);
    await page.goto("/on-call");
    await expect(page.getByRole("region", { name: "Platform" })).toContainText("covering for");
    await capture(page, "on-call");
    await page.goto("/schedules/platform-primary");
    await expect(page.locator(".calendar-week")).toHaveCount(4);
    await expect(page.getByRole("list", { name: "Overrides" })).toBeVisible();
    await capture(page, "schedule");
    await page.getByRole("button", { name: "Add an override" }).click();
    await expect(page.getByRole("dialog")).toBeVisible();
    await capture(page, "override");
    await page.getByRole("dialog").getByRole("button", { name: "Cancel" }).click();
    await signOut(page);
  });

  test("silences", async ({ page, request }) => {
    await signIn(page, "ada");
    await deliver(request, { labels: { alertname: "BackupSlow", host: "db1", severity: "warning" }, annotations: { summary: "The nightly backup of db1 is slow" } });
    await deliver(request, { labels: { alertname: "BackupSlow", host: "db2", severity: "warning" }, annotations: { summary: "The nightly backup of db2 is slow" } });
    for (const silence of [
      { matchers: [{ label: "alertname", operator: "=", value: "DiskFull" }, { label: "host", operator: "=~", value: "db[0-9]+" }], duration: "2h", comment: "Replacing the disks of the database hosts" },
      { matchers: [{ label: "service", operator: "=", value: "checkout" }], starts_at: new Date(Date.now() + 6 * 3600 * 1000).toISOString(), duration: "1h", comment: "Planned failover test of checkout" },
    ]) {
      expect((await page.request.post("/api/v1/silences", { data: silence })).status()).toBe(201);
    }
    await page.goto("/silences");
    await expect(page.getByRole("list", { name: "Silences" }).getByRole("listitem").first()).toBeVisible();
    await capture(page, "silences");

    await page.goto("/silences/new");
    await page.getByLabel("Value of matcher 1").fill("BackupSlow");
    await page.getByLabel("Why").fill("The backup window is being moved");
    await expect(page.getByRole("region", { name: "Open alerts this would silence now" })).toContainText("2 open alerts match.");
    await capture(page, "silence-new");
    await signOut(page);
  });

  test("what the configuration defines", async ({ page }) => {
    await signIn(page, "ada");
    for (const [path, name] of [["/escalation-policies", "escalation-policies"], ["/routing", "routing"], ["/teams", "teams"], ["/integrations", "integrations"]] as const) {
      await page.goto(path);
      await expect(page.getByRole("main").locator(".card").first()).toBeVisible();
      await capture(page, name);
    }
    await signOut(page);
  });

  test("account and API tokens", async ({ page }) => {
    await signIn(page, "ada", "/account");
    await page.getByRole("button", { name: "Get a link code for discord" }).click();
    await expect(page.locator(".secret")).toBeVisible();
    await capture(page, "account");

    for (const token of [
      { name: "deploy-pipeline", role: "member", expires_in: "90d" },
      { name: "terraform", role: "admin", expires_in: "52w" },
    ]) {
      expect((await page.request.post("/api/v1/tokens", { data: token })).status()).toBe(201);
    }
    await page.goto("/tokens");
    await page.getByRole("button", { name: "New token" }).click();
    await page.getByRole("dialog").getByLabel("What it is for").fill("status-page");
    await page.getByRole("dialog").getByRole("button", { name: "Create the token" }).click();
    await expect(page.getByRole("status").filter({ hasText: "cannot be shown again" })).toBeVisible();
    await capture(page, "tokens");
    await signOut(page);
  });
});
