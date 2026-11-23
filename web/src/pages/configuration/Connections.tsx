import { ErrorNotice, Loading, PageHead, Provisioned, useTitle } from "../../app/ui";
import { useIntegrations, useSources, type Integration, type Source } from "./queries";
import { Pairs } from "./Shell";

const sourceTypes: Record<Source["type"], string> = { alertmanager: "Alertmanager", grafana: "Grafana alerting", webhook: "Generic webhook" };
const integrationTypes: Record<Integration["type"], string> = { slack: "Slack", discord: "Discord", email: "Email", webhook: "Outbound webhook" };

/**
 * Where alerts come from and where notifications go. No credential is
 * shown, because the API answers none.
 */
export function Connections() {
  useTitle("Sources and integrations");
  const sources = useSources();
  const integrations = useIntegrations();
  return (
    <div className="page">
      <PageHead title="Sources and integrations" />
      <Provisioned />
      <section className="stack-small" aria-labelledby="sources">
        <h2 id="sources">Sources</h2>
        <p className="muted">The systems that send alerts to Beacon, each to an address of its own with a token of its own.</p>
        {sources.data === undefined && sources.isPending && <Loading />}
        {sources.isError && <ErrorNotice error={sources.error} onRetry={() => void sources.refetch()} />}
        {sources.data?.sources.length === 0 && <p className="muted">The configuration files define no source, so Beacon receives no alerts.</p>}
        {sources.data?.sources.map((source) => (
          <div key={source.name} className="card">
            <dl className="card-body facts">
              <dt>Name</dt>
              <dd>
                <strong>{source.name}</strong>
              </dd>
              <dt>Type</dt>
              <dd>{sourceTypes[source.type]}</dd>
              <dt>Sends to</dt>
              <dd className="mono">{source.ingest_path}</dd>
              <dt>Severity</dt>
              <dd>
                From the label <code>{source.severity.label}</code>, <code>{source.severity.default}</code> when it says nothing known
              </dd>
              {Object.keys(source.labels).length > 0 && (
                <>
                  <dt>Adds the labels</dt>
                  <dd>
                    <Pairs pairs={source.labels} />
                  </dd>
                </>
              )}
            </dl>
          </div>
        ))}
      </section>
      <section className="stack-small" aria-labelledby="integrations">
        <h2 id="integrations">Integrations</h2>
        <p className="muted">The services Beacon notifies through. Their credentials are in the files and are shown nowhere.</p>
        {integrations.data === undefined && integrations.isPending && <Loading />}
        {integrations.isError && <ErrorNotice error={integrations.error} onRetry={() => void integrations.refetch()} />}
        {integrations.data?.integrations.length === 0 && <p className="muted">The configuration files define no integration, so Beacon notifies nobody.</p>}
        {integrations.data?.integrations.map((integration) => (
          <div key={integration.name} className="card">
            <dl className="card-body facts">
              <dt>Name</dt>
              <dd>
                <strong>{integration.name}</strong>
              </dd>
              <dt>Type</dt>
              <dd>{integrationTypes[integration.type]}</dd>
              {integration.interactions_path !== null && (
                <>
                  <dt>Receives at</dt>
                  <dd className="mono">{integration.interactions_path}</dd>
                </>
              )}
              {Object.entries(integration.settings).map(([name, value]) => (
                <Setting key={name} name={name} value={value} />
              ))}
            </dl>
          </div>
        ))}
      </section>
    </div>
  );
}

/** One setting of an integration, whatever shape it has, as text. */
function Setting({ name, value }: { name: string; value: unknown }) {
  const text = typeof value === "string" ? value : Array.isArray(value) ? value.map(String).join(", ") : JSON.stringify(value);
  return (
    <>
      <dt className="mono">{name}</dt>
      <dd className="pair-value">{text === "" ? <span className="muted">None</span> : text}</dd>
    </>
  );
}
