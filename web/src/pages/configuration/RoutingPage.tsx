import { Link } from "react-router";
import { Empty } from "../../app/ui";
import { useRouting, type Routing } from "./queries";
import { ConfigurationPage, Pairs } from "./Shell";

type Rule = Routing["rules"][number];

/** How an alert finds its team and its escalation policy. */
export function RoutingPage() {
  const routing = useRouting();
  return (
    <ConfigurationPage title="Routing" query={routing}>
      {(data) =>
        data === null ? (
          <Empty title="No routing">The configuration files have no routing block: alerts are stored, and nobody is notified.</Empty>
        ) : (
          <>
            <p className="muted">
              A new alert is given to the first rule whose conditions it meets, in this order, and to the default route when it meets none. Alerts of one rule that share the grouping labels are
              told about together.
            </p>
            <ol className="rules" aria-label="Rules">
              {data.rules.map((rule, index) => (
                <RuleCard key={rule.name} rule={rule} position={index + 1} />
              ))}
            </ol>
            <section className="card" aria-labelledby="route-default">
              <div className="card-body stack-small">
                <h2 id="route-default">Default route</h2>
                <dl className="facts">
                  <dt>Team</dt>
                  <dd>{data.default.team !== null ? <Link to="/teams">{data.default.team}</Link> : <span className="muted">None</span>}</dd>
                  <dt>Escalation policy</dt>
                  <dd>
                    <Link to="/escalation-policies">{data.default.escalation_policy}</Link>
                  </dd>
                  <dt>Grouped by</dt>
                  <dd>
                    <Grouping by={data.group_by} wait={data.group_wait} interval={data.group_interval} />
                  </dd>
                </dl>
              </div>
            </section>
          </>
        )
      }
    </ConfigurationPage>
  );
}

function Grouping({ by, wait, interval }: { by: string[]; wait: string; interval: string }) {
  return (
    <>
      {by.length === 0 ? "Nothing: every alert is told about by itself" : by.map((label) => <code key={label} className="matcher">{label}</code>)}
      <span className="muted">
        {" "}
        · first notification after {wait}, updates every {interval}
      </span>
    </>
  );
}

function RuleCard({ rule, position }: { rule: Rule; position: number }) {
  const id = `rule-${rule.name}`;
  const unconditional = Object.keys(rule.match).length === 0 && Object.keys(rule.match_re).length === 0;
  return (
    <li className="card">
      <section className="card-body stack-small" aria-labelledby={id}>
        <h2 id={id}>
          {position}. {rule.name}
        </h2>
        <dl className="facts">
          <dt>When</dt>
          <dd>
            {unconditional && "Every alert"}
            <Pairs pairs={rule.match} /> <Pairs pairs={rule.match_re} operator="=~" />
          </dd>
          <dt>Team</dt>
          <dd>{rule.team !== null ? <Link to="/teams">{rule.team}</Link> : <span className="muted">None</span>}</dd>
          <dt>Service</dt>
          <dd>{rule.service ?? <span className="muted">None</span>}</dd>
          <dt>Escalation policy</dt>
          <dd>
            <Link to="/escalation-policies">{rule.escalation_policy}</Link>
          </dd>
          <dt>Grouped by</dt>
          <dd>
            <Grouping by={rule.group_by} wait={rule.group_wait} interval={rule.group_interval} />
          </dd>
        </dl>
      </section>
    </li>
  );
}
