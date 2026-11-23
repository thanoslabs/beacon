import { Link } from "react-router";
import { Empty } from "../../app/ui";
import { usePolicies, type EscalationPolicy } from "./queries";
import { ConfigurationPage } from "./Shell";

type Target = EscalationPolicy["steps"][number]["notify"][number];

/** The escalation policies: whom each step notifies, and how long it waits. */
export function Policies() {
  const policies = usePolicies();
  return (
    <ConfigurationPage title="Escalation policies" query={policies}>
      {(data) =>
        data.escalation_policies.length === 0 ? (
          <Empty title="No escalation policies">The configuration files define none, so nobody is paged.</Empty>
        ) : (
          data.escalation_policies.map((policy) => <Policy key={policy.name} policy={policy} />)
        )
      }
    </ConfigurationPage>
  );
}

function Policy({ policy }: { policy: EscalationPolicy }) {
  const id = `policy-${policy.name}`;
  return (
    <section className="card" aria-labelledby={id}>
      <div className="card-body stack-small">
        <h2 id={id}>{policy.name}</h2>
        <ol className="steps">
          {policy.steps.map((step, index) => (
            <li key={index} className="step">
              <p>
                <strong>Step {index + 1}</strong> notifies:
              </p>
              <ul className="step-targets">
                {step.notify.map((target, at) => (
                  <li key={at}>
                    <TargetLine target={target} />
                  </li>
                ))}
              </ul>
              <p className="muted">
                {index + 1 < policy.steps.length ? `Without a response after ${step.timeout}, the next step runs.` : `Waits ${step.timeout} for a response.`}
              </p>
            </li>
          ))}
        </ol>
        <p>
          {policy.repeat === 0
            ? "After the last step the policy ends: it does not start again."
            : `After the last step it starts again, ${policy.repeat === 1 ? "once" : `${policy.repeat} times`}, and then ends.`}
        </p>
      </div>
    </section>
  );
}

function TargetLine({ target }: { target: Target }) {
  const via = target.via.length > 0 ? `, through ${target.via.join(", ")}` : "";
  switch (target.kind) {
    case "schedule":
      return (
        <>
          Whoever is on call for <Link to={`/schedules/${encodeURIComponent(target.schedule ?? "")}`}>{target.schedule}</Link>
          {via}
        </>
      );
    case "team":
      return (
        <>
          Everyone in the team <Link to="/teams">{target.team}</Link>
          {via}
        </>
      );
    case "user":
      return (
        <>
          {target.user}
          {via}
        </>
      );
    case "channel":
      return (
        <>
          The channel <code>{target.channel}</code> of the integration {target.integration}
        </>
      );
    case "webhook":
      return <>The outbound webhook {target.integration}</>;
  }
}
