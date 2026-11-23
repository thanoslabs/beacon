import { describe, expect, it, vi } from "vitest";
import { fakeApi, status } from "../test/api";
import { ApiError, api, errorMessage, unwrap } from "./client";

describe("unwrap", () => {
  it("answers the body of a success", async () => {
    fakeApi({ "GET /auth/methods": { oidc: true, bootstrap_admin: false } });
    await expect(unwrap(api.GET("/auth/methods"))).resolves.toEqual({ oidc: true, bootstrap_admin: false });
  });

  it("throws what the API said, with the field at fault", async () => {
    fakeApi({ "GET /api/v1/alerts": () => status(400, { error: "must be firing, acknowledged, snoozed, resolved or open", field: "status" }) });
    const failure = await unwrap(api.GET("/api/v1/alerts")).catch((error: unknown) => error);
    expect(failure).toBeInstanceOf(ApiError);
    expect(failure).toMatchObject({ status: 400, field: "status", message: "must be firing, acknowledged, snoozed, resolved or open" });
  });

  it("says that Beacon cannot be reached when no answer comes", async () => {
    vi.stubGlobal("fetch", () => Promise.reject(new TypeError("Failed to fetch")));
    const failure = await unwrap(api.GET("/api/v1/me")).catch((error: unknown) => error);
    expect(failure).toMatchObject({ status: 0 });
    expect(errorMessage(failure)).toMatch(/cannot be reached/);
  });

  it("sends the session cookie to the same origin only, and no token of its own", async () => {
    const beacon = fakeApi({ "GET /api/v1/me": { display_name: "Ada", role: "admin" } });
    const seen: RequestCredentials[] = [];
    const inner = globalThis.fetch;
    vi.stubGlobal("fetch", (input: RequestInfo | URL, init?: RequestInit) => {
      seen.push(new Request(input, init).credentials);
      return inner(input, init);
    });
    await unwrap(api.GET("/api/v1/me"));
    expect(seen).toEqual(["same-origin"]);
    expect(beacon.calls[0]?.headers.get("Authorization")).toBeNull();
    expect(new URL(window.location.origin).origin).toBe(window.location.origin);
  });
});

describe("errorMessage", () => {
  it("does not show what a failure inside the page says", () => {
    expect(errorMessage(new TypeError("x is undefined"))).not.toContain("undefined");
  });
});
