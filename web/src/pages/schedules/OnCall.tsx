import { Link } from "react-router";
import { useSession } from "../../app/session";
import { Empty, ErrorNotice, Loading, PageHead, Time, useTitle } from "../../app/ui";
import { useTeams } from "../alerts/queries";
import { usePersonName, useSchedules, type OnCall as OnCallNow, type Schedule } from "./queries";

/** Who is on call now, for every schedule, team by team. */
export function OnCall() {
  useTitle("On call");
  const schedules = useSchedules();
  const teams = useTeams();

  // Teams in the order of the files, then any team that only a schedule names.
  const names = teams.data?.teams.map((team) => team.name) ?? [];
  for (const schedule of schedules.data?.schedules ?? []) {
    if (!names.includes(schedule.team)) {
      names.push(schedule.team);
    }
  }
  const displayName = (name: string) => teams.data?.teams.find((team) => team.name === name)?.display_name || name;

  return (
    <div className="page">
      <PageHead title="On call" />
      {schedules.data === undefined && schedules.isPending && <Loading label="Finding out who is on call" />}
      {schedules.isLoadingError && <ErrorNotice error={schedules.error} onRetry={() => void schedules.refetch()} />}
      {schedules.isRefetchError && (
        <p className="notice notice-error" role="alert">
          This page could not be refreshed and may be out of date. Beacon keeps trying.
        </p>
      )}
      {schedules.data !== undefined && schedules.data.schedules.length === 0 && (
        <Empty title="No schedules">The configuration files define no schedule, so nobody is on call.</Empty>
      )}
      {schedules.data !== undefined &&
        names.map((team) => {
          const own = schedules.data.schedules.filter((schedule) => schedule.team === team);
          return (
            <section key={team} className="stack-small" aria-labelledby={`team-${team}`}>
              <h2 id={`team-${team}`}>{displayName(team)}</h2>
              {own.length === 0 ? (
                <p className="muted">This team has no schedule.</p>
              ) : (
                <ul className="on-call-list">
                  {own.map((schedule) => (
                    <ScheduleRow key={schedule.name} schedule={schedule} />
                  ))}
                </ul>
              )}
            </section>
          );
        })}
    </div>
  );
}

function ScheduleRow({ schedule }: { schedule: Schedule }) {
  return (
    <li className="card on-call-row">
      <p className="on-call-schedule">
        <Link to={`/schedules/${encodeURIComponent(schedule.name)}`}>{schedule.name}</Link>
      </p>
      <OnCallNowLine onCall={schedule.on_call} />
    </li>
  );
}

/** Who is on call for one schedule, until when, and for whom they cover. */
export function OnCallNowLine({ onCall }: { onCall: OnCallNow }) {
  const me = useSession();
  const nameOf = usePersonName();
  if (onCall.user === null) {
    return (
      <p className="on-call-who">
        <span className="badge severity-warning">Nobody is on call</span>{" "}
        <span className="muted">No member of the rotation is a user yet, and no override is in force.</span>
      </p>
    );
  }
  const covering = onCall.override !== undefined && onCall.rotation_user !== null && onCall.rotation_user !== onCall.user;
  return (
    <div className="on-call-who">
      <p>
        <strong>{nameOf(onCall.user)}</strong> {onCall.user === me.email && <span className="badge status-acknowledged">You</span>}
      </p>
      <p className="muted">
        Until <Time value={onCall.until} />
        {onCall.override !== undefined && (
          <>
            {" · "}
            {covering && onCall.rotation_user !== null ? `covering for ${nameOf(onCall.rotation_user)}` : "by an override"}
            {onCall.override.reason !== "" && `: ${onCall.override.reason}`}
          </>
        )}
      </p>
    </div>
  );
}
