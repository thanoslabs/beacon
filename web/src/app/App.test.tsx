import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";
import { api, unwrap } from "../api/client";
import { fakeApi, status, unauthenticated } from "../test/api";
import { ada, grace, renderApp } from "../test/render";
import { App } from "./App";

describe("a stranger", () => {
  it("is shown the ways to sign in that exist, at the address they asked for", async () => {
    fakeApi({ "GET /api/v1/me": unauthenticated, "GET /auth/methods": { oidc: true, bootstrap_admin: false } });
    renderApp(<App />, { at: "/alerts/9b2d" });

    const link = await screen.findByRole("link", { name: "Sign in with your identity provider" });
    expect(link).toHaveAttribute("href", "/auth/login");
    expect(screen.getByRole("heading", { level: 1, name: "Sign in" })).toBeInTheDocument();
    expect(screen.queryByLabelText("Bootstrap admin password")).not.toBeInTheDocument();
    expect(screen.queryByRole("navigation")).not.toBeInTheDocument();
  });

  it("is told when nobody can sign in", async () => {
    fakeApi({ "GET /api/v1/me": unauthenticated, "GET /auth/methods": { oidc: false, bootstrap_admin: false } });
    renderApp(<App />);

    expect(await screen.findByText(/Nobody can sign in yet/)).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: /Sign in/ })).not.toBeInTheDocument();
  });

  it("remembers the page that was asked for when it goes to the identity provider", async () => {
    fakeApi({ "GET /api/v1/me": unauthenticated, "GET /auth/methods": { oidc: true, bootstrap_admin: false } });
    renderApp(<App />);
    const link = await screen.findByRole("link", { name: "Sign in with your identity provider" });
    // jsdom does not navigate; the click only runs the handler.
    link.addEventListener("click", (event) => event.preventDefault());
    await userEvent.click(link);

    expect(sessionStorage.getItem("beacon.return_to")).toBe(window.location.pathname + window.location.search);
  });

  it("signs in as the bootstrap admin with the password, and sees why when it is wrong", async () => {
    const beacon = fakeApi({
      "GET /api/v1/me": unauthenticated,
      "GET /auth/methods": { oidc: false, bootstrap_admin: true },
      "POST /auth/bootstrap": () => status(401, { error: "wrong password" }),
    });
    const user = userEvent.setup();
    renderApp(<App />);

    await user.type(await screen.findByLabelText("Bootstrap admin password"), "guess");
    await user.click(screen.getByRole("button", { name: "Sign in as the bootstrap admin" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("wrong password");
    // A refused password is no ended session.
    expect(screen.queryByText(/Your session has ended/)).not.toBeInTheDocument();

    beacon.on({ "POST /auth/bootstrap": { ...ada, display_name: "Bootstrap admin", email: "bootstrap-admin@beacon.invalid" } });
    await user.clear(screen.getByLabelText("Bootstrap admin password"));
    await user.type(screen.getByLabelText("Bootstrap admin password"), "right");
    await user.click(screen.getByRole("button", { name: "Sign in as the bootstrap admin" }));

    expect(await screen.findByRole("navigation", { name: "Main" })).toBeInTheDocument();
    expect(beacon.callsTo("POST /auth/bootstrap").at(-1)?.body).toEqual({ password: "right" });
    // The password is nowhere a script could find it later.
    const stored = [localStorage, sessionStorage].flatMap((storage) => Object.keys(storage).map((key) => storage.getItem(key)));
    expect(stored.join()).not.toContain("right");
    expect(document.cookie).toBe("");
  });
});

describe("someone who is signed in", () => {
  it("lands on the alerts", async () => {
    fakeApi({ "GET /api/v1/me": grace, "GET /api/v1/alerts": { alerts: [] }, "GET /api/v1/teams": { teams: [] } });
    renderApp(<App />);

    expect(await screen.findByRole("heading", { level: 1, name: "Alerts" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Alerts" })).toHaveAttribute("aria-current", "page");
    expect(await screen.findByText("No open alerts")).toBeInTheDocument();
  });

  it("sees who they are on the account page, inside the frame", async () => {
    fakeApi({ "GET /api/v1/me": grace });
    renderApp(<App />, { at: "/account" });

    expect(await screen.findByRole("heading", { level: 1, name: "Account" })).toBeInTheDocument();
    const account = screen.getByRole("region", { name: "Signed in as" });
    expect(within(account).getByText("grace@example.com")).toBeInTheDocument();
    expect(within(account).getByText("Member")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Account" })).toHaveAttribute("aria-current", "page");
    expect(screen.getByRole("link", { name: "Skip to the page" })).toHaveAttribute("href", "#main");
  });

  it("is told that an address is no page, without leaving the app", async () => {
    fakeApi({ "GET /api/v1/me": ada });
    renderApp(<App />, { at: "/no-such-page" });

    expect(await screen.findByRole("heading", { level: 1, name: "Page not found" })).toBeInTheDocument();
    expect(screen.getByRole("navigation", { name: "Main" })).toBeInTheDocument();
  });

  it("signs out, and nothing that was read stays", async () => {
    const beacon = fakeApi({ "GET /api/v1/me": ada, "POST /auth/logout": () => status(204), "GET /auth/methods": { oidc: true, bootstrap_admin: false } });
    const user = userEvent.setup();
    const { queryClient } = renderApp(<App />, { at: "/account" });
    await screen.findByRole("heading", { level: 1, name: "Account" });
    queryClient.setQueryData(["schedules"], [{ id: "kept from the session" }]);

    await user.click(screen.getByRole("button", { name: "Sign out" }));

    expect(await screen.findByRole("heading", { level: 1, name: "Sign in" })).toBeInTheDocument();
    expect(beacon.callsTo("POST /auth/logout")).toHaveLength(1);
    expect(queryClient.getQueryData(["schedules"])).toBeUndefined();
    expect(screen.queryByText("Ada Example")).not.toBeInTheDocument();
  });

  it("is sent to the sign-in page when the session ends while the app is open", async () => {
    fakeApi({ "GET /api/v1/me": ada, "GET /api/v1/alerts": unauthenticated, "GET /auth/methods": { oidc: true, bootstrap_admin: false } });
    const { queryClient } = renderApp(<App />, { at: "/account" });
    await screen.findByRole("heading", { level: 1, name: "Account" });
    queryClient.setQueryData(["schedules"], [{ id: "kept from the session" }]);

    // Any request of any page that the API answers with 401.
    await expect(unwrap(api.GET("/api/v1/alerts"))).rejects.toMatchObject({ status: 401 });

    expect(await screen.findByText(/Your session has ended/)).toBeInTheDocument();
    expect(screen.getByRole("heading", { level: 1, name: "Sign in" })).toBeInTheDocument();
    await waitFor(() => expect(queryClient.getQueryData(["schedules"])).toBeUndefined());
  });

  it("is told when Beacon cannot be reached, and can try again", async () => {
    const beacon = fakeApi({ "GET /api/v1/me": () => status(500, { error: "internal error" }) });
    const user = userEvent.setup();
    renderApp(<App />, { at: "/account" });

    expect(await screen.findByRole("alert")).toHaveTextContent("internal error");
    beacon.on({ "GET /api/v1/me": ada });
    await user.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByRole("navigation", { name: "Main" })).toBeInTheDocument();
  });
});

describe("the theme", () => {
  it("follows the device until a person chooses, and remembers the choice", async () => {
    fakeApi({ "GET /api/v1/me": ada });
    const user = userEvent.setup();
    renderApp(<App />);
    const select = await screen.findByLabelText("Theme");
    expect(select).toHaveValue("system");
    expect(document.documentElement.dataset.theme).toBeUndefined();

    await user.selectOptions(select, "dark");
    expect(document.documentElement.dataset.theme).toBe("dark");
    expect(localStorage.getItem("beacon.theme")).toBe("dark");

    await user.selectOptions(select, "system");
    expect(document.documentElement.dataset.theme).toBeUndefined();
    expect(localStorage.getItem("beacon.theme")).toBeNull();
  });
});
