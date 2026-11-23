import { Link } from "react-router";
import { useIsAdmin } from "../../app/session";
import { Empty } from "../../app/ui";
import { useTeams } from "../alerts/queries";
import type { Team } from "./queries";
import { ConfigurationPage } from "./Shell";

const origins: Record<string, string> = { config: "listed in the files", group: "by a group of the identity provider" };

/** The teams: who belongs to each and why, and what each owns. */
export function Teams() {
  const teams = useTeams();
  return (
    <ConfigurationPage title="Teams" query={teams}>
      {(data) => (data.teams.length === 0 ? <Empty title="No teams">The configuration files define none.</Empty> : data.teams.map((team) => <TeamCard key={team.name} team={team} />))}
    </ConfigurationPage>
  );
}

function TeamCard({ team }: { team: Team }) {
  const admin = useIsAdmin();
  const id = `team-${team.name}`;
  // The API answers the skipped addresses to admins only. The role decides
  // nothing here that the API has not decided already.
  const skipped = admin ? (team.skipped_members ?? []) : [];
  return (
    <section className="card" aria-labelledby={id}>
      <div className="card-body stack-small">
        <h2 id={id}>
          {team.display_name || team.name} {team.display_name !== "" && team.display_name !== team.name && <span className="muted mono">{team.name}</span>}
        </h2>
        <dl className="facts">
          <dt>Members</dt>
          <dd>
            {team.members.length === 0 ? (
              <span className="muted">Nobody yet</span>
            ) : (
              <ul className="stack-small">
                {team.members.map((member) => (
                  <li key={member.email}>
                    {member.display_name || member.email} {member.display_name && <span className="muted">{member.email}</span>}{" "}
                    <span className="muted">({member.origins.map((origin) => origins[origin] ?? origin).join(", and ")})</span>
                  </li>
                ))}
              </ul>
            )}
          </dd>
          <dt>Identity provider groups</dt>
          <dd>{team.groups.length === 0 ? <span className="muted">None</span> : team.groups.map((group) => <code key={group} className="matcher">{group}</code>)}</dd>
          <dt>Schedules</dt>
          <dd>
            {team.schedules.length === 0 ? (
              <span className="muted">None</span>
            ) : (
              <span className="row">
                {team.schedules.map((schedule) => (
                  <Link key={schedule} to={`/schedules/${encodeURIComponent(schedule)}`}>
                    {schedule}
                  </Link>
                ))}
              </span>
            )}
          </dd>
        </dl>
        {skipped.length > 0 && (
          <div className="notice notice-warning">
            <p>
              <strong>Listed in the files, and not users yet.</strong> These people are not in the team, and are not notified with it, until they have signed in for the first time:
            </p>
            <ul className="stack-small">
              {skipped.map((email) => (
                <li key={email}>{email}</li>
              ))}
            </ul>
            <p className="hint">Only admins see this.</p>
          </div>
        )}
      </div>
    </section>
  );
}
