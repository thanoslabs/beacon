// Takes the screenshots that the documentation shows, of the real app with
// the test users and nothing else. It runs only when asked:
//
//   BEACON_SCREENSHOTS=1 npx playwright test screenshots
//
// and writes to docs/images/web. Look at them before committing.
import { resolve } from "node:path";
import type { Page } from "@playwright/test";
import { deliver, expect, looks, signIn, signOut, test } from "./helpers";

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
    await capture(page, "account");
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
});
