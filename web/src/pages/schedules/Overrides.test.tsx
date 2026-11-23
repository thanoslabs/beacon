import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";
import { fakeApi, status } from "../../test/api";
import { users } from "../../test/fixtures";
import { grace, renderApp } from "../../test/render";
import { Overrides } from "./Overrides";

const cover = {
  id: "0d6f3b52-7a0e-4a0f-9d0b-0a6e5f1f3c77",
  schedule: "platform-primary",
  user: "grace@example.com",
  starts_at: "2026-12-24T09:00:00Z",
  ends_at: "2026-12-27T09:00:00Z",
  reason: "covering the holidays",
  created_by: "ada@example.com",
  created_at: "2026-12-01T10:12:00Z",
  state: "pending",
};

const path = "/api/v1/schedules/platform-primary/overrides";

describe("the overrides of a schedule", () => {
  it("lists what is in force and to come, with who covers, when, why and who added it", async () => {
    fakeApi({ "GET /api/v1/users": users, [`GET ${path}`]: { overrides: [{ ...cover, id: "o-active", state: "active", reason: "" }, cover] } });
    renderApp(<Overrides schedule="platform-primary" />, { as: grace });

    const rows = await within(await screen.findByRole("list", { name: "Overrides" })).findAllByRole("listitem");
    expect(rows).toHaveLength(2);
    expect(rows[0]).toHaveTextContent("In force");
    expect(rows[1]).toHaveTextContent("To come");
    expect(await within(rows[1]!).findByText("Grace Example")).toBeInTheDocument();
    expect(rows[1]).toHaveTextContent("covering the holidays · Added by Ada Example");
  });

  it("adds one: the times typed on the device's clock are sent as instants", async () => {
    const beacon = fakeApi({ "GET /api/v1/users": users, [`GET ${path}`]: { overrides: [] }, [`POST ${path}`]: () => status(201, cover) });
    const user = userEvent.setup();
    renderApp(<Overrides schedule="platform-primary" />, { as: grace });

    expect(await screen.findByText("No override is in force or to come.")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Add an override" }));
    const dialog = screen.getByRole("dialog", { name: "Add an override to platform-primary" });
    // Whoever is signed in is the first guess for who covers.
    await waitFor(() => expect(within(dialog).getByLabelText("Who covers")).toHaveValue("grace@example.com"));
    expect(within(dialog).getByText(/zone of your device, America\/Toronto/)).toBeInTheDocument();

    await user.selectOptions(within(dialog).getByLabelText("Who covers"), "ada@example.com");
    await user.type(within(dialog).getByLabelText("Starts"), "2026-12-24T09:00");
    await user.type(within(dialog).getByLabelText("Ends"), "2026-12-27T09:00");
    await user.type(within(dialog).getByLabelText("Reason (optional)"), "  covering the holidays ");
    await user.click(within(dialog).getByRole("button", { name: "Add the override" }));

    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    // 09:00 in Toronto in December is 14:00 UTC.
    expect(beacon.callsTo(`POST ${path}`)[0]?.body).toEqual({ user: "ada@example.com", starts_at: "2026-12-24T14:00:00.000Z", ends_at: "2026-12-27T14:00:00.000Z", reason: "covering the holidays" });
    // The list was asked for again.
    expect(beacon.callsTo(`GET ${path}`).length).toBeGreaterThan(1);
  });

  it("starts now when no start is given, and shows what the API refuses at the field", async () => {
    const beacon = fakeApi({
      "GET /api/v1/users": users,
      [`GET ${path}`]: { overrides: [] },
      [`POST ${path}`]: () => status(400, { error: "must be in the future", field: "ends_at" }),
    });
    const user = userEvent.setup();
    renderApp(<Overrides schedule="platform-primary" />, { as: grace });

    await user.click(await screen.findByRole("button", { name: "Add an override" }));
    const dialog = screen.getByRole("dialog");
    await waitFor(() => expect(within(dialog).getByLabelText("Who covers")).toHaveValue("grace@example.com"));
    await user.type(within(dialog).getByLabelText("Ends"), "2020-01-01T09:00");
    await user.click(within(dialog).getByRole("button", { name: "Add the override" }));

    expect(await within(dialog).findByRole("alert")).toHaveTextContent("must be in the future");
    expect(within(dialog).getByLabelText("Ends")).toHaveAttribute("aria-invalid", "true");
    expect(within(dialog).getByLabelText("Starts")).toHaveAttribute("aria-invalid", "false");
    expect(beacon.callsTo(`POST ${path}`)[0]?.body).toEqual({ user: "grace@example.com", ends_at: "2020-01-01T14:00:00.000Z" });
    // The dialog stays, with what was typed.
    expect(within(dialog).getByLabelText("Ends")).toHaveValue("2020-01-01T09:00");
  });

  it("removes one only after it was confirmed", async () => {
    const beacon = fakeApi({ "GET /api/v1/users": users, [`GET ${path}`]: { overrides: [cover] }, [`DELETE ${path}/${cover.id}`]: () => status(204) });
    const user = userEvent.setup();
    renderApp(<Overrides schedule="platform-primary" />, { as: grace });

    await user.click(await screen.findByRole("button", { name: "Remove the override of Grace Example" }));
    let dialog = screen.getByRole("dialog", { name: "Remove this override?" });
    await user.click(within(dialog).getByRole("button", { name: "Keep it" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(beacon.callsTo(`DELETE ${path}/${cover.id}`)).toHaveLength(0);

    await user.click(screen.getByRole("button", { name: "Remove the override of Grace Example" }));
    dialog = screen.getByRole("dialog", { name: "Remove this override?" });
    beacon.on({ [`GET ${path}`]: { overrides: [] } });
    await user.click(within(dialog).getByRole("button", { name: "Remove the override" }));
    expect(await screen.findByText("No override is in force or to come.")).toBeInTheDocument();
    expect(beacon.callsTo(`DELETE ${path}/${cover.id}`)).toHaveLength(1);
  });
});
