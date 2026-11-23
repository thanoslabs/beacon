// Takes the screenshots that the documentation shows, of the real app with
// the test users and nothing else. It runs only when asked:
//
//   BEACON_SCREENSHOTS=1 npx playwright test screenshots
//
// and writes to docs/images/web. Look at them before committing.
import { resolve } from "node:path";
import type { Page } from "@playwright/test";
import { expect, looks, signIn, test } from "./helpers";

const directory = resolve(import.meta.dirname, "../../docs/images/web");

/** One picture of the page as it is, in each look. */
export async function capture(page: Page, name: string): Promise<void> {
  for (const look of looks.filter((each) => each.name !== "phone-dark")) {
    await page.setViewportSize(look.viewport);
    await page.emulateMedia({ colorScheme: look.colorScheme });
    await page.screenshot({ path: resolve(directory, `${name}-${look.name}.png`), fullPage: look.name === "phone" });
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

    await signIn(page, "ada");
    await expect(page.getByRole("heading", { level: 1, name: "Account" })).toBeVisible();
    await capture(page, "account");
  });
});
