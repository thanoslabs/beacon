import { severityNames, statusNames, type Alert } from "./text";

export function StatusBadge({ status }: { status: Alert["status"] }) {
  return <span className={`badge status-${status}`}>{statusNames[status]}</span>;
}

export function SeverityBadge({ severity }: { severity: Alert["severity"] }) {
  return <span className={`badge severity-${severity}`}>{severityNames[severity]}</span>;
}
