import { useState, type SyntheticEvent } from "react";
import { Dialog, ErrorNotice } from "../../app/ui";
import { useAlertAction, type Action } from "./queries";
import { alertTitle, type Alert } from "./text";

const labels: Record<Action, string> = { acknowledge: "Acknowledge", snooze: "Snooze", resolve: "Resolve" };

// The order the buttons are shown in, whatever order the API lists them in.
const order: Action[] = ["acknowledge", "snooze", "resolve"];

const snoozeChoices = [
  { duration: "30m", label: "30 minutes" },
  { duration: "1h", label: "1 hour" },
  { duration: "4h", label: "4 hours" },
  { duration: "1d", label: "1 day" },
];

/**
 * The buttons for what an alert's state allows, as the API says it
 * (`actions`). The app never works out by itself what is allowed.
 */
export function AlertActions({ alert }: { alert: Alert }) {
  const act = useAlertAction(alert.id);
  const [snoozing, setSnoozing] = useState(false);
  const title = alertTitle(alert);
  const allowed = order.filter((action) => alert.actions.includes(action));

  if (allowed.length === 0) {
    return null;
  }
  return (
    <div className="actions">
      <div className="row">
        {allowed.map((action) => (
          <button
            key={action}
            type="button"
            className={action === "acknowledge" ? "button button-primary" : "button"}
            disabled={act.isPending}
            // The visible word is the same on every row; the name says which alert.
            aria-label={`${labels[action]} ${title}`}
            onClick={() => {
              if (action === "snooze") {
                setSnoozing(true);
              } else {
                act.mutate({ action });
              }
            }}
          >
            {labels[action]}
          </button>
        ))}
      </div>
      {act.isError && <ErrorNotice error={act.error} />}
      {snoozing && (
        <SnoozeDialog
          title={title}
          onClose={() => setSnoozing(false)}
          onSnooze={(duration) => {
            setSnoozing(false);
            act.mutate({ action: "snooze", duration });
          }}
        />
      )}
    </div>
  );
}

function SnoozeDialog({ title, onClose, onSnooze }: { title: string; onClose: () => void; onSnooze: (duration: string) => void }) {
  const [duration, setDuration] = useState("1h");

  function submit(event: SyntheticEvent) {
    event.preventDefault();
    onSnooze(duration);
  }

  return (
    <Dialog title={`Snooze ${title}`} onClose={onClose}>
      <form className="stack" onSubmit={submit}>
        <fieldset className="choices">
          <legend>For how long</legend>
          {snoozeChoices.map((choice) => (
            <label key={choice.duration} className="choice">
              <input
                type="radio"
                name="snooze-duration"
                value={choice.duration}
                checked={duration === choice.duration}
                onChange={() => setDuration(choice.duration)}
              />
              {choice.label}
            </label>
          ))}
        </fieldset>
        <p className="hint">The alert stays quiet until then, and pages again if it is still firing.</p>
        <div className="row">
          <button type="submit" className="button button-primary">
            Snooze
          </button>
          <button type="button" className="button" onClick={onClose}>
            Cancel
          </button>
        </div>
      </form>
    </Dialog>
  );
}
