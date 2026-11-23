import { describe, expect, it } from "vitest";
import { eventOf, userActor } from "../../test/fixtures";
import { describeEvent } from "./timeline";

const zone = { timeZone: "UTC", locale: "en-GB" };
const source = { type: "source" as const, id: "5c1e", name: null, provider: null, external_id: null };

describe("describeEvent", () => {
  it("says who changed the state, and from where", () => {
    expect(describeEvent(eventOf("fired", { actor: source })).title).toBe("Started firing");
    expect(describeEvent(eventOf("acknowledged", { actor: userActor("Ada Example") })).title).toBe("Acknowledged by Ada Example");
    expect(describeEvent(eventOf("acknowledged", { actor: userActor("Ada Example", "slack") })).title).toBe("Acknowledged by Ada Example, from Slack");
    expect(describeEvent(eventOf("resolved", { actor: source })).title).toBe("Resolved by the source");
    expect(describeEvent(eventOf("resolved", { actor: { type: "api_token", id: "t1", name: "deploy-pipeline", provider: null, external_id: null } })).title).toBe(
      "Resolved by API token deploy-pipeline",
    );
    expect(describeEvent(eventOf("snoozed", { actor: userActor("Grace Example"), data: { until: "2026-12-01T12:00:00Z" } }), zone).title).toBe(
      "Snoozed by Grace Example until 1 Dec 2026, 12:00 UTC",
    );
    expect(describeEvent(eventOf("snooze_ended")).title).toMatch(/firing again/);
  });

  it("says whom a step notified and whom it could not", () => {
    const described = describeEvent(
      eventOf("escalated", {
        data: {
          policy: "platform",
          step: 2,
          steps: 3,
          pass: 1,
          passes: 2,
          timeout: "10m",
          notified: [
            { target: "team platform", user: "grace@example.com", name: "Grace Example", integration: "email", destination: "grace@example.com" },
            { target: "channel oncall@example.invalid", integration: "email", destination: "oncall@example.invalid" },
          ],
          skipped: [
            { target: "team platform", user: "linus@example.com", name: "Linus Example", integration: "slack", reason: "the person has no linked account for this integration", shown_as: "Linus Example (Slack not connected)" },
          ],
        },
      }),
    );
    expect(described.title).toBe("Escalation step 2 of 3, round 1 of 2 of the policy platform");
    expect(described.details).toEqual([
      "Notified Grace Example (team platform) through email",
      "Notified oncall@example.invalid (channel oncall@example.invalid) through email",
      "Not notified: Linus Example (Slack not connected) (team platform) through slack (the person has no linked account for this integration)",
      "The next step follows after 10m without a response.",
    ]);
  });

  it("says when a step notified nobody, and when the policy ran out", () => {
    expect(describeEvent(eventOf("escalated", { data: { step: 1, steps: 1, pass: 1, passes: 1 } })).details).toContain("Nobody was notified by this step.");
    expect(describeEvent(eventOf("escalation_exhausted", { data: { reason: "every step ran" } }))).toMatchObject({ tone: "problem", details: ["every step ran"] });
    expect(describeEvent(eventOf("not_notified", { data: { reason: "the group has no escalation policy" } })).title).toBe("Nobody was notified");
  });

  it("says what was delivered and what was not", () => {
    expect(describeEvent(eventOf("notification_sent", { data: { destination: "oncall@example.invalid", reason: "escalation_step" } })).title).toBe(
      "Delivered a page to oncall@example.invalid",
    );
    expect(describeEvent(eventOf("notification_failed", { data: { destination: "C0ALERTS", error: "slack: channel_not_found", will_retry: false } }))).toEqual({
      title: "Could not deliver to C0ALERTS",
      details: ["slack: channel_not_found", "Beacon will not try again."],
      tone: "problem",
    });
    expect(describeEvent(eventOf("notification_silenced", { data: { until: "2026-12-01T12:00:00Z" } }), zone).title).toBe("Kept quiet by a silence until 1 Dec 2026, 12:00 UTC");
  });

  it("shows a kind it does not know by its name, and survives data of any shape", () => {
    expect(describeEvent(eventOf("paged_by_carrier_pigeon", { data: { anything: [1, 2] } }))).toEqual({ title: "paged_by_carrier_pigeon", details: [], tone: "plain" });
    expect(describeEvent(eventOf("escalated", { data: { notified: "not a list", skipped: [null, 3], step: "two" } })).title).toBe("Escalation step");
  });
});
