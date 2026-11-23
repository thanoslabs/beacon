import { deliver, expect, expectAccessible, openMenu, signIn, signOut, test, unique } from "./helpers";

test("a silence is created with a preview of what it matches, keeps Beacon quiet, and is expired", async ({ page, request }) => {
  const name = unique("Backup");
  const comment = `maintenance ${name}`;
  await deliver(request, { labels: { alertname: name, host: "db1", severity: "warning" } });

  await signIn(page, "grace");
  await openMenu(page);
  await page.getByRole("link", { name: "Silences" }).click();
  await expect(page.getByRole("heading", { level: 1, name: "Silences" })).toBeVisible();
  await expectAccessible(page);
  await page.getByRole("link", { name: "New silence" }).click();
  await expect(page).toHaveURL("/silences/new");

  // The preview follows the matchers: the alert that is open matches.
  const preview = page.getByRole("region", { name: "Open alerts this would silence now" });
  await page.getByLabel("Value of matcher 1").fill(name);
  await expect(preview).toContainText("1 open alert matches.");
  await expect(preview.getByRole("link", { name })).toBeVisible();
  // A second matcher that the alert does not meet takes it out again.
  await page.getByRole("button", { name: "Add a matcher" }).click();
  await page.getByLabel("Label of matcher 2").fill("host");
  await page.getByLabel("Value of matcher 2").fill("db2");
  await expect(preview).toContainText("No open alert matches.");
  // A pattern that it does meet brings it back; a broken one is refused by Beacon.
  await page.getByLabel("Operator of matcher 2").selectOption("=~");
  await page.getByLabel("Value of matcher 2").fill("db[0-9]+");
  await expect(preview).toContainText("1 open alert matches.");
  await page.getByLabel("Value of matcher 2").fill("db[0-9");
  await expect(preview).toContainText("matchers[1].value");
  await page.getByLabel("Value of matcher 2").fill("db[0-9]+");
  await expect(preview).toContainText("1 open alert matches.");
  await expectAccessible(page);

  await page.getByLabel("For how long, from now").selectOption("1h");
  await page.getByLabel("Why").fill(comment);
  await page.getByRole("button", { name: "Create the silence" }).click();

  // Back on the list, the silence is active.
  await expect(page).toHaveURL("/silences");
  const row = page.getByRole("list", { name: "Silences" }).getByRole("listitem").filter({ hasText: comment });
  await expect(row).toContainText("Active");
  await expect(row).toContainText(`alertname="${name}"`);
  await expect(row).toContainText('host=~"db[0-9]+"');
  await expect(row).toContainText("Created by Grace Example");

  // An alert that fires under the silence is recorded, and nobody is told.
  const quiet = await deliver(request, { labels: { alertname: name, host: "db7", severity: "warning" } });
  await page.goto(`/alerts/${quiet}`);
  await expect(page.getByRole("region", { name: "Timeline" })).toContainText("Kept quiet by a silence");

  // Expiring asks first.
  await page.goto("/silences");
  await row.getByRole("button", { name: `Expire the silence: ${comment}` }).click();
  const dialog = page.getByRole("dialog", { name: "Expire this silence?" });
  await expect(dialog).toContainText("It ends now.");
  await expectAccessible(page);
  await dialog.getByRole("button", { name: "Keep it" }).click();
  await expect(row).toContainText("Active");
  await row.getByRole("button", { name: `Expire the silence: ${comment}` }).click();
  await dialog.getByRole("button", { name: "Expire the silence" }).click();
  await expect(row).toHaveCount(0);

  // It is kept, among the expired ones, and Beacon tells about the alert now.
  await page.getByLabel("Also show expired silences").check();
  await expect(row).toContainText("Expired");
  await expect(row.getByRole("button")).toHaveCount(0);
  await page.goto(`/alerts/${quiet}`);
  await expect(page.getByRole("region", { name: "Timeline" })).toContainText("Escalation step 1 of 2");
  await signOut(page);
});
