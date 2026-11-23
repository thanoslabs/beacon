// The few shared pieces every page is made of.
import { useEffect, type ReactNode } from "react";
import { errorMessage } from "../api/client";
import { formatDateTime } from "./format";

/** Sets the title of the browser tab for as long as the page is shown. */
export function useTitle(title: string): void {
  useEffect(() => {
    document.title = `${title} · Beacon`;
    return () => {
      document.title = "Beacon";
    };
  }, [title]);
}

export function Loading({ label = "Loading" }: { label?: string }) {
  return (
    <p className="state" role="status">
      <span className="spinner" aria-hidden="true" />
      {label}…
    </p>
  );
}

/** Says what went wrong, and offers to try again when that can help. */
export function ErrorNotice({ error, onRetry }: { error: unknown; onRetry?: () => void }) {
  return (
    <div className="notice notice-error" role="alert">
      <p>{errorMessage(error)}</p>
      {onRetry && (
        <button type="button" className="button" onClick={onRetry}>
          Try again
        </button>
      )}
    </div>
  );
}

export function Empty({ title, children }: { title: string; children?: ReactNode }) {
  return (
    <div className="state">
      <p>
        <strong>{title}</strong>
      </p>
      {children && <p>{children}</p>}
    </div>
  );
}

/** A page's heading, with what can be done on the page beside it. */
export function PageHead({ title, children }: { title: string; children?: ReactNode }) {
  return (
    <div className="page-head">
      <h1>{title}</h1>
      {children && <div className="row">{children}</div>}
    </div>
  );
}

/** A moment, in the viewer's own time zone, machine-readable in UTC. */
export function Time({ value }: { value: string }) {
  return <time dateTime={value}>{formatDateTime(value)}</time>;
}
