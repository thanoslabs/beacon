import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";
import { fakeApi, status } from "../../test/api";
import { ada, grace, renderApp } from "../../test/render";
import { Account } from "./Account";
import { Tokens } from "./Tokens";

const integrations = {
  integrations: [
    { name: "slack", type: "slack", interactions_path: "/api/v1/integrations/slack/interactions", settings: {}, provisioned: true },
    { name: "discord", type: "discord", interactions_path: "/api/v1/integrations/discord/interactions", settings: {}, provisioned: true },
    { name: "mail", type: "email", interactions_path: null, settings: {}, provisioned: true },
  ],
};

describe("chat accounts", () => {
  it("says which accounts are linked and how, and what to do about the others", async () => {
    fakeApi({
      "GET /api/v1/integrations": integrations,
      "GET /api/v1/me/identities": { identities: [{ integration: "slack", provider: "slack", external_id: "U0123ABCDEF", origin: "email", linked_at: "2026-12-01T10:12:00Z" }] },
    });
    renderApp(<Account />, { as: grace });

    const chat = screen.getByRole("region", { name: "Chat accounts" });
    const rows = await within(chat).findAllByRole("listitem");
    // Only the chat services: an email integration needs no link.
    expect(rows).toHaveLength(2);
    expect(rows[0]).toHaveTextContent("Slack");
    expect(rows[0]).toHaveTextContent("Linked");
    expect(rows[0]).toHaveTextContent("U0123ABCDEF, found by your email address");
    // Beacon would find it again, so it is not the person's to remove.
    expect(within(rows[0]!).queryByRole("button")).not.toBeInTheDocument();
    expect(rows[1]).toHaveTextContent("Not linked");
    expect(within(rows[1]!).getByRole("button", { name: "Get a link code for discord" })).toBeInTheDocument();
  });

  it("issues a link code and shows the command to type, once", async () => {
    const beacon = fakeApi({
      "GET /api/v1/integrations": integrations,
      "GET /api/v1/me/identities": { identities: [] },
      "POST /api/v1/me/identities/discord/link-code": () => status(201, { integration: "discord", provider: "discord", code: "K7Q2-M9XD", expires_at: "2026-12-01T10:22:00Z", command: "/beacon-link code: K7Q2-M9XD" }),
    });
    const user = userEvent.setup();
    renderApp(<Account />, { as: grace });

    await user.click(await screen.findByRole("button", { name: "Get a link code for discord" }));
    expect(await screen.findByText("/beacon-link code: K7Q2-M9XD")).toBeInTheDocument();
    expect(beacon.callsTo("POST /api/v1/me/identities/discord/link-code")).toHaveLength(1);
    // The code is nowhere but on the page.
    const stored = [localStorage, sessionStorage].flatMap((storage) => Object.keys(storage).map((key) => storage.getItem(key)));
    expect(stored.join()).not.toContain("K7Q2");
    // Slack is found by Beacon: there is no code to get.
    expect(screen.queryByRole("button", { name: "Get a link code for slack" })).not.toBeInTheDocument();
    expect(screen.getByText(/looks for the Slack account that has your email address/)).toBeInTheDocument();
  });

  it("says why when Beacon issues no code, as for the bootstrap admin", async () => {
    fakeApi({
      "GET /api/v1/integrations": integrations,
      "GET /api/v1/me/identities": { identities: [] },
      "POST /api/v1/me/identities/discord/link-code": () => status(403, { error: "this account is not a person who signed in through the identity provider, and cannot link a chat account" }),
    });
    const user = userEvent.setup();
    renderApp(<Account />, { as: ada });
    await user.click(await screen.findByRole("button", { name: "Get a link code for discord" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("cannot link a chat account");
  });

  it("removes a link the person made, after asking", async () => {
    const beacon = fakeApi({
      "GET /api/v1/integrations": integrations,
      "GET /api/v1/me/identities": { identities: [{ integration: "discord", provider: "discord", external_id: "80351110224678912", origin: "self", linked_at: "2026-12-01T10:12:00Z" }] },
      "DELETE /api/v1/me/identities/discord": () => status(204),
    });
    const user = userEvent.setup();
    renderApp(<Account />, { as: grace });

    await user.click(await screen.findByRole("button", { name: "Remove the link to discord" }));
    const dialog = screen.getByRole("dialog", { name: "Remove the link to your Discord account?" });
    expect(beacon.callsTo("DELETE /api/v1/me/identities/discord")).toHaveLength(0);
    beacon.on({ "GET /api/v1/me/identities": { identities: [] } });
    await user.click(within(dialog).getByRole("button", { name: "Remove the link" }));
    expect(await screen.findByRole("button", { name: "Get a link code for discord" })).toBeInTheDocument();
    expect(beacon.callsTo("DELETE /api/v1/me/identities/discord")).toHaveLength(1);
  });

  it("says when there is no chat service to link", async () => {
    fakeApi({ "GET /api/v1/integrations": { integrations: [integrations.integrations[2]] }, "GET /api/v1/me/identities": { identities: [] } });
    renderApp(<Account />, { as: grace });
    expect(await screen.findByText(/no Slack or Discord integration/)).toBeInTheDocument();
  });
});

describe("API tokens", () => {
  const deploy = { id: "t1", name: "deploy-pipeline", role: "member", created_by: "ada@example.com", created_at: "2026-11-02T09:00:00Z", expires_at: "2027-02-01T09:00:00Z" };

  it("are not shown to a member, and nothing is asked of the API", async () => {
    const beacon = fakeApi({});
    renderApp(<Tokens />, { as: grace });
    expect(screen.getByText(/API tokens are managed by admins/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "New token" })).not.toBeInTheDocument();
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(beacon.calls).toHaveLength(0);
  });

  it("are listed for an admin without their secrets", async () => {
    fakeApi({ "GET /api/v1/tokens": { tokens: [deploy, { ...deploy, id: "t2", name: "old-script", role: "admin", revoked_at: "2026-11-20T09:00:00Z", expires_at: undefined, last_used_at: "2026-11-19T09:00:00Z" }] } });
    renderApp(<Tokens />, { as: ada });

    const rows = await within(await screen.findByRole("list", { name: "API tokens" })).findAllByRole("listitem");
    expect(rows[0]).toHaveTextContent("deploy-pipeline");
    expect(rows[0]).toHaveTextContent("Member");
    expect(rows[0]).toHaveTextContent("by ada@example.com");
    expect(rows[0]).toHaveTextContent("never used");
    expect(within(rows[0]!).getByRole("button", { name: "Revoke the token deploy-pipeline" })).toBeInTheDocument();
    expect(rows[1]).toHaveTextContent("Revoked");
    expect(rows[1]).toHaveTextContent("does not expire");
    expect(within(rows[1]!).queryByRole("button")).not.toBeInTheDocument();
  });

  it("creates a token and shows its secret once, kept nowhere", async () => {
    const beacon = fakeApi({
      "GET /api/v1/tokens": { tokens: [] },
      "POST /api/v1/tokens": () => status(201, { ...deploy, token: "bcn_3f9a1c7e5b2d4f6a8c0e" }),
    });
    const user = userEvent.setup();
    renderApp(<Tokens />, { as: ada });

    await user.click(await screen.findByRole("button", { name: "New token" }));
    const dialog = screen.getByRole("dialog", { name: "New API token" });
    await user.type(within(dialog).getByLabelText("What it is for"), "deploy-pipeline");
    await user.selectOptions(within(dialog).getByLabelText("Valid for"), "30d");
    beacon.on({ "GET /api/v1/tokens": { tokens: [deploy] } });
    await user.click(within(dialog).getByRole("button", { name: "Create the token" }));

    expect(await screen.findByText("bcn_3f9a1c7e5b2d4f6a8c0e")).toBeInTheDocument();
    expect(screen.getByRole("status")).toHaveTextContent("cannot be shown again");
    expect(beacon.callsTo("POST /api/v1/tokens")[0]?.body).toEqual({ name: "deploy-pipeline", role: "member", expires_in: "30d" });
    const stored = [localStorage, sessionStorage].flatMap((storage) => Object.keys(storage).map((key) => storage.getItem(key)));
    expect(stored.join()).not.toContain("bcn_");

    await user.click(screen.getByRole("button", { name: "I have stored it" }));
    expect(screen.queryByText("bcn_3f9a1c7e5b2d4f6a8c0e")).not.toBeInTheDocument();
    expect(await screen.findByText("deploy-pipeline")).toBeInTheDocument();
  });

  it("revokes a token only after it was confirmed", async () => {
    const beacon = fakeApi({ "GET /api/v1/tokens": { tokens: [deploy] }, "DELETE /api/v1/tokens/t1": () => status(204) });
    const user = userEvent.setup();
    renderApp(<Tokens />, { as: ada });

    await user.click(await screen.findByRole("button", { name: "Revoke the token deploy-pipeline" }));
    const dialog = screen.getByRole("dialog", { name: "Revoke the token deploy-pipeline?" });
    expect(beacon.callsTo("DELETE /api/v1/tokens/t1")).toHaveLength(0);
    await user.click(within(dialog).getByRole("button", { name: "Revoke the token" }));
    await waitFor(() => expect(beacon.callsTo("DELETE /api/v1/tokens/t1")).toHaveLength(1));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  });
});
