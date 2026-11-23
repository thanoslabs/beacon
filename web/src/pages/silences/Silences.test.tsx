import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Route, Routes } from "react-router";
import { describe, expect, it } from "vitest";
import { fakeApi, status } from "../../test/api";
import { alertOf, users } from "../../test/fixtures";
import { grace, renderApp } from "../../test/render";
import { NewSilence } from "./NewSilence";
import { SilenceList } from "./SilenceList";

const disks = {
  id: "5b0c1f0e-3c0a-4a53-9a39-0f8f3a2d6c11",
  matchers: [
    { label: "alertname", operator: "=", value: "DiskFull" },
    { label: "host", operator: "=~", value: "db[0-9]+" },
  ],
  starts_at: "2026-12-01T12:00:00Z",
  ends_at: "2026-12-01T14:00:00Z",
  created_by: "ada@example.com",
  comment: "replacing the disks",
  created_at: "2026-12-01T12:00:00Z",
  state: "active",
};

function pages(at = "/silences") {
  return renderApp(
    <Routes>
      <Route path="/silences" element={<SilenceList />} />
      <Route path="/silences/new" element={<NewSilence />} />
    </Routes>,
    { as: grace, at },
  );
}

describe("the list of silences", () => {
  it("shows what each silence matches, when, why and by whom", async () => {
    fakeApi({ "GET /api/v1/users": users, "GET /api/v1/silences": { silences: [disks, { ...disks, id: "s2", state: "pending", comment: "tomorrow's migration", created_by: undefined, created_by_token: "deploy-pipeline" }] } });
    pages();

    const rows = await within(await screen.findByRole("list", { name: "Silences" })).findAllByRole("listitem", { name: "" });
    const first = rows.find((row) => row.textContent.includes("replacing the disks"))!;
    expect(first).toHaveTextContent("Active");
    expect(first).toHaveTextContent('alertname="DiskFull"');
    expect(first).toHaveTextContent('host=~"db[0-9]+"');
    expect(await within(first).findByText(/Created by Ada Example/)).toBeInTheDocument();
    const second = rows.find((row) => row.textContent.includes("tomorrow's migration"))!;
    expect(second).toHaveTextContent("Pending");
    expect(second).toHaveTextContent("Created by API token deploy-pipeline");
  });

  it("asks for the expired ones only when told to, and offers nothing to do with them", async () => {
    const beacon = fakeApi({ "GET /api/v1/users": users, "GET /api/v1/silences": { silences: [] } });
    const user = userEvent.setup();
    pages();

    expect(await screen.findByText("No silence is active or pending")).toBeInTheDocument();
    expect(beacon.callsTo("GET /api/v1/silences")[0]?.search.has("expired")).toBe(false);

    beacon.on({ "GET /api/v1/silences": { silences: [{ ...disks, state: "expired" }] } });
    await user.click(screen.getByLabelText("Also show expired silences"));
    expect(await screen.findByText("Expired")).toBeInTheDocument();
    expect(beacon.callsTo("GET /api/v1/silences").at(-1)?.search.get("expired")).toBe("true");
    expect(screen.queryByRole("button", { name: /Expire the silence/ })).not.toBeInTheDocument();
  });

  it("expires a silence only after it was confirmed", async () => {
    const beacon = fakeApi({ "GET /api/v1/users": users, "GET /api/v1/silences": { silences: [disks] }, [`DELETE /api/v1/silences/${disks.id}`]: () => status(204) });
    const user = userEvent.setup();
    pages();

    await user.click(await screen.findByRole("button", { name: "Expire the silence: replacing the disks" }));
    let dialog = screen.getByRole("dialog", { name: "Expire this silence?" });
    expect(dialog).toHaveTextContent("It ends now.");
    await user.click(within(dialog).getByRole("button", { name: "Keep it" }));
    expect(beacon.callsTo(`DELETE /api/v1/silences/${disks.id}`)).toHaveLength(0);

    await user.click(screen.getByRole("button", { name: "Expire the silence: replacing the disks" }));
    dialog = screen.getByRole("dialog", { name: "Expire this silence?" });
    beacon.on({ "GET /api/v1/silences": { silences: [] } });
    await user.click(within(dialog).getByRole("button", { name: "Expire the silence" }));
    expect(await screen.findByText("No silence is active or pending")).toBeInTheDocument();
    expect(beacon.callsTo(`DELETE /api/v1/silences/${disks.id}`)).toHaveLength(1);
  });

  it("shows what a silence says as text", async () => {
    fakeApi({ "GET /api/v1/users": users, "GET /api/v1/silences": { silences: [{ ...disks, comment: "<img src=x onerror=alert(1)>", matchers: [{ label: "<b>x</b>", operator: "=", value: "<script>1</script>" }] }] } });
    const { container } = pages();
    expect(await screen.findByText("<img src=x onerror=alert(1)>")).toBeInTheDocument();
    expect(container.querySelector("img, script, b")).toBeNull();
  });
});

describe("a new silence", () => {
  it("previews the open alerts it would silence, as Beacon matches them", async () => {
    const matching = alertOf({ labels: { alertname: "DiskFull", host: "db1" } });
    const beacon = fakeApi({ "POST /api/v1/silences/preview": { alerts: [matching], total: 3, complete: false } });
    const user = userEvent.setup();
    pages("/silences/new");

    await user.type(screen.getByLabelText("Value of matcher 1"), "DiskFull");
    const preview = screen.getByRole("region", { name: "Open alerts this would silence now" });
    expect(await within(preview).findByText("3 open alerts match.")).toBeInTheDocument();
    expect(within(preview).getByRole("link", { name: "DiskFull" })).toHaveAttribute("href", `/alerts/${matching.id}`);
    expect(preview).toHaveTextContent("there may be more");
    expect(preview).toHaveTextContent("The newest 1 are shown.");
    // What was asked is what is in the form, once the typing had stopped.
    await waitFor(() => expect(beacon.callsTo("POST /api/v1/silences/preview").at(-1)?.body).toEqual({ matchers: [{ label: "alertname", operator: "=", value: "DiskFull" }] }));
    // Not once for every key that was pressed.
    expect(beacon.callsTo("POST /api/v1/silences/preview").length).toBeLessThan(5);

    // A matcher without a label is not asked about.
    await user.click(screen.getByRole("button", { name: "Add a matcher" }));
    expect(await within(preview).findByText(/Name a label in every matcher/)).toBeInTheDocument();
  });

  it("says what Beacon refuses in a matcher, in the preview", async () => {
    fakeApi({ "POST /api/v1/silences/preview": () => status(400, { error: "is not a regular expression: missing closing )", field: "matchers[0].value" }) });
    const user = userEvent.setup();
    pages("/silences/new");

    await user.selectOptions(screen.getByLabelText("Operator of matcher 1"), "=~");
    await user.type(screen.getByLabelText("Value of matcher 1"), "(x");
    const preview = screen.getByRole("region", { name: "Open alerts this would silence now" });
    expect(await within(preview).findByText(/matchers\[0\]\.value: is not a regular expression/)).toBeInTheDocument();
  });

  it("creates the silence with what was entered, and goes back to the list", async () => {
    const beacon = fakeApi({
      "GET /api/v1/users": users,
      "GET /api/v1/silences": { silences: [disks] },
      "POST /api/v1/silences/preview": { alerts: [], total: 0, complete: true },
      "POST /api/v1/silences": () => status(201, disks),
    });
    const user = userEvent.setup();
    pages("/silences/new");

    await user.type(screen.getByLabelText("Value of matcher 1"), "DiskFull");
    await user.click(screen.getByRole("button", { name: "Add a matcher" }));
    await user.type(screen.getByLabelText("Label of matcher 2"), "host");
    await user.selectOptions(screen.getByLabelText("Operator of matcher 2"), "=~");
    await user.type(screen.getByLabelText("Value of matcher 2"), "db[[0-9]+");
    await user.selectOptions(screen.getByLabelText("For how long, from now"), "4h");
    await user.type(screen.getByLabelText("Why"), "replacing the disks");
    await user.click(screen.getByRole("button", { name: "Create the silence" }));

    expect(await screen.findByRole("heading", { level: 1, name: "Silences" })).toBeInTheDocument();
    expect(beacon.callsTo("POST /api/v1/silences")[0]?.body).toEqual({
      matchers: [
        { label: "alertname", operator: "=", value: "DiskFull" },
        { label: "host", operator: "=~", value: "db[0-9]+" },
      ],
      duration: "4h",
      comment: "replacing the disks",
    });
  });

  it("sends an end that was chosen as an instant, and marks the field that Beacon refuses", async () => {
    const beacon = fakeApi({
      "POST /api/v1/silences/preview": { alerts: [], total: 0, complete: true },
      "POST /api/v1/silences": () => status(400, { error: "must not match an empty value in every matcher", field: "matchers[0].value" }),
    });
    const user = userEvent.setup();
    pages("/silences/new");

    await user.selectOptions(screen.getByLabelText("For how long, from now"), "");
    await user.type(screen.getByLabelText("Ends"), "2026-12-24T09:00");
    await user.type(screen.getByLabelText("Why"), "holidays");
    await user.click(screen.getByRole("button", { name: "Create the silence" }));

    expect(await within(screen.getByRole("form", { name: "New silence" })).findByRole("alert")).toHaveTextContent("must not match an empty value in every matcher");
    expect(screen.getByLabelText("Value of matcher 1")).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByLabelText("Label of matcher 1")).toHaveAttribute("aria-invalid", "false");
    await waitFor(() => expect(beacon.callsTo("POST /api/v1/silences")[0]?.body).toEqual({ matchers: [{ label: "alertname", operator: "=", value: "" }], ends_at: "2026-12-24T14:00:00.000Z", comment: "holidays" }));
    // Still on the form, with what was entered.
    expect(screen.getByLabelText("Why")).toHaveValue("holidays");
  });
});
