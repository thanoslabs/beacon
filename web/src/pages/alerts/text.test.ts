import { describe, expect, it } from "vitest";
import { alertSummary, alertTitle, safeHref } from "./text";

describe("safeHref", () => {
  it("lets web addresses through", () => {
    expect(safeHref("https://runbooks.example.com/disk?x=1#top")).toBe("https://runbooks.example.com/disk?x=1#top");
    expect(safeHref("http://prometheus:9090/graph")).toBe("http://prometheus:9090/graph");
    expect(safeHref("HTTPS://Example.com/")).toBe("https://example.com/");
  });

  it("refuses everything that is not plainly one", () => {
    for (const value of [
      "javascript:alert(document.cookie)",
      "JaVaScRiPt:alert(1)",
      " javascript:alert(1)",
      "java\nscript:alert(1)",
      "data:text/html,<script>alert(1)</script>",
      "vbscript:msgbox(1)",
      "file:///etc/passwd",
      "//evil.example/path",
      "/alerts/1",
      "runbooks.example.com/disk",
      "https://",
      "",
      "https://example.com/" + "a".repeat(3000),
    ]) {
      expect(safeHref(value), value).toBeUndefined();
    }
  });
});

describe("what an alert is called", () => {
  it("is its alertname, or says which alert it is", () => {
    expect(alertTitle({ labels: { alertname: "DiskFull" }, fingerprint: "8d1f" })).toBe("DiskFull");
    expect(alertTitle({ labels: {}, fingerprint: "8d1f" })).toBe("Alert 8d1f");
  });

  it("has a summary when the source sent one", () => {
    expect(alertSummary({ annotations: { summary: "Disk is full", description: "Longer" } })).toBe("Disk is full");
    expect(alertSummary({ annotations: { description: "Longer" } })).toBe("Longer");
    expect(alertSummary({ annotations: {} })).toBeUndefined();
  });
});
