import { Link, useParams } from "react-router";
import { ApiError } from "../../api/client";
import { viewerZone } from "../../app/format";
import { ErrorNotice, Loading, PageHead, Provisioned, Time, useTitle } from "../../app/ui";
import { OnCallNowLine } from "./OnCall";
import { Overrides } from "./Overrides";
import { usePersonName, useSchedule, type Schedule } from "./queries";
import { ShiftCalendar } from "./ShiftCalendar";

type Rotation = Schedule["rotation"];

/** One schedule: who is on call, the shifts around now, and its rotation. */
export function SchedulePage() {
  const { name = "" } = useParams();
  const schedule = useSchedule(name);
  useTitle(name);

  if (schedule.data === undefined) {
    if (schedule.isPending) {
      return (
        <div className="page">
          <Loading label="Loading the schedule" />
        </div>
      );
    }
    const missing = schedule.error instanceof ApiError && schedule.error.status === 404;
    return (
      <div className="page">
        <PageHead title={missing ? "No such schedule" : "Schedule"} />
        {missing ? <p>The configuration has no schedule with this name.</p> : <ErrorNotice error={schedule.error} onRetry={() => void schedule.refetch()} />}
        <p>
          <Link to="/on-call">Who is on call</Link>
        </p>
      </div>
    );
  }
  const data = schedule.data;
  const zone = data.rotation.time_zone;
  return (
    <div className="page">
      <p>
        <Link to="/on-call">← On call</Link>
      </p>
      <PageHead title={data.name} />
      <p className="muted">
        Team {data.team}. The rotation hands over in the time zone <strong>{zone}</strong>.{" "}
        {zone === viewerZone() ? "That is the zone of your device." : `Times on this page are in the zone of your device, ${viewerZone()}.`}
      </p>
      {schedule.isRefetchError && (
        <p className="notice notice-error" role="alert">
          This page could not be refreshed and may be out of date. Beacon keeps trying.
        </p>
      )}

      <section className="card" aria-labelledby="schedule-now">
        <div className="card-body stack-small">
          <h2 id="schedule-now">On call now</h2>
          <OnCallNowLine onCall={data.on_call} />
          <p className="muted">
            The rotation next hands over <Time value={data.next_handoff} />.
          </p>
        </div>
      </section>

      <section className="card" aria-labelledby="schedule-shifts">
        <div className="card-body stack-small">
          <h2 id="schedule-shifts">Shifts</h2>
          <ShiftCalendar schedule={data.name} />
        </div>
      </section>

      <section className="card" aria-labelledby="schedule-overrides">
        <div className="card-body stack-small">
          <h2 id="schedule-overrides">Overrides</h2>
          <Overrides schedule={data.name} />
        </div>
      </section>

      <section className="card" aria-labelledby="schedule-rotation">
        <div className="card-body stack-small">
          <h2 id="schedule-rotation">Rotation</h2>
          <Provisioned />
          <RotationFacts rotation={data.rotation} />
          {data.pending !== undefined && (
            <div className="stack-small">
              <h3>A change that has not taken over yet</h3>
              <p>
                The files define a different rotation. It takes over <Time value={data.pending.takes_over_at} />
                {data.pending.stated ? ", the time the files state" : ", the next handoff"}; the shift in progress is not moved.
              </p>
              <RotationFacts rotation={data.pending.rotation} />
            </div>
          )}
        </div>
      </section>
    </div>
  );
}

const dayNames: Record<string, string> = {
  monday: "Monday",
  tuesday: "Tuesday",
  wednesday: "Wednesday",
  thursday: "Thursday",
  friday: "Friday",
  saturday: "Saturday",
  sunday: "Sunday",
};

function RotationFacts({ rotation }: { rotation: Rotation }) {
  const nameOf = usePersonName();
  return (
    <dl className="facts">
      <dt>Members, in order</dt>
      <dd>
        <ol className="rotation-members">
          {rotation.members.map((member) => (
            <li key={member}>
              {nameOf(member)} {nameOf(member) !== member && <span className="muted">{member}</span>}
            </li>
          ))}
        </ol>
      </dd>
      <dt>Shift length</dt>
      <dd>{rotation.shift_length}</dd>
      <dt>Handoff</dt>
      <dd>
        {rotation.handoff_day !== undefined && `${dayNames[rotation.handoff_day] ?? rotation.handoff_day} at `}
        {rotation.handoff_time}, {rotation.time_zone} time
      </dd>
    </dl>
  );
}
