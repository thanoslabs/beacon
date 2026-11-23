// The few shared pieces every page is made of.
import { useEffect, useId, useRef, useState, type ReactNode } from "react";
import { errorMessage } from "../api/client";
import { formatDateTime, formatRelative } from "./format";

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

/** The current time, renewed often enough for "3 min ago" to stay true. */
export function useNow(everyMs = 30_000): Date {
  const [now, setNow] = useState(() => new Date());
  useEffect(() => {
    const timer = window.setInterval(() => setNow(new Date()), everyMs);
    return () => window.clearInterval(timer);
  }, [everyMs]);
  return now;
}

/** How long ago a moment was; the exact time is its title. */
export function RelativeTime({ value }: { value: string }) {
  const now = useNow();
  return (
    <time dateTime={value} title={formatDateTime(value)}>
      {formatRelative(value, now)}
    </time>
  );
}

/**
 * A modal dialog: the browser's own, which keeps the keyboard inside it and
 * closes on Escape. It is open for as long as it is rendered.
 */
export function Dialog({ title, onClose, children }: { title: string; onClose: () => void; children: ReactNode }) {
  const ref = useRef<HTMLDialogElement>(null);
  const heading = useId();
  useEffect(() => {
    const dialog = ref.current;
    if (dialog === null || dialog.open) {
      return;
    }
    // Not every environment the tests run in has showModal.
    if (typeof dialog.showModal === "function") {
      dialog.showModal();
    } else {
      dialog.setAttribute("open", "");
    }
  }, []);
  return (
    <dialog ref={ref} className="dialog" aria-labelledby={heading} onClose={onClose}>
      <div className="stack">
        <h2 id={heading}>{title}</h2>
        {children}
      </div>
    </dialog>
  );
}

/** Marks something that the configuration files define. */
export function Provisioned() {
  return (
    <p className="notice notice-info">
      Managed in the configuration files. To change it, edit the files and reload Beacon; nothing here can be edited.
    </p>
  );
}
