import { expect, expectAccessible, openMenu, signIn, signOut, someoneOnCall, test } from "./helpers";

test("who is on call is shown per team and schedule, and a schedule shows its shifts and its rotation", async ({ page }) => {
  await signIn(page, "ada");
  await openMenu(page);
  await page.getByRole("link", { name: "On call" }).click();
  await expect(page).toHaveURL("/on-call");
  await expect(page.getByRole("heading", { level: 1, name: "On call" })).toBeVisible();

  const team = page.getByRole("region", { name: "Platform" });
  const row = team.getByRole("listitem").filter({ has: page.getByRole("link", { name: "platform-primary" }) });
  await expect(row).toBeVisible();
  await expectAccessible(page);

  await row.getByRole("link", { name: "platform-primary" }).click();
  await expect(page).toHaveURL("/schedules/platform-primary");
  await expect(page.getByRole("heading", { level: 1, name: "platform-primary" })).toBeVisible();
  // The schedule's zone is stated, and the zone the times are shown in.
  await expect(page.getByText("hands over in the time zone")).toContainText("Europe/London");
  await expect(page.getByText("hands over in the time zone")).toContainText("America/Toronto");

  // The rotation is what the files say, and nothing about it can be changed.
  const rotation = page.getByRole("region", { name: "Rotation" });
  await expect(rotation).toContainText("Managed in the configuration files");
  await expect(rotation).toContainText("Monday at 09:00, Europe/London time");
  await expect(rotation.getByRole("listitem")).toHaveCount(2);
  await expect(rotation.getByRole("button")).toHaveCount(0);
  await expect(rotation.getByRole("textbox")).toHaveCount(0);

  // Four weeks, and the way to others.
  await expect(page.locator(".calendar-week")).toHaveCount(4);
  await page.getByRole("button", { name: "Later" }).click();
  await expect(page).toHaveURL(/from=\d{4}-\d{2}-\d{2}/);
  await page.getByRole("button", { name: "This week" }).click();
  await expect(page).toHaveURL("/schedules/platform-primary");
  await expectAccessible(page);

  await page.goto("/schedules/no-such-schedule");
  await expect(page.getByRole("heading", { level: 1, name: "No such schedule" })).toBeVisible();
  await signOut(page);
});

test("an override changes who is on call, and removing it changes it back", async ({ page }) => {
  // Both members of the rotation are users, so one of them is on call.
  await signIn(page, "ada");
  await signOut(page);
  // A member arranges cover; it needs no admin.
  await signIn(page, "grace");
  await someoneOnCall(page, "platform-primary");
  await page.goto("/schedules/platform-primary");
  const onCall = page.getByRole("region", { name: "On call now" });
  await expect(onCall.locator("strong")).toHaveText(/Ada Example|Grace Example/);
  const before = (await onCall.locator("strong").textContent()) ?? "";
  const cover = before === "Ada Example" ? { name: "Grace Example", email: "grace@example.com" } : { name: "Ada Example", email: "ada@example.com" };

  // The other one covers from now until this time tomorrow.
  const overrides = page.getByRole("region", { name: "Overrides" });
  await expect(overrides).toContainText("No override is in force or to come.");
  await overrides.getByRole("button", { name: "Add an override" }).click();
  const dialog = page.getByRole("dialog", { name: "Add an override to platform-primary" });
  await expectAccessible(page);
  await dialog.getByLabel("Who covers").selectOption(cover.email);
  const tomorrow = await page.evaluate(() => {
    const at = new Date(Date.now() + 24 * 3600 * 1000);
    const pad = (n: number) => String(n).padStart(2, "0");
    return `${at.getFullYear()}-${pad(at.getMonth() + 1)}-${pad(at.getDate())}T${pad(at.getHours())}:${pad(at.getMinutes())}`;
  });
  await dialog.getByLabel("Ends").fill(tomorrow);
  await dialog.getByLabel("Reason (optional)").fill("a browser test covers");
  await dialog.getByRole("button", { name: "Add the override" }).click();
  await expect(dialog).toHaveCount(0);

  // Who is on call has changed, here and on the on-call page.
  await expect(onCall.locator("strong")).toHaveText(cover.name);
  await expect(onCall).toContainText(`covering for ${before}: a browser test covers`);
  const row = overrides.getByRole("listitem").filter({ hasText: cover.name });
  await expect(row).toContainText("In force");
  await expect(row).toContainText("Added by Grace Example");
  await expect(page.getByRole("list", { name: "Shifts" }).getByRole("listitem").filter({ hasText: "Override" })).toContainText(cover.name);
  await expectAccessible(page);
  await page.goto("/on-call");
  await expect(page.getByRole("listitem").filter({ has: page.getByRole("link", { name: "platform-primary" }) })).toContainText(`covering for ${before}`);

  // Removing it asks first, and then the rotation's person is back.
  await page.goto("/schedules/platform-primary");
  await overrides.getByRole("button", { name: `Remove the override of ${cover.name}` }).click();
  const confirm = page.getByRole("dialog", { name: "Remove this override?" });
  await confirm.getByRole("button", { name: "Keep it" }).click();
  await expect(row).toBeVisible();
  await overrides.getByRole("button", { name: `Remove the override of ${cover.name}` }).click();
  await confirm.getByRole("button", { name: "Remove the override" }).click();
  await expect(overrides).toContainText("No override is in force or to come.");
  await expect(onCall.locator("strong")).toHaveText(before);
  await expect(onCall).not.toContainText("covering for");

  // An end in the past is refused by Beacon, and the dialog says why.
  await overrides.getByRole("button", { name: "Add an override" }).click();
  await dialog.getByLabel("Ends").fill("2020-01-01T09:00");
  await dialog.getByRole("button", { name: "Add the override" }).click();
  await expect(dialog.getByRole("alert")).toBeVisible();
  await expect(dialog.getByLabel("Ends")).toHaveAttribute("aria-invalid", "true");
  await dialog.getByRole("button", { name: "Cancel" }).click();
  await signOut(page);
});
