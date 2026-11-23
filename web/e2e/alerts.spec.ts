import { deliver, expect, expectAccessible, signIn, signOut, test, unique } from "./helpers";

test("an alert arrives without a reload, and is acknowledged from the list and resolved from its page", async ({ page, request }) => {
  await signIn(page, "grace");
  await expect(page).toHaveURL("/alerts");
  await expect(page.getByRole("heading", { level: 1, name: "Alerts" })).toBeVisible();

  // A source sends an alert while the list is open.
  const name = unique("DiskFull");
  const id = await deliver(request, {
    labels: { alertname: name, host: "db1", severity: "critical" },
    annotations: { summary: "Disk is full on db1", runbook_url: "https://runbooks.example.com/disk-full" },
  });
  const row = page.getByRole("listitem").filter({ has: page.getByRole("link", { name }) });
  await expect(row).toBeVisible();
  await expect(row).toContainText("Disk is full on db1");
  await expect(row).toContainText("Firing");
  await expect(row).toContainText("Critical");
  await expectAccessible(page);

  // Acknowledge, from the list.
  await row.getByRole("button", { name: `Acknowledge ${name}` }).click();
  await expect(row).toContainText("Acknowledged");
  await expect(row.getByRole("button", { name: `Acknowledge ${name}` })).toHaveCount(0);

  // Its page: what it says, what was decided, what happened.
  await row.getByRole("link", { name }).click();
  await expect(page).toHaveURL(`/alerts/${id}`);
  await expect(page.getByRole("heading", { level: 1, name })).toBeVisible();
  await expect(page.getByRole("region", { name: "Labels" })).toContainText("db1");
  const runbook = page.getByRole("region", { name: "Annotations" }).getByRole("link", { name: "https://runbooks.example.com/disk-full" });
  await expect(runbook).toHaveAttribute("rel", "noopener noreferrer");
  await expect(page.getByRole("region", { name: "About" })).toContainText("platform");
  await expect(page.getByRole("region", { name: "Group" })).toContainText("platform");
  const timeline = page.getByRole("region", { name: "Timeline" });
  await expect(timeline).toContainText("Started firing");
  await expect(timeline).toContainText("Escalation step 1 of 2");
  await expect(timeline).toContainText("Acknowledged by Grace Example");
  await expectAccessible(page);

  // Resolve, from the page.
  await page.getByRole("button", { name: `Resolve ${name}` }).click();
  await expect(page.locator(".page-head")).toContainText("Resolved");
  await expect(timeline).toContainText("Resolved by Grace Example");
  await expect(page.getByRole("button", { name: `Resolve ${name}` })).toHaveCount(0);

  // It has left the open alerts, and is among the resolved ones.
  await page.getByRole("link", { name: "← All alerts" }).click();
  await expect(page.getByRole("list", { name: "Alerts" }).or(page.getByText("No open alerts"))).toBeVisible();
  await expect(page.getByRole("link", { name })).toHaveCount(0);
  await page.getByLabel("State").selectOption("resolved");
  await expect(page).toHaveURL(/status=resolved/);
  await expect(page.getByRole("link", { name })).toBeVisible();
  await signOut(page);
});

test("an alert is snoozed for a chosen time, and leaves the list when its source resolves it", async ({ page, request }) => {
  await signIn(page, "grace");
  const name = unique("HighLatency");
  const labels = { alertname: name, severity: "warning" };
  await deliver(request, { labels });
  const row = page.getByRole("listitem").filter({ has: page.getByRole("link", { name }) });
  await expect(row).toContainText("Warning");

  await row.getByRole("button", { name: `Snooze ${name}` }).click();
  const dialog = page.getByRole("dialog", { name: `Snooze ${name}` });
  await expect(dialog).toBeVisible();
  await expectAccessible(page);
  // Escape closes it and changes nothing.
  await page.keyboard.press("Escape");
  await expect(dialog).toHaveCount(0);
  await expect(row).toContainText("Firing");

  await row.getByRole("button", { name: `Snooze ${name}` }).click();
  await dialog.getByLabel("4 hours").check();
  await dialog.getByRole("button", { name: "Snooze" }).click();
  await expect(row).toContainText("Snoozed");

  // The filters narrow the list to it, and away from it.
  await page.getByLabel("State").selectOption("snoozed");
  await page.getByLabel("Severity").selectOption("warning");
  await page.getByLabel("Team").selectOption("platform");
  await expect(page).toHaveURL(/status=snoozed/);
  await expect(row).toBeVisible();
  await page.getByLabel("Severity").selectOption("critical");
  await expect(row).toHaveCount(0);
  await page.getByLabel("Severity").selectOption("any");
  await expect(row).toBeVisible();

  // The source says the problem is over: the row goes by itself.
  await deliver(request, { status: "resolved", labels });
  await expect(row).toHaveCount(0);
  await signOut(page);
});

test("what an alert says is text, and a link to an alert survives a sign-in", async ({ page, request }) => {
  const name = `<img src=x onerror="window.pwned=1">${unique("x")}`;
  const id = await deliver(request, {
    labels: { alertname: name, severity: "info", note: "<script>window.pwned=1</script>" },
    annotations: { summary: "<b onmouseover='window.pwned=1'>bold</b>", runbook_url: "javascript:window.pwned=1" },
  });

  // As from an email: the link is followed by someone who is not signed in.
  await signIn(page, "ada", `/alerts/${id}`);
  await expect(page).toHaveURL(`/alerts/${id}`);
  await expect(page.getByRole("heading", { level: 1, name })).toBeVisible();
  await expect(page.getByText("<b onmouseover='window.pwned=1'>bold</b>").first()).toBeVisible();
  await expect(page.getByRole("region", { name: "Labels" })).toContainText("<script>window.pwned=1</script>");
  // The address that is no web address is shown, and is no link.
  await expect(page.getByRole("region", { name: "Annotations" })).toContainText("javascript:window.pwned=1");
  await expect(page.locator('a[href^="javascript:" i]')).toHaveCount(0);
  await expect(page.locator("main img, main script, main b")).toHaveCount(0);

  await page.goto("/alerts?status=all&severity=info");
  await expect(page.getByRole("link", { name })).toBeVisible();
  expect(await page.evaluate(() => (window as unknown as { pwned?: number }).pwned)).toBeUndefined();

  // An address that is no alert says so.
  await page.goto("/alerts/00000000-0000-4000-8000-000000000000");
  await expect(page.getByRole("heading", { level: 1, name: "No such alert" })).toBeVisible();
  await signOut(page);
});
