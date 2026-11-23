// Says in words what an event of a timeline records. The API answers a type
// and data that depend on it (docs/alert-lifecycle.md, docs/notifications.md,
// docs/escalation.md); a type this does not know is shown by its name.
import { formatDateTime, type ZoneOptions } from "../../app/format";
import type { AlertEvent } from "./text";

export interface Described {
  /** What happened, in one line. */
  title: string;
  /** More, one line each. */
  details: string[];
  /** How the entry is marked: what changed the alert, what went wrong. */
  tone: "state" | "problem" | "plain";
}

function text(value: unknown): string | undefined {
  return typeof value === "string" && value !== "" ? value : undefined;
}

function count(value: unknown): number | undefined {
  return typeof value === "number" && Number.isFinite(value) ? value : undefined;
}

function entries(value: unknown): Record<string, unknown>[] {
  return Array.isArray(value) ? value.filter((entry): entry is Record<string, unknown> => typeof entry === "object" && entry !== null) : [];
}

const providers: Record<string, string> = { slack: "Slack", discord: "Discord" };

/** Who did it, as the timeline names them. */
export function actorName(actor: AlertEvent["actor"]): string {
  switch (actor.type) {
    case "user": {
      const name = actor.name ?? "Someone";
      const from = actor.provider ? providers[actor.provider] ?? actor.provider : undefined;
      return from ? `${name}, from ${from}` : name;
    }
    case "api_token":
      return `API token ${actor.name ?? "(unnamed)"}`;
    case "source":
      return "The source";
    case "system":
      return "Beacon";
  }
}

const reasons: Record<string, string> = {
  escalation_step: "a page",
  group_update: "an update",
  resolved: "the resolution",
};

/** One person or place of an escalation step. */
function party(entry: Record<string, unknown>): string {
  const who = text(entry.shown_as) ?? text(entry.name) ?? text(entry.user) ?? text(entry.destination) ?? text(entry.target) ?? "someone";
  const target = text(entry.target);
  const through = text(entry.integration);
  let line = who;
  if (target !== undefined && target !== who) {
    line += ` (${target})`;
  }
  if (through !== undefined) {
    line += ` through ${through}`;
  }
  return line;
}

export function describeEvent(event: AlertEvent, zone: ZoneOptions = {}): Described {
  const data = event.data;
  const actor = actorName(event.actor);
  switch (event.type) {
    case "fired":
      return { title: "Started firing", details: [], tone: "state" };
    case "acknowledged":
      return { title: `Acknowledged by ${actor}`, details: [], tone: "state" };
    case "snoozed": {
      const until = text(data.until);
      return { title: `Snoozed by ${actor}${until ? ` until ${formatDateTime(until, zone)}` : ""}`, details: [], tone: "state" };
    }
    case "snooze_ended":
      return { title: "The snooze ended, and the alert is firing again", details: [], tone: "state" };
    case "resolved":
      return { title: event.actor.type === "source" ? "Resolved by the source" : `Resolved by ${actor}`, details: [], tone: "state" };
    case "escalated": {
      const step = count(data.step);
      const steps = count(data.steps);
      const pass = count(data.pass);
      const passes = count(data.passes);
      let title = step !== undefined && steps !== undefined ? `Escalation step ${step} of ${steps}` : "Escalation step";
      if (pass !== undefined && passes !== undefined && passes > 1) {
        title += `, round ${pass} of ${passes}`;
      }
      const policy = text(data.policy);
      if (policy !== undefined) {
        title += ` of the policy ${policy}`;
      }
      const details = [
        ...entries(data.notified).map((entry) => `Notified ${party(entry)}`),
        ...entries(data.skipped).map((entry) => {
          const reason = text(entry.reason);
          return `Not notified: ${party(entry)}${reason ? ` (${reason})` : ""}`;
        }),
      ];
      if (details.length === 0) {
        details.push("Nobody was notified by this step.");
      }
      const timeout = text(data.timeout);
      if (timeout !== undefined) {
        details.push(`The next step follows after ${timeout} without a response.`);
      }
      return { title, details, tone: "plain" };
    }
    case "escalation_exhausted":
      return {
        title: "The escalation ended without a response",
        details: text(data.reason) ? [text(data.reason) ?? ""] : [],
        tone: "problem",
      };
    case "not_notified":
      return { title: "Nobody was notified", details: text(data.reason) ? [text(data.reason) ?? ""] : [], tone: "problem" };
    case "notification_silenced": {
      const until = text(data.until);
      return {
        title: `Kept quiet by a silence${until ? ` until ${formatDateTime(until, zone)}` : ""}`,
        details: [],
        tone: "plain",
      };
    }
    case "notification_sent": {
      const what = reasons[text(data.reason) ?? ""] ?? "a notification";
      return { title: `Delivered ${what} to ${text(data.destination) ?? "its destination"}`, details: [], tone: "plain" };
    }
    case "notification_failed": {
      const details = [text(data.error) ?? "No reason was recorded."];
      details.push(data.will_retry === true ? "Beacon will try again." : "Beacon will not try again.");
      return { title: `Could not deliver to ${text(data.destination) ?? "its destination"}`, details, tone: "problem" };
    }
    default:
      // A kind of event that is newer than this page: its name is all that
      // can be said.
      return { title: event.type, details: [], tone: "plain" };
  }
}
