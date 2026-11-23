import { expect, expectAccessible, openMenu, signIn, signOut, test } from "./helpers";

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
