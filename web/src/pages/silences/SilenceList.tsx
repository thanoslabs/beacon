import { useState } from "react";
import { Link } from "react-router";
import { Dialog, Empty, ErrorNotice, Loading, PageHead, Time, useTitle } from "../../app/ui";
import { usePersonName } from "../schedules/queries";
import { MatcherList } from "./Matchers";
import { useExpireSilence, useSilences, type Silence } from "./queries";

const stateNames: Record<Silence["state"], string> = { active: "Active", pending: "Pending", expired: "Expired" };
const stateClasses: Record<Silence["state"], string> = { active: "status-snoozed", pending: "status-acknowledged", expired: "status-resolved" };

/** The silences in force and to come, and on request the expired ones. */
export function SilenceList() {
  useTitle("Silences");
  const [expired, setExpired] = useState(false);
  const silences = useSilences(expired);
  const [expiring, setExpiring] = useState<Silence | null>(null);

  return (
    <div className="page">
      <PageHead title="Silences">
        <Link className="button button-primary" to="/silences/new">
          New silence
        </Link>
      </PageHead>
      <p className="muted">
        A silence keeps Beacon quiet about the alerts whose labels match, for a period. The alerts are still recorded, and can be acknowledged and resolved.
      </p>
      <label className="choice">
        <input type="checkbox" checked={expired} onChange={(event) => setExpired(event.target.checked)} />
        Also show expired silences
      </label>
      {silences.data === undefined && silences.isPending && <Loading label="Loading silences" />}
      {silences.isLoadingError && <ErrorNotice error={silences.error} onRetry={() => void silences.refetch()} />}
      {silences.isRefetchError && (
        <p className="notice notice-error" role="alert">
          This list could not be refreshed and may be out of date. Beacon keeps trying.
        </p>
      )}
      {silences.data !== undefined && silences.data.length === 0 && (
        <Empty title={expired ? "No silences" : "No silence is active or pending"}>Beacon notifies about every alert.</Empty>
      )}
      {silences.data !== undefined && silences.data.length > 0 && (
        <ul className="silence-list" aria-label="Silences">
          {silences.data.map((silence) => (
            <SilenceRow key={silence.id} silence={silence} onExpire={() => setExpiring(silence)} />
          ))}
        </ul>
      )}
      {expiring !== null && <ExpireSilence silence={expiring} onClose={() => setExpiring(null)} />}
    </div>
  );
}

function SilenceRow({ silence, onExpire }: { silence: Silence; onExpire: () => void }) {
  const nameOf = usePersonName();
  const author = silence.created_by !== undefined ? nameOf(silence.created_by) : silence.created_by_token !== undefined ? `API token ${silence.created_by_token}` : "someone";
  return (
    <li className="card silence-row">
      <div className="stack-small">
        <p className="row">
          <span className={`badge ${stateClasses[silence.state]}`}>{stateNames[silence.state]}</span>
          <span>
            <Time value={silence.starts_at} /> to <Time value={silence.ends_at} />
          </span>
        </p>
        <MatcherList matchers={silence.matchers} />
        <p className="silence-comment">{silence.comment}</p>
        <p className="muted">
          Created by {author}, <Time value={silence.created_at} />
        </p>
      </div>
      {silence.state !== "expired" && (
        <button type="button" className="button" aria-label={`Expire the silence: ${silence.comment}`} onClick={onExpire}>
          Expire
        </button>
      )}
    </li>
  );
}

function ExpireSilence({ silence, onClose }: { silence: Silence; onClose: () => void }) {
  const expire = useExpireSilence();
  return (
    <Dialog title="Expire this silence?" onClose={onClose}>
      <p>
        {silence.state === "active"
          ? "It ends now. Beacon then notifies about the alerts it was keeping quiet that are still firing."
          : "It has not begun, and then never will."}
      </p>
      <MatcherList matchers={silence.matchers} />
      {expire.isError && <ErrorNotice error={expire.error} />}
      <div className="row">
        <button type="button" className="button button-primary" disabled={expire.isPending} onClick={() => expire.mutate(silence.id, { onSuccess: onClose })}>
          {expire.isPending ? "Expiring…" : "Expire the silence"}
        </button>
        <button type="button" className="button" onClick={onClose}>
          Keep it
        </button>
      </div>
    </Dialog>
  );
}
