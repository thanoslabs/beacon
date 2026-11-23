import AxeBuilder from "@axe-core/playwright";
import { test as base, expect, type Page } from "@playwright/test";

/** The users of the Dex of compose.yaml (dev/dex/config.yaml). */
export const people = {
  // In beacon-admins and platform-engineers: an admin.
  ada: { email: "ada@example.com", name: "Ada Example" },
  // In platform-engineers: a member.
  grace: { email: "grace@example.com", name: "Grace Example" },
} as const;

export type Person = keyof typeof people;

/**
 * Every test fails if the page broke its Content-Security-Policy or threw:
 * the app must work under the policy the binary sends, and a refused script
 * or style is otherwise only a line in the console.
 */
export const test = base.extend<{ problems: string[] }>({
  problems: [
    async ({ page }, use) => {
      const problems: string[] = [];
      page.on("pageerror", (error) => problems.push(`the page threw: ${error.message}`));
      page.on("console", (message) => {
        const text = message.text();
        if (/Content Security Policy|Refused to (load|execute|apply|connect|frame)/i.test(text)) {
          problems.push(text);
        }
      });
      await use(problems);
      expect(problems, "the page broke its Content-Security-Policy or threw").toEqual([]);
    },
    { auto: true },
  ],
});

export { expect };

/**
 * Signs a person in as they would: asks for a page, is shown the sign-in
 * page, goes to the identity provider and comes back.
 */
export async function signIn(page: Page, who: Person, at = "/"): Promise<void> {
  await page.goto(at);
  await page.getByRole("link", { name: "Sign in with your identity provider" }).click();
  // Dex's own form.
  await page.locator('input[name="login"]').fill(people[who].email);
  await page.locator('input[name="password"]').fill("password");
  await page.locator('button[type="submit"]').click();
  await expect(page.getByRole("navigation", { name: "Main" })).toBeVisible();
}

export async function signOut(page: Page): Promise<void> {
  await openMenu(page);
  await page.getByRole("button", { name: "Sign out" }).click();
  await expect(page.getByRole("heading", { level: 1, name: "Sign in" })).toBeVisible();
}

/** On a narrow screen the navigation is behind the menu button. */
export async function openMenu(page: Page): Promise<void> {
  const menu = page.getByRole("button", { name: "Menu" });
  if ((await menu.isVisible()) && (await menu.getAttribute("aria-expanded")) === "false") {
    await menu.click();
  }
}

export const looks = [
  { name: "light", colorScheme: "light", viewport: { width: 1280, height: 800 } },
  { name: "dark", colorScheme: "dark", viewport: { width: 1280, height: 800 } },
  { name: "phone", colorScheme: "light", viewport: { width: 390, height: 844 } },
  { name: "phone-dark", colorScheme: "dark", viewport: { width: 390, height: 844 } },
] as const;

/**
 * Checks the page as it is now with axe, in both themes and at the width of
 * a phone, and that nothing is wider than a phone's screen. `ready` is
 * something the finished page shows.
 */
export async function expectAccessible(page: Page): Promise<void> {
  for (const look of looks) {
    await page.setViewportSize(look.viewport);
    await page.emulateMedia({ colorScheme: look.colorScheme });
    const results = await new AxeBuilder({ page }).withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa", "wcag22aa", "best-practice"]).analyze();
    const violations = results.violations.map((violation) => ({
      rule: violation.id,
      help: violation.help,
      where: violation.nodes.map((node) => `${node.target.join(" ")}: ${node.failureSummary ?? ""}`),
    }));
    expect(violations, `accessibility in the look "${look.name}"`).toEqual([]);
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
    expect(overflow, `the page is wider than the screen in the look "${look.name}"`).toBeLessThanOrEqual(0);
  }
  await page.setViewportSize(looks[0].viewport);
  await page.emulateMedia({ colorScheme: looks[0].colorScheme });
}
