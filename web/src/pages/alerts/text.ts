// What an alert is called, and what of it may become a link. Everything an
// alert carries was written by whoever can send alerts to Beacon: it is
// shown as text, and an address in it is followed only when it is plainly
// a web address.
import type { Schemas } from "../../api/client";

export type Alert = Schemas["Alert"];
export type AlertEvent = Schemas["Event"];

/** The name a person knows the alert by. */
export function alertTitle(alert: Pick<Alert, "labels" | "fingerprint">): string {
  return alert.labels.alertname || `Alert ${alert.fingerprint}`;
}

/** One line that says what is wrong, when the source sent one. */
export function alertSummary(alert: Pick<Alert, "annotations">): string | undefined {
  return alert.annotations.summary || alert.annotations.description || alert.annotations.message || undefined;
}

const maxLinkLength = 2048;

/**
 * The address, when it is an http or https address that a browser may be
 * sent to, and undefined otherwise. A `javascript:` or `data:` address from
 * an alert must never become a link.
 */
export function safeHref(value: string): string | undefined {
  if (value.length > maxLinkLength || !/^https?:\/\//i.test(value)) {
    return undefined;
  }
  try {
    const url = new URL(value);
    return url.protocol === "http:" || url.protocol === "https:" ? url.href : undefined;
  } catch {
    return undefined;
  }
}

const linkNames: Record<string, string> = {
  generator: "Open at the source",
  dashboard: "Dashboard",
  panel: "Panel",
  silence: "Silence at the source",
};

export function linkName(key: string): string {
  return linkNames[key] ?? key;
}

export const statusNames: Record<Alert["status"], string> = {
  firing: "Firing",
  acknowledged: "Acknowledged",
  snoozed: "Snoozed",
  resolved: "Resolved",
};

export const severityNames: Record<Alert["severity"], string> = {
  critical: "Critical",
  warning: "Warning",
  info: "Info",
};
