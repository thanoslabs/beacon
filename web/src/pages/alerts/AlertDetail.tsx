import { Link, useParams } from "react-router";
import { ApiError } from "../../api/client";
import { ErrorNotice, Loading, PageHead, RelativeTime, Time, useTitle } from "../../app/ui";
import { AlertActions } from "./AlertActions";
import { SeverityBadge, StatusBadge } from "./Badges";
import { useAlert, useAlertGroup, useTimeline } from "./queries";
import { alertSummary, alertTitle, linkName, safeHref, type Alert } from "./text";
import { describeEvent } from "./timeline";

/** One alert: what it says, what was decided for it, and what happened. */
export function AlertDetail() {
  const { id = "" } = useParams();
  const alert = useAlert(id);
  useTitle(alert.data ? alertTitle(alert.data) : "Alert");

  // A refresh that fails keeps what was read before on the page.
  if (alert.data !== undefined) {
    return <Detail alert={alert.data} stale={alert.isRefetchError} />;
  }
  if (alert.isPending) {
    return (
      <div className="page">
        <Loading label="Loading the alert" />
      </div>
    );
  }
  const missing = alert.error instanceof ApiError && alert.error.status === 404;
  return (
    <div className="page">
      <PageHead title={missing ? "No such alert" : "Alert"} />
      {missing ? <p>There is no alert at this address. It may be a link to another Beacon.</p> : <ErrorNotice error={alert.error} onRetry={() => void alert.refetch()} />}
      <p>
        <Link to="/alerts">All alerts</Link>
      </p>
    </div>
  );
}

function Detail({ alert, stale }: { alert: Alert; stale: boolean }) {
  const summary = alertSummary(alert);
  return (
    <div className="page">
      <p>
        <Link to="/alerts">← All alerts</Link>
      </p>
      <PageHead title={alertTitle(alert)}>
        <StatusBadge status={alert.status} />
        <SeverityBadge severity={alert.severity} />
      </PageHead>
      {summary !== undefined && <p className="lead">{summary}</p>}
      {stale && (
        <p className="notice notice-error" role="alert">
          This page could not be refreshed and may be out of date. Beacon keeps trying.
        </p>
      )}
      <AlertActions alert={alert} />

      <div className="columns">
        <div className="stack">
          <section className="card" aria-labelledby="alert-facts">
            <div className="card-body stack-small">
              <h2 id="alert-facts">About</h2>
              <dl className="facts">
                <dt>Started</dt>
                <dd>
                  <Time value={alert.started_at} /> (<RelativeTime value={alert.started_at} />)
                </dd>
                <dt>Last reported</dt>
                <dd>
                  <Time value={alert.last_received_at} />
                </dd>
                {alert.acknowledged_at !== null && (
                  <>
                    <dt>Acknowledged</dt>
                    <dd>
                      <Time value={alert.acknowledged_at} />
                    </dd>
                  </>
                )}
                {alert.snoozed_until !== null && (
                  <>
                    <dt>Snoozed until</dt>
                    <dd>
                      <Time value={alert.snoozed_until} />
                    </dd>
                  </>
                )}
                {alert.resolved_at !== null && (
                  <>
                    <dt>Resolved</dt>
                    <dd>
                      <Time value={alert.resolved_at} />
                    </dd>
                  </>
                )}
                <dt>Source</dt>
                <dd>{alert.source}</dd>
                <dt>Team</dt>
                <dd>{alert.team ?? <span className="muted">None</span>}</dd>
                <dt>Service</dt>
                <dd>{alert.service ?? <span className="muted">None</span>}</dd>
                <dt>Routing rule</dt>
                <dd>{alert.routing_rule ?? <span className="muted">The default route</span>}</dd>
                <dt>Fingerprint</dt>
                <dd className="mono">{alert.fingerprint}</dd>
              </dl>
            </div>
          </section>
          <Pairs id="alert-labels" title="Labels" pairs={alert.labels} empty="The alert has no labels." />
          <Pairs id="alert-annotations" title="Annotations" pairs={alert.annotations} empty="The alert has no annotations." />
          <Links links={alert.links} />
          <Group alert={alert} />
        </div>
        <TimelineSection id={alert.id} />
      </div>
    </div>
  );
}

/**
 * Labels or annotations, as text. A value that is a web address is a link;
 * nothing else in a value is ever treated as markup or as an address.
 */
function Pairs({ id, title, pairs, empty }: { id: string; title: string; pairs: Record<string, string>; empty: string }) {
  const names = Object.keys(pairs).sort();
  return (
    <section className="card" aria-labelledby={id}>
      <div className="card-body stack-small">
        <h2 id={id}>{title}</h2>
        {names.length === 0 ? (
          <p className="muted">{empty}</p>
        ) : (
          <dl className="facts pairs">
            {names.map((name) => (
              <PairRow key={name} name={name} value={pairs[name] ?? ""} />
            ))}
          </dl>
        )}
      </div>
    </section>
  );
}

function PairRow({ name, value }: { name: string; value: string }) {
  const href = safeHref(value);
  return (
    <>
      <dt className="mono">{name}</dt>
      <dd className="pair-value">{href !== undefined ? <ExternalLink href={href}>{value}</ExternalLink> : value}</dd>
    </>
  );
}

/** A link that leaves Beacon, to an address that was checked by safeHref. */
function ExternalLink({ href, children }: { href: string; children: string }) {
  return (
    <a href={href} target="_blank" rel="noopener noreferrer">
      {children}
    </a>
  );
}

function Links({ links }: { links: Record<string, string> }) {
  const names = Object.keys(links).sort();
  if (names.length === 0) {
    return null;
  }
  return (
    <section className="card" aria-labelledby="alert-links">
      <div className="card-body stack-small">
        <h2 id="alert-links">Links</h2>
        <ul className="stack-small">
          {names.map((name) => {
            const value = links[name] ?? "";
            const href = safeHref(value);
            return (
              <li key={name}>
                {href !== undefined ? (
                  <ExternalLink href={href}>{linkName(name)}</ExternalLink>
                ) : (
                  // Sent by the source, and not an address a browser should follow.
                  <span>
                    {linkName(name)}: <span className="pair-value">{value}</span> <span className="muted">(not a web address)</span>
                  </span>
                )}
              </li>
            );
          })}
        </ul>
      </div>
    </section>
  );
}

/** The group the alert is told about with, and the other alerts in it. */
function Group({ alert }: { alert: Alert }) {
  const group = useAlertGroup(alert.group_id);
  const others = group.data?.alerts.filter((other) => other.id !== alert.id) ?? [];
  return (
    <section className="card" aria-labelledby="alert-group">
      <div className="card-body stack-small">
        <h2 id="alert-group">Group</h2>
        {group.isPending && <Loading label="Loading the group" />}
        {group.isError && <ErrorNotice error={group.error} />}
        {group.data && (
          <>
            <dl className="facts">
              <dt>Escalation policy</dt>
              <dd>{group.data.escalation_policy ?? <span className="muted">None: nobody is paged</span>}</dd>
              <dt>First notified</dt>
              <dd>{group.data.notified_at !== null ? <Time value={group.data.notified_at} /> : <span className="muted">Not yet</span>}</dd>
            </dl>
            {others.length === 0 ? (
              <p className="muted">This is the only alert of its group.</p>
            ) : (
              <>
                <p>Notified together with:</p>
                <ul className="stack-small">
                  {others.map((other) => (
                    <li key={other.id} className="row">
                      <Link to={`/alerts/${other.id}`}>{alertTitle(other)}</Link>
                      <StatusBadge status={other.status} />
                    </li>
                  ))}
                </ul>
              </>
            )}
          </>
        )}
      </div>
    </section>
  );
}

function TimelineSection({ id }: { id: string }) {
  const timeline = useTimeline(id);
  return (
    <section className="card" aria-labelledby="alert-timeline">
      <div className="card-body stack-small">
        <h2 id="alert-timeline">Timeline</h2>
        {timeline.isPending && <Loading label="Loading the timeline" />}
        {timeline.isError && <ErrorNotice error={timeline.error} onRetry={() => void timeline.refetch()} />}
        {timeline.data && (
          <ol className="timeline">
            {timeline.data.events.map((event) => {
              const described = describeEvent(event);
              return (
                <li key={event.id} className={`timeline-entry tone-${described.tone}`}>
                  <p className="timeline-title">{described.title}</p>
                  {described.details.map((line, index) => (
                    <p key={index} className="timeline-detail">
                      {line}
                    </p>
                  ))}
                  <p className="hint">
                    <Time value={event.occurred_at} />
                  </p>
                </li>
              );
            })}
          </ol>
        )}
        {timeline.data?.complete === false && <p className="hint">The timeline is longer than this page shows. The API has all of it.</p>}
      </div>
    </section>
  );
}
