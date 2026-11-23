import { Link, useSearchParams } from "react-router";
import { Empty, ErrorNotice, Loading, PageHead, RelativeTime, useTitle } from "../../app/ui";
import { AlertActions } from "./AlertActions";
import { SeverityBadge, StatusBadge } from "./Badges";
import { useAlerts, useTeams, type AlertFilters, type SeverityFilter, type StatusFilter } from "./queries";
import { alertSummary, alertTitle, type Alert } from "./text";

const statuses: { value: StatusFilter; label: string }[] = [
  { value: "open", label: "Open (not resolved)" },
  { value: "firing", label: "Firing" },
  { value: "acknowledged", label: "Acknowledged" },
  { value: "snoozed", label: "Snoozed" },
  { value: "resolved", label: "Resolved" },
  { value: "all", label: "All" },
];

const severities: { value: SeverityFilter; label: string }[] = [
  { value: "any", label: "Any severity" },
  { value: "critical", label: "Critical" },
  { value: "warning", label: "Warning" },
  { value: "info", label: "Info" },
];

function oneOf<T extends string>(value: string | null, options: { value: T }[], fallback: T): T {
  return options.find((option) => option.value === value)?.value ?? fallback;
}

/**
 * The alerts, newest first. The filters are in the address, so a filtered
 * list can be linked to, and the list asks again every few seconds.
 */
export function AlertList() {
  useTitle("Alerts");
  const [params, setParams] = useSearchParams();
  const filters: AlertFilters = {
    status: oneOf(params.get("status"), statuses, "open"),
    severity: oneOf(params.get("severity"), severities, "any"),
    team: params.get("team") ?? "",
  };
  const alerts = useAlerts(filters);
  const teams = useTeams();

  function setFilter(name: keyof AlertFilters, value: string, unset: string) {
    setParams(
      (current) => {
        const next = new URLSearchParams(current);
        if (value === unset) {
          next.delete(name);
        } else {
          next.set(name, value);
        }
        return next;
      },
      { replace: true },
    );
  }

  const shown = alerts.data?.pages.flatMap((page) => page.alerts) ?? [];
  const filtered = filters.status !== "open" || filters.severity !== "any" || filters.team !== "";
  // A team that the address names and the configuration no longer has is
  // still offered, so that the filter in force can be seen.
  const teamNames = teams.data?.teams.map((team) => team.name) ?? [];
  if (filters.team !== "" && !teamNames.includes(filters.team)) {
    teamNames.push(filters.team);
  }

  return (
    <div className="page">
      <PageHead title="Alerts" />
      <form className="filters" aria-label="Filters" onSubmit={(event) => event.preventDefault()}>
        <div className="field">
          <label htmlFor="filter-status">State</label>
          <select id="filter-status" className="input" value={filters.status} onChange={(event) => setFilter("status", event.target.value, "open")}>
            {statuses.map((option) => (
              <option key={option.value} value={option.value}>
                {option.label}
              </option>
            ))}
          </select>
        </div>
        <div className="field">
          <label htmlFor="filter-severity">Severity</label>
          <select id="filter-severity" className="input" value={filters.severity} onChange={(event) => setFilter("severity", event.target.value, "any")}>
            {severities.map((option) => (
              <option key={option.value} value={option.value}>
                {option.label}
              </option>
            ))}
          </select>
        </div>
        <div className="field">
          <label htmlFor="filter-team">Team</label>
          <select id="filter-team" className="input" value={filters.team} onChange={(event) => setFilter("team", event.target.value, "")}>
            <option value="">Every team</option>
            {teamNames.map((name) => (
              <option key={name} value={name}>
                {name}
              </option>
            ))}
          </select>
        </div>
      </form>

      {alerts.isPending && <Loading label="Loading alerts" />}
      {alerts.isLoadingError && <ErrorNotice error={alerts.error} onRetry={() => void alerts.refetch()} />}
      {alerts.isRefetchError && (
        // Said loudly, so that a stale list is not taken for a quiet one.
        <p className="notice notice-error" role="alert">
          This list could not be refreshed and may be out of date. Beacon keeps trying.
        </p>
      )}
      {alerts.data !== undefined && shown.length === 0 && (
        <Empty title={filtered ? "No alerts match these filters" : "No open alerts"}>
          {filtered ? "Change the filters to see others." : "Nothing is firing, acknowledged or snoozed. New alerts appear here by themselves."}
        </Empty>
      )}
      {shown.length > 0 && (
        <ul className="alert-list" aria-label="Alerts">
          {shown.map((alert) => (
            <AlertRow key={alert.id} alert={alert} />
          ))}
        </ul>
      )}
      {alerts.hasNextPage && (
        <div className="row">
          <button type="button" className="button" disabled={alerts.isFetchingNextPage} onClick={() => void alerts.fetchNextPage()}>
            {alerts.isFetchingNextPage ? "Loading…" : "Show older alerts"}
          </button>
        </div>
      )}
      {alerts.isSuccess && <p className="hint">This list refreshes by itself every few seconds.</p>}
    </div>
  );
}

function AlertRow({ alert }: { alert: Alert }) {
  const summary = alertSummary(alert);
  return (
    <li className={`alert-row card severity-edge-${alert.status === "resolved" ? "none" : alert.severity}`}>
      <div className="alert-row-main">
        <p className="alert-row-title">
          <Link to={`/alerts/${alert.id}`}>{alertTitle(alert)}</Link>
        </p>
        {summary !== undefined && <p className="alert-row-summary">{summary}</p>}
        <p className="row muted alert-row-meta">
          <StatusBadge status={alert.status} />
          <SeverityBadge severity={alert.severity} />
          <span>
            Started <RelativeTime value={alert.started_at} />
          </span>
          {alert.team !== null && <span>Team {alert.team}</span>}
          {alert.service !== null && <span>Service {alert.service}</span>}
        </p>
      </div>
      <AlertActions alert={alert} />
    </li>
  );
}
