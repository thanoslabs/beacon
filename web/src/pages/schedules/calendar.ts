// The arithmetic of the calendar: weeks in the viewer's own time zone, and
// where a shift lies within a week. A day is found from a calendar date,
// never by adding 24 hours, so a week with a clock change has seven days of
// the lengths they really have.
import type { Schemas } from "../../api/client";

export type Shift = Schemas["Shift"];

/** Midnight at the start of the Monday of the week that holds the date. */
export function startOfWeek(date: Date): Date {
  const sinceMonday = (date.getDay() + 6) % 7;
  return new Date(date.getFullYear(), date.getMonth(), date.getDate() - sinceMonday);
}

/** Midnight a number of days later, or earlier. */
export function addDays(date: Date, days: number): Date {
  return new Date(date.getFullYear(), date.getMonth(), date.getDate() + days);
}

export interface Week {
  start: Date;
  /** The midnight that ends the week. */
  end: Date;
  days: Date[];
}

export function weeksFrom(first: Date, count: number): Week[] {
  return Array.from({ length: count }, (_, index) => {
    const start = addDays(first, index * 7);
    return { start, end: addDays(start, 7), days: Array.from({ length: 7 }, (_, day) => addDays(start, day)) };
  });
}

export interface Segment {
  shift: Shift;
  /** Where the part within the week begins and how wide it is, in percent. */
  left: number;
  width: number;
  /** Whether the shift began before the week, or goes on after it. */
  fromBefore: boolean;
  goesOn: boolean;
}

/** The parts of the shifts that lie within a week, in the order they begin. */
export function segmentsOf(shifts: Shift[], week: Week): Segment[] {
  const start = week.start.getTime();
  const length = week.end.getTime() - start;
  return shifts
    .map((shift) => ({ shift, begins: new Date(shift.starts_at).getTime(), ends: new Date(shift.ends_at).getTime() }))
    .filter(({ begins, ends }) => ends > start && begins < start + length && ends > begins)
    .sort((a, b) => a.begins - b.begins)
    .map(({ shift, begins, ends }) => {
      const from = Math.max(begins, start);
      const to = Math.min(ends, start + length);
      return { shift, left: ((from - start) / length) * 100, width: ((to - from) / length) * 100, fromBefore: begins < start, goesOn: ends > start + length };
    });
}

/** Where a moment lies within a week, in percent, or undefined outside it. */
export function positionIn(week: Week, moment: Date): number | undefined {
  const start = week.start.getTime();
  const length = week.end.getTime() - start;
  const at = moment.getTime();
  return at >= start && at < start + length ? ((at - start) / length) * 100 : undefined;
}

/** "2026-12-07", the form the address keeps a week in. */
export function toDateParam(date: Date): string {
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`;
}

/** The date an address names, or undefined when it names none. */
export function fromDateParam(value: string | null): Date | undefined {
  const match = /^(\d{4})-(\d{2})-(\d{2})$/.exec(value ?? "");
  if (match === null) {
    return undefined;
  }
  const date = new Date(Number(match[1]), Number(match[2]) - 1, Number(match[3]));
  return Number.isNaN(date.getTime()) ? undefined : date;
}
