import { screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { fakeApi, status } from "../../test/api";
import { ada, grace, renderApp } from "../../test/render";
import { Connections } from "./Connections";
import { Policies } from "./Policies";
import { RoutingPage } from "./RoutingPage";
import { Teams } from "./Teams";

const target = { schedule: null, team: null, user: null, channel: null, integration: null, via: [] as string[] };

const policies = {
  escalation_policies: [
    {
      name: "platform",
      repeat: 1,
      provisioned: true,
      steps: [
        { timeout: "5m", notify: [{ ...target, kind: "schedule", schedule: "platform-primary" }] },
        {
          timeout: "10m",
          notify: [
            { ...target, kind: "team", team: "platform", via: ["slack"] },
            { ...target, kind: "channel", channel: "C0ALERTS", integration: "slack" },
            { ...target, kind: "user", user: "ada@example.com" },
            { ...target, kind: "webhook", integration: "tickets" },
          ],
        },
      ],
    },
  ],
};

const routing = {
  group_by: ["alertname"],
  group_wait: "30s",
  group_interval: "5m",
  default: { team: "platform", escalation_policy: "platform" },
  rules: [
    { name: "database", match: { service: "postgres" }, match_re: { severity: "critical|page" }, team: "dba", service: "postgres", escalation_policy: "platform", group_by: ["alertname", "host"], group_wait: "0s", group_interval: "1m" },
    { name: "everything-else", match: {}, match_re: {}, team: null, service: null, escalation_policy: "platform", group_by: [], group_wait: "30s", group_interval: "5m" },
  ],
  provisioned: true,
};

const team = {
  name: "platform",
  display_name: "Platform",
  groups: ["platform-engineers"],
  members: [
    { email: "ada@example.com", display_name: "Ada Example", origins: ["config", "group"] },
    { email: "grace@example.com", display_name: null, origins: ["group"] },
  ],
  schedules: ["platform-primary"],
  provisioned: true,
};

/** Nothing on a page of the configuration can change anything. */
function expectReadOnly() {
  expect(screen.getByText(/Managed in the configuration files/)).toBeInTheDocument();
  expect(screen.queryByRole("button")).not.toBeInTheDocument();
  expect(screen.queryByRole("textbox")).not.toBeInTheDocument();
  expect(screen.queryByRole("combobox")).not.toBeInTheDocument();
  expect(screen.queryByRole("checkbox")).not.toBeInTheDocument();
}

describe("escalation policies", () => {
  it("say whom each step notifies, how long it waits and how often the policy repeats", async () => {
    fakeApi({ "GET /api/v1/escalation-policies": policies });
    renderApp(<Policies />, { as: grace });

    const policy = await screen.findByRole("region", { name: "platform" });
    const steps = within(policy).getAllByRole("listitem").filter((item) => item.classList.contains("step"));
    expect(steps).toHaveLength(2);
    expect(steps[0]).toHaveTextContent("Step 1 notifies:");
    expect(within(steps[0]!).getByRole("link", { name: "platform-primary" })).toHaveAttribute("href", "/schedules/platform-primary");
    expect(steps[0]).toHaveTextContent("Without a response after 5m, the next step runs.");
    expect(steps[1]).toHaveTextContent("Everyone in the team platform, through slack");
    expect(steps[1]).toHaveTextContent("The channel C0ALERTS of the integration slack");
    expect(steps[1]).toHaveTextContent("ada@example.com");
    expect(steps[1]).toHaveTextContent("The outbound webhook tickets");
    expect(policy).toHaveTextContent("After the last step it starts again, once, and then ends.");
    expectReadOnly();
  });

  it("say when there are none", async () => {
    fakeApi({ "GET /api/v1/escalation-policies": { escalation_policies: [] } });
    renderApp(<Policies />, { as: grace });
    expect(await screen.findByText("No escalation policies")).toBeInTheDocument();
  });
});

describe("routing", () => {
  it("shows the rules in the order they are tried, and the default route", async () => {
    fakeApi({ "GET /api/v1/routing": routing });
    renderApp(<RoutingPage />, { as: grace });

    const rules = within(await screen.findByRole("list", { name: "Rules" }));
    expect(rules.getAllByRole("heading", { level: 2 }).map((heading) => heading.textContent)).toEqual(["1. database", "2. everything-else"]);
    const database = rules.getByRole("region", { name: "1. database" });
    expect(database).toHaveTextContent('service="postgres"');
    expect(database).toHaveTextContent('severity=~"critical|page"');
    expect(database).toHaveTextContent("dba");
    expect(database).toHaveTextContent("first notification after 0s, updates every 1m");
    expect(rules.getByRole("region", { name: "2. everything-else" })).toHaveTextContent("Every alert");
    expect(rules.getByRole("region", { name: "2. everything-else" })).toHaveTextContent("Nothing: every alert is told about by itself");

    const fallback = screen.getByRole("region", { name: "Default route" });
    expect(within(fallback).getAllByRole("link", { name: "platform" })).toHaveLength(2);
    expect(fallback).toHaveTextContent("first notification after 30s, updates every 5m");
    expectReadOnly();
  });

  it("says what it means when the configuration has no routing", async () => {
    fakeApi({ "GET /api/v1/routing": () => status(404, { error: "the configuration has no routing block" }) });
    renderApp(<RoutingPage />, { as: grace });
    expect(await screen.findByText("No routing")).toBeInTheDocument();
    expect(screen.getByText(/nobody is notified/)).toBeInTheDocument();
  });
});

describe("teams", () => {
  it("show who belongs and why, and what the team owns", async () => {
    fakeApi({ "GET /api/v1/teams": { teams: [team] } });
    renderApp(<Teams />, { as: grace });

    const card = await screen.findByRole("region", { name: /Platform/ });
    expect(card).toHaveTextContent("Ada Example ada@example.com (listed in the files, and by a group of the identity provider)");
    expect(card).toHaveTextContent("grace@example.com (by a group of the identity provider)");
    expect(card).toHaveTextContent("platform-engineers");
    expect(within(card).getByRole("link", { name: "platform-primary" })).toHaveAttribute("href", "/schedules/platform-primary");
    expect(screen.queryByText(/not users yet/)).not.toBeInTheDocument();
    expectReadOnly();
  });

  it("show an admin the listed people who are not users yet", async () => {
    fakeApi({ "GET /api/v1/teams": { teams: [{ ...team, skipped_members: ["hopper@example.com"] }] } });
    renderApp(<Teams />, { as: ada });

    const card = await screen.findByRole("region", { name: /Platform/ });
    expect(within(card).getByText(/Listed in the files, and not users yet/)).toBeInTheDocument();
    expect(within(card).getByText("hopper@example.com")).toBeInTheDocument();
    expect(within(card).getByText("Only admins see this.")).toBeInTheDocument();
  });

  it("show a member none of them, even if the answer had them", async () => {
    // The API leaves the field out for a member. Should it ever not, the
    // page still does not show it.
    fakeApi({ "GET /api/v1/teams": { teams: [{ ...team, skipped_members: ["hopper@example.com"] }] } });
    renderApp(<Teams />, { as: grace });

    await screen.findByRole("region", { name: /Platform/ });
    expect(screen.queryByText("hopper@example.com")).not.toBeInTheDocument();
    expect(screen.queryByText(/not users yet/)).not.toBeInTheDocument();
  });
});

describe("sources and integrations", () => {
  it("show what is not a credential, as text", async () => {
    fakeApi({
      "GET /api/v1/sources": { sources: [{ name: "prod", type: "alertmanager", ingest_path: "/api/v1/ingest/prod", labels: { env: "prod" }, severity: { label: "severity", mapping: {}, default: "warning" }, provisioned: true }] },
      "GET /api/v1/integrations": {
        integrations: [
          { name: "slack", type: "slack", interactions_path: "/api/v1/integrations/slack/interactions", settings: {}, provisioned: true },
          { name: "tickets", type: "webhook", interactions_path: null, settings: { url: "https://tickets.example.com/hook", headers: ["X-Team"] }, provisioned: true },
          { name: "mail", type: "email", interactions_path: null, settings: { host: "smtp.example.com", port: 587, authenticated: true, from: "<b>beacon</b>@example.com" }, provisioned: true },
        ],
      },
    });
    const { container } = renderApp(<Connections />, { as: grace });

    const sources = await screen.findByRole("region", { name: "Sources" });
    expect(await within(sources).findByText("Alertmanager")).toBeInTheDocument();
    expect(sources).toHaveTextContent("/api/v1/ingest/prod");
    expect(sources).toHaveTextContent('env="prod"');
    const integrations = screen.getByRole("region", { name: "Integrations" });
    expect(await within(integrations).findByText("Outbound webhook")).toBeInTheDocument();
    expect(integrations).toHaveTextContent("https://tickets.example.com/hook");
    expect(integrations).toHaveTextContent("X-Team");
    expect(integrations).toHaveTextContent("587");
    expect(integrations).toHaveTextContent("<b>beacon</b>@example.com");
    expect(container.querySelector("b")).toBeNull();
    expectReadOnly();
  });
});
