import { useId, useState, type SyntheticEvent } from "react";
import { Link, useNavigate } from "react-router";
import { ApiError } from "../../api/client";
import { viewerZone } from "../../app/format";
import { ErrorNotice, Loading, PageHead, useTitle } from "../../app/ui";
import { StatusBadge } from "../alerts/Badges";
import { alertSummary, alertTitle } from "../alerts/text";
import { useCreateSilence, usePreview, useSettled, type Matcher } from "./queries";

const operators: { value: Matcher["operator"]; label: string }[] = [
  { value: "=", label: "= is" },
  { value: "!=", label: "!= is not" },
  { value: "=~", label: "=~ matches the pattern" },
  { value: "!~", label: "!~ does not match the pattern" },
];

const durations = [
  { value: "30m", label: "30 minutes" },
  { value: "1h", label: "1 hour" },
  { value: "2h", label: "2 hours" },
  { value: "4h", label: "4 hours" },
  { value: "8h", label: "8 hours" },
  { value: "1d", label: "1 day" },
  { value: "1w", label: "1 week" },
  { value: "", label: "Until a time I choose" },
];

interface Row extends Matcher {
  /** What React tells the rows apart by. */
  key: number;
}

/**
 * The form for a new silence, with a preview of the open alerts it would
 * keep quiet now. What a matcher matches is Beacon's to say, so the preview
 * asks the API and the form has no rules of its own.
 */
export function NewSilence() {
  useTitle("New silence");
  const navigate = useNavigate();
  const create = useCreateSilence();
  const ids = { duration: useId(), ends: useId(), comment: useId(), zone: useId() };
  const [rows, setRows] = useState<Row[]>([{ key: 0, label: "alertname", operator: "=", value: "" }]);
  const [nextKey, setNextKey] = useState(1);
  const [duration, setDuration] = useState("2h");
  const [ends, setEnds] = useState("");
  const [comment, setComment] = useState("");

  const matchers: Matcher[] = rows.map(({ label, operator, value }) => ({ label: label.trim(), operator, value }));

  function change(key: number, part: Partial<Matcher>) {
    setRows((current) => current.map((row) => (row.key === key ? { ...row, ...part } : row)));
  }

  function submit(event: SyntheticEvent) {
    event.preventDefault();
    const endsAt = duration === "" && ends !== "" ? new Date(ends).toISOString() : undefined;
    create.mutate(
      { matchers, duration: duration === "" ? undefined : duration, ends_at: endsAt, comment: comment.trim() },
      { onSuccess: () => void navigate("/silences") },
    );
  }

  // The API names the part at fault, such as matchers[1].value.
  const fault = create.error instanceof ApiError ? create.error.field : undefined;

  return (
    <div className="page">
      <p>
        <Link to="/silences">← Silences</Link>
      </p>
      <PageHead title="New silence" />
      <div className="columns">
        <form className="card" onSubmit={submit} aria-label="New silence">
          <div className="card-body stack">
            <fieldset className="choices">
              <legend>Alerts whose labels match all of these</legend>
              {rows.map((row, index) => (
                <div key={row.key} className="matcher-row">
                  <input
                    className="input mono"
                    aria-label={`Label of matcher ${index + 1}`}
                    placeholder="label"
                    required
                    value={row.label}
                    onChange={(event) => change(row.key, { label: event.target.value })}
                    aria-invalid={fault === `matchers[${index}].label`}
                    autoCapitalize="none"
                    spellCheck={false}
                  />
                  <select className="input" aria-label={`Operator of matcher ${index + 1}`} value={row.operator} onChange={(event) => change(row.key, { operator: event.target.value as Matcher["operator"] })}>
                    {operators.map((operator) => (
                      <option key={operator.value} value={operator.value}>
                        {operator.label}
                      </option>
                    ))}
                  </select>
                  <input
                    className="input mono"
                    aria-label={`Value of matcher ${index + 1}`}
                    placeholder={row.operator.includes("~") ? "pattern" : "value"}
                    value={row.value}
                    onChange={(event) => change(row.key, { value: event.target.value })}
                    aria-invalid={fault === `matchers[${index}].value`}
                    autoCapitalize="none"
                    spellCheck={false}
                  />
                  <button type="button" className="button" aria-label={`Remove matcher ${index + 1}`} disabled={rows.length === 1} onClick={() => setRows((current) => current.filter((other) => other.key !== row.key))}>
                    Remove
                  </button>
                </div>
              ))}
              <div className="row">
                <button
                  type="button"
                  className="button"
                  onClick={() => {
                    setRows((current) => [...current, { key: nextKey, label: "", operator: "=", value: "" }]);
                    setNextKey(nextKey + 1);
                  }}
                >
                  Add a matcher
                </button>
              </div>
              <p className="hint">A pattern is a regular expression that must match the whole value, as in Prometheus: <code>db[0-9]+</code> matches db1 and not db1-replica.</p>
            </fieldset>

            <div className="field">
              <label htmlFor={ids.duration}>For how long, from now</label>
              <select id={ids.duration} className="input" value={duration} onChange={(event) => setDuration(event.target.value)} aria-invalid={fault === "duration"}>
                {durations.map((choice) => (
                  <option key={choice.value} value={choice.value}>
                    {choice.label}
                  </option>
                ))}
              </select>
            </div>
            {duration === "" && (
              <div className="field">
                <label htmlFor={ids.ends}>Ends</label>
                <input id={ids.ends} className="input" type="datetime-local" required value={ends} onChange={(event) => setEnds(event.target.value)} aria-describedby={ids.zone} aria-invalid={fault === "ends_at"} />
                <p className="hint" id={ids.zone}>
                  In the zone of your device, {viewerZone()}.
                </p>
              </div>
            )}
            <div className="field">
              <label htmlFor={ids.comment}>Why</label>
              <textarea id={ids.comment} className="input" rows={3} required maxLength={1000} value={comment} onChange={(event) => setComment(event.target.value)} aria-invalid={fault === "comment"} />
              <p className="hint">Shown to everyone who wonders why Beacon is quiet.</p>
            </div>
            {create.isError && <ErrorNotice error={create.error} />}
            <div className="row">
              <button type="submit" className="button button-primary" disabled={create.isPending}>
                {create.isPending ? "Creating…" : "Create the silence"}
              </button>
              <Link className="button" to="/silences">
                Cancel
              </Link>
            </div>
          </div>
        </form>
        <Preview matchers={matchers} />
      </div>
    </div>
  );
}

function Preview({ matchers }: { matchers: Matcher[] }) {
  const settled = useSettled(JSON.stringify(matchers));
  const asked = JSON.parse(settled) as Matcher[];
  // Nothing is asked until every matcher names a label.
  const ready = asked.every((matcher) => matcher.label !== "");
  const preview = usePreview(asked, ready);
  return (
    <section className="card" aria-labelledby="silence-preview">
      <div className="card-body stack-small" aria-live="polite">
        <h2 id="silence-preview">Open alerts this would silence now</h2>
        {!ready && <p className="muted">Name a label in every matcher to see which alerts match.</p>}
        {ready && preview.isPending && <Loading label="Matching" />}
        {ready && preview.isError && (
          // A matcher that Beacon refuses, such as a broken pattern.
          <p className="notice notice-error">
            {preview.error instanceof ApiError && preview.error.field !== undefined ? `${preview.error.field}: ` : ""}
            {preview.error.message}
          </p>
        )}
        {ready && preview.data !== undefined && (
          <>
            <p>
              <strong>
                {preview.data.total === 0 ? "No open alert matches." : preview.data.total === 1 ? "1 open alert matches." : `${preview.data.total} open alerts match.`}
              </strong>{" "}
              {!preview.data.complete && "There are more open alerts than a preview looks through, so there may be more."}
            </p>
            {preview.data.total > 0 && (
              <ul className="stack-small">
                {preview.data.alerts.map((alert) => (
                  <li key={alert.id} className="row">
                    <Link to={`/alerts/${alert.id}`}>{alertTitle(alert)}</Link>
                    <StatusBadge status={alert.status} />
                    {alertSummary(alert) !== undefined && <span className="muted">{alertSummary(alert)}</span>}
                  </li>
                ))}
              </ul>
            )}
            {preview.data.total > preview.data.alerts.length && <p className="muted">The newest {preview.data.alerts.length} are shown.</p>}
            <p className="hint">Alerts that fire later and match are silenced too.</p>
          </>
        )}
      </div>
    </section>
  );
}
