import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useId, useState, type SyntheticEvent } from "react";
import { ApiError, api, unwrap, type Schemas } from "../../api/client";
import { viewerZone } from "../../app/format";
import { useSession } from "../../app/session";
import { Dialog, ErrorNotice, Loading, Time } from "../../app/ui";
import { scheduleKeys, usePersonName, useUsers } from "./queries";

type Override = Schemas["Override"];

function useOverrides(schedule: string) {
  return useQuery({
    queryKey: scheduleKeys.overrides(schedule),
    queryFn: ({ signal }) => unwrap(api.GET("/api/v1/schedules/{name}/overrides", { params: { path: { name: schedule }, query: { limit: 200 } }, signal })),
    refetchInterval: 30_000,
  });
}

/**
 * The overrides of a schedule that are in force or to come, and the ways to
 * add one and to remove one. Overrides are runtime state: this is the one
 * part of a schedule that is changed here and not in the files.
 */
export function Overrides({ schedule }: { schedule: string }) {
  const overrides = useOverrides(schedule);
  const [adding, setAdding] = useState(false);
  const [removing, setRemoving] = useState<Override | null>(null);

  return (
    <div className="stack-small">
      <p className="muted">
        An override puts one person on call for a period, in place of whoever the rotation has. Where two overlap, the one created last is in force.
      </p>
      {overrides.data === undefined && overrides.isPending && <Loading label="Loading the overrides" />}
      {overrides.isError && <ErrorNotice error={overrides.error} onRetry={() => void overrides.refetch()} />}
      {overrides.data !== undefined &&
        (overrides.data.overrides.length === 0 ? (
          <p className="muted">No override is in force or to come.</p>
        ) : (
          <ul className="override-list" aria-label="Overrides">
            {overrides.data.overrides.map((override) => (
              <OverrideRow key={override.id} override={override} onRemove={() => setRemoving(override)} />
            ))}
          </ul>
        ))}
      <div className="row">
        <button type="button" className="button button-primary" onClick={() => setAdding(true)}>
          Add an override
        </button>
      </div>
      {adding && <AddOverride schedule={schedule} onClose={() => setAdding(false)} />}
      {removing !== null && <RemoveOverride schedule={schedule} override={removing} onClose={() => setRemoving(null)} />}
    </div>
  );
}

function OverrideRow({ override, onRemove }: { override: Override; onRemove: () => void }) {
  const nameOf = usePersonName();
  const author = override.created_by !== undefined ? nameOf(override.created_by) : override.created_by_token !== undefined ? `API token ${override.created_by_token}` : undefined;
  return (
    <li className="override-row">
      <div className="stack-small">
        <p>
          <strong>{nameOf(override.user)}</strong>{" "}
          <span className={override.state === "active" ? "badge status-acknowledged" : "badge status-snoozed"}>{override.state === "active" ? "In force" : "To come"}</span>
        </p>
        <p>
          <Time value={override.starts_at} /> to <Time value={override.ends_at} />
        </p>
        <p className="muted">
          {override.reason !== "" && <>{override.reason} · </>}
          {author !== undefined && <>Added by {author}</>}
        </p>
      </div>
      <button type="button" className="button" aria-label={`Remove the override of ${nameOf(override.user)}`} onClick={onRemove}>
        Remove
      </button>
    </li>
  );
}

/** datetime-local gives a time on the device's clock; the API takes an instant. */
function toInstant(local: string): string | undefined {
  if (local === "") {
    return undefined;
  }
  const date = new Date(local);
  return Number.isNaN(date.getTime()) ? undefined : date.toISOString();
}

function AddOverride({ schedule, onClose }: { schedule: string; onClose: () => void }) {
  const me = useSession();
  const users = useUsers();
  const queryClient = useQueryClient();
  const ids = { user: useId(), starts: useId(), ends: useId(), reason: useId(), zone: useId() };
  const people = (users.data ?? []).filter((user) => !user.disabled);
  const [user, setUser] = useState(me.email ?? "");
  const [starts, setStarts] = useState("");
  const [ends, setEnds] = useState("");
  const [reason, setReason] = useState("");

  const create = useMutation({
    mutationFn: () =>
      unwrap(
        api.POST("/api/v1/schedules/{name}/overrides", {
          params: { path: { name: schedule } },
          body: { user, starts_at: toInstant(starts), ends_at: toInstant(ends), reason: reason.trim() === "" ? undefined : reason.trim() },
        }),
      ),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: scheduleKeys.all });
      onClose();
    },
  });

  function submit(event: SyntheticEvent) {
    event.preventDefault();
    create.mutate();
  }

  // The API names the part of the request at fault; the field says so too.
  const fault = create.error instanceof ApiError ? create.error.field : undefined;
  const selected = people.some((person) => person.email === user) ? user : "";

  return (
    <Dialog title={`Add an override to ${schedule}`} onClose={onClose}>
      <form className="stack" onSubmit={submit}>
        <div className="field">
          <label htmlFor={ids.user}>Who covers</label>
          <select id={ids.user} className="input" required value={selected} onChange={(event) => setUser(event.target.value)} aria-invalid={fault === "user"}>
            <option value="" disabled>
              {users.isPending ? "Loading people…" : "Choose a person"}
            </option>
            {people.map((person) => (
              <option key={person.email} value={person.email}>
                {person.display_name ? `${person.display_name} (${person.email})` : person.email}
              </option>
            ))}
          </select>
        </div>
        <div className="field">
          <label htmlFor={ids.starts}>Starts</label>
          <input id={ids.starts} className="input" type="datetime-local" value={starts} onChange={(event) => setStarts(event.target.value)} aria-describedby={ids.zone} aria-invalid={fault === "starts_at"} />
          <p className="hint">Leave empty to start now.</p>
        </div>
        <div className="field">
          <label htmlFor={ids.ends}>Ends</label>
          <input id={ids.ends} className="input" type="datetime-local" required value={ends} onChange={(event) => setEnds(event.target.value)} aria-describedby={ids.zone} aria-invalid={fault === "ends_at"} />
          <p className="hint" id={ids.zone}>
            Times are in the zone of your device, {viewerZone()}.
          </p>
        </div>
        <div className="field">
          <label htmlFor={ids.reason}>Reason (optional)</label>
          <input id={ids.reason} className="input" type="text" maxLength={500} value={reason} onChange={(event) => setReason(event.target.value)} aria-invalid={fault === "reason"} />
        </div>
        {create.isError && <ErrorNotice error={create.error} />}
        <div className="row">
          <button type="submit" className="button button-primary" disabled={create.isPending}>
            {create.isPending ? "Adding…" : "Add the override"}
          </button>
          <button type="button" className="button" onClick={onClose}>
            Cancel
          </button>
        </div>
      </form>
    </Dialog>
  );
}

function RemoveOverride({ schedule, override, onClose }: { schedule: string; override: Override; onClose: () => void }) {
  const nameOf = usePersonName();
  const queryClient = useQueryClient();
  const remove = useMutation({
    mutationFn: () => unwrap(api.DELETE("/api/v1/schedules/{name}/overrides/{id}", { params: { path: { name: schedule, id: override.id } } })),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: scheduleKeys.all });
      onClose();
    },
  });
  return (
    <Dialog title="Remove this override?" onClose={onClose}>
      <p>
        {nameOf(override.user)} stops covering {schedule}
        {override.state === "active" ? " now" : ""}. Whoever is beneath takes over: an earlier override that covers the same time, or else the rotation.
      </p>
      {remove.isError && <ErrorNotice error={remove.error} />}
      <div className="row">
        <button type="button" className="button button-primary" disabled={remove.isPending} onClick={() => remove.mutate()}>
          {remove.isPending ? "Removing…" : "Remove the override"}
        </button>
        <button type="button" className="button" onClick={onClose}>
          Keep it
        </button>
      </div>
    </Dialog>
  );
}
