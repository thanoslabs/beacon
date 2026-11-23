import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { api, unwrap, type Schemas } from "../../api/client";
import { Dialog, ErrorNotice, Loading, Time } from "../../app/ui";
import { useIntegrations, type Integration } from "../configuration/queries";

type Identity = Schemas["Identity"];
type LinkCode = Schemas["LinkCode"];

const identitiesKey = ["me", "identities"] as const;
const services: Record<string, string> = { slack: "Slack", discord: "Discord" };

/**
 * Which accounts in Slack and Discord are the person's. Acknowledging and
 * snoozing from chat need one. Slack accounts are found by Beacon; a
 * Discord account is linked by the person, with a code issued here.
 */
export function ChatAccounts() {
  const integrations = useIntegrations();
  const identities = useQuery({
    queryKey: identitiesKey,
    queryFn: ({ signal }) => unwrap(api.GET("/api/v1/me/identities", { signal })),
    // A code that was typed in the chat service makes the link there.
    refetchInterval: 10_000,
  });
  const chat = integrations.data?.integrations.filter((integration) => integration.type === "slack" || integration.type === "discord") ?? [];

  if (integrations.isPending || identities.isPending) {
    return <Loading label="Loading your chat accounts" />;
  }
  if (integrations.isError || identities.isError) {
    return <ErrorNotice error={integrations.error ?? identities.error} onRetry={() => void Promise.all([integrations.refetch(), identities.refetch()])} />;
  }
  if (chat.length === 0) {
    return <p className="muted">The configuration has no Slack or Discord integration, so there is nothing to link.</p>;
  }
  return (
    <ul className="override-list">
      {chat.map((integration) => (
        <ChatAccount key={integration.name} integration={integration} identity={identities.data.identities.find((identity) => identity.integration === integration.name)} />
      ))}
    </ul>
  );
}

function ChatAccount({ integration, identity }: { integration: Integration; identity: Identity | undefined }) {
  const queryClient = useQueryClient();
  const service = services[integration.type] ?? integration.type;
  const [code, setCode] = useState<LinkCode | null>(null);
  const [unlinking, setUnlinking] = useState(false);
  const issue = useMutation({
    mutationFn: () => unwrap(api.POST("/api/v1/me/identities/{integration}/link-code", { params: { path: { integration: integration.name } } })),
    onSuccess: setCode,
  });
  const unlink = useMutation({
    mutationFn: () => unwrap(api.DELETE("/api/v1/me/identities/{integration}", { params: { path: { integration: integration.name } } })),
    onSuccess: async () => {
      setUnlinking(false);
      await queryClient.invalidateQueries({ queryKey: identitiesKey });
    },
  });

  return (
    <li className="override-row">
      <div className="stack-small">
        <p>
          <strong>{service}</strong> <span className="muted">integration {integration.name}</span>{" "}
          {identity !== undefined ? <span className="badge status-resolved">Linked</span> : <span className="badge severity-warning">Not linked</span>}
        </p>
        {identity !== undefined && (
          <p>
            Your account is <code>{identity.external_id}</code>, {identity.origin === "email" ? "found by your email address" : "linked by you"} on <Time value={identity.linked_at} />.
          </p>
        )}
        {identity === undefined && integration.type === "slack" && (
          <p className="muted">Beacon looks for the Slack account that has your email address, by itself. It has not found one yet; until it does, you can resolve alerts from Slack and cannot acknowledge or snooze them there.</p>
        )}
        {identity === undefined && integration.type === "discord" && code === null && (
          <p className="muted">Link your Discord account with a one-time code. Until then you can resolve alerts from Discord and cannot acknowledge or snooze them there, and Beacon cannot page you there.</p>
        )}
        {identity === undefined && code !== null && (
          <div className="notice notice-info">
            <p>
              In Discord, type this command, in any channel where the Beacon application is. The code works once, until <Time value={code.expires_at} />, and is shown only now:
            </p>
            <p>
              <code className="secret">{code.command}</code>
            </p>
            <p className="hint">This page notices by itself when the account is linked.</p>
          </div>
        )}
        {issue.isError && <ErrorNotice error={issue.error} />}
      </div>
      {identity === undefined && integration.type === "discord" && (
        // The name says which integration, and begins with the words on the button.
        <button type="button" className="button" disabled={issue.isPending} aria-label={`${code === null ? "Get a link code" : "Get a new code"} for ${integration.name}`} onClick={() => issue.mutate()}>
          {code === null ? "Get a link code" : "Get a new code"}
        </button>
      )}
      {identity?.origin === "self" && (
        <button type="button" className="button" aria-label={`Remove the link to ${integration.name}`} onClick={() => setUnlinking(true)}>
          Remove the link
        </button>
      )}
      {unlinking && (
        <Dialog title={`Remove the link to your ${service} account?`} onClose={() => setUnlinking(false)}>
          <p>Beacon will no longer know that the account is yours: you cannot acknowledge or snooze from {service}, and are not paged there, until you link it again.</p>
          {unlink.isError && <ErrorNotice error={unlink.error} />}
          <div className="row">
            <button type="button" className="button button-primary" disabled={unlink.isPending} onClick={() => unlink.mutate()}>
              Remove the link
            </button>
            <button type="button" className="button" onClick={() => setUnlinking(false)}>
              Keep it
            </button>
          </div>
        </Dialog>
      )}
    </li>
  );
}
