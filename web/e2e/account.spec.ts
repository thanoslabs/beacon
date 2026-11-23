import { expect, expectAccessible, openMenu, signIn, signOut, test, unique } from "./helpers";

test("a person gets a code to link their Discord account, and a member is not offered what is for admins", async ({ page }) => {
  await signIn(page, "grace", "/account");
  const chat = page.getByRole("region", { name: "Chat accounts" });
  const discord = chat.getByRole("listitem").filter({ hasText: "Discord" });
  await expect(discord).toContainText("Not linked");
  await discord.getByRole("button", { name: "Get a link code for discord" }).click();
  // The command to type in Discord, with a code that Beacon issued.
  await expect(discord.locator(".secret")).toHaveText(/^\/beacon-link code: \S+$/);
  await expect(discord).toContainText("The code works once");
  await expectAccessible(page);

  // What is for admins: not in the navigation, not on the page, not from the API.
  await openMenu(page);
  await expect(page.getByRole("navigation", { name: "Main" }).getByRole("link", { name: "Account" })).toBeVisible();
  await expect(page.getByRole("link", { name: "API tokens" })).toHaveCount(0);
  await page.goto("/tokens");
  await expect(page.getByText("API tokens are managed by admins")).toBeVisible();
  await expect(page.getByRole("button", { name: "New token" })).toHaveCount(0);
  const refused = await page.request.get("/api/v1/tokens");
  expect(refused.status()).toBe(403);
  const created = await page.request.post("/api/v1/tokens", { data: { name: "a member's attempt", role: "admin" } });
  expect(created.status()).toBe(403);
  await signOut(page);
});

test("an admin creates an API token, sees its secret once, and revokes it", async ({ page, playwright, baseURL }) => {
  const name = unique("pipeline");
  await signIn(page, "ada");
  await openMenu(page);
  await page.getByRole("link", { name: "API tokens" }).click();
  await expect(page).toHaveURL("/tokens");
  await expect(page.getByRole("heading", { level: 1, name: "API tokens" })).toBeVisible();

  await page.getByRole("button", { name: "New token" }).click();
  const dialog = page.getByRole("dialog", { name: "New API token" });
  await expectAccessible(page);
  await dialog.getByLabel("What it is for").fill(name);
  await dialog.getByLabel("Valid for").selectOption("30d");
  await dialog.getByRole("button", { name: "Create the token" }).click();

  const notice = page.getByRole("status").filter({ hasText: "cannot be shown again" });
  const secret = (await notice.locator(".secret").textContent()) ?? "";
  expect(secret).toMatch(/^bcn_/);
  const row = page.getByRole("list", { name: "API tokens" }).getByRole("listitem").filter({ hasText: name });
  await expect(row).toContainText("Member");
  await expect(row).toContainText("by ada@example.com");
  await expectAccessible(page);

  // The token works, by itself, for something that has no session.
  const machine = await playwright.request.newContext({ baseURL, extraHTTPHeaders: { Authorization: `Bearer ${secret}` } });
  const who = await machine.get("/api/v1/me");
  expect(who.status()).toBe(200);
  expect(await who.json()).toMatchObject({ display_name: name, role: "member" });

  // The secret is on the page until it is put away, and nowhere after.
  await notice.getByRole("button", { name: "I have stored it" }).click();
  await expect(page.getByText(secret)).toHaveCount(0);
  await page.reload();
  await expect(row).toBeVisible();
  await expect(page.getByText(secret)).toHaveCount(0);
  const stored = await page.evaluate(() => JSON.stringify([Object.entries(localStorage), Object.entries(sessionStorage)]));
  expect(stored).not.toContain("bcn_");

  // Revoking asks first, and the token stops working at once.
  await row.getByRole("button", { name: `Revoke the token ${name}` }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Revoke the token" }).click();
  await expect(page.getByRole("dialog")).toHaveCount(0);
  expect((await machine.get("/api/v1/me")).status()).toBe(401);
  await machine.dispose();
  await signOut(page);
});
