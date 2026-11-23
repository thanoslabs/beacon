import { describe, expect, it } from "vitest";
import { rememberReturnTo, takeReturnTo } from "./session";

describe("the page to return to after a sign-in", () => {
  it("is given once", () => {
    rememberReturnTo("/alerts/9b2d?tab=timeline");
    expect(takeReturnTo()).toBe("/alerts/9b2d?tab=timeline");
    expect(takeReturnTo()).toBeNull();
  });

  it("is only ever a path of this app", () => {
    // Anything else in storage, however it got there, leads nowhere.
    for (const elsewhere of ["https://evil.example/", "//evil.example/alerts", "/\\evil.example", "javascript:alert(1)", "alerts", "", "/"]) {
      rememberReturnTo(elsewhere);
      expect(takeReturnTo(), elsewhere).toBeNull();
    }
  });
});
