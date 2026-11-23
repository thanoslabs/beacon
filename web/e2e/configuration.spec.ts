import { expect, expectAccessible, openMenu, signIn, signOut, test } from "./helpers";

const pages = [
  { link: "Escalation policies", path: "/escalation-policies" },
  { link: "Routing", path: "/routing" },
  { link: "Teams", path: "/teams" },
  { link: "Sources and integrations", path: "/integrations" },
];

test("what the configuration defines is shown, marked as managed in the files, with nothing to edit", async ({ page }) => {
  await signIn(page, "grace");
  for (const { link, path } of pages) {
    await openMenu(page);
    await page.getByRole("navigation", { name: "Main" }).getByRole("link", { name: link, exact: true }).click();
    await expect(page).toHaveURL(path);
    await expect(page.getByRole("heading", { level: 1, name: link })).toBeVisible();
    const main = page.getByRole("main");
    await expect(main.getByText("Managed in the configuration files")).toBeVisible();
    // Nothing in the page can be pressed, typed into or chosen.
    await expect(main.getByRole("button")).toHaveCount(0);
    await expect(main.getByRole("textbox")).toHaveCount(0);
    await expect(main.getByRole("combobox")).toHaveCount(0);
    await expectAccessible(page);
  }

  // What the files of the test configuration say.
  await page.goto("/escalation-policies");
  const policy = page.getByRole("region", { name: "platform" });
  await expect(policy).toContainText("Whoever is on call for platform-primary");
  await expect(policy).toContainText("The channel oncall@example.invalid of the integration email");
  await expect(policy).toContainText("starts again, once");

  await page.goto("/routing");
  await expect(page.getByRole("region", { name: "1. database" })).toContainText('service="postgres"');
  await expect(page.getByRole("region", { name: "Default route" })).toContainText("platform");

  await page.goto("/integrations");
  await expect(page.getByRole("region", { name: "Sources" })).toContainText("/api/v1/ingest/default");
  await expect(page.getByRole("region", { name: "Integrations" })).toContainText("127.0.0.1");
  // No credential of the configuration is anywhere in the page.
  for (const secret of ["browser-test-source-token", "beacon-local-development-password", "beacon-local-development-client-secret"]) {
    await expect(page.getByText(secret)).toHaveCount(0);
  }
  await signOut(page);
});

test("an admin sees the listed members who are not users yet, and a member does not", async ({ page }) => {
  // hopper is listed in the files and has never signed in.
  await signIn(page, "ada", "/teams");
  const team = page.getByRole("region", { name: /Platform/ });
  await expect(team).toContainText("Listed in the files, and not users yet");
  await expect(team).toContainText("hopper@example.com");
  await expect(team).toContainText("Ada Example");
  await expectAccessible(page);
  await signOut(page);

  await signIn(page, "grace", "/teams");
  await expect(team).toContainText("Ada Example");
  await expect(team).toContainText("Grace Example");
  await expect(page.getByText("hopper@example.com")).toHaveCount(0);
  await expect(page.getByText("not users yet")).toHaveCount(0);
  // Not hidden by the page: the API does not answer it to a member.
  const answer = await page.request.get("/api/v1/teams");
  expect(JSON.stringify(await answer.json())).not.toContain("hopper");
  await signOut(page);
});
