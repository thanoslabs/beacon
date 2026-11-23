import { describe, expect, it } from "vitest";
import { formatDateTime } from "./format";

describe("formatDateTime", () => {
  it("says the time in the zone it is asked for, and names the zone", () => {
    const at = "2026-12-01T10:12:00Z";
    expect(formatDateTime(at, { timeZone: "UTC", locale: "en-GB" })).toBe("1 Dec 2026, 10:12 UTC");
    expect(formatDateTime(at, { timeZone: "America/Toronto", locale: "en-GB" })).toMatch(/^1 Dec 2026, 05:12 (GMT-5|EST)$/);
    expect(formatDateTime(at, { timeZone: "Australia/Sydney", locale: "en-GB" })).toMatch(/^1 Dec 2026, 21:12 (GMT\+11|AEDT)$/);
  });

  it("does not show a broken time as a time", () => {
    expect(formatDateTime("not a time")).toBe("unknown time");
  });
});
