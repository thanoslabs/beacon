import { useSearchParams } from "react-router";
import { formatDateTime } from "../../app/format";
import { ErrorNotice, Loading, Time, useNow } from "../../app/ui";
import { addDays, fromDateParam, positionIn, segmentsOf, startOfWeek, toDateParam, weeksFrom, type Shift } from "./calendar";
import { usePersonName, useShifts } from "./queries";

const weeksShown = 4;
// How many colours people are told apart by. Each shift also carries the
// person's name, so the colour is never the only sign.
const colours = 5;

const day = new Intl.DateTimeFormat(undefined, { weekday: "short", day: "numeric", month: "short" });

/**
 * Who was and will be on call, four weeks at a time: a bar for each week,
 * and the same shifts as a list. Past time is the record; time to come is
 * the plan, as the rotation and the overrides stand now.
 */
export function ShiftCalendar({ schedule }: { schedule: string }) {
  const [params, setParams] = useSearchParams();
  const now = useNow();
  // Without an address that says otherwise: last week, this one, the next two.
  const thisWeek = startOfWeek(now);
  const first = startOfWeek(fromDateParam(params.get("from")) ?? addDays(thisWeek, -7));
  const weeks = weeksFrom(first, weeksShown);
  const last = weeks.at(-1)?.end ?? first;
  const shifts = useShifts(schedule, first, last);
  const nameOf = usePersonName();

  function show(from: Date | undefined) {
    setParams(
      (current) => {
        const next = new URLSearchParams(current);
        if (from === undefined) {
          next.delete("from");
        } else {
          next.set("from", toDateParam(from));
        }
        return next;
      },
      { replace: true },
    );
  }

  const all = shifts.data?.shifts ?? [];
  const people = [...new Set(all.map((shift) => shift.user))].sort();
  const colourOf = (email: string) => `who-${people.indexOf(email) % colours}`;

  return (
    <div className="stack-small">
      <div className="row">
        <button type="button" className="button" onClick={() => show(addDays(first, -7 * weeksShown))}>
          Earlier
        </button>
        <button type="button" className="button" onClick={() => show(undefined)}>
          This week
        </button>
        <button type="button" className="button" onClick={() => show(addDays(first, 7 * weeksShown))}>
          Later
        </button>
      </div>
      {shifts.data === undefined && shifts.isPending && <Loading label="Loading the shifts" />}
      {shifts.isError && <ErrorNotice error={shifts.error} onRetry={() => void shifts.refetch()} />}
      {shifts.data !== undefined && (
        <>
          {/* The picture; the list below says the same in words. */}
          <div className="calendar" aria-hidden="true">
            {weeks.map((week) => {
              const marker = positionIn(week, now);
              return (
                <div key={week.start.getTime()} className="calendar-week">
                  <div className="calendar-days">
                    {week.days.map((date) => (
                      <span key={date.getTime()}>{day.format(date)}</span>
                    ))}
                  </div>
                  <div className="calendar-bar">
                    {segmentsOf(all, week).map((segment) => (
                      <span
                        key={`${segment.shift.starts_at}-${segment.shift.user}-${segment.shift.kind}`}
                        className={["calendar-shift", colourOf(segment.shift.user), segment.shift.planned ? "is-planned" : "", segment.shift.kind === "override" ? "is-override" : ""].join(" ")}
                        style={{ left: `${segment.left}%`, width: `${segment.width}%` }}
                        title={`${nameOf(segment.shift.user)}: ${formatDateTime(segment.shift.starts_at)} to ${formatDateTime(segment.shift.ends_at)}`}
                      >
                        {nameOf(segment.shift.user)}
                      </span>
                    ))}
                    {marker !== undefined && <span className="calendar-now" style={{ left: `${marker}%` }} />}
                  </div>
                </div>
              );
            })}
          </div>
          <p className="hint">
            <span className="legend legend-recorded" aria-hidden="true" /> On record <span className="legend legend-planned" aria-hidden="true" /> Planned{" "}
            <span className="legend legend-override" aria-hidden="true" /> Override. The line is now.
          </p>
          {all.length === 0 ? (
            <p className="muted">Nobody is on call in these weeks.</p>
          ) : (
            <ShiftList shifts={all} now={now} />
          )}
        </>
      )}
    </div>
  );
}

function ShiftList({ shifts, now }: { shifts: Shift[]; now: Date }) {
  const nameOf = usePersonName();
  return (
    <ol className="shift-list" aria-label="Shifts">
      {shifts.map((shift) => {
        const current = new Date(shift.starts_at) <= now && now < new Date(shift.ends_at);
        return (
          <li key={`${shift.starts_at}-${shift.user}-${shift.kind}`} className={current ? "shift-row is-current" : "shift-row"}>
            <span className="shift-who">
              <strong>{nameOf(shift.user)}</strong>
            </span>
            <span>
              <Time value={shift.starts_at} /> to <Time value={shift.ends_at} />
            </span>
            <span className="row">
              {current && <span className="badge status-acknowledged">Now</span>}
              {shift.kind === "override" && <span className="badge status-snoozed">Override</span>}
              <span className="muted">{shift.planned ? "Planned" : "On record"}</span>
            </span>
          </li>
        );
      })}
    </ol>
  );
}
