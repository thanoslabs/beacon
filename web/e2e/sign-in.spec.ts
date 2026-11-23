import { expect, expectAccessible, people, signIn, signOut, test } from "./helpers";

test("a stranger gets the sign-in page and nothing else", async ({ page }) => {
  // A page deep in the app: the address stays, the sign-in page is shown.
  const answer = await page.goto("/alerts/9b2d6c1e-0a4f-4a57-8a0c-5f3c7d2e1b90");
  await expect(page.getByRole("heading", { level: 1, name: "Sign in" })).toBeVisible();
  await expect(page.getByRole("link", { name: "Sign in with your identity provider" })).toBeVisible();
  await expect(page.getByRole("navigation")).toHaveCount(0);
  expect(new URL(page.url()).pathname).toBe("/alerts/9b2d6c1e-0a4f-4a57-8a0c-5f3c7d2e1b90");

  // The page is served with the policy that keeps it to itself.
  const headers = answer?.headers() ?? {};
  expect(headers["content-security-policy"]).toContain("script-src 'self'");
  expect(headers["content-security-policy"]).not.toContain("unsafe");
  expect(headers["x-frame-options"]).toBe("DENY");
  expect(headers["x-content-type-options"]).toBe("nosniff");
  expect(headers["cache-control"]).toBe("no-cache");

  // An address of the API that is no route is still the API's to answer.
  const missing = await page.request.get("/api/v1/no-such-thing");
  expect(missing.status()).toBe(401);
  expect(missing.headers()["content-type"]).toBe("application/json");
  expect(await missing.json()).toHaveProperty("error");

  await expectAccessible(page);
});

test("a person signs in through the identity provider, arrives where they asked to go, and signs out", async ({ page, context }) => {
  // The page they asked for is where they land after the provider.
  await signIn(page, "ada", "/account?from=a-link");
  await expect(page).toHaveURL("/account?from=a-link");
  await expect(page.getByRole("heading", { level: 1, name: "Account" })).toBeVisible();
  const account = page.getByRole("region", { name: "Signed in as" });
  await expect(account).toContainText(people.ada.name);
  await expect(account).toContainText(people.ada.email);
  await expect(account).toContainText("Admin");

  // The session is a cookie that scripts cannot read, and nothing that
  // could stand for it is in the browser's storage.
  const cookies = await context.cookies();
  const session = cookies.find((cookie) => cookie.name === "beacon_session");
  expect(session?.httpOnly).toBe(true);
  expect(session?.sameSite).toBe("Lax");
  const stored = await page.evaluate(() => JSON.stringify([Object.entries(localStorage), Object.entries(sessionStorage), document.cookie]));
  expect(stored).not.toContain(session?.value ?? "no session");
  expect(stored).toBe("[[],[],\"\"]");

  await expectAccessible(page);

  await signOut(page);
  expect((await page.request.get("/api/v1/me")).status()).toBe(401);
  // The page that was open is not shown again by going back.
  await page.goBack();
  await expect(page.getByText(people.ada.email)).toHaveCount(0);
});

test("a member is told their role, and the frame works from the keyboard", async ({ page }) => {
  await signIn(page, "grace", "/account");
  await expect(page.getByRole("region", { name: "Signed in as" })).toContainText("Member");

  // The first stop of the keyboard is the link past the navigation.
  await page.keyboard.press("Tab");
  await expect(page.getByRole("link", { name: "Skip to the page" })).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(page.locator("#main")).toBeFocused();

  // An address that is no page says so, inside the app.
  await page.goto("/no-such-page");
  await expect(page.getByRole("heading", { level: 1, name: "Page not found" })).toBeVisible();

  // The choice of theme is kept across a reload, and applied before the
  // page is drawn.
  await page.getByLabel("Theme").selectOption("dark");
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  await page.reload();
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  await expect(page.getByLabel("Theme")).toHaveValue("dark");
  await signOut(page);
});
