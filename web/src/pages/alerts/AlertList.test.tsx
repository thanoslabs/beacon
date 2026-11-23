import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { fakeApi, status } from "../../test/api";
import { alertOf, teams } from "../../test/fixtures";
import { grace, renderApp } from "../../test/render";
import { AlertList } from "./AlertList";
import { pollInterval } from "./queries";

afterEach(() => {
  vi.useRealTimers();
});

function row(name: string) {
  return screen.getByRole("link", { name }).closest("li") as HTMLElement;
}

describe("the list of alerts", () => {
  it("shows what is open, newest first, and says what each alert is", async () => {
    const beacon = fakeApi({
      "GET /api/v1/teams": teams,
      "GET /api/v1/alerts": { alerts: [alertOf(), alertOf({ labels: { alertname: "HighLatency" }, annotations: {}, severity: "warning", status: "acknowledged", service: "checkout" })] },
    });
    renderApp(<AlertList />, { as: grace, at: "/alerts" });

    const first = within(await screen.findByRole("list", { name: "Alerts" })).getAllByRole("listitem")[0]!;
    expect(within(first).getByRole("link", { name: "DiskFull" })).toHaveAttribute("href", expect.stringMatching(/^\/alerts\/0000/));
    expect(within(first).getByText("Disk is full on db1")).toBeInTheDocument();
    expect(within(first).getByText("Firing")).toBeInTheDocument();
    expect(within(first).getByText("Critical")).toBeInTheDocument();
    expect(within(first).getByText("Team platform")).toBeInTheDocument();
    expect(within(row("HighLatency")).getByText("Service checkout")).toBeInTheDocument();

    // Without filters the list is what is not resolved.
    expect(beacon.callsTo("GET /api/v1/alerts")[0]?.search.getAll("status")).toEqual(["open"]);
    expect(beacon.callsTo("GET /api/v1/alerts")[0]?.search.has("severity")).toBe(false);
  });

  it("filters by state, severity and team, and keeps the filters in the address", async () => {
    const beacon = fakeApi({ "GET /api/v1/teams": teams, "GET /api/v1/alerts": { alerts: [] } });
    const user = userEvent.setup();
    renderApp(<AlertList />, { as: grace, at: "/alerts?severity=warning" });

    expect(await screen.findByText("No alerts match these filters")).toBeInTheDocument();
    expect(screen.getByLabelText("Severity")).toHaveValue("warning");
    await screen.findByRole("option", { name: "payments" });

    await user.selectOptions(screen.getByLabelText("State"), "resolved");
    await user.selectOptions(screen.getByLabelText("Team"), "payments");
    await waitFor(() => {
      const last = beacon.callsTo("GET /api/v1/alerts").at(-1)?.search;
      expect(last?.getAll("status")).toEqual(["resolved"]);
      expect(last?.getAll("severity")).toEqual(["warning"]);
      expect(last?.get("team")).toBe("payments");
    });

    await user.selectOptions(screen.getByLabelText("State"), "all");
    await waitFor(() => expect(beacon.callsTo("GET /api/v1/alerts").at(-1)?.search.has("status")).toBe(false));
  });

  it("says that all is quiet when nothing is open", async () => {
    fakeApi({ "GET /api/v1/teams": teams, "GET /api/v1/alerts": { alerts: [] } });
    renderApp(<AlertList />, { as: grace, at: "/alerts" });
    expect(await screen.findByText("No open alerts")).toBeInTheDocument();
  });

  it("shows a new alert without being asked, and says when it cannot refresh", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const beacon = fakeApi({ "GET /api/v1/teams": teams, "GET /api/v1/alerts": { alerts: [] } });
    renderApp(<AlertList />, { as: grace, at: "/alerts" });
    expect(await screen.findByText("No open alerts")).toBeInTheDocument();

    beacon.on({ "GET /api/v1/alerts": { alerts: [alertOf({ labels: { alertname: "JustFired" } })] } });
    await vi.advanceTimersByTimeAsync(pollInterval + 100);
    expect(await screen.findByRole("link", { name: "JustFired" })).toBeInTheDocument();

    // The list stays, with a warning: a stale list must not look quiet.
    beacon.on({ "GET /api/v1/alerts": () => status(500, { error: "internal error" }) });
    await vi.advanceTimersByTimeAsync(pollInterval + 100);
    expect(await screen.findByRole("alert")).toHaveTextContent(/could not be refreshed/);
    expect(screen.getByRole("link", { name: "JustFired" })).toBeInTheDocument();
  });

  it("pages with the cursor the API gave", async () => {
    const beacon = fakeApi({
      "GET /api/v1/teams": teams,
      "GET /api/v1/alerts": (request: Request) =>
        new URL(request.url).searchParams.get("cursor") === "next-1"
          ? { alerts: [alertOf({ labels: { alertname: "Older" } })] }
          : { alerts: [alertOf({ labels: { alertname: "Newer" } })], next_cursor: "next-1" },
    });
    const user = userEvent.setup();
    renderApp(<AlertList />, { as: grace, at: "/alerts" });

    await user.click(await screen.findByRole("button", { name: "Show older alerts" }));
    expect(await screen.findByRole("link", { name: "Older" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Newer" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Show older alerts" })).not.toBeInTheDocument();
    expect(beacon.callsTo("GET /api/v1/alerts").some((call) => call.search.get("limit") === "50")).toBe(true);
  });
});

describe("acting on an alert from the list", () => {
  it("offers what the API says the state allows, and nothing else", async () => {
    fakeApi({
      "GET /api/v1/teams": teams,
      "GET /api/v1/alerts": {
        alerts: [alertOf({ labels: { alertname: "Firing" } }), alertOf({ labels: { alertname: "Acked" }, status: "acknowledged" }), alertOf({ labels: { alertname: "Done" }, status: "resolved" })],
      },
    });
    renderApp(<AlertList />, { as: grace, at: "/alerts?status=all" });

    await screen.findByRole("link", { name: "Firing" });
    expect(within(row("Firing")).getAllByRole("button").map((button) => button.textContent)).toEqual(["Acknowledge", "Snooze", "Resolve"]);
    expect(within(row("Acked")).getAllByRole("button").map((button) => button.textContent)).toEqual(["Snooze", "Resolve"]);
    expect(within(row("Done")).queryByRole("button")).not.toBeInTheDocument();
  });

  it("acknowledges and resolves with one press each", async () => {
    const firing = alertOf();
    const beacon = fakeApi({
      "GET /api/v1/teams": teams,
      "GET /api/v1/alerts": { alerts: [firing] },
      [`POST /api/v1/alerts/${firing.id}/acknowledge`]: () => {
        const acknowledged = { ...firing, status: "acknowledged", acknowledged_at: "2026-12-01T10:14:00Z", actions: ["snooze", "resolve"] };
        beacon.on({ "GET /api/v1/alerts": { alerts: [acknowledged] } });
        return acknowledged;
      },
      [`POST /api/v1/alerts/${firing.id}/resolve`]: () => {
        beacon.on({ "GET /api/v1/alerts": { alerts: [] } });
        return { ...firing, status: "resolved", resolved_at: "2026-12-01T10:30:00Z", actions: [] };
      },
    });
    const user = userEvent.setup();
    renderApp(<AlertList />, { as: grace, at: "/alerts" });

    await user.click(await screen.findByRole("button", { name: "Acknowledge DiskFull" }));
    expect(await within(row("DiskFull")).findByText("Acknowledged")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Acknowledge DiskFull" })).not.toBeInTheDocument();
    expect(beacon.callsTo(`POST /api/v1/alerts/${firing.id}/acknowledge`)).toHaveLength(1);

    await user.click(screen.getByRole("button", { name: "Resolve DiskFull" }));
    expect(await screen.findByText("No open alerts")).toBeInTheDocument();
  });

  it("snoozes for the time that was chosen", async () => {
    const firing = alertOf();
    const beacon = fakeApi({
      "GET /api/v1/teams": teams,
      "GET /api/v1/alerts": { alerts: [firing] },
      [`POST /api/v1/alerts/${firing.id}/snooze`]: { ...firing, status: "snoozed", snoozed_until: "2026-12-01T14:12:00Z", actions: ["resolve"] },
    });
    const user = userEvent.setup();
    renderApp(<AlertList />, { as: grace, at: "/alerts" });

    await user.click(await screen.findByRole("button", { name: "Snooze DiskFull" }));
    const dialog = screen.getByRole("dialog", { name: "Snooze DiskFull" });
    expect(within(dialog).getByLabelText("1 hour")).toBeChecked();
    await user.click(within(dialog).getByLabelText("4 hours"));
    await user.click(within(dialog).getByRole("button", { name: "Snooze" }));

    await waitFor(() => expect(beacon.callsTo(`POST /api/v1/alerts/${firing.id}/snooze`)[0]?.body).toEqual({ duration: "4h" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("says why when the state no longer allows it", async () => {
    const firing = alertOf();
    fakeApi({
      "GET /api/v1/teams": teams,
      "GET /api/v1/alerts": { alerts: [firing] },
      [`POST /api/v1/alerts/${firing.id}/acknowledge`]: () => status(409, { error: "the alert is resolved and cannot be acknowledged" }),
    });
    const user = userEvent.setup();
    renderApp(<AlertList />, { as: grace, at: "/alerts" });

    await user.click(await screen.findByRole("button", { name: "Acknowledge DiskFull" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("the alert is resolved and cannot be acknowledged");
  });
});

describe("what an alert says", () => {
  it("is shown as text, whatever it holds", async () => {
    const hostile = alertOf({
      labels: { alertname: '<img src=x onerror="window.pwned=1">' },
      annotations: { summary: "<script>window.pwned=1</script><b>bold</b>" },
      team: "<i>team</i>",
    });
    fakeApi({ "GET /api/v1/teams": teams, "GET /api/v1/alerts": { alerts: [hostile] } });
    const { container } = renderApp(<AlertList />, { as: grace, at: "/alerts" });

    expect(await screen.findByRole("link", { name: '<img src=x onerror="window.pwned=1">' })).toBeInTheDocument();
    expect(screen.getByText("<script>window.pwned=1</script><b>bold</b>")).toBeInTheDocument();
    expect(container.querySelector("img, script, b, i")).toBeNull();
    expect((window as unknown as { pwned?: number }).pwned).toBeUndefined();
  });
});
