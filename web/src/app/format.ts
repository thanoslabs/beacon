// How times are written. Everything is shown in the viewer's own zone unless
// a zone is given, as on the pages of a schedule, which has one of its own.

export interface ZoneOptions {
  /** An IANA zone such as Europe/London; the viewer's own when absent. */
  timeZone?: string;
  locale?: string;
}

/** "1 Dec 2026, 10:12 GMT": a date, a time and the zone it is said in. */
export function formatDateTime(value: string | Date, { timeZone, locale }: ZoneOptions = {}): string {
  const date = typeof value === "string" ? new Date(value) : value;
  if (Number.isNaN(date.getTime())) {
    return "unknown time";
  }
  return new Intl.DateTimeFormat(locale, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    timeZoneName: "short",
    timeZone,
  }).format(date);
}

/** The zone the viewer's device is set to, such as America/Toronto. */
export function viewerZone(): string {
  return Intl.DateTimeFormat().resolvedOptions().timeZone;
}

/**
 * How long ago, or how far ahead: "3 min ago", "in 2 hr". Under a minute is
 * "just now", so that a page that is watched does not count seconds.
 */
export function formatRelative(value: string | Date, now: Date, locale?: string): string {
  const date = typeof value === "string" ? new Date(value) : value;
  const seconds = Math.round((date.getTime() - now.getTime()) / 1000);
  if (Number.isNaN(seconds)) {
    return "unknown time";
  }
  const size = Math.abs(seconds);
  if (size < 60) {
    return seconds > 0 ? "in under a minute" : "just now";
  }
  const format = new Intl.RelativeTimeFormat(locale, { numeric: "always", style: "short" });
  if (size < 3600) {
    return format.format(Math.trunc(seconds / 60), "minute");
  }
  if (size < 86_400) {
    return format.format(Math.trunc(seconds / 3600), "hour");
  }
  return format.format(Math.trunc(seconds / 86_400), "day");
}
