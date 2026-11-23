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
