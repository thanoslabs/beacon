import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Route, Routes } from "react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { fakeApi, status } from "../../test/api";
import { scheduleOf, shiftOf, teams, users } from "../../test/fixtures";
import { ada, grace, renderApp } from "../../test/render";
import { OnCall } from "./OnCall";
import { SchedulePage } from "./SchedulePage";

// Thursday 3 December 2026, 10:00 in Toronto.
const now = new Date("2026-12-03T15:00:00Z");

beforeEach(() => {
  vi.useFakeTimers({ shouldAdvanceTime: true, now });
});

afterEach(() => {
  vi.useRealTimers();
});

function schedulePage(name: string, at = `/schedules/${name}`) {
  return renderApp(
    <Routes>
      <Route path="/schedules/:name" element={<SchedulePage />} />
    </Routes>,
    { as: grace, at },
  );
}

describe("who is on call", () => {
  it("is said for every schedule, team by team, with names in place of addresses", async () => {
    fakeApi({
      "GET /api/v1/teams": teams,
      "GET /api/v1/users": users,
      "GET /api/v1/schedules": {
        schedules: [
          scheduleOf(),
          scheduleOf({
            name: "platform-secondary",
            on_call: {
              schedule: "platform-secondary",
              user: "grace@example.com",
              since: "2026-12-03T09:00:00Z",
              until: "2026-12-04T09:00:00Z",
              rotation_user: "ada@example.com",
              override: { id: "o1", schedule: "platform-secondary", user: "grace@example.com", starts_at: "2026-12-03T09:00:00Z", ends_at: "2026-12-04T09:00:00Z", reason: "dentist", created_at: "2026-12-01T10:00:00Z", state: "active" },
            },
          }),
          scheduleOf({ name: "payments-primary", team: "payments", on_call: { schedule: "payments-primary", user: null, since: "2026-12-01T09:00:00Z", until: "2026-12-08T09:00:00Z", rotation_user: null } }),
        ],
      },
    });
    renderApp(<OnCall />, { as: grace, at: "/on-call" });

    const platform = await screen.findByRole("region", { name: "Platform" });
    const [primary, secondary] = within(platform).getAllByRole("listitem");
    expect(within(primary!).getByRole("link", { name: "platform-primary" })).toHaveAttribute("href", "/schedules/platform-primary");
    expect(await within(primary!).findByText("Ada Example")).toBeInTheDocument();
    expect(within(primary!).queryByText("You")).not.toBeInTheDocument();
    // Whoever is signed in is told that it is them, and for whom they cover.
    expect(within(secondary!).getByText("Grace Example")).toBeInTheDocument();
    expect(within(secondary!).getByText("You")).toBeInTheDocument();
    expect(secondary).toHaveTextContent("covering for Ada Example: dentist");

    const payments = screen.getByRole("region", { name: "Payments" });
    expect(within(payments).getByText("Nobody is on call")).toBeInTheDocument();
  });

  it("says when there is no schedule at all", async () => {
    fakeApi({ "GET /api/v1/teams": { teams: [] }, "GET /api/v1/users": { users: [] }, "GET /api/v1/schedules": { schedules: [] } });
    renderApp(<OnCall />, { as: ada, at: "/on-call" });
    expect(await screen.findByText("No schedules")).toBeInTheDocument();
  });
});

describe("the page of a schedule", () => {
  const shifts = {
    schedule: "platform-primary",
    from: "2026-11-23T05:00:00Z",
    to: "2026-12-21T05:00:00Z",
    shifts: [
      shiftOf("grace@example.com", "2026-11-23T09:00:00Z", "2026-11-30T09:00:00Z"),
      shiftOf("ada@example.com", "2026-11-30T09:00:00Z", "2026-12-07T09:00:00Z", { planned: true }),
      shiftOf("grace@example.com", "2026-12-07T09:00:00Z", "2026-12-09T09:00:00Z", { planned: true, kind: "override", override_id: "o1" }),
    ],
  };

  it("shows who is on call, the shifts around now and the rotation, which cannot be edited", async () => {
    const beacon = fakeApi({ "GET /api/v1/users": users, "GET /api/v1/schedules/platform-primary": scheduleOf(), "GET /api/v1/schedules/platform-primary/shifts": shifts });
    const { container } = schedulePage("platform-primary");

    expect(await screen.findByRole("heading", { level: 1, name: "platform-primary" })).toBeInTheDocument();
    // The schedule's own zone is named, and so is the zone the times are in.
    expect(screen.getByText(/hands over in the time zone/)).toHaveTextContent("Europe/London");
    expect(screen.getByText(/hands over in the time zone/)).toHaveTextContent("zone of your device, America/Toronto");
    expect(await within(screen.getByRole("region", { name: "On call now" })).findByText("Ada Example")).toBeInTheDocument();

    // The weeks asked for: last week, this one and the next two, from Monday midnight here.
    await waitFor(() => expect(beacon.callsTo("GET /api/v1/schedules/platform-primary/shifts")).not.toHaveLength(0));
    const asked = beacon.callsTo("GET /api/v1/schedules/platform-primary/shifts")[0]!.search;
    expect(asked.get("from")).toBe("2026-11-23T05:00:00.000Z");
    expect(asked.get("to")).toBe("2026-12-21T05:00:00.000Z");

    const rows = await within(screen.getByRole("list", { name: "Shifts" })).findAllByRole("listitem");
    expect(rows).toHaveLength(3);
    expect(rows[0]).toHaveTextContent("Grace Example");
    expect(rows[0]).toHaveTextContent("On record");
    expect(rows[1]).toHaveTextContent("Ada Example");
    expect(rows[1]).toHaveTextContent("Now");
    expect(rows[1]).toHaveTextContent("Planned");
    expect(rows[2]).toHaveTextContent("Override");
    // The picture has the shifts, and the line for now in this week only.
    expect(container.querySelectorAll(".calendar-week")).toHaveLength(4);
    expect(container.querySelectorAll(".calendar-now")).toHaveLength(1);
    expect(container.querySelectorAll(".calendar-shift.is-override")).toHaveLength(1);

    const rotation = screen.getByRole("region", { name: "Rotation" });
    expect(within(rotation).getByText(/Managed in the configuration files/)).toBeInTheDocument();
    expect(within(rotation).getByText("Monday at 09:00, Europe/London time")).toBeInTheDocument();
    expect(within(rotation).getAllByRole("listitem").map((item) => item.textContent)).toEqual(["Ada Example ada@example.com", "Grace Example grace@example.com"]);
    // Nothing on the rotation can be pressed or typed into.
    expect(within(rotation).queryByRole("button")).not.toBeInTheDocument();
    expect(within(rotation).queryByRole("textbox")).not.toBeInTheDocument();
  });

  it("moves through the weeks, and keeps the week in the address", async () => {
    const beacon = fakeApi({ "GET /api/v1/users": users, "GET /api/v1/schedules/platform-primary": scheduleOf(), "GET /api/v1/schedules/platform-primary/shifts": shifts });
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    schedulePage("platform-primary");

    await user.click(await screen.findByRole("button", { name: "Later" }));
    await waitFor(() => expect(beacon.callsTo("GET /api/v1/schedules/platform-primary/shifts").at(-1)?.search.get("from")).toBe("2026-12-21T05:00:00.000Z"));
    await user.click(screen.getByRole("button", { name: "Earlier" }));
    await user.click(screen.getByRole("button", { name: "Earlier" }));
    // Four weeks before the first view; the clocks had not gone back yet.
    await waitFor(() => expect(beacon.callsTo("GET /api/v1/schedules/platform-primary/shifts").at(-1)?.search.get("from")).toBe("2026-10-26T04:00:00.000Z"));
    await user.click(screen.getByRole("button", { name: "This week" }));
    await waitFor(() => expect(beacon.callsTo("GET /api/v1/schedules/platform-primary/shifts").at(-1)?.search.get("from")).toBe("2026-11-23T05:00:00.000Z"));
  });

  it("shows a change of the rotation that has not taken over yet", async () => {
    fakeApi({
      "GET /api/v1/users": users,
      "GET /api/v1/schedules/platform-primary": scheduleOf({
        pending: { rotation: { time_zone: "Europe/London", members: ["grace@example.com"], shift_length: "1d", handoff_time: "08:00" }, takes_over_at: "2026-12-07T09:00:00Z", stated: false },
      }),
      "GET /api/v1/schedules/platform-primary/shifts": { ...shifts, shifts: [] },
    });
    schedulePage("platform-primary");

    expect(await screen.findByRole("heading", { level: 3, name: "A change that has not taken over yet" })).toBeInTheDocument();
    expect(screen.getByText(/the next handoff; the shift in progress is not moved/)).toBeInTheDocument();
    expect(screen.getByText("08:00, Europe/London time")).toBeInTheDocument();
    expect(await screen.findByText("Nobody is on call in these weeks.")).toBeInTheDocument();
  });

  it("says when the configuration has no such schedule", async () => {
    fakeApi({ "GET /api/v1/users": users, "GET /api/v1/schedules/gone": () => status(404, { error: "no such schedule" }) });
    schedulePage("gone");
    expect(await screen.findByRole("heading", { level: 1, name: "No such schedule" })).toBeInTheDocument();
  });
});
