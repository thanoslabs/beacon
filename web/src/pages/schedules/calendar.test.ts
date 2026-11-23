import { describe, expect, it } from "vitest";
import { shiftOf } from "../../test/fixtures";
import { addDays, fromDateParam, positionIn, segmentsOf, startOfWeek, toDateParam, weeksFrom } from "./calendar";

// The tests run in America/Toronto (vite.config.ts), which has clock changes.
describe("weeks in the viewer's zone", () => {
  it("runs in the zone the tests say they run in", () => {
    expect(new Date(2026, 0, 15).getTimezoneOffset()).toBe(300);
    expect(new Date(2026, 6, 15).getTimezoneOffset()).toBe(240);
  });

  it("begin on Monday at midnight", () => {
    // Thursday 3 December 2026, mid-afternoon.
    expect(startOfWeek(new Date(2026, 11, 3, 15, 30))).toEqual(new Date(2026, 10, 30));
    // A Sunday belongs to the week that began six days before.
    expect(startOfWeek(new Date(2026, 11, 6, 23, 59))).toEqual(new Date(2026, 10, 30));
    expect(startOfWeek(new Date(2026, 10, 30))).toEqual(new Date(2026, 10, 30));
  });

  it("have seven days of their real length across a clock change", () => {
    // Clocks go back on Sunday 1 November 2026 and forward on Sunday 14 March 2027.
    const [autumn] = weeksFrom(new Date(2026, 9, 26), 1);
    expect(autumn!.days).toHaveLength(7);
    expect(autumn!.days.every((day) => day.getHours() === 0)).toBe(true);
    expect((autumn!.end.getTime() - autumn!.start.getTime()) / 3_600_000).toBe(7 * 24 + 1);
    const [spring] = weeksFrom(new Date(2027, 2, 8), 1);
    expect((spring!.end.getTime() - spring!.start.getTime()) / 3_600_000).toBe(7 * 24 - 1);
    expect(addDays(new Date(2027, 2, 13), 1)).toEqual(new Date(2027, 2, 14));
    expect(addDays(new Date(2027, 2, 14), 1)).toEqual(new Date(2027, 2, 15));
  });

  it("are counted from the first", () => {
    const weeks = weeksFrom(new Date(2026, 10, 30), 4);
    expect(weeks.map((week) => toDateParam(week.start))).toEqual(["2026-11-30", "2026-12-07", "2026-12-14", "2026-12-21"]);
    expect(weeks[3]!.end).toEqual(new Date(2026, 11, 28));
  });
});

describe("where a shift lies in a week", () => {
  const [week] = weeksFrom(new Date(2026, 10, 30), 1);

  it("is the part of it within the week", () => {
    const segments = segmentsOf(
      [
        // Began the week before, ends Wednesday at noon, Toronto time.
        shiftOf("ada@example.com", "2026-11-23T14:00:00Z", "2026-12-02T17:00:00Z"),
        // From then until after the week.
        shiftOf("grace@example.com", "2026-12-02T17:00:00Z", "2026-12-09T17:00:00Z", { planned: true }),
        // Wholly before, and wholly after: not in this week.
        shiftOf("linus@example.com", "2026-11-01T00:00:00Z", "2026-11-30T05:00:00Z"),
        shiftOf("linus@example.com", "2026-12-07T05:00:00Z", "2026-12-08T05:00:00Z"),
      ],
      week!,
    );
    expect(segments.map((segment) => segment.shift.user)).toEqual(["ada@example.com", "grace@example.com"]);
    expect(segments[0]).toMatchObject({ left: 0, fromBefore: true, goesOn: false });
    // Two and a half days of seven.
    expect(segments[0]!.width).toBeCloseTo((2.5 / 7) * 100, 5);
    expect(segments[1]!.left).toBeCloseTo((2.5 / 7) * 100, 5);
    expect(segments[1]!.left + segments[1]!.width).toBeCloseTo(100, 5);
    expect(segments[1]).toMatchObject({ fromBefore: false, goesOn: true });
  });

  it("puts now where it is, and nowhere in another week", () => {
    expect(positionIn(week!, new Date(2026, 11, 3, 12))).toBeCloseTo((3.5 / 7) * 100, 5);
    expect(positionIn(week!, new Date(2026, 11, 7))).toBeUndefined();
    expect(positionIn(week!, new Date(2026, 10, 29, 23))).toBeUndefined();
  });
});

describe("a week in the address", () => {
  it("is a date, and anything else is no date", () => {
    expect(fromDateParam("2026-12-07")).toEqual(new Date(2026, 11, 7));
    expect(toDateParam(new Date(2026, 0, 5))).toBe("2026-01-05");
    for (const value of [null, "", "next week", "2026-12", "2026-12-07T00:00:00Z", "<script>"]) {
      expect(fromDateParam(value)).toBeUndefined();
    }
  });
});
