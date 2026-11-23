import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";
import { Route, Routes } from "react-router";
import { fakeApi, status } from "../../test/api";
import { alertOf, eventOf, userActor } from "../../test/fixtures";
import { grace, renderApp } from "../../test/render";
import { AlertDetail } from "./AlertDetail";

function detail(id: string) {
  return renderApp(
    <Routes>
      <Route path="/alerts/:id" element={<AlertDetail />} />
    </Routes>,
    { as: grace, at: `/alerts/${id}` },
  );
}

function groupOf(alerts: ReturnType<typeof alertOf>[]) {
  return {
    id: alerts[0]!.group_id,
    status: "open",
    labels: { alertname: "DiskFull" },
    team: "platform",
    routing_rule: null,
    escalation_policy: "platform",
    created_at: "2026-12-01T10:12:00Z",
    notified_at: "2026-12-01T10:12:01Z",
    resolved_at: null,
    alerts,
    actions: ["acknowledge", "snooze", "resolve"],
  };
}

describe("the page of an alert", () => {
  it("shows what the alert says, what was decided for it and what happened", async () => {
    const alert = alertOf({
      labels: { alertname: "DiskFull", host: "db1", severity: "critical" },
      annotations: { summary: "Disk is full on db1", runbook_url: "https://runbooks.example.com/disk" },
      links: { generator: "https://prometheus.example.com/graph?g0.expr=up" },
      service: "postgres",
      routing_rule: "database",
    });
    const sibling = alertOf({ labels: { alertname: "DiskFull", host: "db2" }, group_id: alert.group_id, status: "acknowledged" });
    fakeApi({
      [`GET /api/v1/alerts/${alert.id}`]: alert,
      [`GET /api/v1/alert-groups/${alert.group_id}`]: groupOf([alert, sibling]),
      [`GET /api/v1/alerts/${alert.id}/timeline`]: {
        events: [
          eventOf("fired", { alert_id: alert.id, actor: { type: "source", id: "s1", name: null, provider: null, external_id: null } }),
          eventOf("escalated", { data: { policy: "platform", step: 1, steps: 2, pass: 1, passes: 2, timeout: "5m", notified: [{ target: "schedule platform-primary", name: "Ada Example", integration: "email" }], skipped: [] } }),
          eventOf("acknowledged", { alert_id: alert.id, actor: userActor("Ada Example", "slack") }),
          eventOf("something_new"),
        ],
      },
    });
    detail(alert.id);

    expect(await screen.findByRole("heading", { level: 1, name: "DiskFull" })).toBeInTheDocument();
    const about = screen.getByRole("region", { name: "About" });
    expect(within(about).getByText("postgres")).toBeInTheDocument();
    expect(within(about).getByText("database")).toBeInTheDocument();

    const labels = screen.getByRole("region", { name: "Labels" });
    expect(within(labels).getByText("host")).toBeInTheDocument();
    expect(within(labels).getByText("db1")).toBeInTheDocument();

    // A web address in an annotation is a link that leaves no trace.
    const runbook = within(screen.getByRole("region", { name: "Annotations" })).getByRole("link", { name: "https://runbooks.example.com/disk" });
    expect(runbook).toHaveAttribute("href", "https://runbooks.example.com/disk");
    expect(runbook).toHaveAttribute("rel", "noopener noreferrer");
    expect(within(screen.getByRole("region", { name: "Links" })).getByRole("link", { name: "Open at the source" })).toHaveAttribute("href", "https://prometheus.example.com/graph?g0.expr=up");

    const group = await screen.findByRole("region", { name: "Group" });
    expect(await within(group).findByText("platform")).toBeInTheDocument();
    expect(within(group).getByRole("link", { name: "DiskFull" })).toHaveAttribute("href", `/alerts/${sibling.id}`);

    const timeline = within(screen.getByRole("region", { name: "Timeline" }));
    const entries = (await timeline.findAllByRole("listitem")).map((entry) => entry.querySelector(".timeline-title")?.textContent);
    expect(entries).toEqual(["Started firing", "Escalation step 1 of 2, round 1 of 2 of the policy platform", "Acknowledged by Ada Example, from Slack", "something_new"]);
    expect(timeline.getByText("Notified Ada Example (schedule platform-primary) through email")).toBeInTheDocument();
  });

  it("acts on the alert, and shows the state it is in afterwards", async () => {
    const alert = alertOf();
    const beacon = fakeApi({
      [`GET /api/v1/alerts/${alert.id}`]: alert,
      [`GET /api/v1/alert-groups/${alert.group_id}`]: groupOf([alert]),
      [`GET /api/v1/alerts/${alert.id}/timeline`]: { events: [] },
      [`POST /api/v1/alerts/${alert.id}/acknowledge`]: () => {
        const acknowledged = { ...alert, status: "acknowledged", acknowledged_at: "2026-12-01T10:14:00Z", actions: ["snooze", "resolve"] };
        beacon.on({ [`GET /api/v1/alerts/${alert.id}`]: acknowledged });
        return acknowledged;
      },
    });
    const user = userEvent.setup();
    detail(alert.id);

    await user.click(await screen.findByRole("button", { name: "Acknowledge DiskFull" }));
    expect(await screen.findByText("Acknowledged", { selector: ".badge" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Acknowledge DiskFull" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Resolve DiskFull" })).toBeInTheDocument();
    expect(await screen.findByText("This is the only alert of its group.")).toBeInTheDocument();
  });

  it("never turns what an alert says into markup or into a link that runs a script", async () => {
    const alert = alertOf({
      labels: { alertname: "<svg onload=alert(1)>", "<b>key</b>": '"><img src=x onerror=alert(1)>' },
      annotations: { summary: "<a href='javascript:alert(1)'>click</a>", runbook_url: "javascript:alert(document.cookie)", dashboard: "data:text/html,<script>alert(1)</script>" },
      links: { generator: "javascript:alert(1)", dashboard: "https://grafana.example.com/d/abc" },
    });
    fakeApi({
      [`GET /api/v1/alerts/${alert.id}`]: alert,
      [`GET /api/v1/alert-groups/${alert.group_id}`]: groupOf([alert]),
      [`GET /api/v1/alerts/${alert.id}/timeline`]: {
        events: [eventOf("acknowledged", { actor: userActor("<img src=x onerror=alert(1)>") }), eventOf("notification_failed", { data: { destination: "<script>1</script>", error: "<b>x</b>" } })],
      },
    });
    const { container } = detail(alert.id);

    expect(await screen.findByRole("heading", { level: 1, name: "<svg onload=alert(1)>" })).toBeInTheDocument();
    await screen.findByText("Acknowledged by <img src=x onerror=alert(1)>");
    expect(screen.getByText('"><img src=x onerror=alert(1)>')).toBeInTheDocument();
    expect(container.querySelector("svg, img, script, b")).toBeNull();

    // Only the one web address is a link out of Beacon.
    const outside = Array.from(container.querySelectorAll("a")).filter((a) => !a.getAttribute("href")?.startsWith("/"));
    expect(outside.map((a) => a.getAttribute("href"))).toEqual(["https://grafana.example.com/d/abc"]);
    expect(container.querySelectorAll('a[href^="javascript:" i], a[href^="data:" i]')).toHaveLength(0);
    expect(screen.getByText("javascript:alert(document.cookie)")).toBeInTheDocument();
    expect(screen.getByText("(not a web address)")).toBeInTheDocument();
  });

  it("says when there is no such alert", async () => {
    fakeApi({ "GET /api/v1/alerts/nope": () => status(404, { error: "no such alert" }) });
    detail("nope");
    expect(await screen.findByRole("heading", { level: 1, name: "No such alert" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "All alerts" })).toHaveAttribute("href", "/alerts");
  });
});
